package investigate

import (
	"log/slog"
	"time"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
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
// The gate is deliberately human-deferential, in both directions:
//   - it only suppresses a CONCLUSIVE prior answer (verdict no_action /
//     action_suggested / action_required) — an inconclusive or pre-verdict prior
//     never suppresses, because there is no answer worth not repeating;
//   - a standing 👎 on the trigger breaks the cooldown immediately — a human
//     saying "that diagnosis is wrong" re-arms the very next occurrence.
//
// A suppressed occurrence costs nothing and says nothing: no model call, no
// notification, no ledger open. That last part is load-bearing — recording an
// open would slide the byTrigger newest-open timestamp and the cooldown would
// never lapse while the incident keeps firing. Anchored on the last REAL
// investigation, a persistent failure is re-investigated once per cooldown
// (with its recurrence count intact) instead of once per resync.
type RecurrenceGate struct {
	Outcome  RecurrenceStats
	Cooldown time.Duration // 0 disables the gate (default: off, opt-in)
	Log      *slog.Logger  // optional; nil-safe
}

// suppress reports whether req should be suppressed, returning the prior
// investigation's facts for the caller's log line. now is a parameter so the
// decision matrix is testable without sleeping.
func (g *RecurrenceGate) suppress(req Request, now time.Time) (outcome.TriggerRecurrence, bool) {
	if g == nil || g.Outcome == nil || g.Cooldown <= 0 || req.TriggerKey == "" {
		return outcome.TriggerRecurrence{}, false
	}
	r := g.Outcome.Recurrence(req.TriggerKey)
	if r.Count == 0 || now.Sub(r.Last) >= g.Cooldown {
		return r, false
	}
	switch providers.Verdict(r.Verdict) {
	case providers.VerdictNoAction, providers.VerdictActionSuggested, providers.VerdictActionRequired:
		// conclusive — eligible for suppression
	default:
		return r, false // inconclusive or pre-verdict: retry, we owe a real answer
	}
	if r.FeedbackDown > 0 {
		return r, false // a human contested the diagnosis: cooldown broken
	}
	return r, true
}
