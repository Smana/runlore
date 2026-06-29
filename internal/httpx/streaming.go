package httpx

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrIdleTimeout is returned by an IdleTimeoutReader when no bytes arrive within the
// configured idle window — a stalled stream, distinct from a clean io.EOF.
var ErrIdleTimeout = errors.New("idle stream timeout")

// SecureStreamingClient returns an http.Client tuned for consuming a long-lived SSE
// stream. Unlike SecureClient it has NO flat overall Timeout — a flat deadline would
// kill a legitimate long completion mid-stream — and instead relies on the request
// context (for cancellation) plus an idle/read timeout applied by the caller (see
// NewIdleTimeoutReader). It keeps the DenyInternalRedirect SSRF guard.
//
// responseHeaderTimeout bounds the time spent waiting for the response headers
// (time-to-first-byte at the protocol level) so a backend that accepts the
// connection but never replies cannot hang the request before streaming begins;
// once headers arrive, the body may stream for as long as it stays active.
func SecureStreamingClient(responseHeaderTimeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{
		// No flat Timeout: ctx + the idle reader bound the request instead.
		Transport:     tr,
		CheckRedirect: DenyInternalRedirect,
	}
}

// idleTimeoutReader wraps an io.ReadCloser and bounds the gap between successful
// reads: if a single Read blocks longer than idle, Read returns ErrIdleTimeout and
// invokes onIdle once (the provider passes the request's context cancel func, so the
// stalled underlying HTTP read is also torn down). A single pump goroutine owns an
// internal buffer and performs the blocking reads, so the timer can fire even while
// the underlying Read is blocked — without ever sharing the caller's buffer across
// the goroutine boundary (which would race a timed-out read against a late write).
type idleTimeoutReader struct {
	idle   time.Duration
	onIdle func()
	rc     io.ReadCloser

	results chan readResult // one entry per completed pump read
	once    sync.Once       // onIdle fires at most once

	// pending holds bytes read by the pump but not yet fully copied to the caller.
	pending []byte
	pendErr error
}

type readResult struct {
	data []byte
	err  error
}

// NewIdleTimeoutReader wraps rc so that any Read which stalls for longer than idle
// fails with ErrIdleTimeout and calls onIdle once. idle <= 0 disables the timeout
// (rc is returned unwrapped). onIdle may be nil.
func NewIdleTimeoutReader(rc io.ReadCloser, idle time.Duration, onIdle func()) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	r := &idleTimeoutReader{
		idle:    idle,
		onIdle:  onIdle,
		rc:      rc,
		results: make(chan readResult, 1),
	}
	go r.pump()
	return r
}

// pump reads the wrapped stream into freshly-allocated buffers and delivers each
// chunk. It exits after delivering the first error (EOF or a read error), or when a
// Close unblocks its in-flight Read.
func (r *idleTimeoutReader) pump() {
	for {
		buf := make([]byte, 32*1024)
		n, err := r.rc.Read(buf)
		r.results <- readResult{data: buf[:n], err: err}
		if err != nil {
			return
		}
	}
}

// Read returns the next chunk, or ErrIdleTimeout if no chunk arrives within idle.
func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	// Drain any buffered remainder from a previous oversized chunk first.
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		if len(r.pending) == 0 && r.pendErr != nil {
			err := r.pendErr
			r.pendErr = nil
			return n, err
		}
		return n, nil
	}
	timer := time.NewTimer(r.idle)
	defer timer.Stop()
	select {
	case res, ok := <-r.results:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, res.data)
		if n < len(res.data) {
			// Caller's buffer was smaller than the chunk: stash the rest (and defer
			// the chunk's error until the remainder is consumed).
			r.pending = res.data[n:]
			r.pendErr = res.err
			return n, nil
		}
		return n, res.err
	case <-timer.C:
		r.tripIdle()
		return 0, ErrIdleTimeout
	}
}

func (r *idleTimeoutReader) tripIdle() {
	r.once.Do(func() {
		if r.onIdle != nil {
			r.onIdle()
		}
		_ = r.rc.Close() // unblock the pump's in-flight Read
	})
}

// Close closes the underlying reader and consumes the once-guard so a subsequent
// idle expiry cannot fire onIdle after an explicit close.
func (r *idleTimeoutReader) Close() error {
	r.once.Do(func() {})
	return r.rc.Close()
}
