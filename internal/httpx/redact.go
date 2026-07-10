// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"net/http"
	"strings"
)

// maxHeaderLen caps a sanitized header value. Request-ids are short; a cap
// bounds any abuse from a misbehaving upstream that returns an oversized value.
const maxHeaderLen = 200

// requestIDHeaders is the precedence order of request-id header names across the
// upstreams RunLore talks to (OpenAI-compatible, Anthropic, Gemini, plus an AWS
// gateway form). The first present, non-empty value wins.
var requestIDHeaders = []string{
	"X-Request-Id",      // OpenAI, most proxies
	"Request-Id",        // Anthropic
	"X-Goog-Request-Id", // Gemini / Google
	"X-Amzn-Requestid",  // AWS API gateways fronting a model
}

// SanitizeHeader makes an upstream-supplied header value safe to embed in a log
// line: it drops control characters (anything below 0x20 plus DEL 0x7f, which
// includes CR/LF/TAB/NUL and ANSI ESC) so the value cannot forge log structure
// or inject terminal escapes, then caps the length and trims surrounding space.
func SanitizeHeader(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxHeaderLen {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// RequestID returns the first present upstream request-id header (by the
// precedence in requestIDHeaders), sanitized for safe logging. It returns "" when
// none is set. The result is operator-trusted correlation metadata — not body
// content — suitable for an error surfaced at Error level.
func RequestID(h http.Header) string {
	for _, name := range requestIDHeaders {
		if v := h.Get(name); v != "" {
			return SanitizeHeader(v)
		}
	}
	return ""
}
