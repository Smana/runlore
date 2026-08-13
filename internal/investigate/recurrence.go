// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"time"

	"github.com/Smana/runlore/internal/outcome"
)

// RecurrenceStats is the per-TriggerKey ledger snapshot the suppression gate
// reads. *outcome.Ledger satisfies it.
type RecurrenceStats interface {
	Recurrence(triggerKey string) outcome.TriggerRecurrence
}

// RecurrenceGate suppresses re-investigating a trigger that was conclusively
// investigated moments ago. Without it, nothing keys on TriggerKey before the
// paid loop: Alertmanager re-sends a still-firing alert every repeat_interval
// and a persistently-failing GitOps resource re-emits every informer resync
// (~10m), each re-running a full investigation that re-delivers the same answer
// as fresh noise — the recall short-circuit only helps once the KB PR is MERGED,
// so the human-review window is exactly when the repetition is worst.
//
// The gate reads two independent facts off the ledger's per-trigger index, and
// conflating them is what broke it once already (#471):
//
//   - WHEN did we last look? The newest open of any kind, conclusive or not. The
//     cooldown lapses from there, because a full investigation was paid for at that
//     moment whatever it concluded.
//   - Is there an ANSWER worth not repeating? The newest CONCLUSIVE prior (verdict
//     no_action / action_suggested / action_required), which is not necessarily the
//     newest one. Anchoring this on the latest prior instead let a single run that
//     mislabelled a known recurrence as `inconclusive` erase an arbitrarily long
//     history of conclusive ones — and since the evidence does not change between
//     firings, neither does the mislabel, so the gate stayed disarmed and every
//     later firing bought a full investigation. Reading the newest conclusive prior
//     costs one run per mislabel instead of all of them.
//
// A trigger that has NEVER concluded still bypasses the gate on every firing: there
// is no answer to stand on, and suppressing would leave the on-call with silence.
//
// The gate is deliberately human-deferential in the other direction too: a standing
// 👎 on the trigger breaks the cooldown immediately — a human saying "that diagnosis
// is wrong" re-arms the very next occurrence.
//
// A suppressed occurrence makes no model call, sends no notification, and
// records no ledger open. (It does still consume a workqueue turn and a
// rate-limiter slot — the gate runs below Queue.process like its sibling, the
// recall short-circuit; the same accepted tradeoff for both.) Not recording
// the open is load-bearing — an open would slide the byTrigger newest-open
// timestamp and the cooldown would never lapse while the incident keeps
// firing. Anchored on the last REAL investigation, a persistent failure is
// re-investigated once per cooldown (with its recurrence count intact) instead
// of once per resync. The flip side: the ledger keeps no durable record of
// suppressed firings (only the recurrence_suppressed metric and a log line see
// them) — a future consumer needing raw firing frequency is the moment to
// promote suppression to a first-class event kind, not before.
// The gate itself holds no ledger: LoopInvestigator.TriggerHistory reads the
// per-trigger snapshot once per investigation and hands it here, so the suppression
// decision and the seed's known-recurrence block are made from the same facts.
type RecurrenceGate struct {
	Cooldown time.Duration // 0 disables the gate (default: off, opt-in)
}

// priorForTrigger reads the trigger's recurrence snapshot: what earlier
// investigations of this same incident concluded. Zero value — a clean "nothing
// known" that every consumer already handles — when no ledger is wired, the ledger
// is disabled, or the request carries no trigger key to group by.
func (li *LoopInvestigator) priorForTrigger(triggerKey string) outcome.TriggerRecurrence {
	if li.TriggerHistory == nil || triggerKey == "" {
		return outcome.TriggerRecurrence{}
	}
	return li.TriggerHistory.Recurrence(triggerKey)
}

// recurrenceDecision is WHY the gate did or did not suppress an occurrence. The
// gate's failure surface is "suppression silently stops happening" — with a bare
// boolean, a gate that never fires again looks exactly like a quiet trigger, and
// runlore_recurrence_suppressed simply stays at zero with nothing to explain it
// (#471). Naming each outcome lets the caller log the reason, so the interesting
// one — within the cooldown, but nothing conclusive to stand on — is visible.
type recurrenceDecision string

const (
	recurrenceOff            recurrenceDecision = "gate_off"            // no gate, no cooldown, or no trigger key
	recurrenceFirstLook      recurrenceDecision = "first_look"          // this trigger has never been investigated
	recurrenceCooldownLapsed recurrenceDecision = "cooldown_lapsed"     // the last look is older than the cooldown
	recurrenceNoAnswer       recurrenceDecision = "no_conclusive_prior" // looked recently, but never reached an answer
	recurrenceContested      recurrenceDecision = "contested_by_human"  // a standing 👎 re-arms investigation
	recurrenceSuppressed     recurrenceDecision = "recurrence_suppressed"
)

// suppressed reports whether d is the one decision that skips the paid loop.
func (d recurrenceDecision) suppressed() bool { return d == recurrenceSuppressed }

// decide reports whether req should be suppressed and why, given the trigger's
// prior-investigation snapshot. A pure function of (config, history, clock): now is
// a parameter so the decision matrix is testable without sleeping.
func (g *RecurrenceGate) decide(req Request, prior outcome.TriggerRecurrence, now time.Time) recurrenceDecision {
	if g == nil || g.Cooldown <= 0 || req.TriggerKey == "" {
		return recurrenceOff
	}
	switch {
	case prior.Count == 0:
		return recurrenceFirstLook
	case now.Sub(prior.Last) >= g.Cooldown:
		return recurrenceCooldownLapsed
	case !prior.Concluded():
		return recurrenceNoAnswer // we still owe a real answer: retry
	case prior.Contested():
		return recurrenceContested
	}
	return recurrenceSuppressed
}
