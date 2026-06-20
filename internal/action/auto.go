package action

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

// Auto executes remediations without human approval (mode "auto"), under layered
// safety controls: it only ever runs REVERSIBLE actions, requires a minimum
// investigation confidence, is rate-limited, can be paused instantly (kill-switch),
// supports dry-run, and audits every decision. Anything failing a gate is surfaced
// (annotated), never executed.
type Auto struct {
	exec          Executor
	dryRun        bool
	minConfidence float64
	maxPerWindow  int
	window        time.Duration
	policy        *Policy
	audit         audit.Auditor
	log           *slog.Logger
	now           func() time.Time

	mu     sync.Mutex
	paused bool
	recent []time.Time // recent execution timestamps (rate limiting)
}

// NewAuto builds the auto executor from the policy (window defaults to 1h). policy
// is the action envelope, re-checked at the exec boundary (defense in depth). A nil
// auditor falls back to a no-op. The returned Auto starts with the kill-switch
// ENGAGED (paused) — fail closed by construction, so a process/leader restart can
// never resume unattended execution on its own; an operator must Resume() it.
func NewAuto(exec Executor, p config.AutoPolicy, policy *Policy, aud audit.Auditor, log *slog.Logger) *Auto {
	window := p.Window.Std()
	if window <= 0 {
		window = time.Hour
	}
	if aud == nil {
		aud = audit.Nop{}
	}
	return &Auto{
		exec: exec, dryRun: p.DryRun, minConfidence: p.MinConfidence,
		maxPerWindow: p.MaxPerWindow, window: window, policy: policy, audit: aud, log: log, now: time.Now,
		paused: true,
	}
}

// Pause engages the kill-switch: all auto-execution halts until Resume.
func (a *Auto) Pause() { a.mu.Lock(); a.paused = true; a.mu.Unlock() }

// Resume clears the kill-switch.
func (a *Auto) Resume() { a.mu.Lock(); a.paused = false; a.mu.Unlock() }

// Paused reports whether the kill-switch is engaged.
func (a *Auto) Paused() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.paused }

// Run evaluates each (already envelope-compliant) action against the auto safety
// gates and executes — or, in dry-run, logs — the eligible ones. It returns the
// actions with their outcome annotated into the description for delivery/audit.
func (a *Auto) Run(ctx context.Context, inv providers.Investigation) []providers.Action {
	out := make([]providers.Action, 0, len(inv.Actions))
	for _, act := range inv.Actions {
		out = append(out, a.runOne(ctx, inv, act))
	}
	return out
}

func (a *Auto) runOne(ctx context.Context, inv providers.Investigation, act providers.Action) providers.Action {
	annotate := func(prefix string) providers.Action {
		act.Description = prefix + " " + act.Description
		return act
	}
	switch {
	case a.Paused():
		a.log.Warn("auto paused; action not executed (kill-switch)", "op", act.Op, "target", target(act))
		a.record(act, audit.DecisionSkipped, "paused")
		return annotate("[auto: skipped — paused]")
	case inv.Confidence < a.minConfidence:
		a.log.Info("auto skipped: confidence below threshold", "confidence", inv.Confidence, "min", a.minConfidence)
		a.record(act, audit.DecisionSkipped, fmt.Sprintf("confidence %.2f < %.2f", inv.Confidence, a.minConfidence))
		return annotate(fmt.Sprintf("[auto: skipped — confidence %.2f < %.2f]", inv.Confidence, a.minConfidence))
	case !act.Reversible:
		a.log.Warn("auto refuses irreversible action", "op", act.Op, "target", target(act))
		a.record(act, audit.DecisionSkipped, "irreversible")
		return annotate("[auto: skipped — irreversible]")
	case act.Op == "":
		a.record(act, audit.DecisionSkipped, "no executable op")
		return annotate("[auto: skipped — no executable op]")
	case a.dryRun:
		a.log.Info("auto dry-run (would execute)", "op", act.Op, "target", target(act))
		a.record(act, audit.DecisionDryRun, "")
		return annotate("[auto: dry-run — would execute]")
	case !a.reserve():
		a.log.Warn("auto rate limit reached; action not executed", "max", a.maxPerWindow, "window", a.window.String())
		a.record(act, audit.DecisionSkipped, "rate-limited")
		return annotate("[auto: skipped — rate-limited]")
	default:
		// Re-check the kill-switch immediately before executing: Pause() may have
		// landed between the switch evaluation above and here (gate TOCTOU).
		if a.Paused() {
			a.log.Warn("auto paused before execute (kill-switch)", "op", act.Op, "target", target(act))
			a.record(act, audit.DecisionSkipped, "paused")
			return annotate("[auto: skipped — paused]")
		}
		// Defense in depth: re-derive + re-validate the full envelope at the exec
		// boundary (as Approvals.Approve does), so a mutated or un-Reviewed action
		// can't reach the cluster — reversibility/blast come from the op, not the model.
		act = deriveSafety(act)
		if a.policy != nil {
			if reason := a.policy.violation(act); reason != "" {
				a.log.Warn("auto denied at exec boundary", "op", act.Op, "target", target(act), "reason", reason)
				a.record(act, audit.DecisionDenied, reason)
				return annotate("[auto: denied — " + reason + "]")
			}
		}
		// executed/failed are audited at the executor seam (NewAuditedExecutor).
		if err := a.exec.Execute(ContextWithActor(ctx, "auto"), act); err != nil {
			a.log.Error("auto-execute failed", "op", act.Op, "target", target(act), "err", err)
			return annotate("[auto: FAILED — " + err.Error() + "]")
		}
		a.log.Info("auto-executed", "op", act.Op, "target", target(act))
		return annotate("[auto-executed]")
	}
}

// record appends an audit entry for an auto action attempt.
func (a *Auto) record(act providers.Action, d audit.Decision, reason string) {
	recordAttempt(a.audit, "auto", act, d, reason)
}

// recordAttempt writes an action-attempt audit record. Shared by both rungs
// (auto + approvals) so the record shape stays in one place.
func recordAttempt(aud audit.Auditor, actor string, act providers.Action, d audit.Decision, reason string) {
	_ = aud.Log(audit.Record{Actor: actor, Op: act.Op, Target: target(act), Decision: d, Reason: reason})
}

// reserve consumes one slot from the rate-limit window if available.
func (a *Auto) reserve() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := a.now().Add(-a.window)
	kept := a.recent[:0]
	for _, t := range a.recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	a.recent = kept
	if a.maxPerWindow > 0 && len(a.recent) >= a.maxPerWindow {
		return false
	}
	a.recent = append(a.recent, a.now())
	return true
}

func target(a providers.Action) string {
	return a.Target.Kind + "/" + a.Target.Namespace + "/" + a.Target.Name
}
