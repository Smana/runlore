// SPDX-License-Identifier: Apache-2.0

package clientcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// writeStream writes chunks to w as an incrementally flushed 200 stream, so the
// client sees real streaming delivery rather than one buffered body.
func writeStream(t *testing.T, w http.ResponseWriter, chunks ...string) {
	t.Helper()
	fl, ok := w.(http.Flusher)
	if !ok {
		t.Error("server needs http.Flusher")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	for _, c := range chunks {
		_, _ = io.WriteString(w, c)
		fl.Flush()
	}
}

// textAccumulate is the simplest accumulate: read the whole stream into Text.
// It stands in for a provider's SSE fold where only the pipeline is under test.
func textAccumulate(r io.Reader) (providers.CompletionResponse, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return providers.CompletionResponse{}, err
	}
	return providers.CompletionResponse{Text: string(b)}, nil
}

// baseFor builds a Base pointed at the test server, as a provider constructor would.
func baseFor(url string) Base {
	return NewBase(url, "", "model-x", "key-y", 0)
}

// wireReq is a stand-in provider request body.
type wireReq struct {
	Prompt string `json:"prompt"`
}

// streamRequest builds a Request the way a provider does: full URL, JSON body,
// injected headers, and an ErrorDetail that recognizes nothing (overridden by
// tests that exercise the detail path).
func streamRequest(url string) Request {
	return Request{
		Op:   "chat",
		URL:  url + "/v1/chat",
		Body: wireReq{Prompt: "hi"},
		SetHeaders: func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Api-Key", "key-y")
		},
		ErrorDetail: func([]byte) string { return "" },
	}
}

// TestStreamSendsRequestAndAccumulates asserts the pipeline end to end on the
// happy path: the wire body is the marshaled req.Body, method/URL/headers are
// as injected, and accumulate receives the full incrementally-flushed response
// body (through the idle-timeout guard) with its result returned untouched.
func TestStreamSendsRequestAndAccumulates(t *testing.T) {
	var (
		mu                                sync.Mutex
		gotMethod, gotPath, gotCT, gotKey string
		gotBody                           []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotPath, gotCT, gotKey, gotBody = r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("X-Api-Key"), body
		mu.Unlock()
		writeStream(t, w, "data: hel", "lo\n\n")
	}))
	defer srv.Close()

	b := baseFor(srv.URL)
	resp, err := b.Stream(context.Background(), streamRequest(srv.URL), textAccumulate)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if want := "data: hello\n\n"; resp.Text != want {
		t.Errorf("accumulated body = %q, want %q", resp.Text, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/chat" {
		t.Errorf("path = %q, want /v1/chat", gotPath)
	}
	if gotCT != "application/json" || gotKey != "key-y" {
		t.Errorf("SetHeaders not applied: content-type %q, api key %q", gotCT, gotKey)
	}
	if want := `{"prompt":"hi"}`; string(gotBody) != want {
		t.Errorf("wire body = %q, want %q", gotBody, want)
	}
}

// TestStreamAccumulateErrorPassesThrough asserts accumulate's error is returned
// verbatim: the provider folds surface their own rich errors (e.g. "stream
// ended before message_stop") and the pipeline must not re-wrap or swallow them.
func TestStreamAccumulateErrorPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeStream(t, w, "data: x\n\n")
	}))
	defer srv.Close()

	sentinel := errors.New("stream ended before message_stop")
	b := baseFor(srv.URL)
	_, err := b.Stream(context.Background(), streamRequest(srv.URL), func(io.Reader) (providers.CompletionResponse, error) {
		return providers.CompletionResponse{}, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Stream error = %v, want the accumulate sentinel", err)
	}
}

// TestStreamMarshalFailureNeverSendsRequest asserts an unmarshalable body fails
// fast with a "marshal request" error before any bytes hit the network.
func TestStreamMarshalFailureNeverSendsRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req := streamRequest(srv.URL)
	req.Body = make(chan int) // json.Marshal cannot encode a channel
	b := baseFor(srv.URL)
	_, err := b.Stream(context.Background(), req, textAccumulate)
	if err == nil || !strings.Contains(err.Error(), "marshal request") {
		t.Errorf("want a marshal error, got %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("server was hit %d times; a marshal failure must not send a request", got)
	}
}

