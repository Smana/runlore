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
	"github.com/Smana/runlore/internal/thread"
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

func (c *capturingThreadHandler) HandleMention(_ context.Context, channel, root, author, text string, _ *thread.Context) {
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

func (b *blockingThreadHandler) HandleMention(_ context.Context, _, _, _, _ string, _ *thread.Context) {
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

// ctxAwareThreadHandler is a ThreadHandler whose HandleMention blocks until EITHER
// the test releases it (release) or its own context is cancelled (ctxDone fires) —
// deterministic on both axes via channels, never a wall-clock sleep. It backs the
// mention-handler-timeout and shutdown-drain tests below.
type ctxAwareThreadHandler struct {
	entered chan struct{}
	release chan struct{}
	ctxDone chan struct{}
}

func newCtxAwareThreadHandler() *ctxAwareThreadHandler {
	return &ctxAwareThreadHandler{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		ctxDone: make(chan struct{}, 1),
	}
}

func (h *ctxAwareThreadHandler) HandleMention(ctx context.Context, _, _, _, _ string, _ *thread.Context) {
	h.entered <- struct{}{}
	select {
	case <-ctx.Done():
		h.ctxDone <- struct{}{}
	case <-h.release:
	}
}

func (h *ctxAwareThreadHandler) Busy(context.Context, string, string) {}

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

// TestEventsMentionHandlerContextTimesOut is the regression test for the
// unbounded-lifetime defect: handleSlackEvent used to hand the detached
// handler a context.WithoutCancel-derived context with NO deadline, so a
// degraded/rate-limited forge could hold a pool slot indefinitely instead of
// for as long as the mention itself takes. s.eventDispatcher is swapped for one
// with a short timeout here so the test proves the real wiring deterministically,
// without waiting out the production mentionHandlerTimeout (2 minutes) for real.
func TestEventsMentionHandlerContextTimesOut(t *testing.T) {
	h := newCtxAwareThreadHandler()
	s := newEventServer(t, h)
	s.eventDispatcher = thread.NewDispatcher(maxConcurrentMentions, 20*time.Millisecond, discardLog)
	t.Cleanup(func() { close(h.release) }) // safety net if the handler is somehow still blocked

	body := `{"type":"event_callback","event_id":"Etimeout","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"C1","thread_ts":"111.222"}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	select {
	case <-h.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}
	select {
	case <-h.ctxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler outran its timeout without its context being cancelled — an unbounded " +
			"detached handler can hold a pool slot for as long as a degraded forge stays degraded")
	}
}

// TestDrainWaitsForInFlightHandler is the regression test for the no-shutdown-drain
// defect: httpSrv.Shutdown only waits for request HANDLERS, and handleSlackEvent's
// detached goroutine has already escaped that scope by the time it does its actual
// work — so without Server.Drain, SIGTERM could kill the process mid-OpenPR with the
// human told nothing. The handler is released only after Drain is proven to still be
// blocked, so the assertion cannot pass vacuously.
func TestDrainWaitsForInFlightHandler(t *testing.T) {
	h := newCtxAwareThreadHandler()
	s := newEventServer(t, h)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(h.release) }) }
	t.Cleanup(release)

	body := `{"type":"event_callback","event_id":"Edrain","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"C1","thread_ts":"111.222"}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case <-h.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	drainDone := make(chan struct{})
	go func() {
		s.Drain(context.Background())
		close(drainDone)
	}()

	select {
	case <-drainDone:
		t.Fatal("Drain returned before the in-flight handler finished — shutdown would have abandoned it")
	case <-time.After(200 * time.Millisecond):
	}

	release() // let the handler finish

	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after the in-flight handler finished")
	}
}

// TestDrainIsBounded proves Drain never blocks shutdown forever: with the handler
// still running and never released, Drain must still return once its OWN ctx's
// deadline fires.
func TestDrainIsBounded(t *testing.T) {
	h := newCtxAwareThreadHandler()
	s := newEventServer(t, h)
	t.Cleanup(func() { close(h.release) }) // release the still-blocked handler so it doesn't leak past the test

	body := `{"type":"event_callback","event_id":"Ebound","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"C1","thread_ts":"111.222"}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case <-h.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	dctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Drain(dctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Drain took %v to return after its bound expired — it must never block shutdown forever", elapsed)
	}
}

// syncLogBuf is a concurrency-safe io.Writer that captures log output so a test
// can inspect exactly what was logged (slog handlers may be written to from
// multiple goroutines).
type syncLogBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestDrainAwaitsBusyDispatcherConcurrentlyWithEventDispatcher is the
// regression test for the sequential-drain defect: Server.Drain used to drain
// eventDispatcher and busyDispatcher one after another against the SAME ctx.
// Once eventDispatcher's wait hit ctx.Done() (mention work outlasting the
// grace period — precisely the degraded-forge scenario the drain exists for),
// busyDispatcher.Drain(ctx) was handed an already-expired context and returned
// almost instantly WITHOUT waiting for its own real in-flight work — while
// still logging "drain timed out with work still in flight" for work it never
// actually awaited.
//
// This constructs exactly that state: eventDispatcher holds work that never
// completes (so its Drain call is guaranteed to hit ctx.Done()), and
// busyDispatcher holds REAL in-flight work that finishes with a huge margin
// inside the grace period — but only if busyDispatcher gets to wait for it
// concurrently with eventDispatcher, rather than being queued behind it.
func TestDrainAwaitsBusyDispatcherConcurrentlyWithEventDispatcher(t *testing.T) {
	logBuf := &syncLogBuf{}
	s := New(nil, Actions{}, nil, nil, nil, nil, slog.New(slog.NewTextHandler(logBuf, nil)))

	// eventDispatcher's one in-flight handler never completes on its own — it
	// only stops when Drain gives up on its ctx, matching "mention work
	// outlasts the grace period".
	eventRelease := make(chan struct{})
	t.Cleanup(func() { close(eventRelease) })
	if !s.eventDispatcher.Go(context.Background(), func(context.Context) { <-eventRelease }) {
		t.Fatal("eventDispatcher.Go refused")
	}

	// busyDispatcher's one in-flight handler is REAL work that finishes on its
	// own well before the grace period ends.
	busyStarted := make(chan struct{})
	busyFinished := make(chan struct{})
	busyRelease := make(chan struct{})
	if !s.busyDispatcher.Go(context.Background(), func(context.Context) {
		close(busyStarted)
		<-busyRelease
		close(busyFinished)
	}) {
		t.Fatal("busyDispatcher.Go refused")
	}
	<-busyStarted

	const grace = 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	// Release the busy work at a fixed, generous margin before the grace
	// period ends: a Drain that gives busyDispatcher independent access to the
	// full ctx budget has ~110ms of slack to observe this; a Drain that only
	// starts waiting on busyDispatcher AFTER eventDispatcher has already
	// exhausted that budget gets none, no matter how early the busy work
	// finished.
	go func() {
		time.Sleep(40 * time.Millisecond)
		close(busyRelease)
	}()

	drainDone := make(chan struct{})
	go func() {
		s.Drain(ctx)
		close(drainDone)
	}()

	select {
	case <-busyFinished:
	case <-time.After(2 * time.Second):
		t.Fatal("busy work never finished")
	}
	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain never returned")
	}

	// eventDispatcher's work never finished, so exactly one legitimate timeout
	// log is expected. A second occurrence means busyDispatcher was ALSO
	// handed an expired context and logged a spurious timeout for work that
	// had already completed on its own — the exact defect this test guards
	// against.
	const timeoutMsg = "drain timed out with work still in flight"
	if got := strings.Count(logBuf.String(), timeoutMsg); got != 1 {
		t.Fatalf("%q logged %d times, want exactly 1 (only eventDispatcher's genuine timeout); log:\n%s",
			timeoutMsg, got, logBuf.String())
	}
}
