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
// solved→ready-to-merge when the incident resolves) and the Recurrence pass
// (opens a knowledge-gap issue for repeatedly-unresolved patterns).
func RunCurate(args []string) error {
	fs := flag.NewFlagSet("curate", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
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
	// StaleAfter is honoured as-is: 0/unset disables the lifecycle sweep (Lifecycle.Run
	// returns early). The Helm chart ships config.curate.stale_after: 720h, so scheduled
	// runs sweep at 30 days; a bare `lore curate` with no config does dedup only.
	agent := curate.Agent{Log: log, Passes: []curate.Pass{
		curate.Dedup{Forge: forge, Log: log},
		curate.Lifecycle{Forge: forge, StaleAfter: cfg.Curate.StaleAfter.Std(), Log: log},
	}}
	// Queue + Recurrence read the outcome ledger; wire them only when it is configured.
	if cfg.Outcome.LedgerPath != "" {
		ledger, lerr := outcome.New(cfg.Outcome.LedgerPath)
		if lerr != nil {
			return fmt.Errorf("open outcome ledger %q: %w", cfg.Outcome.LedgerPath, lerr)
		}
		agent.Passes = append(agent.Passes,
			curate.Queue{Forge: forge, Checker: curate.LedgerResolutionChecker{Ledger: ledger}, Log: log},
			curate.Recurrence{Forge: forge, Ledger: ledger, Threshold: cfg.Curate.RecurrenceThreshold, Log: log},
		)
		// Warn loudly when the ledger this pod sees is absent/empty: outcome.New
		// succeeds on a missing file, so the passes would otherwise run silently
		// against zero episodes (a misconfigured mount, not "no work").
		LogLedgerStartup(log, ledger.Status())
	} else {
		LogLedgerStartup(log, outcome.Status{}) // disabled: a plain info, no warning
	}
	log.Info("curate: grooming KB backlog", "repo", cfg.Forge.KBRepo)
	agent.Run(context.Background())
	return nil
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
