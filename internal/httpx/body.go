// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"errors"
	"fmt"
	"io"
)

// maxToolOutputDefault mirrors the default of investigation.max_tool_output_bytes
// (internal/config/load.go, deploy/helm/runlore/values.yaml): the most text one
// tool result may put in front of the model.
const maxToolOutputDefault = 32 << 10

// MaxResponseBytes bounds how much of a query backend's HTTP response body is
// read into memory.
//
// The logs/metrics queries are MODEL-CHOSEN and the pod is memory-capped, so an
// unbounded io.ReadAll makes one over-broad expression ({__name__=~".+"} over a
// week) an OOM rather than a bad answer. No attacker required.
//
// The bound is derived from the tool-output cap rather than picked fresh: a
// response only matters insofar as it becomes a tool result, and a tool result
// is truncated to maxToolOutputDefault before the model ever sees it. The ×64
// headroom is the gap between the wire form and the rendered form — JSON
// envelopes, per-series label sets repeated on every sample and RFC3339
// timestamps routinely inflate a response an order of magnitude over the few KB
// of summary a backend renders from it — while still refusing the pathological
// case that no rendering could survive.
const MaxResponseBytes = 64 * maxToolOutputDefault // 2 MiB

// ErrResponseTooLarge reports that a backend response exceeded MaxResponseBytes.
// Callers wrap it; tests match it with errors.Is.
var ErrResponseTooLarge = errors.New("response too large")

// tooLargeError builds the operator- and model-facing overflow message. The
// investigation loop hands a tool error straight to the model ("error: <text>"),
// so this is the sentence the model reads: it must say plainly that the result
// was NOT seen in full, and what to change. Silently returning a partial result
// would be the worse failure — the model would conclude from evidence it had no
// reason to doubt.
func tooLargeError() error {
	return fmt.Errorf("%w: over %d bytes — narrow the query or shorten the time window", ErrResponseTooLarge, MaxResponseBytes)
}

// CappedReader wraps r so that reading past MaxResponseBytes fails with
// ErrResponseTooLarge instead of stopping quietly. Use it for STREAMING parsers
// (NDJSON scanners): an io.LimitReader would look like a clean end-of-stream, so
// the parser would return a partial result the caller believes is complete.
func CappedReader(r io.Reader) io.Reader {
	// One byte of slack: a body of exactly MaxResponseBytes must succeed, and
	// only the byte after it proves the upstream had more to send.
	return &cappedReader{r: r, left: MaxResponseBytes + 1}
}

type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, tooLargeError()
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// ReadBody reads a response body whole, up to MaxResponseBytes, returning an
// error wrapping ErrResponseTooLarge when the upstream sent more. Overflow is an
// error rather than a truncation because these bodies are parsed as JSON: a body
// cut at the cap does not parse, so there is no partial result to salvage — only
// a confusing "unexpected end of JSON input" where an actionable sentence
// belongs.
func ReadBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(CappedReader(r))
}

// maxDrainBytes bounds how much of an unread response body Drain consumes.
//
// Sized from the measurement below, not from taste. Under ~2 KB there is nothing
// to win — net/http has already buffered the body — so a useful cap sits above
// that, and 32 KB covers the error envelopes real backends send (Elasticsearch
// and Loki are the verbose ones here) without turning cleanup into work. Past it
// the connection is dropped, which is what a bare Close would have done anyway.
const maxDrainBytes = 32 << 10

// Drain consumes up to maxDrainBytes of an unread response body so the connection
// underneath it can go back to net/http's idle pool, which only happens once the
// body reaches EOF. Pair it with the Close it does NOT perform:
//
//	defer func() { httpx.Drain(resp.Body); _ = resp.Body.Close() }()
//
// It deliberately does not close. A DrainClose(resp) helper reads better and was
// written first, but it hides the Close from the bodyclose linter — which then
// reports "response body must be closed" at EVERY call site. That is not a
// tolerable trade: bodyclose catches a leak that is invisible in tests and fatal
// in production, and this drain is worth far less than that. (The breakage is
// easy to miss: golangci-lint's max-same-issues defaults to 3, so 25 flagged
// sites surface as 3 — a different 3 each run.)
//
// How much it buys is size-dependent and easy to overstate — this comment did
// overstate it, twice, before the tests were written. net/http has often already
// buffered a small response by the time Close runs, in which case the connection
// is pooled either way and draining changes nothing; past maxDrainBytes the body
// never reaches EOF, so the connection goes either way again. In between there is
// a band where draining rescues a connection a bare Close would drop, and where
// that band falls moves with the header size and the transport's read-buffer
// boundary — so no fixed table of "body size to dial count" is portable, and the
// tests here assert only that draining is never WORSE.
//
// The stronger reason to use it uniformly is that the alternative is three
// spellings of one idiom — bare Close, unbounded io.Copy, bounded io.Copy — and
// the unbounded spelling is a hazard: a runaway upstream holds the caller's whole
// timeout open inside what reads like cleanup.
//
// Belongs in the DEFER rather than at the read site, because the paths that skip
// the read are exactly the early returns: a non-2xx branch, a header that fails
// validation, a context cancelled mid-parse. On the path that did read the body
// whole it finds EOF immediately, so one placement is correct for every path.
func Drain(body io.Reader) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
}

// ReadErrorBody reads the diagnostic prefix of a non-2xx response body and makes
// it safe to embed in an error: bounded at maxErrorBody (all SafeErrorBody would
// keep anyway) and stripped of the given credentials. A read failure yields ""
// — the body is best-effort diagnostics, never the reason the call failed.
func ReadErrorBody(r io.Reader, secrets ...string) string {
	data, _ := io.ReadAll(io.LimitReader(r, maxErrorBody))
	return SafeErrorBody(data, secrets...)
}
