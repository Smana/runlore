// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSanitizeHeader(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"clean", "req_abc-123.XYZ", "req_abc-123.XYZ"},
		{"empty", "", ""},
		{"strips newline", "a\nb", "ab"},
		{"strips crlf", "a\r\nb", "ab"},
		{"strips tab", "a\tb", "ab"},
		{"strips null", "a\x00b", "ab"},
		{"strips ansi esc", "a\x1b[31mb", "a[31mb"},
		{"strips del", "a\x7fb", "ab"},
		{"trims surrounding space", "  abc  ", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeHeader(tc.in); got != tc.want {
				t.Errorf("SanitizeHeader(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeHeaderCaps(t *testing.T) {
	got := SanitizeHeader(strings.Repeat("x", 1000))
	if len(got) > maxHeaderLen {
		t.Fatalf("len = %d, want <= %d", len(got), maxHeaderLen)
	}
}

func TestRequestID(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"none", nil, ""},
		{"x-request-id", map[string]string{"X-Request-Id": "rid1"}, "rid1"},
		{"request-id", map[string]string{"Request-Id": "rid2"}, "rid2"},
		{"goog", map[string]string{"X-Goog-Request-Id": "rid3"}, "rid3"},
		{"amzn", map[string]string{"X-Amzn-Requestid": "rid4"}, "rid4"},
		{
			"x-request-id wins over request-id",
			map[string]string{"X-Request-Id": "first", "Request-Id": "second"},
			"first",
		},
		{"value sanitized", map[string]string{"X-Request-Id": "bad\nid"}, "badid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			if got := RequestID(h); got != tc.want {
				t.Errorf("RequestID(%v) = %q, want %q", tc.headers, got, tc.want)
			}
		})
	}
}

// TestSanitizeURLError: for an incoming webhook the URL is the credential — the
// secret is the path — and net/http masks only the userinfo password, so the
// secret survives into any error that wraps it, and from there into the logs.
func TestSanitizeURLError(t *testing.T) {
	const secret = "XoXbSuperSecretWebhookKey123"
	cause := errors.New("dial tcp: lookup hooks.slack.com: no such host")

	got := SanitizeURLError(&url.Error{
		Op:  "Post",
		URL: "https://hooks.slack.com/services/T00000000/B00000000/" + secret,
		Err: cause,
	})
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("the webhook secret survived sanitizing: %v", got)
	}
	if !strings.Contains(got.Error(), "https://hooks.slack.com") {
		t.Fatalf("the host must be kept — it is what makes the error diagnosable: %v", got)
	}
	// The cause must stay unwrappable, or callers lose errors.Is on timeouts.
	if !errors.Is(got, cause) {
		t.Fatalf("cause no longer unwraps: %v", got)
	}

	// A userinfo password is masked by net/http, but the path never is; check we
	// drop both regardless of what the URL carries.
	got = SanitizeURLError(&url.Error{Op: "Post", URL: "https://u:pw@example.com/a/" + secret, Err: cause})
	for _, leak := range []string{secret, "pw"} {
		if strings.Contains(got.Error(), leak) {
			t.Fatalf("%q survived sanitizing: %v", leak, got)
		}
	}

	// Unparseable URL: fall back to a placeholder rather than echoing it.
	got = SanitizeURLError(&url.Error{Op: "Post", URL: "://" + secret, Err: cause})
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("an unparseable URL must not be echoed: %v", got)
	}

	// A non-url.Error is returned untouched.
	if got := SanitizeURLError(cause); !errors.Is(got, cause) {
		t.Fatalf("a plain error must pass through: %v", got)
	}
}

// TestSafeErrorBody: the two forge clients embed an upstream response body in
// errors that get logged. Stripping must happen before the length cap, or a
// credential straddling the cut leaves a fragment behind.
func TestSafeErrorBody(t *testing.T) {
	const secret = "glpat-SUPERSECRETVALUE1234"

	got := SafeErrorBody([]byte(`{"message":"401 for `+secret+`"}`), secret)
	if strings.Contains(got, secret) {
		t.Errorf("credential survived: %q", got)
	}
	if !strings.Contains(got, "401 for") {
		t.Errorf("diagnostic body must survive: %q", got)
	}

	// The secret sits astride the 512-byte cut: cap-then-strip would keep its head.
	pad := strings.Repeat("x", maxErrorBody-len(secret)/2)
	if got := SafeErrorBody([]byte(pad+secret+"tail"), secret); strings.Contains(got, secret[:8]) {
		t.Errorf("a credential straddling the cap left a fragment: %q", got)
	}

	if got := SafeErrorBody(bytes.Repeat([]byte("y"), 4096), ""); len(got) != maxErrorBody {
		t.Errorf("body not capped: len=%d want %d", len(got), maxErrorBody)
	}
}
