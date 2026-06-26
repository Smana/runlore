// Command lore is the RunLore CLI and in-cluster agent entrypoint.
//
// RunLore is a self-improving, GitOps-native SRE agent: it reacts to incidents,
// investigates by correlating "what changed" across the GitOps engine and the
// observability stack, and learns into an open knowledge catalog.
//
// See docs/design.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // G108: pprof is opt-in (RUNLORE_PPROF) and bound to 127.0.0.1 only, never the Service
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/app"
	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/coalesce"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	"github.com/Smana/runlore/internal/eval"
	fluxexec "github.com/Smana/runlore/internal/executor/flux"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/mcp"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
	"github.com/Smana/runlore/internal/server"
	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/trigger"

	github "github.com/Smana/runlore/internal/forge/github"
)

var version = "0.0.0-dev"

const usage = `lore — the RunLore SRE agent

Usage:
  lore investigate --alert <name> [--namespace <ns>] [--message <text>]   investigate on-demand, print findings
  lore serve [--config <path>] [--addr <addr>]        run the in-cluster agent (react to incidents)
  lore catalog sync [--config <path>]                 clone/pull + index the knowledge catalog
  lore eval [--config <path>] [--cases <dir>]         replay recorded cases, score RCA identification
  lore eval --live [--scenarios <dir>] [--n 3]        live-fire on the cluster: grade coverage + RCA
  lore curate [--config <path>]                       groom the KB backlog (dedup open PRs)
  lore mcp [--config <path>]                          serve GitOps what-changed over MCP (stdio; for HolmesGPT et al.)
  lore version                                        print version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Printf("lore %s\n", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
	case "eval":
		if err := runEval(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "eval:", err)
			os.Exit(1)
		}
	case "investigate":
		if err := runInvestigate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "investigate:", err)
			os.Exit(1)
		}
	case "catalog":
		if err := runCatalog(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "catalog:", err)
			os.Exit(1)
		}
	case "curate":
		if err := runCurate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "curate:", err)
			os.Exit(1)
		}
	case "mcp":
		if err := runMCP(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "mcp:", err)
			os.Exit(1)
		}
	case "validate-kb":
		if err := runValidateKB(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "validate-kb:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	addr := fs.String("addr", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := logging.FromConfig(os.Stdout, cfg.Logging.Format, cfg.Logging.Level)
	app.SetMemoryLimitFromCgroup(log) // make the GC respect the container memory cap

	// Opt-in pprof on loopback only (reachable via `kubectl port-forward`, never the
	// Service) — so a memory/CPU issue can be profiled in-cluster without exposing it.
	if os.Getenv("RUNLORE_PPROF") == "true" {
		go func() {
			log.Info("pprof listening", "addr", "127.0.0.1:6060")
			// ReadHeaderTimeout guards against a slow-header (Slowloris) hold even
			// on this loopback-only debug listener.
			pprofSrv := &http.Server{Addr: "127.0.0.1:6060", ReadHeaderTimeout: 10 * time.Second}
			if err := pprofSrv.ListenAndServe(); err != nil {
				log.Warn("pprof server stopped", "err", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// drainGracePeriod bounds how long shutdown waits for the in-flight investigation
	// to finish before forcing it. Keep it under the pod's terminationGracePeriodSeconds
	// (the chart sets 40s) so the process exits cleanly before SIGKILL.
	const drainGracePeriod = 25 * time.Second
	// workCtx drives the leader-only work (leader election, the investigation queue,
	// failure watch, coalescer) and is intentionally SEPARATE from the SIGTERM signal
	// (ctx): on shutdown the leader keeps workCtx alive, drains the in-flight
	// investigation to completion (lease still held, so no other replica acts), then
	// cancels workCtx — releasing the lease. Lost leadership still cancels its own
	// LE-derived context and aborts promptly.
	workCtx, stopWork := context.WithCancel(context.Background())
	defer stopWork()

	// Build kube clients once (best-effort): the dynamic client backs the
	// GitOps-failure watch + what-changed tool; the clientset backs leader election.
	var (
		gitops    providers.GitOpsProvider
		clientset *kubernetes.Clientset
		executor  action.Executor // rung-2 action executor (Flux), when a cluster is reachable
	)
	if restCfg, err := app.RestConfig(); err != nil {
		log.Warn("no kube client; GitOps features + leader election disabled", "err", err)
	} else {
		if dc, derr := dynamic.NewForConfig(restCfg); derr != nil {
			log.Warn("dynamic client unavailable; GitOps features disabled", "err", derr)
		} else {
			gitops = app.BuildGitOps(cfg, dc, log)
			executor = fluxexec.New(dc)
		}
		if cs, cerr := kubernetes.NewForConfig(restCfg); cerr != nil {
			log.Warn("clientset unavailable; leader election disabled", "err", cerr)
		} else {
			clientset = cs
		}
	}

	// Rung-2 (approve) and rung-3 (auto) execution — mutually exclusive by mode, both
	// requiring a reachable cluster. Token + Slack secret gate the control endpoints.
	approvalToken := os.Getenv(cfg.Actions.ApprovalTokenEnv)
	if cfg.Actions.Enabled() && approvalToken == "" {
		return fmt.Errorf("actions enabled (mode=%s) but %s is empty: refusing to start with unauthenticated control endpoints (fail closed)",
			cfg.Actions.Mode, cfg.Actions.ApprovalTokenEnv)
	}
	aud, auditClose, aerr := app.BuildAuditor(cfg)
	if aerr != nil {
		return aerr
	}
	defer auditClose()
	// Audit every cluster mutation at the single Execute seam (both rungs go through it).
	execForActions := executor
	if executor != nil {
		execForActions = action.NewAuditedExecutor(executor, aud)
	}
	approvals := app.BuildApprovals(cfg, execForActions, aud, log)
	auto := app.BuildAuto(cfg, execForActions, aud, log)
	slackSigningSecret := os.Getenv(cfg.Notify.Slack.SigningSecretEnv)
	webhookToken := os.Getenv(cfg.Server.WebhookTokenEnv)
	if err := app.RequireWebhookAuth(cfg, webhookToken); err != nil {
		return err
	}

	// Set up the single shared OTel metrics instance before building the investigator
	// so recall + the investigation loop can record to it from the first request.
	var metricsHandler http.Handler
	if cfg.Telemetry.MetricsEnabled {
		h, shutdown, err := telemetry.Setup(ctx)
		if err != nil {
			return fmt.Errorf("telemetry setup: %w", err)
		}
		defer func() { _ = shutdown(context.Background()) }()
		metricsHandler = h
	}
	metrics := telemetry.NewMetrics() // bound to global provider (no-op when disabled)

	ledger, err := outcome.New(cfg.Outcome.LedgerPath)
	if err != nil {
		return fmt.Errorf("outcome ledger: %w", err)
	}
	if cfg.Outcome.LedgerPath != "" {
		log.Info("outcome ledger enabled", "path", cfg.Outcome.LedgerPath)
	}

	inv, cat := app.BuildInvestigator(ctx, cfg, gitops, approvals, auto, metrics, ledger, log)
	queue := investigate.NewQueue(inv, log)
	var rlStarts *ratelimit.Window
	if rl := cfg.Investigation.RateLimit; rl.MaxPerWindow > 0 {
		w := rl.Window.Std()
		rlStarts = ratelimit.New(rl.MaxPerWindow, w)
		log.Info("investigation rate limit configured",
			"max_per_window", rl.MaxPerWindow, "window", w, "max_requeues", rl.MaxRequeues)
	}
	// Always wire metrics into the Queue so InvestigationsStarted emits even when
	// rate-limiting is unconfigured (max_per_window == 0).
	queue.ConfigureRateLimit(rlStarts, cfg.Investigation.RateLimit.MaxRequeues, metrics)

	// Coalescer: fold correlated incidents into one investigation per group key.
	// out is the flush sink — converts a batch into a single Request and enqueues it.
	var cz *coalesce.Coalescer
	if cc := cfg.Investigation.Coalesce; cc.Enabled {
		out := func(incs []config.Incident) {
			rep := investigate.FromIncident(incs[0])
			if len(incs) > 1 {
				rep.Message = coalesce.Summarize(incs)
			}
			// Record every constituent fingerprint so each alert's resolve webhook
			// matches an open (a single incident stays one fingerprint).
			var fps []string
			for _, inc := range incs {
				if inc.Fingerprint != "" {
					fps = append(fps, inc.Fingerprint)
				}
			}
			rep.Fingerprints = fps
			queue.Enqueue(rep)
		}
		cz = coalesce.New(coalesce.Config{
			Debounce:          cc.Debounce.Std(),
			MaxWait:           cc.MaxWait.Std(),
			MaxBatch:          cc.MaxBatch,
			Cooldown:          cc.Cooldown.Std(),
			CorrelationLabels: cc.CorrelationLabels,
		}, out)
		cz.Metrics = metrics
		log.Info("investigation coalescer enabled",
			"debounce", cc.Debounce.Std(), "max_wait", cc.MaxWait.Std(),
			"max_batch", cc.MaxBatch, "cooldown", cc.Cooldown.Std())
	}

	reinv := app.BuildReinvestigator(ctx, cfg, gitops, metrics, log)

	// failureDedup is created ONCE (process scope), not per leadership term, so it
	// survives leader flaps: the gitops informer's initial-LIST replay of every
	// still-failing workload on each re-acquire is then suppressed instead of
	// re-investigated (GO-P1A). Deduper is safe for concurrent use across terms.
	failureDedup := trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std())

	// startWork runs the leader-only loops (investigation queue + failure watch +
	// re-investigate poller), scoped to a context cancelled when leadership is lost.
	startWork := func(workCtx context.Context) {
		go queue.Run(workCtx)
		if cfg.Triggers.GitOpsFailures.Enabled && gitops != nil {
			app.StartGitOpsFailureWatch(workCtx, cfg, queue, gitops, failureDedup, metrics, log)
		}
		if reinv != nil {
			log.Info("re-investigate poller enabled", "label", investigate.ReinvestigateLabel)
			go reinv.Poll(workCtx, 2*time.Minute)
		}
	}

	var leader atomic.Bool
	useLE := cfg.LeaderElection.Enabled && clientset != nil
	if useLE {
		go app.RunLeaderElection(workCtx, cfg, clientset, &leader, log, startWork)
	} else {
		leader.Store(true) // no leader election: this replica is always active + ready
		startWork(workCtx)
	}

	// Build-info + leadership gauges (runlore_build_info / runlore_leader). No-op
	// unless telemetry installed a real provider above.
	if cfg.Telemetry.MetricsEnabled {
		if err := telemetry.RegisterRuntimeGauges(version, leader.Load); err != nil {
			log.Warn("runtime gauges registration failed", "err", err)
		}
	}

	// readyz reflects leadership so the Service routes webhooks only to the leader.
	acts := server.Actions{
		Approvals:    approvals,
		Token:        approvalToken,
		SlackSecret:  slackSigningSecret,
		WebhookToken: webhookToken,
		ApproverIDs:  cfg.Notify.Slack.ApproverIDs,
	}
	if auto != nil {
		acts.Pauser = auto // avoid a typed-nil interface when auto is disabled
	}
	srv := server.New(cfg, queue, app.ReadyFunc(leader.Load, cat, app.CatalogConfigured(cfg)), acts, metricsHandler, log)
	srv.SetMetrics(metrics) // ingress counters emit regardless of coalescing
	srv.SetOutcomeLedger(ledger)
	if cz != nil {
		srv.SetCoalescer(cz)
		go cz.Run(workCtx, cfg.Investigation.Coalesce.Debounce.Std()/2)
	}
	httpSrv := app.NewHTTPServer(*addr, srv.Handler())
	// Graceful shutdown: on SIGTERM, stop accepting webhooks, let the in-flight
	// investigation finish within a bounded grace (lease still held), then release.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		log.Info("shutdown: stopping intake; draining in-flight investigation")
		_ = httpSrv.Shutdown(context.Background())
		dctx, cancelDrain := context.WithTimeout(context.Background(), drainGracePeriod)
		queue.Drain(dctx)
		cancelDrain()
		stopWork() // release the leader lease + stop the queue/watch/coalescer
	}()
	log.Info("runlore serving", "addr", *addr, "leader_election", useLE)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	<-drained // wait for the graceful drain to finish before exiting
	return nil
}

// runEval replays recorded incident cases through the investigation loop and
// reports the RCA-identification rate. Requires a configured model.
func runEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	casesDir := fs.String("cases", "examples/eval", "directory of replay cases")
	live := fs.Bool("live", false, "live-fire mode: run scenarios against the real cluster")
	scnDir := fs.String("scenarios", "eval/scenarios", "directory of live-fire scenarios")
	recordDir := fs.String("record", "eval/fixtures", "where to write recorded runs (replay corpus)")
	reportDir := fs.String("report-dir", "eval/reports", "where to write the campaign report")
	prevReport := fs.String("baseline", "", "previous report JSON for regression diff")
	n := fs.Int("n", 1, "runs per case: replay defaults to 1, live to 10 when unset")
	failUnder := fs.Float64("fail-under", 0, "fail (non-zero exit) when campaign pass-rate < this (0 = no gate)")
	stamp := fs.String("stamp", "", "report timestamp (RFC3339); blank = now")
	jProvider := fs.String("judge-provider", "", "judge model provider (default: investigation model)")
	jBaseURL := fs.String("judge-base-url", "", "judge model base URL")
	jModel := fs.String("judge-model", "", "judge model name")
	jKeyEnv := fs.String("judge-api-key-env", "", "env var holding the judge API key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	nExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "n" {
			nExplicit = true
		}
	})
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !app.ModelConfigured(cfg) {
		return fmt.Errorf("eval requires a configured model (set config.model)")
	}
	if *live {
		if !nExplicit {
			*n = 10
		}
		return runEvalLive(cfg, *scnDir, *recordDir, *reportDir, *prevReport, *stamp, *n,
			*jProvider, *jBaseURL, *jModel, *jKeyEnv)
	}
	// ---- existing replay path (unchanged) ----
	cases, err := eval.Load(*casesDir)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no eval cases found in %s", *casesDir)
	}
	apiKey := ""
	if cfg.Model.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Model.APIKeyEnv)
	}
	runner := &eval.Runner{Model: app.BuildModel(cfg, apiKey), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	camp := runner.RunN(context.Background(), cases, *n)
	for _, a := range camp.Aggregates {
		status := "MISSED"
		if a.Reached {
			status = "REACHED"
		}
		flaky := ""
		if a.Flaky {
			flaky = " flaky"
		}
		fmt.Printf("%-7s  %-32s  pass-rate=%.0f%% (n=%d)%s", status, a.Name, a.PassRate*100, a.Runs, flaky)
		if len(a.Missing) > 0 {
			fmt.Printf("  missing: %s", strings.Join(a.Missing, ", "))
		}
		fmt.Println()
	}
	if len(camp.Aggregates) == 0 {
		fmt.Print("\nno eval cases ran")
	} else {
		fmt.Printf("\nreached %d/%d cases (%.0f%%)", camp.ReachedCases(), len(camp.Aggregates), camp.PassRate()*100)
		if *failUnder > 0 {
			fmt.Printf("  threshold=%.0f%%", *failUnder*100)
		}
	}
	fmt.Println()

	if *reportDir != "" {
		st := *stamp
		if st == "" {
			st = time.Now().UTC().Format(time.RFC3339)
		}
		if b, err := camp.JSON(); err != nil {
			fmt.Fprintf(os.Stderr, "eval: report not written: %v\n", err)
		} else if mkErr := os.MkdirAll(*reportDir, 0o750); mkErr != nil {
			fmt.Fprintf(os.Stderr, "eval: report not written: %v\n", mkErr)
		} else {
			path := filepath.Join(*reportDir, strings.ReplaceAll(st, ":", "-")+"-replay.json")
			if wErr := os.WriteFile(path, b, 0o600); wErr != nil {
				fmt.Fprintf(os.Stderr, "eval: report not written: %v\n", wErr)
			} else {
				fmt.Printf("report: %s\n", path)
			}
		}
	}

	return eval.GateError(camp, *failUnder)
}

// shellStepRunner executes a scenario step as a shell command (kubectl/flux/test).
type shellStepRunner struct{}

func (shellStepRunner) Run(ctx context.Context, step string) error {
	// step is an operator-authored eval scenario command (kubectl/flux/test), not
	// untrusted input — executing it as a shell command is the runner's purpose.
	cmd := exec.CommandContext(ctx, "sh", "-c", step) //nolint:gosec // G204: step is operator-authored scenario YAML

	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr // step output is progress, not findings
	return cmd.Run()
}

// runEvalLive runs the live-fire campaign and writes a dated report.
func runEvalLive(cfg *config.Config, scnDir, recordDir, reportDir, prevReport, stamp string, n int,
	jProvider, jBaseURL, jModel, jKeyEnv string) error {
	scns, err := eval.LoadScenarios(scnDir)
	if err != nil {
		return err
	}
	if len(scns) == 0 {
		return fmt.Errorf("no scenarios found in %s", scnDir)
	}
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	ctx := context.Background()
	model, tools, recall, _ := app.BuildModelAndTools(ctx, cfg, app.GitOpsFromKube(cfg, log), nil, log)
	judge := eval.ModelJudge{Model: app.BuildJudgeModel(cfg, jProvider, jBaseURL, jModel, jKeyEnv)}

	runner := &eval.LiveRunner{
		Model: model, BaseTools: tools, Judge: judge, Steps: shellStepRunner{}, Log: log, N: n, Recall: recall,
		OnRecord: func(scn eval.Scenario, calls []eval.Call) {
			if err := eval.WriteCase(recordDir, eval.RecordedCase(scn, calls)); err != nil {
				log.Warn("record case failed", "id", scn.ID, "err", err)
			}
		},
	}
	var results []eval.LiveResult
	for _, scn := range scns {
		log.Info("running scenario", "id", scn.ID)
		results = append(results, runner.RunScenario(ctx, scn))
	}

	if stamp == "" {
		stamp = time.Now().UTC().Format(time.RFC3339)
	}
	rep := eval.NewLiveReport(stamp, n, results)
	if err := os.MkdirAll(reportDir, 0o750); err != nil {
		return err
	}
	base := filepath.Join(reportDir, strings.ReplaceAll(stamp, ":", "-"))
	if err := os.WriteFile(base+".json", rep.JSON(), 0o600); err != nil {
		return err
	}
	md := rep.Markdown()
	if prevReport != "" {
		// prevReport is an operator-supplied --prev report path, not untrusted input.
		if data, rerr := os.ReadFile(prevReport); rerr == nil { //nolint:gosec // G304: operator-supplied baseline report path
			var prev eval.LiveReport
			if json.Unmarshal(data, &prev) == nil {
				if reg := rep.RegressionsVS(prev); len(reg) > 0 {
					md += "\n## ⚠️ Regressions vs baseline\n\n- " + strings.Join(reg, "\n- ") + "\n"
				}
			}
		}
	}
	if err := os.WriteFile(base+".md", []byte(md), 0o600); err != nil {
		return err
	}
	fmt.Print(md)
	fmt.Printf("\nreport: %s.md / .json\n", base)
	return nil
}

// runCurate grooms the KB backlog (Phase-2 curation agent). It runs the
// backlog-dedup pass (collapses duplicate open PRs across history) and the
// lifecycle sweep (closes stale, unprotected PRs by forge age). When
// outcome.ledger_path is configured, it also runs the Queue pass (promotes
// solved→ready-to-merge when the incident resolves) and the Recurrence pass
// (opens a knowledge-gap issue for repeatedly-unresolved patterns).
func runCurate(args []string) error {
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
	tok := app.BuildForgeTokenSource(cfg, log)
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
		logLedgerStartup(log, ledger.Status())
	} else {
		logLedgerStartup(log, outcome.Status{}) // disabled: a plain info, no warning
	}
	log.Info("curate: grooming KB backlog", "repo", cfg.Forge.KBRepo)
	agent.Run(context.Background())
	return nil
}

// logLedgerStartup reports, at the right level, what the outcome ledger looks
// like to this `lore curate` process — turning the previously-silent no-op into
// a visible warning. The Queue + Recurrence passes read the ledger, but
// outcome.New succeeds even when the file is absent, so a misconfigured mount
// (the ledger lives on a volume the CronJob doesn't see — e.g. persistence not
// enabled, the path not under catalog.mountPath, or a fresh per-Job emptyDir)
// would silently produce zero work. We still run the passes; we just make the
// likely misconfiguration loud.
func logLedgerStartup(log *slog.Logger, s outcome.Status) {
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

// runCatalog dispatches catalog subcommands.
func runCatalog(args []string) error {
	if len(args) == 0 || args[0] != "sync" {
		return fmt.Errorf("usage: lore catalog sync [--config <path>]")
	}
	return runCatalogSync(args[1:])
}

// runCatalogSync clones/pulls the catalog Git repo into the mirror and reports the
// indexed entry count — a one-shot of the background sync that serve runs.
func runCatalogSync(args []string) error {
	fs := flag.NewFlagSet("catalog sync", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)

	g := cfg.Catalog.Git
	if g.URL == "" {
		if cfg.Catalog.Dir == "" {
			return fmt.Errorf("no catalog configured (set catalog.git.url or catalog.dir)")
		}
		cat, err := catalog.New(cfg.Catalog.Dir)
		if err != nil {
			return err
		}
		fmt.Printf("catalog: %d entries at %s (no git-sync configured)\n", cat.Len(), cfg.Catalog.Dir)
		return nil
	}
	dir := cfg.Catalog.Dir
	if dir == "" {
		dir = "/var/lib/runlore/catalog"
	}
	var token catalog.TokenFunc
	if env := g.TokenEnv; env != "" {
		if t := os.Getenv(env); t != "" {
			token = func(context.Context) (string, error) { return t, nil }
		}
	} else if ft := app.BuildForgeTokenSource(cfg, log); ft != nil {
		token = catalog.TokenFunc(ft)
	}
	syncer := &catalog.Syncer{URL: g.URL, Branch: g.Branch, Dir: dir, Token: token, Log: log}
	if _, err := syncer.Sync(context.Background()); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	cat, err := catalog.New(dir)
	if err != nil {
		return err
	}
	fmt.Printf("synced %s -> %s (%d entries)\n", g.URL, dir, cat.Len())
	return nil
}

// runMCP serves RunLore's GitOps what-changed capability over the Model Context
// Protocol (stdio JSON-RPC), so an MCP client (e.g. HolmesGPT) can call it as a
// toolset. stdout is the protocol channel; logs go to stderr. Read-only.
func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	gp := app.GitOpsFromKube(cfg, log)
	if gp == nil {
		return fmt.Errorf("what_changed needs a Kubernetes client + config.gitops")
	}
	tool := investigate.WhatChangedTool{GitOps: gp}
	srv := mcp.NewServer("runlore-whatchanged", version, log)
	srv.AddTool(mcp.Tool{
		Name:        "gitops_what_changed",
		Description: tool.Description(),
		InputSchema: json.RawMessage(tool.Schema()),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return tool.Call(ctx, string(args))
		},
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("runlore mcp server ready (stdio)", "tool", "gitops_what_changed")
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}

// runInvestigate runs a single on-demand investigation and prints the findings.
func runInvestigate(args []string) error {
	fs := flag.NewFlagSet("investigate", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	alert := fs.String("alert", "", "alert/symptom name to investigate")
	namespace := fs.String("namespace", "", "namespace of the affected workload")
	message := fs.String("message", "", "free-text symptom description")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *alert == "" && *message == "" {
		return fmt.Errorf("provide --alert and/or --message")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !app.ModelConfigured(cfg) {
		return fmt.Errorf("investigate requires a configured model (set config.model)")
	}
	// Progress logs go to stderr; the findings go to stdout.
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	ctx := context.Background()

	model, tools, recall, _ := app.BuildModelAndTools(ctx, cfg, app.GitOpsFromKube(cfg, log), nil, log)
	var result *providers.Investigation
	li := &investigate.LoopInvestigator{
		Model: model, VerifyModel: app.BuildVerifyModel(cfg), Tools: tools, Recall: recall, Actions: action.New(cfg.Actions), Log: log, Verify: true,
		ModelProvider: cfg.Model.Provider,
		Timeout:       cfg.Investigation.Timeout.Std(),
		OnComplete:    func(inv providers.Investigation) { result = &inv },
	}
	title := *alert
	if title == "" {
		title = "on-demand investigation"
	}
	req := investigate.Request{
		Source: investigate.SourceAlert, Title: title, Message: *message,
		Workload: providers.Workload{Namespace: *namespace},
	}
	if err := li.Investigate(ctx, req); err != nil {
		return err
	}
	if result == nil {
		return fmt.Errorf("investigation produced no findings")
	}
	fmt.Println(notify.Format(*result))
	return nil
}
