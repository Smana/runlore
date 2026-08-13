// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

type fakeRecurrenceStats struct{ r outcome.TriggerRecurrence }

func (f fakeRecurrenceStats) Recurrence(string) outcome.TriggerRecurrence { return f.r }

// concluded builds the recurrence snapshot a real ledger produces for a trigger
// whose newest open IS its newest conclusive one — the common case. The two folds
// always move together there, and building them by hand invites a snapshot that no
// ledger could ever emit.
func concluded(count int, at time.Time, verdict string) outcome.TriggerRecurrence {
	return outcome.TriggerRecurrence{Count: count, Last: at, Verdict: verdict,
		Conclusive: outcome.ConclusivePrior{At: at, Verdict: verdict}}
}

// TestRecurrenceGateDecisions pins the full suppression matrix, asserting the
// REASON and not just the boolean: suppress only when the trigger was investigated
// within the cooldown, an answer stands for it, and no human currently contests
// that answer — every other combination re-investigates, each for its own reason.
func TestRecurrenceGateDecisions(t *testing.T) {
	now := time.Unix(50000, 0)
	recent := now.Add(-5 * time.Minute)
	stale := now.Add(-2 * time.Hour)
	req := Request{Title: "t", TriggerKey: "k"}
	// An inconclusive run 5m ago with the action_required answer it failed to restate
	// still standing behind it, from 2h ago.
	mislabelled := outcome.TriggerRecurrence{Count: 2, Last: recent, Verdict: "inconclusive",
		Conclusive: outcome.ConclusivePrior{At: stale, Verdict: "action_required", Title: "broken down-migration"}}
	on := &RecurrenceGate{Cooldown: time.Hour}
	cases := []struct {
		name  string
		gate  *RecurrenceGate
		req   Request
		prior outcome.TriggerRecurrence
		want  recurrenceDecision
	}{
		{"suppresses a fresh conclusive uncontested recurrence",
			on, req, concluded(1, recent, "no_action"), recurrenceSuppressed},
		{"action_required is conclusive too",
			on, req, concluded(2, recent, "action_required"), recurrenceSuppressed},
		{"nil gate never suppresses", nil, req, concluded(1, recent, "no_action"), recurrenceOff},
		{"cooldown 0 (off) never suppresses",
			&RecurrenceGate{}, req, concluded(1, recent, "no_action"), recurrenceOff},
		{"no trigger key never suppresses",
			on, Request{Title: "t"}, concluded(1, recent, "no_action"), recurrenceOff},
		{"never investigated", on, req, outcome.TriggerRecurrence{}, recurrenceFirstLook},
		{"cooldown expired — re-investigate",
			on, req, concluded(3, stale, "no_action"), recurrenceCooldownLapsed},
		{"inconclusive prior, nothing ever concluded — retry, we owe a real answer",
			on, req, outcome.TriggerRecurrence{Count: 1, Last: recent, Verdict: "inconclusive"}, recurrenceNoAnswer},
		{"pre-verdict prior (old events) — retry",
			on, req, outcome.TriggerRecurrence{Count: 1, Last: recent}, recurrenceNoAnswer},
		{"inconclusive prior with an answer standing behind it — suppress; the mislabel costs one run",
			on, req, mislabelled, recurrenceSuppressed},
		{"a standing 👎 breaks the cooldown — the human re-arms investigation",
			on, req, outcome.TriggerRecurrence{Count: 1, Last: recent, Verdict: "no_action",
				Conclusive: outcome.ConclusivePrior{At: recent, Verdict: "no_action"}, FeedbackDown: 1}, recurrenceContested},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.gate.decide(c.req, c.prior, now)
			if got != c.want {
				t.Fatalf("decide = %q, want %q", got, c.want)
			}
			if want := c.want == recurrenceSuppressed; got.suppressed() != want {
				t.Fatalf("decision %q suppressed() = %v, want %v", got, got.suppressed(), want)
			}
		})
	}
}

// TestPriorForTriggerIsNothingKnownWithoutALedger: the two consumers of a trigger's
// history — the suppression gate and the seed's known-recurrence block — must both
// see a clean "nothing known" when there is nothing to read, rather than needing a
// nil check of their own.
func TestPriorForTriggerIsNothingKnownWithoutALedger(t *testing.T) {
	full := fakeRecurrenceStats{concluded(2, time.Now(), "action_required")}
	for _, c := range []struct {
		name string
		li   *LoopInvestigator
		key  string
	}{
		{"no ledger wired", &LoopInvestigator{}, "k"},
		{"no trigger key to group by", &LoopInvestigator{TriggerHistory: full}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.li.priorForTrigger(c.key); got.Count != 0 || got.Concluded() {
				t.Fatalf("priorForTrigger = %+v, want the zero value", got)
			}
		})
	}
}

