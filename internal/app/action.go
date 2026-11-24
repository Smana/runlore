// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/executor/revertpr"
	github "github.com/Smana/runlore/internal/forge/github"
	gitlab "github.com/Smana/runlore/internal/forge/gitlab"
	"github.com/Smana/runlore/internal/providers"
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

// CompositeExecutor dispatches revert_pr actions to the pure-Go revert executor
// and cluster GitOps ops (suspend/resume/reconcile) to the Flux/Argo executor.
type CompositeExecutor struct {
	Cluster action.Executor
	Revert  action.Executor
}

// Execute routes a to the appropriate backend executor, failing closed when the
// required backend is unavailable.
func (c CompositeExecutor) Execute(ctx context.Context, a providers.Action) error {
	if a.Op == "revert_pr" {
		if c.Revert == nil {
			return fmt.Errorf("revert_pr executor is not configured")
		}
		return c.Revert.Execute(ctx, a)
	}
	if c.Cluster == nil {
		return fmt.Errorf("cluster action executor is not configured for op %q", a.Op)
	}
	return c.Cluster.Execute(ctx, a)
}

// CombineExecutors composes the optional cluster executor and optional revert_pr
// executor into a single action.Executor (or nil when neither is available).
func CombineExecutors(clusterExec, revertExec action.Executor) action.Executor {
	if clusterExec == nil && revertExec == nil {
		return nil
	}
	if revertExec == nil {
		return clusterExec
	}
	return CompositeExecutor{Cluster: clusterExec, Revert: revertExec}
}

// BuildRevertForge builds the forge client for actions.revert_pr from either
// its dedicated credential block or, when explicitly opted in via
// reuse_forge_credentials: true, the top-level forge credentials. Returns nil
// when revert_pr is disabled or credentials are unavailable.
func BuildRevertForge(cfg *config.Config, log *slog.Logger) providers.RevertForge {
	rp := cfg.Actions.RevertPR
	if !rp.Enabled {
		return nil
	}
	if rp.Forge != nil {
		return buildRevertForgeFrom(rp.Forge.Provider, rp.Forge.GitHubAPIURL, rp.Forge.GitHubApp, rp.Forge.GitLab, log)
	}
	if !rp.ReuseForgeCredentials {
		log.Warn("revert_pr disabled: no dedicated forge credentials and reuse_forge_credentials is false")
		return nil
	}
	return buildRevertForgeFrom(cfg.Forge.Provider, cfg.Forge.GitHubAPIURL, cfg.Forge.GitHubApp, cfg.Forge.GitLab, log)
}

func buildRevertForgeFrom(provider, ghAPIURL string, ga config.GitHubApp, gl config.GitLab, log *slog.Logger) providers.RevertForge {
	switch provider {
	case "gitlab":
		if gl.TokenEnv == "" {
			return nil
		}
		tok := os.Getenv(gl.TokenEnv)
		if tok == "" {
			log.Warn("revert_pr gitlab auth disabled: empty token env", "env", gl.TokenEnv)
			return nil
		}
		return gitlab.New(gl.BaseURL, "", "", func(context.Context) (string, error) { return tok, nil }).WithLogger(log)
	case "", "github":
		if ga.AppID == 0 || ga.InstallationID == 0 || ga.PrivateKeyEnv == "" {
			return nil
		}
		pemData := os.Getenv(ga.PrivateKeyEnv)
		if pemData == "" {
			log.Warn("revert_pr github auth disabled: empty private key env", "env", ga.PrivateKeyEnv)
			return nil
		}
		key, err := github.ParsePrivateKey(pemData)
		if err != nil {
			log.Warn("revert_pr github auth disabled: bad private key", "err", err)
			return nil
		}
		tok := github.NewAppTokenSource(ghAPIURL, ga.AppID, ga.InstallationID, key).Token
		return github.New(ghAPIURL, "", "", "", tok)
	default:
		log.Warn("revert_pr disabled: unknown forge provider", "provider", provider)
		return nil
	}
}

// BuildRevertExecutor wires the pure-Go revert_pr executor when enabled and
// authenticated, else returns nil.
func BuildRevertExecutor(cfg *config.Config, aud audit.Auditor, onOpened revertpr.OnOpenedFunc, log *slog.Logger) action.Executor {
	rf := BuildRevertForge(cfg, log)
	if rf == nil {
		return nil
	}
	log.Info("revert_pr approve-only executor enabled", "allowed_repos", cfg.Actions.RevertPR.AllowedRepos)
	return revertpr.New(rf, cfg.Actions.RevertPR.AllowedRepos, cfg.Actions.RevertPR.ProtectedPaths,
		revertpr.WithAuditor(aud),
		revertpr.WithLogger(log),
		revertpr.WithOnOpened(onOpened),
	)
}
