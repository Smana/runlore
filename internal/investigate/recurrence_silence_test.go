// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// TestDecideSilence is the full matrix for the human silence branch. The cases
// that matter most are the ones where a silence must NOT win: the escape hatches
// are the whole reason suppressing the paid loop is acceptable at all.
func TestDecideSilence(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	past := now.Add(-2 * time.Hour)

	// wantSkip pins suppressed() — the predicate Investigate actually branches on
	// — independently of the decision name, so the silence arm cannot quietly stop
	// skipping the paid loop while still reporting itself as silenced.
	for _, tc := range []struct {
		name     string
		gate     *RecurrenceGate
		req      Request
		prior    outcome.TriggerRecurrence
		want     recurrenceDecision
		wantSkip bool
	}{
		{
			name:  "a standing silence suppresses, with the cooldown OFF",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceSilenced, wantSkip: true,
		},
		{
			name:  "a standing silence suppresses even with NO conclusive prior",
			gate:  &RecurrenceGate{Cooldown: time.Hour},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{Count: 3, Last: now, SilencedUntil: future},
			want:  recurrenceSilenced, wantSkip: true,
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
			got := tc.gate.decide(tc.req, tc.prior, now)
			if got != tc.want {
				t.Errorf("decide() = %q, want %q", got, tc.want)
			}
			if got.suppressed() != tc.wantSkip {
				t.Errorf("decision %q suppressed() = %v, want %v", got, got.suppressed(), tc.wantSkip)
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

// TestDecideSilenceVersusFeedbackOrdering pins NEWEST HUMAN WINS at the gate.
// Before it, decide read prior.Contested() — which compares no timestamps — so a
// 👎 cast once outranked every later silence permanently: the click was recorded,
// the human was told "RunLore will NOT investigate this incident", and every
// firing re-investigated anyway.
func TestDecideSilenceVersusFeedbackOrdering(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	monday := now.Add(-48 * time.Hour)
	tuesday := now.Add(-24 * time.Hour)

	for _, tc := range []struct {
		name     string
		severity string
		prior    outcome.TriggerRecurrence
		want     recurrenceDecision
	}{
		{
			name:     "the silence is newer than the standing thumbs-down: it suppresses",
			severity: "warning",
			prior: outcome.TriggerRecurrence{
				SilencedUntil: future, SilencedAt: tuesday,
				FeedbackDown: 1, FeedbackDownLatest: monday,
			},
			want: recurrenceSilenced,
		},
		{
			name:     "the thumbs-down is newer than the silence: it re-arms",
			severity: "warning",
			prior: outcome.TriggerRecurrence{
				SilencedUntil: future, SilencedAt: monday,
				FeedbackDown: 1, FeedbackDownLatest: tuesday,
			},
			want: recurrenceOff,
		},
		{
			name:     "an unknown ordering leaves the thumbs-down standing",
			severity: "warning",
			prior: outcome.TriggerRecurrence{
				SilencedUntil: future, SilencedAt: tuesday, FeedbackDown: 1,
			},
			want: recurrenceOff,
		},
		{
			name:     "a newer silence still loses to a CRITICAL firing",
			severity: "critical",
			prior: outcome.TriggerRecurrence{
				SilencedUntil: future, SilencedAt: tuesday,
				FeedbackDown: 1, FeedbackDownLatest: monday,
			},
			want: recurrenceOff,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &RecurrenceGate{Cooldown: 0}
			req := Request{TriggerKey: "k", Severity: tc.severity}
			if got := g.decide(req, tc.prior, now); got != tc.want {
				t.Errorf("decide() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPruningALapsedSilenceChangesNoDecision is the observational-equivalence pin
// for outcome's lapsed-silence prune. Dropping a lapsed entry is only safe because
// decide consults a silence exclusively behind `now < SilencedUntil` — but the
// prune also clears SilencedAt, which SilenceOutranksFeedback DOES read, so the
// two snapshots genuinely differ on a field the gate calls. This drives a real
// ledger through both states and requires every decision to match.
func TestPruningALapsedSilenceChangesNoDecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.jsonl")
	live, err := outcome.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now()
	if err := live.Open(outcome.Event{Fingerprint: "fp", TriggerKey: "k", Kind: "fresh",
		Verdict: string(providers.VerdictNoAction), At: now.Add(-5 * time.Minute)}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// A 👎 cast BEFORE the silence, so SilenceOutranksFeedback reads true while the
	// lapsed entry is still on the books and false once it is pruned — the field the
	// prune actually moves.
	if err := live.Feedback("k", "down", "U1", now.Add(-72*time.Hour)); err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	if err := live.Silence("k", time.Hour, "U2", now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Silence: %v", err)
	}

	replayed, err := outcome.New(path) // a fresh process: loadLocked prunes the lapsed entry
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}
	before, after := live.Recurrence("k"), replayed.Recurrence("k")
	if before.SilencedAt.IsZero() || !after.SilencedAt.IsZero() {
		t.Fatalf("precondition: want the entry present before (%v) and pruned after (%v)",
			before.SilencedAt, after.SilencedAt)
	}
	if !before.SilenceOutranksFeedback() || after.SilenceOutranksFeedback() {
		t.Fatalf("precondition: the prune must actually move SilenceOutranksFeedback (before=%v after=%v)",
			before.SilenceOutranksFeedback(), after.SilenceOutranksFeedback())
	}

	for _, gate := range []*RecurrenceGate{{}, {Cooldown: time.Hour}, {Cooldown: 24 * time.Hour}} {
		for _, sev := range []string{"warning", "critical", ""} {
			req := Request{Title: "t", TriggerKey: "k", Severity: sev}
			got, want := gate.decide(req, after, now), gate.decide(req, before, now)
			if got != want {
				t.Errorf("cooldown=%v severity=%q: pruned decision %q, unpruned %q — the prune moved a decision",
					gate.Cooldown, sev, got, want)
			}
		}
	}
}

// TestInvestigateSkipsExactlyWhatSuppressedReports is the coupling between the
// gate's vocabulary and what Investigate DOES with it. Investigate gates its early
// return on decision.suppressed(); the switch that follows only picks the metric
// label and the log line. Asserting the skip against suppressed() rather than
// against a literal is the point — a decision added to suppressed() later has to
// keep skipping the paid loop, not merely claim it did in a log line.
func TestInvestigateSkipsExactlyWhatSuppressedReports(t *testing.T) {
	now := time.Now()
	gate := &RecurrenceGate{Cooldown: time.Hour}
	for _, tc := range []struct {
		name  string
		prior outcome.TriggerRecurrence
	}{
		{"a human silence", outcome.TriggerRecurrence{
			Count: 1, Last: now.Add(-time.Minute), SilencedUntil: now.Add(2 * time.Hour), SilencedAt: now}},
		{"the machine cooldown", outcome.TriggerRecurrence{Count: 1, Last: now.Add(-time.Minute),
			Verdict:    string(providers.VerdictNoAction),
			Conclusive: outcome.ConclusivePrior{At: now.Add(-time.Minute), Verdict: string(providers.VerdictNoAction)}}},
		{"neither: the cooldown has lapsed", outcome.TriggerRecurrence{Count: 1, Last: now.Add(-2 * time.Hour),
			Verdict:    string(providers.VerdictNoAction),
			Conclusive: outcome.ConclusivePrior{At: now.Add(-2 * time.Hour), Verdict: string(providers.VerdictNoAction)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{Title: "t", TriggerKey: "k", Severity: "warning"}
			wantSkip := gate.decide(req, tc.prior, time.Now()).suppressed()

			model := &blockingModel{}
			delivered := 0
			li := &LoopInvestigator{
				Model:          model,
				Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
				OnComplete:     func(providers.Investigation) { delivered++ },
				Recurrence:     gate,
				TriggerHistory: fakeRecurrenceStats{tc.prior},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_ = li.Investigate(ctx, req)

			if gotSkip := model.calls == 0; gotSkip != wantSkip {
				t.Fatalf("model calls = %d (skipped=%v), but suppressed() = %v", model.calls, gotSkip, wantSkip)
			}
			if wantSkip && delivered != 0 {
				t.Fatalf("OnComplete called %d times on a suppressed occurrence, want 0", delivered)
			}
		})
	}
}
