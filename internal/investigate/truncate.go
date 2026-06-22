package investigate

import "fmt"

// truncateOutput caps s to max bytes, keeping a head and tail with a marker in
// the middle. Returns the (possibly truncated) string and the number of bytes
// elided (0 when max <= 0 or s fits within max). max <= 0 disables truncation.
// Note: the marker (~40 bytes) is not budget-constrained — for max < ~42 bytes
// the head/tail are each clipped to 1 byte, which is harmless at real values.
func truncateOutput(s string, max int) (string, int) {
	if max <= 0 || len(s) <= max {
		return s, 0
	}
	// Reserve room for the marker; split the remaining budget head/tail.
	const minMarker = 40
	keep := max - minMarker
	if keep < 2 {
		keep = 2
	}
	head := keep / 2
	tail := keep - head
	trimmed := len(s) - head - tail
	return s[:head] + fmt.Sprintf("\n…[truncated %d bytes]…\n", trimmed) + s[len(s)-tail:], trimmed
}
