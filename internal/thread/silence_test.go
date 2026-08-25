// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"strings"
	"testing"
	"time"
)

// TestSilenceAckCarriesTheWarning pins the four facts that must survive any
// rewording of the ack: it must read as a warning, name the severity that still
// breaks through, name the escape hatch, and state the chosen window.
func TestSilenceAckCarriesTheWarning(t *testing.T) {
	got := SilenceAck("bob", 4*time.Hour, time.Date(2026, 8, 25, 18, 42, 0, 0, time.UTC))
	// Substrings, not the whole string: the wording should be tunable without
	// breaking the test, but these four facts must always survive a rewrite.
	for _, want := range []string{"will NOT investigate", "CRITICAL", "👎", "(4h)"} {
		if !strings.Contains(got, want) {
			t.Errorf("SilenceAck() = %q, missing %q", got, want)
		}
	}
	// The window must read the way the docs and the YAML write it. Asserting
	// Contains(got, "4h") alone was satisfied by "4h0m0s", which is exactly what
	// the ack used to print — the assertion was there and pinned nothing.
	if strings.Contains(got, "0m0s") {
		t.Errorf("SilenceAck() = %q, want the window as an operator writes it (4h), not %s", got, 4*time.Hour)
	}
}

// TestShortDuration pins the rendering every human-facing silence window goes
// through. time.Duration.String() always appends the zero minute/second tail, so
// a 1h preset advertised as `1h` in the docs and the chart rendered as
// "🔕 Silence 1h0m0s" on the card.
func TestShortDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{time.Hour, "1h"},
		{4 * time.Hour, "4h"},
		{24 * time.Hour, "24h"},
		{90 * time.Minute, "1h30m"},
		{30 * time.Minute, "30m"},
		{45 * time.Second, "45s"},
		// The trims are suffix-anchored, so a value merely ENDING in the digits the
		// tail is made of must survive untouched.
		{10 * time.Second, "10s"},
		{time.Minute + 10*time.Second, "1m10s"},
		{2*time.Hour + 30*time.Second, "2h0m30s"},
		{0, "0s"},
	} {
		if got := ShortDuration(tc.in); got != tc.want {
			t.Errorf("ShortDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
		if _, err := time.ParseDuration(ShortDuration(tc.in)); err != nil {
			t.Errorf("ShortDuration(%v) = %q, which no longer parses as a duration: %v", tc.in, ShortDuration(tc.in), err)
		}
	}
}