// TestRecurrenceGateSuppressionSurvivesOneMislabelledRun is the #471 regression,
// driven through a REAL ledger so the gate and the per-trigger fold it reads are
// exercised together — the bug lived in their interaction, not in either alone.
//
// A persistent fault fires every 10m under a 30m cooldown. The first investigation
// concludes; from then on the model mislabels the known recurrence as
// `inconclusive` every single time (the realistic worst case — the evidence never
// changes, so neither does the mislabel). The gate must still hold the fault to ONE
// investigation per cooldown. Before the fix it anchored on the newest open's
// verdict alone, so the first mislabel disarmed it and every later firing bought a
// full investigation: 16 of these 18 firings ran the loop instead of 6.
func TestRecurrenceGateSuppressionSurvivesOneMislabelledRun(t *testing.T) {
	led, err := outcome.New(filepath.Join(t.TempDir(), "o.jsonl"))
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	li := &LoopInvestigator{TriggerHistory: led, Recurrence: &RecurrenceGate{Cooldown: 30 * time.Minute}}
	req := Request{Title: "wet-collab-api CrashLoopBackOff", TriggerKey: "k"}
	t0 := time.Unix(60000, 0)
	investigated := 0
	for i := 0; i < 18; i++ { // every 10m for 3h
		now := t0.Add(time.Duration(i) * 10 * time.Minute)
		if li.Recurrence.decide(req, li.priorForTrigger(req.TriggerKey), now).suppressed() {
			continue
		}
		investigated++
		// The model's answer: correct once, then "already known, not a new incident"
		// squeezed into the one enum value that means the opposite.
		verdict := providers.VerdictActionRequired
		if investigated > 1 {
			verdict = providers.VerdictInconclusive
		}
		if err := led.Open(outcome.Event{Fingerprint: fmt.Sprintf("f%d", i), Kind: "fresh", TriggerKey: "k",
			Title: "broken DB down-migration to schema 94", Verdict: string(verdict), At: now}); err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	if want := 6; investigated != want { // t=0, then one per 30m cooldown through the last firing at t=170m
		t.Fatalf("investigations = %d, want %d (one per cooldown)", investigated, want)
	}
}

// TestRecurrenceGateNeverSuppressesAnUnansweredTrigger: the human-deferential half
// of the fix. A trigger that has never reached a conclusion must keep buying a
// fresh investigation on every firing however often it fires — there is no answer
// to stand on, and silence would leave the on-call with nothing.
func TestRecurrenceGateNeverSuppressesAnUnansweredTrigger(t *testing.T) {
	led, err := outcome.New(filepath.Join(t.TempDir(), "o.jsonl"))
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	li := &LoopInvestigator{TriggerHistory: led, Recurrence: &RecurrenceGate{Cooldown: 30 * time.Minute}}
	req := Request{Title: "t", TriggerKey: "k"}
	t0 := time.Unix(70000, 0)
	for i := 0; i < 6; i++ {
		now := t0.Add(time.Duration(i) * 5 * time.Minute)
		prior := li.priorForTrigger(req.TriggerKey)
		if li.Recurrence.decide(req, prior, now).suppressed() {
			t.Fatalf("firing %d suppressed with no answer standing: %+v", i, prior)
		}
		if err := led.Open(outcome.Event{Fingerprint: fmt.Sprintf("f%d", i), Kind: "fresh", TriggerKey: "k",
			Verdict: string(providers.VerdictInconclusive), At: now}); err != nil {
			t.Fatalf("open: %v", err)
		}
	}
}

// TestInvestigateSuppressedRecurrenceSkipsModelAndDelivery: a suppressed
// recurrence must cost nothing and say nothing — no model call, no OnComplete
// (no notification, no curation, no ledger open), nil error. The previous
// notification remains THE answer until the cooldown lapses or a 👎 lands.
func TestInvestigateSuppressedRecurrenceSkipsModelAndDelivery(t *testing.T) {
	model := &blockingModel{}
	delivered := 0
	li := &LoopInvestigator{
		Model:          model,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete:     func(providers.Investigation) { delivered++ },
		Recurrence:     &RecurrenceGate{Cooldown: time.Hour},
		TriggerHistory: fakeRecurrenceStats{concluded(1, time.Now().Add(-time.Minute), "no_action")},
	}
	if err := li.Investigate(context.Background(), Request{Title: "t", TriggerKey: "k"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if model.calls != 0 {
		t.Fatalf("model called %d times during a suppressed recurrence, want 0", model.calls)
	}
	if delivered != 0 {
		t.Fatalf("OnComplete called %d times during a suppressed recurrence, want 0", delivered)
	}
}
