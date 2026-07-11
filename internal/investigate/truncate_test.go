// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateOutput(t *testing.T) {
	// Under the limit: passes through unchanged, no bytes trimmed.
	got, n := truncateOutput("short", 100)
	if got != "short" || n != 0 {
		t.Fatalf("under limit must pass through unchanged, got %q n=%d", got, n)
	}

	big := strings.Repeat("x", 1000)
	got, n = truncateOutput(big, 100)
	if len(got) >= len(big) {
		t.Fatalf("expected truncation, got len %d", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("missing truncation marker: %q", got)
	}
	if n <= 0 {
		t.Fatalf("expected positive trimmed byte count, got %d", n)
	}
	// head and tail are preserved
	if got[:10] != "xxxxxxxxxx" || got[len(got)-10:] != "xxxxxxxxxx" {
		t.Fatalf("head/tail not preserved: %q", got)
	}

	// max <= 0 disables truncation entirely.
	if got, n := truncateOutput(big, 0); got != big || n != 0 {
		t.Fatalf("max=0 must be unlimited, got len=%d n=%d", len(got), n)
	}
}

// TestTruncateOutputMultiByte verifies that cut points land on rune boundaries
// when the input contains multi-byte UTF-8 sequences (emoji, accented chars).
// Before the fix, s[:head] and s[len(s)-tail:] could slice mid-rune, producing
// invalid UTF-8 in the output.
func TestTruncateOutputMultiByte(t *testing.T) {
	// Build a string whose natural byte-split lands mid-rune.
	// "café" has 5 bytes (é is 2 bytes: 0xC3 0xA9).
	// Pad with multi-byte chars so the naive cut point falls inside one of them.
	// Use 🎉 (4 bytes each) and é (2 bytes) to ensure misalignment is likely.
	head := strings.Repeat("🎉", 20) // 80 bytes
	mid := strings.Repeat("é", 20)  // 40 bytes
	tail := strings.Repeat("🎉", 20) // 80 bytes
	s := head + mid + tail          // 200 bytes total

	// maxBytes chosen so the nominal head/tail cuts land inside a multi-byte rune.
	// keep = 200 - 40 = 160 → head_nominal = 80, tail_nominal = 80.
	// 80 bytes lands exactly at a rune boundary for 🎉 (4-byte), so shift slightly.
	// Use maxBytes=198: keep=158 → head_nominal=79 (inside a 🎉 rune), tail_nominal=79.
	out, elided := truncateOutput(s, 198)

	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8: %q", out)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("missing truncation marker in output: %q", out)
	}
	if elided <= 0 {
		t.Fatalf("expected positive elided count, got %d", elided)
	}
	// The head prefix must start with a complete 🎉 rune.
	r, _ := utf8.DecodeRuneInString(out)
	if r == utf8.RuneError {
		t.Fatalf("head starts with a replacement rune (invalid UTF-8 boundary): %q", out[:8])
	}
	// The tail suffix must end with a complete 🎉 rune.
	rLast, _ := utf8.DecodeLastRuneInString(out)
	if rLast == utf8.RuneError {
		t.Fatalf("tail ends with a replacement rune (invalid UTF-8 boundary)")
	}
}
