// SPDX-License-Identifier: Apache-2.0

// Package httpx provides small HTTP helpers shared across providers.
package httpx

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

const (
	// baseBackoff is the first exponential-backoff step; the schedule is
	// baseBackoff·2^(attempt-1), matching the prior fixed behavior.
	baseBackoff = 200 * time.Millisecond
	// maxDelay caps any wait — whether server-hinted or computed — so a hostile
	// or buggy Retry-After (e.g. an hour) can't pin an investigation worker.
	maxDelay = 30 * time.Second
)

// DoWithRetry issues build()'s request with bounded backoff, retrying on a
// network error, HTTP 429, or 5xx — a transient upstream failure shouldn't fail
// the whole investigation. Other 4xx and 2xx return immediately. build is invoked
// fresh each attempt so a consumed request body is rebuilt. On a 429 the wait
// honors the server's Retry-After / retry-after-ms hint (capped); otherwise it
// uses capped exponential backoff. The wait respects ctx cancellation. The last
// response (a 429/5xx) is returned so the caller can surface its status.
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
			case <-time.After(retryDelay(i, resp)):
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

// retryDelay computes how long to wait before the next attempt. attempt is the
// 1-based index of the wait (the wait BEFORE attempt #attempt; attempt >= 1).
// On a 429 it honors the server's hint — retry-after-ms, then Retry-After as
// delta-seconds, then as an HTTP-date — capped at maxDelay. With no usable hint
// (no 429, missing/malformed/expired header) it falls back to capped exponential
// backoff: baseBackoff·2^(attempt-1).
func retryDelay(attempt int, resp *http.Response) time.Duration {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if d := parseRetryAfter(resp.Header, time.Now); d > 0 {
			if d > maxDelay {
				return maxDelay
			}
			return d
		}
	}
	// Exponential fallback, guarding against shift overflow on large attempts.
	if attempt < 1 {
		attempt = 1
	}
	if attempt-1 >= 63 {
		return maxDelay
	}
	d := baseBackoff << uint(attempt-1)
	if d <= 0 || d > maxDelay {
		return maxDelay
	}
	return d
}

// parseRetryAfter extracts a positive backoff hint from rate-limit headers, in
// priority order: Retry-After-Ms (integer milliseconds, the most precise — sent
// by OpenAI and as retry_after_ms by Matrix), then Retry-After as RFC 9110
// delta-seconds, then Retry-After as an HTTP-date relative to now. Returns 0 when
// no header yields a positive duration. now is injected for deterministic tests.
func parseRetryAfter(h http.Header, now func() time.Time) time.Duration {
	if ms := h.Get("Retry-After-Ms"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	ra := h.Get("Retry-After")
	if ra == "" {
		return 0
	}
	if secs, err := strconv.Atoi(ra); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(ra); err == nil {
		if d := t.Sub(now()); d > 0 {
			return d
		}
	}
	return 0
}
