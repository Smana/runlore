// Package httpx provides small HTTP helpers shared across providers.
package httpx

import (
	"context"
	"net/http"
	"time"
)

// DoWithRetry issues build()'s request with bounded exponential backoff, retrying
// on a network error, HTTP 429, or 5xx — a transient upstream failure shouldn't
// fail the whole investigation. Other 4xx and 2xx return immediately. build is
// invoked fresh each attempt so a consumed request body is rebuilt. The backoff
// respects ctx cancellation. The last response (a 429/5xx) is returned so the
// caller can surface its status.
func DoWithRetry(ctx context.Context, client *http.Client, attempts int, build func() (*http.Request, error)) (*http.Response, error) {
	if attempts < 1 {
		attempts = 1
	}
	var resp *http.Response
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(200*(1<<uint(i-1))) * time.Millisecond):
			}
		}
		var req *http.Request
		if req, err = build(); err != nil {
			return nil, err
		}
		resp, err = client.Do(req)
		if err != nil {
			continue // network error: retry
		}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return resp, nil // success or non-retryable
		}
		if i < attempts-1 {
			_ = resp.Body.Close() // retrying: discard this body
		}
	}
	if err != nil {
		return nil, err
	}
	return resp, nil
}
