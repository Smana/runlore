// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"net/http"
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
