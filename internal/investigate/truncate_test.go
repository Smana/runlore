// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"strings"
	"testing"
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
