// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

// SanitizeURLError rewrites a *url.Error so only the scheme and host survive —
// never the path or query. Use it on every error out of http.Client.Do or
// http.NewRequest whose URL may be a credential.
//
// For an incoming webhook (Slack, Discord, Teams) the URL *is* the credential: the
// secret lives in the path. net/http builds a *url.Error whose Error() prints the
// full URL, and it masks only the userinfo password — the path is verbatim:
//
//	Post "https://hooks.slack.com/services/T0/B0/REAL_SECRET": dial tcp: ...
//
// Wrapping that with %w and logging it writes a live posting credential into the
// operator's log store on any transient DNS or dial failure. No attacker required.
//
// The cause is re-wrapped with %w, so errors.Is/As against the underlying error
// (context.DeadlineExceeded, net.Error) keeps working; the *url.Error itself is
// deliberately dropped, since it is the thing carrying the secret.
func SanitizeURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	// Parse failures fall back to "?" rather than to ue.URL: an unparseable URL is
	// exactly the case where echoing it back is least justified.
	loc := "?"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		loc = u.Scheme + "://" + u.Host
	}
	if ue.Err == nil {
		return fmt.Errorf("%s %s", ue.Op, loc)
	}
	return fmt.Errorf("%s %s: %w", ue.Op, loc, ue.Err)
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
