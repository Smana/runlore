// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	"github.com/Smana/runlore/internal/outcome"

	github "github.com/Smana/runlore/internal/forge/github"
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
		return nil // no forge, nothing to groom — sweeps are strictly additive
	}
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		log.Warn("curate sweeps disabled: forge.kb_repo must be owner/name", "kb_repo", cfg.Forge.KBRepo)
		return nil
	}
	base := cfg.Forge.BaseBranch
	if base == "" {
		base = "main"
	}
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, github.TokenFunc(tok))
	guarded := curate.Guard{Inner: client, DryRun: cfg.Curate.Sweeps.DryRun(), Audit: aud, Log: log}
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
