package action

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
	log           *slog.Logger
	now           func() time.Time

	mu     sync.Mutex
	paused bool
	recent []time.Time // recent execution timestamps (rate limiting)
}

// NewAuto builds the auto executor from the policy (window defaults to 1h).
func NewAuto(exec Executor, p config.AutoPolicy, log *slog.Logger) *Auto {
	window := p.Window.Std()
	if window <= 0 {
		window = time.Hour
	}
	return &Auto{
		exec: exec, dryRun: p.DryRun, minConfidence: p.MinConfidence,
		maxPerWindow: p.MaxPerWindow, window: window, log: log, now: time.Now,
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
		return annotate("[auto: skipped — paused]")
	case inv.Confidence < a.minConfidence:
		a.log.Info("auto skipped: confidence below threshold", "confidence", inv.Confidence, "min", a.minConfidence)
		return annotate(fmt.Sprintf("[auto: skipped — confidence %.2f < %.2f]", inv.Confidence, a.minConfidence))
	case !act.Reversible:
		a.log.Warn("auto refuses irreversible action", "op", act.Op, "target", target(act))
		return annotate("[auto: skipped — irreversible]")
	case act.Op == "":
		return annotate("[auto: skipped — no executable op]")
	case a.dryRun:
		a.log.Info("auto dry-run (would execute)", "op", act.Op, "target", target(act))
		return annotate("[auto: dry-run — would execute]")
	case !a.reserve():
		a.log.Warn("auto rate limit reached; action not executed", "max", a.maxPerWindow, "window", a.window.String())
		return annotate("[auto: skipped — rate-limited]")
	default:
		if err := a.exec.Execute(ctx, act); err != nil {
			a.log.Error("auto-execute failed", "op", act.Op, "target", target(act), "err", err)
			return annotate("[auto: FAILED — " + err.Error() + "]")
		}
		a.log.Info("auto-executed", "op", act.Op, "target", target(act))
		return annotate("[auto-executed]")
	}
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
