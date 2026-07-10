// SPDX-License-Identifier: Apache-2.0

package clientcore

import (
	"strings"
	"testing"
)

// TestDetail asserts the sanitized ": kind: message" formatting: control
// characters (newlines, CR, ANSI escape, DEL) collapse to spaces so an
// attacker-controlled provider error body cannot forge log lines or inject
// terminal escapes when the detail is embedded in an Error-level log.
func TestDetail(t *testing.T) {
	cases := []struct{ name, kind, message, want string }{
		{"plain kind and message", "invalid_request_error", "prompt is too long", ": invalid_request_error: prompt is too long"},
		{"empty kind and message", "", "", ": : "},
		{"newline collapses to a space", "k", "line1\nline2", ": k: line1 line2"},
		{"carriage return collapses", "k", "a\rb", ": k: a b"},
		{"ANSI escape collapses", "k", "a\x1b[2Kb", ": k: a [2Kb"},
		{"DEL collapses", "k", "a\x7fb", ": k: a b"},
		{"the kind is sanitized too", "bad\nkind", "m", ": bad kind: m"},
		{"surrounding whitespace is trimmed", "  k\t", " m \n", ": k: m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detail(tc.kind, tc.message); got != tc.want {
				t.Errorf("Detail(%q, %q) = %q, want %q", tc.kind, tc.message, got, tc.want)
			}
		})
	}
}

// TestDetailTruncation asserts an oversized upstream message is truncated with
// an ellipsis (a hostile error body must not bloat logs) while a message exactly
// at the limit passes through untouched.
func TestDetailTruncation(t *testing.T) {
	atLimit := strings.Repeat("a", maxDetailLen)
	if got, want := Detail("k", atLimit), ": k: "+atLimit; got != want {
		t.Errorf("message at the limit was altered: got %d bytes, want %d", len(got), len(want))
	}
	got := Detail("k", strings.Repeat("a", maxDetailLen+1))
	if want := ": k: " + atLimit + "…"; got != want {
		t.Errorf("over-limit message: got %d bytes ending %q, want %d bytes ending %q",
			len(got), got[len(got)-3:], len(want), want[len(want)-3:])
	}
}

// TestDetailLogInjection feeds a forged-log-record payload (the classic
// clear-line-then-fake-record attack) and asserts no control character survives
// into the formatted detail.
func TestDetailLogInjection(t *testing.T) {
	got := Detail("x", "\n\x1b[2Kforged=record level=error msg=\"fake\"\r\n")
	if strings.ContainsAny(got, "\n\r\x1b\x7f") {
		t.Errorf("detail leaked control characters: %q", got)
	}
}
