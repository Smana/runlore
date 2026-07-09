// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fixedNow returns a deterministic clock for the HTTP-date branch.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestParseRetryAfter(t *testing.T) {
	base := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	httpDate := func(d time.Duration) string { return base.Add(d).UTC().Format(http.TimeFormat) }

	tests := []struct {
		name   string
		header map[string]string
		want   time.Duration
	}{
		{"delta-seconds", map[string]string{"Retry-After": "5"}, 5 * time.Second},
		{"http-date-future", map[string]string{"Retry-After": httpDate(10 * time.Second)}, 10 * time.Second},
		{"retry-after-ms", map[string]string{"Retry-After-Ms": "1500"}, 1500 * time.Millisecond},
		{"ms-beats-seconds", map[string]string{"Retry-After": "5", "Retry-After-Ms": "1500"}, 1500 * time.Millisecond},
		{"missing", map[string]string{}, 0},
		{"malformed-seconds", map[string]string{"Retry-After": "soon"}, 0},
		{"malformed-ms", map[string]string{"Retry-After-Ms": "lots"}, 0},
		{"past-date", map[string]string{"Retry-After": httpDate(-30 * time.Second)}, 0},
		{"zero-seconds", map[string]string{"Retry-After": "0"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.header {
				h.Set(k, v)
			}
			got := parseRetryAfter(h, fixedNow(base))
			// HTTP-date arithmetic can be off by sub-second rounding; allow a tiny slack.
			if d := got - tc.want; d < -time.Second || d > time.Second {
				t.Fatalf("parseRetryAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	tests := []struct {
		name    string
		attempt int
		status  int               // 0 ⇒ nil response
		header  map[string]string // applied when status != 0
		want    time.Duration
	}{
		{"429-delta-seconds", 1, 429, map[string]string{"Retry-After": "3"}, 3 * time.Second},
		{"429-ms", 1, 429, map[string]string{"Retry-After-Ms": "2500"}, 2500 * time.Millisecond},
		{"429-over-cap-clamps", 1, 429, map[string]string{"Retry-After": "9999"}, maxDelay},
		{"429-no-header-falls-back", 1, 429, nil, baseBackoff},
		{"nil-resp-exponential", 1, 0, nil, baseBackoff},
		{"nil-resp-exponential-attempt2", 2, 0, nil, baseBackoff * 2},
		{"nil-resp-exponential-attempt3", 3, 0, nil, baseBackoff * 4},
		{"non-429-ignores-header", 1, 503, map[string]string{"Retry-After": "7"}, baseBackoff},
		{"huge-attempt-clamps", 40, 0, nil, maxDelay},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if tc.status != 0 {
				resp = &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: http.NoBody}
				for k, v := range tc.header {
					resp.Header.Set(k, v)
				}
			}
			got := retryDelay(tc.attempt, resp)
			if d := got - tc.want; d < -time.Second || d > time.Second {
				t.Fatalf("retryDelay(%d) = %v, want %v", tc.attempt, got, tc.want)
			}
		})
	}
}

func TestDoWithRetryHonorsRetryAfterHeader(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&n, 1) < 2 {
			w.Header().Set("Retry-After", "0") // 0 ⇒ no real wait, but routes through the parse path
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := DoWithRetry(context.Background(), srv.Client(), 3, func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("attempts = %d, want 2 (one 429 then 200)", got)
	}
}

func TestDoWithRetryCancelDuringHintedWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30") // honored wait far longer than the cancel
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the first response comes back, while we're in the hinted wait.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	resp, err := DoWithRetry(ctx, srv.Client(), 3, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if resp != nil {
		_ = resp.Body.Close()
	}
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cancellation took %v; should abort well before the 30s hint", elapsed)
	}
}

func TestDoWithRetrySucceedsAfterTransient(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := DoWithRetry(context.Background(), srv.Client(), 3, func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&n); got != 3 {
		t.Fatalf("attempts = %d, want 3 (two 500s then 200)", got)
	}
}

func TestDoWithRetryNoRetryOn4xx(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	resp, err := DoWithRetry(context.Background(), srv.Client(), 3, func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("attempts = %d, want 1 (4xx is not retried)", got)
	}
}