// TestStreamBadURL asserts a malformed URL surfaces as a wrapped "<op> request"
// error from the build path rather than a panic or a bare transport error.
func TestStreamBadURL(t *testing.T) {
	b := baseFor("")
	req := streamRequest("")
	req.URL = "://not-a-url"
	_, err := b.Stream(context.Background(), req, textAccumulate)
	if err == nil || !strings.Contains(err.Error(), "chat request:") {
		t.Errorf("want a wrapped request error naming the op, got %v", err)
	}
}

// TestStreamStatusClassification asserts the permanence/retry contract for
// non-200 responses: a 4xx other than 429 is permanent (the investigation
// workqueue must drop a doomed request instead of requeuing it forever) and is
// never retried, while 429 and 5xx stay transient and are retried up to
// retryAttempts before surfacing.
func TestStreamStatusClassification(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		wantPermanent bool
		wantAttempts  int32
	}{
		{"400 invalid request is permanent, no retry", http.StatusBadRequest, true, 1},
		{"401 bad auth is permanent, no retry", http.StatusUnauthorized, true, 1},
		{"404 unknown model is permanent, no retry", http.StatusNotFound, true, 1},
		{"429 rate limit is transient and retried", http.StatusTooManyRequests, false, retryAttempts},
		{"500 upstream failure is transient and retried", http.StatusInternalServerError, false, retryAttempts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				// Keep the 429 retry waits near-zero via the server hint; a 5xx
				// uses the client's own exponential backoff (still test-fast).
				w.Header().Set("Retry-After-Ms", "1")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			b := baseFor(srv.URL)
			_, err := b.Stream(context.Background(), streamRequest(srv.URL), textAccumulate)
			if err == nil {
				t.Fatal("want an error for a non-200 response")
			}
			if got := providers.IsPermanent(err); got != tc.wantPermanent {
				t.Errorf("IsPermanent = %v, want %v (err: %v)", got, tc.wantPermanent, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("status %d", tc.status)) {
				t.Errorf("error should carry the status: %v", err)
			}
			if !strings.Contains(err.Error(), "chat") {
				t.Errorf("error should carry the op name: %v", err)
			}
			if got := attempts.Load(); got != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", got, tc.wantAttempts)
			}
		})
	}
}

// TestStreamErrorDetailAndBoundedBody asserts a non-200 error carries the two
// fields that make a 4xx diagnosable — the upstream request-id and the
// provider's sanitized detail suffix — while the raw body is never echoed and
// the body read handed to ErrorDetail is bounded (an attacker-sized error body
// must not be slurped into memory).
func TestStreamErrorDetailAndBoundedBody(t *testing.T) {
	huge := strings.Repeat("A", maxErrorBody*4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Request-Id", "req-42")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, huge)
	}))
	defer srv.Close()

	// gotLen is written synchronously inside the Stream call — no lock needed.
	gotLen := -1
	req := streamRequest(srv.URL)
	req.ErrorDetail = func(body []byte) string {
		gotLen = len(body)
		return Detail("invalid_request_error", "prompt too long")
	}
	b := baseFor(srv.URL)
	_, err := b.Stream(context.Background(), req, textAccumulate)
	if err == nil {
		t.Fatal("want an error for a 400 response")
	}
	msg := err.Error()
	if !strings.Contains(msg, `(request-id "req-42")`) {
		t.Errorf("error should carry the request-id: %q", msg)
	}
	if !strings.Contains(msg, ": invalid_request_error: prompt too long") {
		t.Errorf("error should carry the sanitized detail suffix: %q", msg)
	}
	if strings.Contains(msg, "AAAA") {
		t.Errorf("error echoed the raw upstream body: %q", msg)
	}
	if gotLen != maxErrorBody {
		t.Errorf("ErrorDetail saw %d body bytes, want the bounded %d", gotLen, maxErrorBody)
	}
}

