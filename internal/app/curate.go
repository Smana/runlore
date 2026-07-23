// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/outcome"

	github "github.com/Smana/runlore/internal/forge/github"
)

// RunCurate grooms the KB backlog (Phase-2 curation agent). It runs the
// backlog-dedup pass (collapses duplicate open PRs across history) and the
// lifecycle sweep (closes stale, unprotected PRs by forge age). When
// outcome.ledger_path is configured, it also runs the Queue pass (promotes
// solved→ready-to-merge when the incident resolves), the Recurrence pass
// (opens a knowledge-gap issue for repeatedly-unresolved patterns), and the
// Contested pass (warns the open KB PR when humans 👎'd the investigation
// behind it).
func RunCurate(args []string) error {
	fs := flag.NewFlagSet("curate", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	dry := fs.Bool("dry-run", false, "log and audit what the passes would do without writing to the forge")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Forge.KBRepo == "" {
		return fmt.Errorf("curate requires forge.kb_repo")
	}
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	tok := BuildForgeTokenSource(cfg, log)
	if tok == nil {
		return fmt.Errorf("curate requires a configured GitHub App (forge.github_app)")
	}
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		return fmt.Errorf("forge.kb_repo must be owner/name")
	}
	base := cfg.Forge.BaseBranch
	if base == "" {
		base = "main"
	}
	forge := github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, github.TokenFunc(tok))
	// Same audit chain as the action executors (actions.audit_log_path); Nop when
	// unconfigured. Every forge write (or dry-run skip) below lands in it.
	aud, auditClose, aerr := BuildAuditor(cfg, log)
	if aerr != nil {
		return aerr
	}
	defer auditClose()
	guarded := curate.Guard{Inner: forge, DryRun: *dry, Audit: aud, Log: log}

	// Open the outcome ledger when configured; the ledger-backed passes are wired
	// only when it exists. outcome.New succeeds on a missing file, so LogLedgerStartup
	// makes a misconfigured/empty ledger loud (a mount problem, not "no work").
	var ledger *outcome.Ledger
	if cfg.Outcome.LedgerPath != "" {
		l, lerr := outcome.New(cfg.Outcome.LedgerPath)
		if lerr != nil {
			return fmt.Errorf("open outcome ledger %q: %w", cfg.Outcome.LedgerPath, lerr)
		}
		ledger = l
		LogLedgerStartup(log, ledger.Status())
	} else {
		LogLedgerStartup(log, outcome.Status{}) // disabled: a plain info, no warning
	}

	log.Info("curate: grooming KB backlog", "repo", cfg.Forge.KBRepo, "dry_run", *dry)
	BuildCurateAgent(cfg, guarded, ledger, log).Run(context.Background())
	return nil
}

// BuildCurateAgent assembles the grooming passes over a (typically Guard-wrapped)
// forge. ledger may be nil — the ledger-backed passes (Queue, Recurrence,
// Contested, Retirement) are then skipped. Shared by the one-shot `lore curate`
// CLI and the in-server sweeper so the two can never drift.
//
// StaleAfter is honoured as-is: 0/unset disables the lifecycle sweep (Lifecycle.Run
// returns early). The Helm chart ships config.curate.stale_after: 720h, so scheduled
// runs sweep at 30 days; a bare `lore curate` with no config does suppress+dedup only.
func BuildCurateAgent(cfg *config.Config, forge curate.GuardedForge, ledger *outcome.Ledger, log *slog.Logger) curate.Agent {
	agent := curate.Agent{Log: log, Passes: []curate.Pass{
		// Suppress runs FIRST: a re-draft of a rejected entry must not survive long
		// enough for Dedup to bless it as a cluster canonical.
		curate.Suppress{Forge: forge, Source: curate.ClosedPRSuppression{Forge: forge}, Log: log},
		curate.Dedup{Forge: forge, Log: log},
		curate.Lifecycle{Forge: forge, StaleAfter: cfg.Curate.StaleAfter.Std(), Log: log},
	}}
	if ledger == nil {
		return agent
	}
	agent.Passes = append(agent.Passes,
		curate.Queue{Forge: forge, Checker: curate.LedgerResolutionChecker{Ledger: ledger}, Log: log},
		// Recurrence escalates recurring UNRESOLVED patterns AND recurring
		// closed-unmerged (human-rejected) entries: the latter via a knowledge-gap
		// issue that links the closed PR — never reopening it. The suppression set is
		// derived from the forge's closed-unmerged KB PRs on every run (no store).
		curate.Recurrence{
			Forge:      forge,
			Ledger:     ledger,
			Threshold:  cfg.Curate.RecurrenceThreshold,
			Suppressed: curate.ClosedPRSuppression{Forge: forge},
			Log:        log,
		},
		// Contested surfaces standing 👎 votes on the OPEN KB PR they relate to:
		// a 👎 on a fresh investigation weighs nothing in recall trust (no catalog
		// entry yet), but it is exactly what the human reviewing the pending entry
		// needs to see before merging. Idempotent via a hidden per-trigger comment
		// marker — no store, mirroring the other passes.
		curate.Contested{Forge: forge, Ledger: ledger, KBRepo: cfg.Forge.KBRepo, Log: log},
	)
	// Retirement (opt-in) closes the garbage-collection half of the loop: it opens a
	// human-reviewed "retire" PR for a MERGED entry whose outcome factor stayed below
	// the trust floor across a sustained run of observations. It never merges and never
	// deletes — a human is the load-bearing gate; the PR only stamps `status: retired`.
	// Idempotent and human-veto-aware via a hidden per-entry PR-body marker (no store).
	if cfg.Curate.Retirement.Enabled {
		agent.Passes = append(agent.Passes, curate.Retirement{
			Forge:           forge,
			Stats:           ledger,
			MinObservations: cfg.Curate.Retirement.MinObservations,
			Floor:           cfg.Curate.Retirement.Floor,
			Prior:           cfg.Curate.Retirement.Prior,
			Log:             log,
		})
	}
	return agent
}

// LogLedgerStartup reports, at the right level, what the outcome ledger looks
// like to this `lore curate` process — turning the previously-silent no-op into
// a visible warning. The Queue + Recurrence passes read the ledger, but
// outcome.New succeeds even when the file is absent, so a misconfigured mount
// (the ledger lives on a volume the CronJob doesn't see — e.g. persistence not
// enabled, the path not under catalog.mountPath, or a fresh per-Job emptyDir)
// would silently produce zero work. We still run the passes; we just make the
// likely misconfiguration loud.
func LogLedgerStartup(log *slog.Logger, s outcome.Status) {
	switch {
	case !s.Configured:
		log.Info("curate: outcome ledger not configured; Queue + Recurrence passes skipped")
	case !s.Present:
		log.Warn("curate: outcome ledger configured but its file is absent here — "+
			"Queue + Recurrence will find nothing to do (check the ledger is on a volume "+
			"this CronJob mounts: enable persistence and point outcome.ledger_path under catalog.mountPath)",
			"ledger", s.Path)
	case s.Events == 0:
		log.Warn("curate: outcome ledger is present but empty — Queue + Recurrence have no episodes "+
			"to act on (if the serve pod is recording outcomes, verify both pods share the same "+
			"persistent volume rather than separate emptyDirs)",
			"ledger", s.Path)
	default:
		log.Info("curate: Queue + Recurrence enabled", "ledger", s.Path, "events", s.Events)
	}
}
