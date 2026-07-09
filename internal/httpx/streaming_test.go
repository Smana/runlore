// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSecureStreamingClientNoFlatDeadline asserts the streaming client has no flat
// overall Timeout (so a long-but-active stream is never killed by a wall-clock
// budget) yet keeps the SSRF redirect guard.
func TestSecureStreamingClientNoFlatDeadline(t *testing.T) {
	c := SecureStreamingClient(5 * time.Second)
	if c.Timeout != 0 {
		t.Fatalf("streaming client must have no flat Timeout, got %v", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("streaming client must keep the DenyInternalRedirect guard")
	}
}

// TestStreamingSlowerThanOldDeadlineSurvives asserts a stream that keeps sending
// data past the old flat 2-minute budget is not aborted: the client reads the whole
// body even though each chunk is paced and the total exceeds a short header timeout.
// (We compress the "2 minutes" to a fast, deterministic paced stream.)
func TestStreamingSlowerThanOldDeadlineSurvives(t *testing.T) {
	const chunks = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("test server needs a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for i := 0; i < chunks; i++ {
			_, _ = io.WriteString(w, "data: chunk\n\n")
			fl.Flush()
			time.Sleep(20 * time.Millisecond) // paced, but never idle long enough to trip the idle timeout
		}
	}))
	defer srv.Close()

	// Small ResponseHeaderTimeout (the time-to-headers cap) — the body then streams
	// well past it. A flat Timeout would have killed this; the streaming client must not.
	c := SecureStreamingClient(200 * time.Millisecond)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Wrap with a generous idle timeout so only true stalls trip it.
	body := NewIdleTimeoutReader(resp.Body, time.Second, func() {})
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll over paced stream: %v", err)
	}
	if got := strings.Count(string(data), "chunk"); got != chunks {
		t.Fatalf("read %d chunks, want %d (paced stream was cut short)", got, chunks)
	}
}

// TestStreamingContextCancelAborts asserts cancelling the request context aborts the
// read promptly rather than blocking on an indefinitely open stream.
func TestStreamingContextCancelAborts(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		close(started)
		<-r.Context().Done() // hold the stream open until the client cancels
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := SecureStreamingClient(time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	<-started
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		done <- err
	}()
	select {
	case <-done:
		// aborted (with or without an error) — the read returned promptly
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not abort the streaming read promptly")
	}
}

// TestIdleTimeoutReaderTimesOut asserts an idle stream (no bytes for longer than the
// idle window) trips the idle timeout: the read returns an error and the onIdle hook
// fires (the provider passes the request's cancel func here).
func TestIdleTimeoutReaderTimesOut(t *testing.T) {
	pr, pw := io.Pipe()
	// Write one chunk, then go idle forever (never close, never write again).
	go func() {
		_, _ = pw.Write([]byte("data: first\n\n"))
		// deliberately block: no more writes, no close → the reader goes idle
		select {}
	}()

	idled := make(chan struct{}, 1)
	r := NewIdleTimeoutReader(pr, 50*time.Millisecond, func() { idled <- struct{}{} })

	buf := make([]byte, 64)
	// First read returns the first chunk.
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("first read should succeed, got %v", err)
	}
	// Second read blocks (no more data) and must trip the idle timeout.
	_, err := r.Read(buf)
	if err == nil {
		t.Fatal("idle read must return an error once the idle window elapses")
	}
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("idle read error = %v, want ErrIdleTimeout", err)
	}
	select {
	case <-idled:
	case <-time.After(time.Second):
		t.Fatal("onIdle hook did not fire on idle timeout")
	}
}

// TestDoWithRetryDoesNotRetryMidStream asserts DoWithRetry returns a streamed 200
// immediately (it must not buffer or retry a successful streaming response): the
// handler is invoked exactly once.
func TestDoWithRetryDoesNotRetryMidStream(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fl := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: x\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	c := SecureStreamingClient(time.Second)
	build := func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	}
	resp, err := DoWithRetry(context.Background(), c, 3, build)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if calls != 1 {
		t.Fatalf("streaming 200 must not be retried; handler called %d times", calls)
	}
}
