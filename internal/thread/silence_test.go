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
	got := SilenceAck("bob", 4*time.Hour, time.Date(2026, 8, 25, 18, 42, 0, 0, time.UTC), true)
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

// TestSilenceAckDatesTheExpiry pins the fix for a time-only acknowledgement.
// "15:04 MST" made the SHIPPED DEFAULT the ambiguous case: 24h is what
// values-full.yaml and both docs pages configure, so a 🔕 clicked at 11:58 acked
// as "until 11:58 UTC" — indistinguishable from "expires now" — and a 20h window
// clicked at 18:00 read "until 14:00", which looks four hours in the past.
func TestSilenceAckDatesTheExpiry(t *testing.T) {
	at := time.Date(2026, 8, 25, 11, 58, 0, 0, time.UTC)
	got := SilenceAck("bob", 24*time.Hour, at.Add(24*time.Hour), true)
	if !strings.Contains(got, "2026-08-26 11:58 UTC") {
		t.Errorf("SilenceAck() = %q, want the expiry to carry its DATE (2026-08-26 11:58 UTC)", got)
	}
}

// TestSilenceAckDoesNotPromiseAnAbsentThumbsDown pins the honesty fix. The ack
// promised "a 👎 … re-arms it immediately" unconditionally, while the docs
// recommend exactly the deployment where no 👎 can be cast at all
// (feedback_buttons: false + silence_button: true on Slack; feedback_reactions:
// false + silence_reactions: true on Matrix). Combine that with a GitOps trigger
// — no severity, so no CRITICAL escape; a synthetic fingerprint, so no resolve
// escape — and expiry is the ONLY bound, with the ack still naming three.
func TestSilenceAckDoesNotPromiseAnAbsentThumbsDown(t *testing.T) {
	until := time.Date(2026, 8, 25, 18, 42, 0, 0, time.UTC)

	with := SilenceAck("bob", 4*time.Hour, until, true)
	if !strings.Contains(with, "👎 re-arms it") {
		t.Errorf("with feedback enabled the ack must offer the 👎 escape: %q", with)
	}

	without := SilenceAck("bob", 4*time.Hour, until, false)
	if strings.Contains(without, "👎 re-arms it") {
		t.Errorf("with no 👍/👎 control enabled the ack must not promise a 👎 escape: %q", without)
	}
	if !strings.Contains(without, "not enabled") {
		t.Errorf("the ack must SAY the 👎 escape is unavailable rather than quietly dropping it: %q", without)
	}
	// The unconditional escapes survive in both.
	for _, got := range []string{with, without} {
		for _, want := range []string{"will NOT investigate", "CRITICAL"} {
			if !strings.Contains(got, want) {
				t.Errorf("SilenceAck() = %q, missing %q", got, want)
			}
		}
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

// TestSilenceMarkerNamesWhoAndUntilWhen pins the one-line stamp left on the card
// itself. It is deliberately not SilenceAck: the ack explains the consequences to
// the person who just clicked, while the marker is scannable evidence for someone
// scrolling the channel a day later.
func TestSilenceMarkerNamesWhoAndUntilWhen(t *testing.T) {
	// Both the ref and the time are passed through verbatim — the caller owns the
	// mention syntax and the date rendering — so this pins that the marker neither
	// decorates, re-prefixes, nor reformats what it is handed.
	got := SilenceMarker("<@U9>", "<!date^1756219200^{date_short_pretty} {time}|2026-08-26T14:40:00Z>", 48*time.Hour)
	for _, want := range []string{"🔕", "<@U9>", "<!date^1756219200^", "48h"} {
		if !strings.Contains(got, want) {
			t.Errorf("marker %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("the marker is a single context line, got %q", got)
	}
}
