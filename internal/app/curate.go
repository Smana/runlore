// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"

	github "github.com/Smana/runlore/internal/forge/github"
)

// RunCurate grooms the KB backlog (Phase-2 curation agent). It runs the
// backlog-dedup pass (collapses duplicate open PRs across history) and the
// lifecycle sweep (closes stale, unprotected PRs by forge age). When
// outcome.ledger_path is configured, it also runs the Queue pass (promotes
// solved→ready-to-merge when the incident resolves and, under factory.curation.defer,
// labels or withdraws from the factory's verified evidence), the Recurrence pass
// (opens a knowledge-gap issue for repeatedly-unresolved patterns), and the
// Contested pass (warns the open KB PR when humans 👎'd the investigation
// behind it).
//
// `lore curate queue-issue` does not groom. It reconciles labels through the Queue pass, rebuilds the
// conflicting entry PRs that can use it, then rewrites the Knowledge queue issue at once, whatever
// curate.queue_issue.schedule says. So it can force-push RunLore's own runlore/kb- branches and add or
// strip ready-to-merge, unless --dry-run. `lore curate refresh-entries` only rebuilds: a PR labelled
// ready-to-merge or with held evidence, at most 10 a run, or with --pr the one named.
func RunCurate(args []string) error {
	fs := flag.NewFlagSet("curate", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	dry := fs.Bool("dry-run", false, "log and audit what the passes would do without writing to the forge")
	onlyPR := fs.Int("pr", 0, "refresh-entries: rebuild this one PR, whatever its label or evidence")
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
		if sub != "queue-issue" && sub != "refresh-entries" {
			return fmt.Errorf("unknown curate subcommand %q (want queue-issue, refresh-entries, or none to groom)", sub)
		}
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A positional left after the flags would be ignored and the run would groom the whole KB in
	// apply: refuse it rather than guess what was meant.
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q: a subcommand comes first (lore curate queue-issue --config <path>)", fs.Arg(0))
	}
	if *onlyPR < 0 || *onlyPR != 0 && sub != "refresh-entries" {
		return fmt.Errorf("--pr takes a pull request number, with refresh-entries only")
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
		// A GitLab-configured deployment always lands here, and NOT because the
		// operator got their config wrong: BuildForgeTokenSource only mints GitHub
		// App tokens, because Phase-2 grooming has no GitLab code path at all.
		// Sending them off to configure a GitHub App — which they cannot have and
		// would not help — is a wild goose chase, so name the real limitation.
		// See website/content/docs/integrations/gitlab.md#not-yet-supported-on-gitlab.
		if cfg.Forge.Provider == "gitlab" {
			return fmt.Errorf("lore curate does not support GitLab yet (forge.provider: gitlab): " +
				"the Learn loop works on GitLab, but the Phase-2 grooming passes are GitHub-only — " +
				"review KB merge requests by hand for now")
		}
		return fmt.Errorf("curate requires a configured GitHub App (forge.github_app)")
	}
	// Same audit chain as the action executors (actions.audit_log_path); Nop when
	// unconfigured. Every forge write (or dry-run skip) below lands in it.
	aud, auditClose, aerr := BuildAuditor(cfg, log)
	if aerr != nil {
		return aerr
	}
	defer auditClose()
	guarded, gerr := buildGuardedForge(cfg, tok, *dry, aud, log)
	if gerr != nil {
		return gerr
	}

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

	line := queueLine(cfg, *dry, log)
	if sub == "queue-issue" {
		log.Info("curate: reconciling labels, rebuilding conflicting entry PRs, rewriting the Knowledge queue issue", "repo", cfg.Forge.KBRepo, "dry_run", *dry)
		return runQueueIssue(context.Background(), buildQueueIssue(cfg, guarded, ledger, line, log))
	}
	if sub == "refresh-entries" {
		log.Info("curate: rebuilding conflicting entry PRs", "repo", cfg.Forge.KBRepo, "pr", *onlyPR, "dry_run", *dry)
		refresh := buildRefresh(cfg, guarded, ledger, log)
		refresh.Only = *onlyPR
		return runRefresh(context.Background(), refresh)
	}
	log.Info("curate: grooming KB backlog", "repo", cfg.Forge.KBRepo, "dry_run", *dry)
	runGroom(context.Background(), cfg, guarded, ledger, line, log)
	return nil
}

