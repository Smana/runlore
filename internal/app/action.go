package app

import (
	"fmt"
	"log/slog"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
)

// BuildAuditor opens the append-only action audit log when configured, else a
// no-op. Validate already requires AuditLogPath when actions.mode=auto.
func BuildAuditor(cfg *config.Config) (audit.Auditor, func(), error) {
	if cfg.Actions.AuditLogPath == "" {
		return audit.Nop{}, func() {}, nil
	}
	l, err := audit.Open(cfg.Actions.AuditLogPath)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open audit log: %w", err)
	}
	return l, func() { _ = l.Close() }, nil
}

// BuildApprovals enables rung-2 approval-gated execution for action mode "approve"
// (requires a reachable cluster).
func BuildApprovals(cfg *config.Config, exec action.Executor, aud audit.Auditor, log *slog.Logger) *action.Approvals {
	if cfg.Actions.Mode != config.ActionApprove {
		return nil
	}
	if exec == nil {
		log.Warn("approval-gated actions disabled: no cluster executor available")
		return nil
	}
	log.Info("rung-2 approval-gated actions enabled (Flux suspend/resume/reconcile)")
	return action.NewApprovals(exec, action.New(cfg.Actions), aud, log)
}

// BuildAuto enables rung-3 unattended execution for action mode "auto" (requires a
// reachable cluster). Heavily gated: reversible-only, confidence-floored, rate-
// limited, kill-switchable, and audited. The rung is EXPERIMENTAL and FROZEN
// (FEAT-1): unattended execution contradicts the read-only-first posture and is the
// only path that turns a prompt-injected finding into a cluster mutation, so it gets
// no further investment and may be removed — prefer "approve", which captures nearly
// all the value. Recommend dry_run if you evaluate it.
func BuildAuto(cfg *config.Config, exec action.Executor, aud audit.Auditor, log *slog.Logger) *action.Auto {
	if cfg.Actions.Mode != config.ActionAuto {
		return nil
	}
	if exec == nil {
		log.Warn("auto-execution disabled: no cluster executor available")
		return nil
	}
	a := cfg.Actions.Auto
	log.Warn("rung-3 AUTO execution ENABLED — EXPERIMENTAL and NOT recommended on real clusters; "+
		"reversible actions execute WITHOUT human approval. Prefer mode=approve (human-click).",
		"dry_run", a.DryRun, "min_confidence", a.MinConfidence, "max_per_window", a.MaxPerWindow, "window", a.Window.Std().String())
	au := action.NewAuto(exec, a, action.New(cfg.Actions), aud, log)
	// NewAuto starts paused (fail closed by construction across cold start / failover);
	// surface that to the operator, who resumes via the authenticated /actions/resume.
	log.Warn("rung-3 auto starts PAUSED (kill-switch engaged) — POST /actions/resume to begin auto-execution")
	return au
}
