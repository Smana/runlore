// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	"github.com/Smana/runlore/internal/outcome"
)

// BuildSweeper assembles the in-server grooming sweeper, or nil when sweeps are
// off (curate.sweeps.mode: off) or the KB forge is not configured. No new required
// config: with forge credentials already present, sweeps default to DRY-RUN — the
// operator flips curate.sweeps.mode: apply to let them act.
func BuildSweeper(cfg *config.Config, ledger *outcome.Ledger, aud audit.Auditor, log *slog.Logger) *curate.Sweeper {
	if !cfg.Curate.Sweeps.Enabled() {
		return nil
	}
	tok := BuildForgeTokenSource(cfg, log)
	if tok == nil || cfg.Forge.KBRepo == "" {
		// Say so out loud. Reaching here means the operator EXPLICITLY asked for
		// sweeps (Sweeps.Enabled() is true just above), so returning nil in silence
		// is indistinguishable from sweeps running and finding nothing to groom —
		// the backlog quietly rots and the logs never mention it. The very next
		// branch already warns on its failure; this one was the odd one out.
		//
		// A GitLab deployment lands here every time (grooming is GitHub-only), which
		// is the case most likely to be mistaken for "working".
		log.Warn("curate sweeps disabled: no usable KB forge — Phase-2 grooming is GitHub-only",
			"provider", forgeProviderName(cfg), "kb_repo", cfg.Forge.KBRepo)
		return nil
	}
	guarded, err := buildGuardedForge(cfg, tok, cfg.Curate.Sweeps.DryRun(), aud, log)
	if err != nil {
		log.Warn("curate sweeps disabled", "err", err)
		return nil
	}
	// The LIVE serve ledger: the ledger-backed passes see the same episodes the
	// investigation loop records — no shared-volume mount to misconfigure (the
	// CronJob's classic footgun; see LogLedgerStartup in curate.go).
	var l *outcome.Ledger
	if ledger != nil && ledger.Enabled() {
		l = ledger
	}
	return &curate.Sweeper{
		Agent:    BuildCurateAgent(cfg, guarded, l, log),
		Interval: cfg.Curate.Sweeps.Interval.Std(),
		Log:      log,
	}
}