// runGroom, runQueueIssue and runRefresh are what RunCurate runs, package variables so a test can tell which
// of them a command line reached.
var (
	runGroom = func(ctx context.Context, cfg *config.Config, forge curate.GuardedForge, ledger *outcome.Ledger, line providers.LineNotifier, log *slog.Logger) {
		BuildCurateAgent(cfg, forge, ledger, line, log).Run(ctx)
	}
	runQueueIssue = func(ctx context.Context, q curate.QueueIssue) error { return q.RunNow(ctx) }
	runRefresh    = func(ctx context.Context, r curate.RefreshEntries) error { return r.Run(ctx) }
)

// queueLine is the Slack line sink of the Knowledge queue: the configured notifiers when
// curate.queue_issue.slack is set and the run writes (a dry-run posts nothing), else nil. The
// interface is nil, never a typed nil, when no notifier can carry a line.
func queueLine(cfg *config.Config, dryRun bool, log *slog.Logger) providers.LineNotifier {
	if !cfg.Curate.QueueIssue.Slack || dryRun {
		return nil
	}
	m, err := BuildNotifier(cfg, nil, log)
	if err != nil {
		log.Warn("curate: queue_issue.slack is set but the notifiers did not build; no Slack line", "err", err)
		return nil
	}
	if m == nil || m.Len() == 0 {
		log.Warn("curate: queue_issue.slack is set but no notifier is configured; no Slack line")
		return nil
	}
	return m
}

// buildQueueIssue assembles the Knowledge queue pass, shared by `lore curate queue-issue` and the
// agent so the two cannot drift. Evidence and Stats are set only with a ledger: a nil ledger must
// not become a typed-nil interface.
func buildQueueIssue(cfg *config.Config, forge curate.GuardedForge, ledger *outcome.Ledger, line providers.LineNotifier, log *slog.Logger) curate.QueueIssue {
	refresh := buildRefresh(cfg, forge, ledger, log)
	q := curate.QueueIssue{Forge: forge, Line: line, KBRepo: cfg.Forge.KBRepo, Refresh: &refresh, ScreenEntries: cfg.Factory.Curation.Defer, Log: log}
	if ledger != nil {
		q.Evidence, q.Stats = ledger, ledger
		q.Reconcile = buildQueue(cfg, forge, ledger, log)
	}
	if cfg.Curate.QueueIssue.Enabled() {
		wd, h, m, err := config.ParseSchedule(cfg.Curate.QueueIssue.Schedule)
		if err == nil {
			q.Weekday, q.Hour, q.Minute = wd, h, m
			return q
		}
		log.Warn("curate: queue_issue.schedule ignored", "err", err)
	}
	q.Unscheduled = true
	return q
}

// buildQueue assembles the Queue pass over a ledger. Under factory.curation.defer the ledger holds
// the factory's verifications, and the label follows them. Otherwise Evidence stays nil and adopters
// keep the resolution path.
func buildQueue(cfg *config.Config, forge curate.GuardedForge, ledger *outcome.Ledger, log *slog.Logger) curate.Queue {
	queue := curate.Queue{Forge: forge, Checker: curate.LedgerResolutionChecker{Ledger: ledger}, Log: log}
	if cfg.Factory.Curation.Defer {
		queue.Evidence, queue.KBRepo = ledger, cfg.Forge.KBRepo
	}
	return queue
}

// buildRefresh assembles the refresh a caller that does not run the Queue right after uses: with a
// ledger, a rebuilt PR goes straight back through the Queue's evidence check.
func buildRefresh(cfg *config.Config, forge curate.GuardedForge, ledger *outcome.Ledger, log *slog.Logger) curate.RefreshEntries {
	r := curate.RefreshEntries{Forge: forge, Log: log}
	if ledger != nil {
		queue := buildQueue(cfg, forge, ledger, log)
		r.Evidence, r.Promote = queue.Evidence, queue.PromoteOne
	}
	return r
}

