// SPDX-License-Identifier: Apache-2.0

package clientcore

import "testing"

// TestRawObject asserts the "" → {} normalization: a model sometimes emits a
// tool call with empty arguments, and several provider APIs reject an empty
// input payload with a 400 — so the empty string must become the empty object,
// while any non-blank payload passes through verbatim (RawObject normalizes, it
// does not validate).
func TestRawObject(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty becomes the empty object", "", "{}"},
		{"whitespace-only becomes the empty object", " \t\r\n ", "{}"},
		{"an object passes through verbatim", `{"a":1}`, `{"a":1}`},
		{"non-object JSON passes through verbatim (no validation)", `[1,2]`, `[1,2]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(RawObject(tc.in)); got != tc.want {
				t.Errorf("RawObject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
