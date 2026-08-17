// SPDX-License-Identifier: Apache-2.0

package clientcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

const (
	// retryAttempts bounds httpx.DoWithRetry's attempts per completion request.
	retryAttempts = 3
	// maxErrorBody bounds how much of a non-200 response body is read for the
	// error detail.
	maxErrorBody = 4 << 10
)

// Request describes one streaming completion call. Everything
// provider-specific about the HTTP exchange is injected here; the pipeline
// itself (retry, status classification, idle-timeout guard) is shared.
type Request struct {
	// Op names the operation in error messages (e.g. "messages", "chat").
	Op string
	// URL is the full endpoint URL.
	URL string
	// Body is the provider's wire-format request struct, marshaled as JSON.
	Body any
	// SetHeaders sets the provider's headers (content type, auth, version).
	SetHeaders func(*http.Request)
	// ErrorDetail parses a provider error body into a sanitized
	// ": kind: message" suffix for the status error (see Detail), or "" if
	// the body isn't a recognizable provider error.
	ErrorDetail func(body []byte) string
}

// Stream sends req as a streaming POST and hands the response body — guarded
// by the idle-timeout reader — to accumulate, which folds the provider's SSE
// stream into a single CompletionResponse.
//
// One Stream is one logical completion but up to retryAttempts upstream
// requests, and a provider bills each request it accepted. That count is
// reported back on every path a caller can observe: CompletionResponse.Attempts
// when an attempt succeeded or a 200 stream failed mid-fold, and
// providers.AttemptsOf on the returned error always — so a caller charging a
// spend budget can bill what the exchange really cost rather than one request's
// worth of it.
//
// An error return is likewise not a zero CompletionResponse once a 200 stream
// has flowed: it carries the usage the fold observed before it failed (see
// providers.CompletionResponse). The paths before that — marshal, transport,
// non-200 — return a zero Usage, since no provider ever generated a token for
// them and fabricating one would bill a request that never ran.
func (b *Base) Stream(ctx context.Context, req Request, accumulate func(io.Reader) (providers.CompletionResponse, error)) (providers.CompletionResponse, error) {
	body, err := json.Marshal(req.Body)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	// A child context lets the idle-timeout reader abort a stalled stream by
	// cancelling the in-flight HTTP read; cancel always runs on return to
	// release resources.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// attempts counts the requests actually handed to the transport. DoWithRetry
	// invokes build once per attempt, so counting here needs no signature change
	// and reports the same number for every retry reason it handles. It is what
	// a caller charging a spend budget bills against: the provider accepted —
	// and billed — every one of these, while the caller sees a single return.
	attempts := 0
	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(streamCtx, http.MethodPost, req.URL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.SetHeaders(r)
		attempts++
		return r, nil
	}
	// DoWithRetry retries only connection establishment / 429 / 5xx (before
	// the stream begins); once a 200 stream is flowing it is never retried
	// mid-stream.
	resp, err := httpx.DoWithRetry(streamCtx, b.HTTP, retryAttempts, newReq)
	if err != nil {
		return providers.CompletionResponse{}, providers.WithAttempts(fmt.Errorf("%s request: %w", req.Op, err), attempts)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Read a bounded prefix of the error body. Never echo the raw bytes
		// into an Error-level log (info disclosure + log injection); surface
		// only the provider's structured, sanitized detail so a 4xx cause
		// (e.g. an invalid request, an unknown model, a rejected API key) is
		// diagnosable from the error alone.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		err := fmt.Errorf("%s status %d (request-id %q)%s", req.Op, resp.StatusCode, httpx.RequestID(resp.Header), req.ErrorDetail(errBody))
		// A 4xx other than 429 is permanent: the request itself is bad (e.g.
		// 400 invalid request, 401/403 auth, 404 unknown model), so retrying
		// can't help. Mark it so the investigation workqueue drops it instead
		// of requeuing forever. 429 and 5xx are already retried by DoWithRetry
		// and stay transient here.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return providers.CompletionResponse{}, providers.WithAttempts(providers.Permanent(err), attempts)
		}
		return providers.CompletionResponse{}, providers.WithAttempts(err, attempts)
	}
	out, err := accumulate(httpx.NewIdleTimeoutReader(resp.Body, IdleTimeout, cancel))
	if err != nil {
		// A fold that failed mid-stream still knows what the exchange cost: the
		// provider reports usage AS the stream runs, and a completion that died
		// after generating thousands of tokens was billed for them. Hand back
		// that usage and the attempt count on the same response the success path
		// reports Attempts on — a caller charging a spend budget reads both from
		// there — while every field describing a reply is dropped (CostOnly).
		// The error keeps its own attempt marker for callers that only look at
		// it. The error paths ABOVE stay zero-valued: none of them saw a 200
		// stream, so no usage is knowable client-side.
		cost := out.CostOnly()
		cost.Attempts = attempts
		return cost, providers.WithAttempts(err, attempts)
	}
	// The fold reports the usage of the ONE attempt that streamed a body; this
	// is what the whole exchange cost upstream.
	out.Attempts = attempts
	return out, nil
}