// buildGuardedForge constructs the KB forge client and wraps it in the dry-run +
// audit Guard, in one place so the one-shot `lore curate` CLI and the in-server
// sweeper groom against an identically-built forge (the drift BuildCurateAgent
// exists to prevent, one layer up). The differing error posture stays at the
// call sites: RunCurate returns the error, BuildSweeper warns and disables.
func buildGuardedForge(cfg *config.Config, tok ForgeToken, dryRun bool, aud audit.Auditor, log *slog.Logger) (curate.GuardedForge, error) {
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		return nil, fmt.Errorf("forge.kb_repo must be owner/name, got %q", cfg.Forge.KBRepo)
	}
	base := cfg.Forge.BaseBranch
	if base == "" {
		base = "main"
	}
	client := newKBClient(cfg, owner, repo, base, github.TokenFunc(tok))
	return curate.Guard{Inner: client, DryRun: dryRun, Audit: aud, Log: log}, nil
}

// BuildCurateAgent assembles the grooming passes over a (typically Guard-wrapped)
// forge. ledger may be nil — the ledger-backed passes (Queue, Recurrence,
// Contested, Retirement, Revalidation) are then skipped. Shared by the one-shot
// `lore curate` CLI and the in-server sweeper so the two can never drift.
//
// StaleAfter is honoured as-is: 0/unset disables the lifecycle sweep (Lifecycle.Run
// returns early). The Helm chart ships config.curate.stale_after: 720h, so scheduled
// runs sweep at 30 days; a bare `lore curate` with no config does suppress+dedup only.
//
// line posts the Knowledge queue's Slack line; nil in dry-run, or when queue_issue.slack is off.
// The queue pass is added whenever queue_issue.schedule is set, last, with or without a ledger.
func BuildCurateAgent(cfg *config.Config, forge curate.GuardedForge, ledger *outcome.Ledger, line providers.LineNotifier, log *slog.Logger) curate.Agent {
	agent := curate.Agent{Log: log, Passes: []curate.Pass{
		// Suppress runs FIRST: a re-draft of a rejected entry must not survive long
		// enough for Dedup to bless it as a cluster canonical.
		curate.Suppress{Forge: forge, Source: curate.ClosedPRSuppression{Forge: forge}, Log: log},
		curate.Dedup{Forge: forge, Log: log},
		curate.Lifecycle{Forge: forge, StaleAfter: cfg.Curate.StaleAfter.Std(), Log: log},
	}}
	addQueueIssue := func() {
		if cfg.Curate.QueueIssue.Enabled() {
			qi := buildQueueIssue(cfg, forge, ledger, line, log)
			// The Queue pass just ran in this agent. And the refresh force-pushes, so a schedule alone
			// does not switch it on: it takes the same gate as Queue.Refresh.
			qi.Reconcile = nil
			if !cfg.Factory.Curation.Defer {
				qi.Refresh = nil
			}
			agent.Passes = append(agent.Passes, qi)
		}
	}
	if ledger == nil {
		addQueueIssue()
		return agent
	}
	queue := buildQueue(cfg, forge, ledger, log)
	// The rebuild force-pushes, so an adopter who has not opted in keeps today's Queue. No Promote:
	// Queue.Run lists right after.
	if cfg.Factory.Curation.Defer {
		queue.Refresh = &curate.RefreshEntries{Forge: forge, Evidence: queue.Evidence, Log: log}
	}
	agent.Passes = append(agent.Passes,
		queue,
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
	// Revalidation (opt-in) is retirement's mirror image: it proposes CONFIRMING a
	// merged entry that was recalled for a live incident which then resolved, by
	// stamping `last_validated` — the field the forge deliberately leaves unset at
	// draft time because it claims human confirmation. Merging the PR is that
	// confirmation; RunLore only brings the evidence. Sharing the retirement floor
	// keeps the two disjoint (below the floor is retirement's, at or above it is
	// this pass's), so one sweep can never propose both for the same entry.
	if cfg.Curate.Revalidation.Enabled {
		agent.Passes = append(agent.Passes, curate.Revalidation{
			Forge:       forge,
			Stats:       ledger,
			MinInterval: cfg.Curate.Revalidation.MinInterval.Std(),
			MaxOpen:     cfg.Curate.Revalidation.MaxOpen,
			Floor:       cfg.Curate.Revalidation.Floor,
			Prior:       cfg.Curate.Revalidation.Prior,
			Log:         log,
		})
	}
	addQueueIssue()
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