// TestStreamRetryThenSuccess asserts a transient 500 on the first attempt is
// retried into a successful stream, and that the retry carries the full request
// body and headers again — the body reader is consumed per attempt, so build()
// must reconstruct the request from scratch.
func TestStreamRetryThenSuccess(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
		keys   []string
	)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		keys = append(keys, r.Header.Get("X-Api-Key"))
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeStream(t, w, "data: ok\n\n")
	}))
	defer srv.Close()

	b := baseFor(srv.URL)
	resp, err := b.Stream(context.Background(), streamRequest(srv.URL), textAccumulate)
	if err != nil {
		t.Fatalf("Stream after a transient 500: %v", err)
	}
	if want := "data: ok\n\n"; resp.Text != want {
		t.Errorf("accumulated body = %q, want %q", resp.Text, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want 2", len(bodies))
	}
	for i, got := range bodies {
		if want := `{"prompt":"hi"}`; got != want {
			t.Errorf("attempt %d body = %q, want %q (the body must be rebuilt per attempt)", i+1, got, want)
		}
	}
	for i, k := range keys {
		if k != "key-y" {
			t.Errorf("attempt %d lost its headers: api key %q", i+1, k)
		}
	}
}

// TestStreamContextCancelMidStream asserts cancelling the caller's context
// while the body is streaming surfaces promptly as a read error inside
// accumulate (via the child context wired into the HTTP request) instead of
// blocking forever on an open stream.
func TestStreamContextCancelMidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeStream(t, w, "data: first\n\n")
		<-r.Context().Done() // hold the stream open until the client aborts
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := baseFor(srv.URL)
	done := make(chan error, 1)
	go func() {
		_, err := b.Stream(ctx, streamRequest(srv.URL), func(r io.Reader) (providers.CompletionResponse, error) {
			buf := make([]byte, 64)
			if _, err := r.Read(buf); err != nil { // the first chunk arrives fine
				return providers.CompletionResponse{}, fmt.Errorf("first chunk: %w", err)
			}
			cancel() // the caller's deadline/cancellation fires mid-stream
			if _, err := io.ReadAll(r); err != nil {
				return providers.CompletionResponse{}, err
			}
			return providers.CompletionResponse{}, errors.New("read past cancellation without an error")
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error after mid-stream cancellation")
		}
		if strings.Contains(err.Error(), "read past cancellation") {
			t.Fatalf("the stream kept flowing after cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not abort the stream promptly")
	}
}

// TestStreamWithSSEEventsEndToEnd drives the two seams together exactly as a
// provider does: Stream hands the idle-guarded body to an accumulate folding
// SSEEvents. Keep-alive comments, event: fields, and chunk boundaries that
// split an event mid-line must all be invisible to the fold.
func TestStreamWithSSEEventsEndToEnd(t *testing.T) {
	chunks := []string{
		": keep-alive\n\n",
		"data: {\"type\":\"delta\",\"te", // an event split across two flushes
		"xt\":\"hel\"}\n\n",
		"event: content\ndata: {\"type\":\"delta\",\"text\":\"lo\"}\n\n",
		"data: {\"type\":\"stop\"}\n\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeStream(t, w, chunks...)
	}))
	defer srv.Close()

	type wireEvent struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	acc := func(r io.Reader) (providers.CompletionResponse, error) {
		var out providers.CompletionResponse
		sawStop := false
		for ev, err := range SSEEvents[wireEvent](r) {
			if err != nil {
				return providers.CompletionResponse{}, err
			}
			switch ev.Type {
			case "delta":
				out.Text += ev.Text
			case "stop":
				sawStop = true
			}
		}
		if !sawStop {
			return providers.CompletionResponse{}, errors.New("stream ended before stop")
		}
		return out, nil
	}

	b := baseFor(srv.URL)
	resp, err := b.Stream(context.Background(), streamRequest(srv.URL), acc)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Text != "hello" {
		t.Errorf("folded text = %q, want %q", resp.Text, "hello")
	}
}
