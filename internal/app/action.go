// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"log/slog"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
)

// BuildAuditor opens the append-only action audit log when configured, else a
// no-op. Validate already requires AuditLogPath for both executing modes
// (approve and auto), so the Nop fallback below only ever applies to off/suggest.
//
// The existing hash chain is verified on open (OpenVerified). If it is broken
// (insertion, edit, or mid-chain deletion) the response is mode-gated, because
// this is where cfg is available:
//   - approve/auto (RunLore executes + audits actions) → return an error so
//     startup FAILS CLOSED: it must not append to, or act against, a chain whose
//     integrity can no longer be vouched for.
//   - off/suggest (nothing is executed) → log a WARNING and proceed: the auditor
//     still opens and keeps appending, so a non-executing deployment isn't blocked.
//
// An empty or absent chain is valid (not broken). Tail-truncation (dropping the
// most-recent records) leaves a valid prefix and is NOT detectable here — see
// docs/security-model.md.
func BuildAuditor(cfg *config.Config, log *slog.Logger) (audit.Auditor, func(), error) {
	if cfg.Actions.AuditLogPath == "" {
		return audit.Nop{}, func() {}, nil
	}
	l, err := audit.OpenVerified(cfg.Actions.AuditLogPath)
	if err != nil {
		if cfg.Actions.Mode == config.ActionApprove || cfg.Actions.Mode == config.ActionAuto {
			// Fail closed: an executing rung must not act against an untrustworthy chain.
			return nil, func() {}, fmt.Errorf("audit log integrity check failed (mode=%s, fail closed): %w", cfg.Actions.Mode, err)
		}
		// Non-executing mode: warn and fall back to a plain append so the deployment
		// still records, but the broken history is surfaced loudly.
		log.Warn("audit log chain failed verification; proceeding because actions are not executed in this mode",
			"mode", cfg.Actions.Mode, "path", cfg.Actions.AuditLogPath, "err", err)
		plain, oerr := audit.Open(cfg.Actions.AuditLogPath)
		if oerr != nil {
			return nil, func() {}, fmt.Errorf("open audit log: %w", oerr)
		}
		return plain, func() { _ = plain.Close() }, nil
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
	log.Info("rung-2 approval-gated actions enabled (GitOps suspend/resume/reconcile)")
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
