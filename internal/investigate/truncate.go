// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"fmt"
	"unicode/utf8"
)

// truncateOutput caps s to max bytes, keeping a head and tail with a marker in
// the middle. Returns the (possibly truncated) string and the number of bytes
// elided (0 when max <= 0 or s fits within max). max <= 0 disables truncation.
// Note: the marker (~40 bytes) is not budget-constrained — for max < ~42 bytes
// the head/tail are each clipped to 1 byte, which is harmless at real values.
//
// Cut points are aligned to UTF-8 rune boundaries so the head and tail are
// always valid UTF-8 (no partial multi-byte sequences).
func truncateOutput(s string, maxBytes int) (string, int) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, 0
	}
	// Reserve room for the marker; split the remaining budget head/tail.
	const minMarker = 40
	keep := maxBytes - minMarker
	if keep < 2 {
		keep = 2
	}
	head := keep / 2
	tail := keep - head

	// Back head off to the start of the rune that straddles the cut point.
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	// Advance the tail start forward past any continuation bytes.
	tailStart := len(s) - tail
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}

	trimmed := len(s) - head - (len(s) - tailStart)
	return s[:head] + fmt.Sprintf("\n…[truncated %d bytes]…\n", trimmed) + s[tailStart:], trimmed
}
