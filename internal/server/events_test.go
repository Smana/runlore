// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/runlore/internal/telemetry"
)

type capturingThreadHandler struct {
	mu       sync.Mutex
	calls    []struct{ channel, root, author, text string }
	busy     []struct{ channel, root string }
	done     chan struct{}
	busyDone chan struct{}
}

func newCapturingThreadHandler() *capturingThreadHandler {
	return &capturingThreadHandler{done: make(chan struct{}, 16), busyDone: make(chan struct{}, 16)}
}

func (c *capturingThreadHandler) HandleMention(_ context.Context, channel, root, author, text string) {
	c.mu.Lock()
	c.calls = append(c.calls, struct{ channel, root, author, text string }{channel, root, author, text})
	c.mu.Unlock()
	c.done <- struct{}{}
}

// Busy records the call so tests can assert the happy path never triggers it.
func (c *capturingThreadHandler) Busy(_ context.Context, channel, root string) {
	c.mu.Lock()
	c.busy = append(c.busy, struct{ channel, root string }{channel, root})
	c.mu.Unlock()
	c.busyDone <- struct{}{}
}

func (c *capturingThreadHandler) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *capturingThreadHandler) busyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.busy)
}

// blockingThreadHandler is a ThreadHandler whose HandleMention blocks until the test
// releases it, so a saturation test can deterministically fill every pool slot instead
// of racing against goroutine scheduling.
type blockingThreadHandler struct {
	entered  chan struct{}
	release  chan struct{}
	busyDone chan struct{}

	mu   sync.Mutex
	busy []struct{ channel, root string }
}

func newBlockingThreadHandler(capacity int) *blockingThreadHandler {
	return &blockingThreadHandler{
		entered:  make(chan struct{}, capacity),
		release:  make(chan struct{}),
		busyDone: make(chan struct{}, capacity),
	}
}

func (b *blockingThreadHandler) HandleMention(_ context.Context, _, _, _, _ string) {
	b.entered <- struct{}{}
	<-b.release
}

func (b *blockingThreadHandler) Busy(_ context.Context, channel, root string) {
	b.mu.Lock()
	b.busy = append(b.busy, struct{ channel, root string }{channel, root})
	b.mu.Unlock()
	b.busyDone <- struct{}{}
}

func (b *blockingThreadHandler) busyCalls() []struct{ channel, root string } {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]struct{ channel, root string }(nil), b.busy...)
}

// meteredInstruments installs a REAL SDK meter provider backed by a manual reader and
// returns the instrument set bound to it, plus a reader that sums an int64 counter by
// its exported series name (0, false when the series was never recorded). The provider
// is global, so a test using this must not run in parallel with another that does; the
// cleanup restores the no-op provider.
func meteredInstruments(t *testing.T) (*telemetry.Metrics, func(series string) (int64, bool)) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	return telemetry.NewMetrics(), func(series string) (int64, bool) {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if md.Name != series {
					continue
				}
				sum, ok := md.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("series %q is not an int64 sum (%T)", series, md.Data)
				}
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				return total, true
			}
		}
		return 0, false
	}
}

const testSigningSecret = "s3cr3t"

func signedEventRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	_, _ = mac.Write([]byte("v0:" + ts + ":" + body))
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func newEventServer(t *testing.T, h ThreadHandler) *Server {
	t.Helper()
	return New(nil, Actions{SlackSecret: testSigningSecret, Threads: h}, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func waitForMention(t *testing.T, h *capturingThreadHandler) {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the mention to be dispatched")
	}
}

func TestEventsAppMentionDispatches(t *testing.T) {
	h := newCapturingThreadHandler()
	s := newEventServer(t, h)

	body := `{"type":"event_callback","event_id":"Ev1","event":{"type":"app_mention","user":"U1","text":"<@U0BOT> note: x","channel":"C1","ts":"333.444","thread_ts":"111.222"}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	waitForMention(t, h)
	if h.calls[0].root != "111.222" {
		t.Errorf("root = %q, want 111.222 (the thread root, not the reply ts)", h.calls[0].root)
	}
	if h.calls[0].channel != "C1" || h.calls[0].author != "U1" {
		t.Errorf("dispatch = %+v, want channel C1 author U1", h.calls[0])
	}
	if h.calls[0].text != "<@U0BOT> note: x" {
		t.Errorf("text = %q, want the raw text (the responder strips the mention)", h.calls[0].text)
	}
	if h.busyCount() != 0 {
		t.Errorf("busy calls = %d, want 0 — the happy path must never tell the human the pool is busy", h.busyCount())
	}
}

func TestEventsURLVerification(t *testing.T) {
	s := newEventServer(t, newCapturingThreadHandler())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, `{"type":"url_verification","challenge":"abc123"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "abc123" {
		t.Fatalf("body = %q, want the challenge echoed back", got)
	}
}

func TestEventsRejectsBadSignature(t *testing.T) {
	h := newCapturingThreadHandler()
	s := newEventServer(t, h)

	req := signedEventRequest(t, `{"type":"event_callback","event_id":"Ev1","event":{"type":"app_mention","thread_ts":"111.222"}}`)
	req.Header.Set("X-Slack-Signature", "v0=deadbeef")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if h.count() != 0 {
		t.Fatal("an unsigned event must never be dispatched")
	}
}

func TestEventsDisabledWhenNoHandlerWired(t *testing.T) {
	s := New(nil, Actions{SlackSecret: testSigningSecret}, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, `{"type":"url_verification","challenge":"abc"}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when thread capture is off", rec.Code)
	}
}

func TestEventsRetryIsDeduped(t *testing.T) {
	h := newCapturingThreadHandler()
	s := newEventServer(t, h)

	body := `{"type":"event_callback","event_id":"Ev-dup","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"C1","thread_ts":"111.222"}}`
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, signedEventRequest(t, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200 (a retry must still be acked)", i, rec.Code)
		}
	}
	waitForMention(t, h)
	time.Sleep(100 * time.Millisecond)
	if h.count() != 1 {
		t.Fatalf("dispatches = %d, want 1 — Slack retries must not file the note repeatedly", h.count())
	}
}

func TestEventsIgnoresBotAndNonThreadAndNonMention(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"bot author", `{"type":"event_callback","event_id":"E1","event":{"type":"app_mention","bot_id":"B1","text":"note: x","channel":"C1","thread_ts":"111.222"}}`},
		{"not in a thread", `{"type":"event_callback","event_id":"E2","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"C1","ts":"111.222"}}`},
		{"not an app_mention", `{"type":"event_callback","event_id":"E3","event":{"type":"message","user":"U1","text":"note: x","channel":"C1","thread_ts":"111.222"}}`},
		{"no user and no bot", `{"type":"event_callback","event_id":"E4","event":{"type":"app_mention","text":"note: x","channel":"C1","thread_ts":"111.222"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCapturingThreadHandler()
			s := newEventServer(t, h)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, signedEventRequest(t, tt.body))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — an ignored event is still acked so Slack stops retrying", rec.Code)
			}
			time.Sleep(50 * time.Millisecond)
			if h.count() != 0 {
				t.Fatalf("dispatches = %d, want 0", h.count())
			}
		})
	}
}

func TestEventsStaleTimestampRejected(t *testing.T) {
	h := newCapturingThreadHandler()
	s := newEventServer(t, h)

	body := `{"type":"event_callback","event_id":"E1","event":{"type":"app_mention","user":"U1","thread_ts":"111.222"}}`
	old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	_, _ = mac.Write([]byte("v0:" + old + ":" + body))
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", old)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a replayed request must be rejected", rec.Code)
	}
	if h.count() != 0 {
		t.Fatal("a replayed event must never be dispatched")
	}
}

// fillMentionPool sends maxConcurrentMentions app_mention deliveries to s and blocks
// (deterministically, via h.entered) until every one of them has actually started
// running inside the handler pool — i.e. every slot is held. It registers a cleanup
// that releases the blocked handlers so no goroutine leaks past the test.
func fillMentionPool(t *testing.T, s *Server, h *blockingThreadHandler, tsPrefix string) {
	t.Helper()
	for i := 0; i < maxConcurrentMentions; i++ {
		body := fmt.Sprintf(`{"type":"event_callback","event_id":"E%s%d","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"C1","thread_ts":"%s.%d"}}`,
			tsPrefix, i, tsPrefix, i)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, signedEventRequest(t, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("fill %d: status = %d, want 200", i, rec.Code)
		}
	}
	for i := 0; i < maxConcurrentMentions; i++ {
		select {
		case <-h.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for handler %d to start — the pool did not fill", i)
		}
	}
	t.Cleanup(func() { close(h.release) })
}

// TestEventsSaturatedPoolNotifiesBusyAndCountsMetric is the regression test for the
// silent-drop defect: acking a mention Slack will never retry, then dropping it with
// only a log line, permanently loses the human's note. Saturation must instead (a)
// tell the human via Busy so they know to retype, and (b) increment a counter — not a
// silent return.
func TestEventsSaturatedPoolNotifiesBusyAndCountsMetric(t *testing.T) {
	h := newBlockingThreadHandler(maxConcurrentMentions)
	m, read := meteredInstruments(t)
	s := New(nil, Actions{SlackSecret: testSigningSecret, Threads: h, Metrics: m}, nil, nil, nil, nil, discardLog)
	fillMentionPool(t, s, h, "fill")

	// One more mention arrives while every slot is held: it must be dropped, not
	// queued or blocked — but the ack must still happen, the human must be told, and
	// the drop must be counted.
	overflow := `{"type":"event_callback","event_id":"Eoverflow","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"Coverflow","thread_ts":"999.999"}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, overflow))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a dropped mention must still be acked (Slack must never retry it)", rec.Code)
	}

	select {
	case <-h.busyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Busy was never called for the dropped mention — the human was left silently believing it worked")
	}
	calls := h.busyCalls()
	if len(calls) != 1 {
		t.Fatalf("busy calls = %d, want 1", len(calls))
	}
	if calls[0].channel != "Coverflow" || calls[0].root != "999.999" {
		t.Errorf("Busy(channel=%q, root=%q), want channel=Coverflow root=999.999", calls[0].channel, calls[0].root)
	}

	if got, ok := read("runlore_mentions_dropped_on_saturation_total"); !ok || got != 1 {
		t.Fatalf("runlore_mentions_dropped_on_saturation_total = %d (recorded=%v), want 1", got, ok)
	}
}

// TestEventsSaturatedPoolWithNilMetricsDoesNotPanic pins the "nil Metrics is a no-op,
// not a panic" contract on the exact code path that increments the counter.
func TestEventsSaturatedPoolWithNilMetricsDoesNotPanic(t *testing.T) {
	h := newBlockingThreadHandler(maxConcurrentMentions)
	s := New(nil, Actions{SlackSecret: testSigningSecret, Threads: h}, nil, nil, nil, nil, discardLog) // Metrics left nil
	fillMentionPool(t, s, h, "nil")

	overflow := `{"type":"event_callback","event_id":"Noverflow","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"C1","thread_ts":"999.999"}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, overflow)) // must not panic with nil Metrics
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case <-h.busyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Busy was never called for the dropped mention")
	}
}
