// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
)

// TestDecideSilence is the full matrix for the human silence branch. The cases
// that matter most are the ones where a silence must NOT win: the escape hatches
// are the whole reason suppressing the paid loop is acceptable at all.
func TestDecideSilence(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	past := now.Add(-2 * time.Hour)

	for _, tc := range []struct {
		name  string
		gate  *RecurrenceGate
		req   Request
		prior outcome.TriggerRecurrence
		want  recurrenceDecision
	}{
		{
			name:  "a standing silence suppresses, with the cooldown OFF",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceSilenced,
		},
		{
			name:  "a standing silence suppresses even with NO conclusive prior",
			gate:  &RecurrenceGate{Cooldown: time.Hour},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{Count: 3, Last: now, SilencedUntil: future},
			want:  recurrenceSilenced,
		},
		{
			name:  "a LAPSED silence does not suppress",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: past},
			want:  recurrenceOff,
		},
		{
			name:  "a CRITICAL firing is never silenced",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "critical"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceOff,
		},
		{
			name:  "a CRITICAL firing is never silenced, whatever the casing",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "CRITICAL"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceOff,
		},
		{
			name:  "a standing thumbs-down re-arms investigation",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future, FeedbackDown: 1},
			want:  recurrenceOff,
		},
		{
			name:  "no trigger key: nothing to silence on",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceOff,
		},
		{
			name:  "a nil gate never suppresses",
			gate:  nil,
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceOff,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.gate.decide(tc.req, tc.prior, now); got != tc.want {
				t.Errorf("decide() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDecideSilenceDoesNotDisturbTheCooldown: the existing ladder must behave
// identically when no silence stands. This is a regression guard on #471.
func TestDecideSilenceDoesNotDisturbTheCooldown(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	g := &RecurrenceGate{Cooldown: time.Hour}
	req := Request{TriggerKey: "k", Severity: "warning"}

	prior := outcome.TriggerRecurrence{
		Count:      2,
		Last:       now.Add(-10 * time.Minute),
		Conclusive: outcome.ConclusivePrior{At: now.Add(-10 * time.Minute), Verdict: "no_action"},
	}
	if got := g.decide(req, prior, now); got != recurrenceSuppressed {
		t.Errorf("decide() = %q, want %q — the cooldown ladder changed", got, recurrenceSuppressed)
	}
}
