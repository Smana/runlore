// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturingThreadHandler struct {
	mu    sync.Mutex
	calls []struct{ channel, root, author, text string }
	done  chan struct{}
}

func newCapturingThreadHandler() *capturingThreadHandler {
	return &capturingThreadHandler{done: make(chan struct{}, 16)}
}

func (c *capturingThreadHandler) HandleMention(_ context.Context, channel, root, author, text string) {
	c.mu.Lock()
	c.calls = append(c.calls, struct{ channel, root, author, text string }{channel, root, author, text})
	c.mu.Unlock()
	c.done <- struct{}{}
}

func (c *capturingThreadHandler) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
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
