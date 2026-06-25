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
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/coalesce"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/eval"
	fluxexec "github.com/Smana/runlore/internal/executor/flux"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/logs/victorialogs"
	"github.com/Smana/runlore/internal/metrics/prometheus"
	anthropic "github.com/Smana/runlore/internal/model/anthropic"
	gemini "github.com/Smana/runlore/internal/model/gemini"
	openai "github.com/Smana/runlore/internal/model/openai"
	"github.com/Smana/runlore/internal/network/awsvpc"
	"github.com/Smana/runlore/internal/network/gcpfirewall"
	"github.com/Smana/runlore/internal/network/hubble"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	awscloud "github.com/Smana/runlore/internal/providers/cloud/aws"
	"github.com/Smana/runlore/internal/providers/cluster"
	"github.com/Smana/runlore/internal/providers/gitops/argocd"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
	"github.com/Smana/runlore/internal/ratelimit"
	"github.com/Smana/runlore/internal/server"
	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/trigger"
	"github.com/Smana/runlore/internal/whatchanged"

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
	setMemoryLimitFromCgroup(log) // make the GC respect the container memory cap

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

	// Build kube clients once (best-effort): the dynamic client backs the
	// GitOps-failure watch + what-changed tool; the clientset backs leader election.
	var (
		gitops    providers.GitOpsProvider
		clientset *kubernetes.Clientset
		executor  action.Executor // rung-2 action executor (Flux), when a cluster is reachable
	)
	if restCfg, err := restConfig(); err != nil {
		log.Warn("no kube client; GitOps features + leader election disabled", "err", err)
	} else {
		if dc, derr := dynamic.NewForConfig(restCfg); derr != nil {
			log.Warn("dynamic client unavailable; GitOps features disabled", "err", derr)
		} else {
			gitops = buildGitOps(cfg, dc, log)
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
	aud, auditClose, aerr := buildAuditor(cfg)
	if aerr != nil {
		return aerr
	}
	defer auditClose()
	// Audit every cluster mutation at the single Execute seam (both rungs go through it).
	execForActions := executor
	if executor != nil {
		execForActions = action.NewAuditedExecutor(executor, aud)
	}
	approvals := buildApprovals(cfg, execForActions, aud, log)
	auto := buildAuto(cfg, execForActions, aud, log)
	slackSigningSecret := os.Getenv(cfg.Notify.Slack.SigningSecretEnv)
	webhookToken := os.Getenv(cfg.Server.WebhookTokenEnv)
	if err := requireWebhookAuth(cfg, webhookToken); err != nil {
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

	inv, cat := buildInvestigator(ctx, cfg, gitops, approvals, auto, metrics, ledger, log)
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

	reinv := buildReinvestigator(ctx, cfg, gitops, metrics, log)

	// startWork runs the leader-only loops (investigation queue + failure watch +
	// re-investigate poller), scoped to a context cancelled when leadership is lost.
	startWork := func(workCtx context.Context) {
		go queue.Run(workCtx)
		if cfg.Triggers.GitOpsFailures.Enabled && gitops != nil {
			startGitOpsFailureWatch(workCtx, cfg, queue, gitops, metrics, log)
		}
		if reinv != nil {
			log.Info("re-investigate poller enabled", "label", investigate.ReinvestigateLabel)
			go reinv.Poll(workCtx, 2*time.Minute)
		}
	}

	var leader atomic.Bool
	useLE := cfg.LeaderElection.Enabled && clientset != nil
	if useLE {
		go runLeaderElection(ctx, cfg, clientset, &leader, log, startWork)
	} else {
		leader.Store(true) // no leader election: this replica is always active + ready
		startWork(ctx)
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
	srv := server.New(cfg, queue, readyFunc(leader.Load, cat, catalogConfigured(cfg)), acts, metricsHandler, log)
	srv.SetMetrics(metrics) // ingress counters emit regardless of coalescing
	srv.SetOutcomeLedger(ledger)
	if cz != nil {
		srv.SetCoalescer(cz)
		go cz.Run(ctx, cfg.Investigation.Coalesce.Debounce.Std()/2)
	}
	httpSrv := newHTTPServer(*addr, srv.Handler())
	go func() {
		<-ctx.Done()
		_ = httpSrv.Shutdown(context.Background())
	}()
	log.Info("runlore serving", "addr", *addr, "leader_election", useLE)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// newHTTPServer builds the serving http.Server with every inbound bound set. Go's
// zero defaults leave each of these unlimited, exposing the long-lived server to
// Slowloris (slow header/body), unbounded idle keep-alives, and oversized headers.
// Payloads (Alertmanager/Slack) are small and synchronous, so 30s read/write is
// generous while still cutting off slow attackers; the body itself is capped per
// handler (1 MiB).
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// runLeaderElection blocks running Lease-based leader election; the leader runs
// startWork and reports ready. Lost leadership cancels the work context.
func runLeaderElection(ctx context.Context, cfg *config.Config, cs *kubernetes.Clientset, leader *atomic.Bool, log *slog.Logger, startWork func(context.Context)) {
	name := cfg.LeaderElection.Name
	if name == "" {
		name = "runlore-leader"
	}
	id := podName()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: name, Namespace: podNamespace()},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(workCtx context.Context) {
				log.Info("acquired leadership", "id", id)
				leader.Store(true)
				startWork(workCtx)
			},
			OnStoppedLeading: func() {
				log.Info("lost leadership", "id", id)
				leader.Store(false)
			},
			OnNewLeader: func(current string) {
				if current != id {
					log.Info("standby; another replica leads", "leader", current)
				}
			},
		},
	})
}

// podName returns this pod's identity for leader election.
func podName() string {
	if n := os.Getenv("POD_NAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

// podNamespace resolves the namespace from the downward API or the service-account mount.
func podNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "default"
}

// modelProvider returns the configured model provider name (default "openai").
func modelProvider(cfg *config.Config) string {
	switch cfg.Model.Provider {
	case "anthropic", "gemini":
		return cfg.Model.Provider
	default:
		return "openai"
	}
}

// modelConfigured reports whether a usable model is configured: a provider with a
// built-in default endpoint (anthropic, gemini), or any provider with a base_url.
func modelConfigured(cfg *config.Config) bool {
	switch cfg.Model.Provider {
	case "anthropic", "gemini":
		return true
	default:
		return cfg.Model.BaseURL != ""
	}
}

// catalogConfigured reports whether the operator asked for a knowledge catalog
// (a mounted dir or a git-sync repo). It is independent of whether the load
// succeeded: readyFunc uses it to keep a configured-but-failed catalog (which
// buildCatalog returns as nil) from collapsing readiness to pure leadership and
// serving incident traffic with no knowledge base.
func catalogConfigured(cfg *config.Config) bool {
	return cfg.Catalog.Dir != "" || cfg.Catalog.Git.URL != ""
}

// requireWebhookAuth fails closed on the serve path when the LLM investigator is
// wired but the alert webhook is anonymous. The webhook's labels/annotations flow
// verbatim into the LLM prompt (and bill the model), so an unauthenticated caller
// must not reach it once a model is configured — regardless of actions.mode. This
// lives on the serve path, NOT in config.Validate: Validate is shared by every
// subcommand (e.g. `lore investigate` legitimately needs a model and has no
// webhook), so the requirement is scoped to where the webhook is actually served.
// It mirrors the approval-token fail-closed guard above.
func requireWebhookAuth(cfg *config.Config, webhookToken string) error {
	if modelConfigured(cfg) && webhookToken == "" {
		return fmt.Errorf("model configured but server.webhook_token_env (%q) is empty: refusing to start with an unauthenticated alert webhook (fail closed)",
			cfg.Server.WebhookTokenEnv)
	}
	return nil
}

// buildModel builds the ModelProvider for the configured provider.
func buildModel(cfg *config.Config, apiKey string) providers.ModelProvider {
	switch cfg.Model.Provider {
	case "anthropic":
		return anthropic.New(cfg.Model.BaseURL, cfg.Model.Model, apiKey)
	case "gemini":
		return gemini.New(cfg.Model.BaseURL, cfg.Model.Model, apiKey)
	default:
		return openai.New(cfg.Model.BaseURL, cfg.Model.Model, apiKey)
	}
}

// buildCatalog returns the kb_search backing store, or nil when no catalog is
// configured. With a Git URL it starts a background syncer (running on every
// replica, so a failover standby is already warm) that re-indexes after each pull;
// otherwise it loads a static mounted directory once.
func buildCatalog(ctx context.Context, cfg *config.Config, forgeTok forgeToken, metrics *telemetry.Metrics, log *slog.Logger) *catalog.Catalog {
	// warnInvalid surfaces structurally-invalid (but parseable) entries at load
	// time — a backstop for the CI gate. The entry is still indexed and served
	// (one bad entry never empties the catalog); we just log loudly + count it.
	warnInvalid := func(cat *catalog.Catalog) {
		kbvalidate.WarnInvalid(cat.Entries(), func(path string, errs []kbvalidate.Issue) {
			log.Warn("invalid KB entry indexed", "path", path,
				"issues", len(errs), "first", errs[0].Field+": "+errs[0].Message)
			if metrics != nil {
				metrics.CatalogInvalidEntries.Add(ctx, 1)
			}
		})
	}
	if cfg.Catalog.Git.URL != "" {
		dir := cfg.Catalog.Dir
		if dir == "" {
			dir = "/var/lib/runlore/catalog"
		}
		cat := catalog.NewEmpty()
		// Auth precedence: explicit token_env, else the shared forge GitHub App
		// identity (one credential for both curation writes and catalog reads).
		var token catalog.TokenFunc
		if env := cfg.Catalog.Git.TokenEnv; env != "" {
			if t := os.Getenv(env); t != "" {
				token = func(context.Context) (string, error) { return t, nil }
			}
		} else if forgeTok != nil {
			token = catalog.TokenFunc(forgeTok)
			log.Info("catalog git-sync using the forge GitHub App identity")
		}
		syncer := &catalog.Syncer{URL: cfg.Catalog.Git.URL, Branch: cfg.Catalog.Git.Branch, Dir: dir, Token: token, Log: log}
		go syncer.Run(ctx, cfg.Catalog.Git.Interval.Std(), func() {
			skipped, err := cat.Reload(dir)
			if err != nil {
				log.Warn("catalog reload failed", "dir", dir, "err", err)
				return
			}
			if len(skipped) > 0 {
				log.Warn("catalog entries skipped (unparseable)", "count", len(skipped), "files", skipped)
			}
			log.Info("catalog synced", "url", cfg.Catalog.Git.URL, "entries", cat.Len())
			warnInvalid(cat)
		})
		log.Info("catalog git-sync enabled", "url", cfg.Catalog.Git.URL, "dir", dir)
		return cat
	}
	if cfg.Catalog.Dir != "" {
		cat, err := catalog.New(cfg.Catalog.Dir)
		if err != nil {
			log.Warn("catalog disabled", "dir", cfg.Catalog.Dir, "err", err)
			return nil
		}
		log.Info("catalog loaded", "dir", cfg.Catalog.Dir, "entries", cat.Len())
		warnInvalid(cat)
		return cat
	}
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
	if !modelConfigured(cfg) {
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
	runner := &eval.Runner{Model: buildModel(cfg, apiKey), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

// buildJudgeModel builds the (stronger) grader model from --judge-* flags, falling
// back to the configured investigation model when unset.
func buildJudgeModel(cfg *config.Config, provider, baseURL, model, apiKeyEnv string) providers.ModelProvider {
	if provider == "" && model == "" {
		apiKey := ""
		if cfg.Model.APIKeyEnv != "" {
			apiKey = os.Getenv(cfg.Model.APIKeyEnv)
		}
		return buildModel(cfg, apiKey)
	}
	apiKey := os.Getenv(apiKeyEnv)
	switch provider {
	case "anthropic":
		return anthropic.New(baseURL, model, apiKey)
	case "gemini":
		return gemini.New(baseURL, model, apiKey)
	default:
		return openai.New(baseURL, model, apiKey)
	}
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
	model, tools, recall, _ := buildModelAndTools(ctx, cfg, gitOpsFromKube(cfg, log), nil, log)
	judge := eval.ModelJudge{Model: buildJudgeModel(cfg, jProvider, jBaseURL, jModel, jKeyEnv)}

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

// buildNotifier assembles the configured chat notifiers (best-effort fan-out).
func buildNotifier(cfg *config.Config, log *slog.Logger) *notify.Multi {
	var ns []providers.Notifier
	// Bot token (chat.postMessage) takes precedence over an incoming webhook.
	if sl := cfg.Notify.Slack; sl.BotTokenEnv != "" && sl.Channel != "" {
		if tok := os.Getenv(sl.BotTokenEnv); tok != "" {
			ns = append(ns, notify.NewSlackBot(tok, sl.Channel))
		}
	} else if env := cfg.Notify.Slack.WebhookURLEnv; env != "" {
		if url := os.Getenv(env); url != "" {
			ns = append(ns, notify.NewSlack(url))
		}
	}
	if mc := cfg.Notify.Matrix; mc.Homeserver != "" && mc.RoomID != "" && mc.AccessTokenEnv != "" {
		if tok := os.Getenv(mc.AccessTokenEnv); tok != "" {
			ns = append(ns, notify.NewMatrix(mc.Homeserver, mc.RoomID, tok))
		}
	}
	return notify.NewMulti(log, ns...)
}

// forgeToken mints GitHub App installation tokens.
type forgeToken func(context.Context) (string, error)

// buildForgeTokenSource builds the GitHub App installation-token source shared by
// the curator (issues/PRs) and catalog git-sync (clone auth) — one identity for
// both forge writes and reads. Returns nil when no App is configured.
func buildForgeTokenSource(cfg *config.Config, log *slog.Logger) forgeToken {
	ga := cfg.Forge.GitHubApp
	if ga.AppID == 0 || ga.InstallationID == 0 || ga.PrivateKeyEnv == "" {
		return nil
	}
	pemData := os.Getenv(ga.PrivateKeyEnv)
	if pemData == "" {
		log.Warn("forge auth disabled: empty private key env", "env", ga.PrivateKeyEnv)
		return nil
	}
	key, err := github.ParsePrivateKey(pemData)
	if err != nil {
		log.Warn("forge auth disabled: bad private key", "err", err)
		return nil
	}
	return github.NewAppTokenSource(cfg.Forge.GitHubAPIURL, ga.AppID, ga.InstallationID, key).Token
}

// buildAuditor opens the append-only action audit log when configured, else a
// no-op. Validate already requires AuditLogPath when actions.mode=auto.
func buildAuditor(cfg *config.Config) (audit.Auditor, func(), error) {
	if cfg.Actions.AuditLogPath == "" {
		return audit.Nop{}, func() {}, nil
	}
	l, err := audit.Open(cfg.Actions.AuditLogPath)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open audit log: %w", err)
	}
	return l, func() { _ = l.Close() }, nil
}

// buildApprovals enables rung-2 approval-gated execution for action mode "approve"
// (requires a reachable cluster).
func buildApprovals(cfg *config.Config, exec action.Executor, aud audit.Auditor, log *slog.Logger) *action.Approvals {
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

// buildAuto enables rung-3 unattended execution for action mode "auto" (requires a
// reachable cluster). Heavily gated: reversible-only, confidence-floored, rate-
// limited, kill-switchable, and audited. The rung is EXPERIMENTAL and FROZEN
// (FEAT-1): unattended execution contradicts the read-only-first posture and is the
// only path that turns a prompt-injected finding into a cluster mutation, so it gets
// no further investment and may be removed — prefer "approve", which captures nearly
// all the value. Recommend dry_run if you evaluate it.
func buildAuto(cfg *config.Config, exec action.Executor, aud audit.Auditor, log *slog.Logger) *action.Auto {
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

// buildCurator returns a Curator when the GitHub App token + KB repo are
// configured, else nil.
func buildCurator(cfg *config.Config, token forgeToken, cat *catalog.Catalog, metrics *telemetry.Metrics, log *slog.Logger) *curator.Curator {
	if token == nil || cfg.Forge.KBRepo == "" {
		return nil
	}
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		log.Warn("curator disabled: kb_repo must be owner/name", "kb_repo", cfg.Forge.KBRepo)
		return nil
	}
	base := cfg.Forge.BaseBranch
	if base == "" {
		base = "main"
	}
	dup := cfg.Forge.DupScore
	if dup == 0 {
		dup = 5.0
	}
	minConf := cfg.Forge.MinConfidence
	if minConf == 0 {
		minConf = 0.75
	}
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, github.TokenFunc(token))
	log.Info("curator enabled", "repo", cfg.Forge.KBRepo, "dup_score", dup, "min_confidence", minConf)
	cur := &curator.Curator{Forge: client, DupScore: dup, MinConfidence: minConf, Metrics: metrics, Log: log}
	if cat != nil { // assign via concrete check to avoid a typed-nil interface
		cur.Catalog = cat
	}
	return cur
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
	tok := buildForgeTokenSource(cfg, log)
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

// buildReinvestigator returns a poller that re-runs KB issues labelled
// "reinvestigate" and posts the fresh findings back, or nil when the forge isn't
// configured. RunLore polls the forge (outbound) — it has no inbound webhooks.
func buildReinvestigator(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, metrics *telemetry.Metrics, log *slog.Logger) *investigate.Reinvestigator {
	token := buildForgeTokenSource(cfg, log)
	if token == nil || cfg.Forge.KBRepo == "" {
		return nil
	}
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		return nil
	}
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, cfg.Forge.BaseBranch, github.TokenFunc(token))
	model, tools, recall, _ := buildModelAndTools(ctx, cfg, gp, metrics, log)
	if recall != nil {
		recall.Metrics = metrics
		recall.Log = log
	}
	run := func(ctx context.Context, req investigate.Request) (providers.Investigation, error) {
		var res providers.Investigation
		var got bool
		li := &investigate.LoopInvestigator{
			Model: model, Tools: tools, Recall: recall, Verify: true, Log: log,
			Metrics:                   metrics,
			ModelProvider:             cfg.Model.Provider,
			MaxSteps:                  cfg.Investigation.MaxSteps,
			MaxToolOutputBytes:        cfg.Investigation.MaxToolOutputBytes,
			MaxTokensPerInvestigation: cfg.Investigation.MaxTokensPerInvestigation,
			Timeout:                   cfg.Investigation.Timeout.Std(),
			OnComplete:                func(inv providers.Investigation) { res, got = inv, true },
		}
		if err := li.Investigate(ctx, req); err != nil {
			return providers.Investigation{}, err
		}
		if !got {
			return providers.Investigation{}, fmt.Errorf("re-investigation was inconclusive")
		}
		return res, nil
	}
	return &investigate.Reinvestigator{Forge: client, Run: run, Log: log}
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
	} else if ft := buildForgeTokenSource(cfg, log); ft != nil {
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

// buildModelAndTools assembles the model, investigation tools, and the instant-recall
// short-circuit from config + the GitOps provider. Shared by serve and investigate.
func buildModelAndTools(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, metrics *telemetry.Metrics, log *slog.Logger) (providers.ModelProvider, []investigate.Tool, *investigate.Recall, *catalog.Catalog) {
	apiKey := ""
	if cfg.Model.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Model.APIKeyEnv)
	}
	model := buildModel(cfg, apiKey)
	forgeTok := buildForgeTokenSource(cfg, log)
	var tools []investigate.Tool
	if gp != nil {
		tools = append(tools, investigate.WhatChangedTool{GitOps: gp})
		// Deep read-only Flux introspection (status/events + dependency tree), when
		// the GitOps provider supports it (Flux does).
		if insp, ok := gp.(providers.GitOpsInspector); ok {
			tools = append(tools, investigate.FluxStatusTool{Inspector: insp}, investigate.FluxTreeTool{Inspector: insp})
		}
	}
	var recall *investigate.Recall
	cat := buildCatalog(ctx, cfg, forgeTok, metrics, log)
	if cat != nil {
		tools = append(tools, investigate.KBSearchTool{Catalog: cat})
		if cfg.Catalog.InstantRecall.Enabled {
			recall = &investigate.Recall{
				Catalog:              cat,
				MinScore:             cfg.Catalog.InstantRecall.MinScore,
				MarginGap:            cfg.Catalog.InstantRecall.MarginGap,
				SoloFloor:            cfg.Catalog.InstantRecall.SoloFloor,
				RequireWorkloadMatch: cfg.Catalog.InstantRecall.RequireWorkloadMatch,
				OutcomePrior:         cfg.Catalog.InstantRecall.OutcomePrior,
				OutcomeFloor:         cfg.Catalog.InstantRecall.OutcomeFloor,
			}
			log.Info("instant recall enabled",
				"min_score", cfg.Catalog.InstantRecall.MinScore,
				"margin_gap", cfg.Catalog.InstantRecall.MarginGap, "solo_floor", cfg.Catalog.InstantRecall.SoloFloor,
				"outcome_prior", cfg.Catalog.InstantRecall.OutcomePrior, "outcome_floor", cfg.Catalog.InstantRecall.OutcomeFloor)
		}
	}
	if cfg.Metrics.URL != "" {
		tools = append(tools, investigate.QueryMetricsTool{Metrics: prometheus.New(cfg.Metrics.URL)})
	}
	if cfg.Logs.URL != "" {
		tools = append(tools, investigate.QueryLogsTool{Logs: victorialogs.New(cfg.Logs.URL)})
	}
	// Network-flow data source (the network_drops tool). Pluggable and CNI-agnostic:
	// no provider is enabled by default. The selected provider must match the cluster's
	// environment (Cilium Hubble, AWS VPC Flow Logs, or GCP Firewall Logs).
	switch cfg.Network.Provider {
	case config.NetworkHubble:
		if cfg.Network.Hubble.URL != "" {
			tools = append(tools, investigate.NetworkDropsTool{Network: hubble.New(cfg.Network.Hubble.URL)})
			log.Info("network provider enabled", "provider", config.NetworkHubble, "url", cfg.Network.Hubble.URL)
			if cfg.Network.URL != "" {
				log.Warn("config.network.url is deprecated; set config.network.provider=hubble and config.network.hubble.url")
			}
		}
	case config.NetworkAWSVPCFlowLogs:
		if nw, err := awsvpc.New(ctx, cfg.Network.AWS.Region, cfg.Network.AWS.LogGroup); err != nil {
			log.Warn("aws-vpc-flow-logs network provider unavailable; network_drops disabled", "err", err)
		} else {
			tools = append(tools, investigate.NetworkDropsTool{Network: nw})
			log.Info("network provider enabled", "provider", config.NetworkAWSVPCFlowLogs, "log_group", cfg.Network.AWS.LogGroup)
		}
	case config.NetworkGCPFirewallLogs:
		if nw, err := gcpfirewall.New(ctx, cfg.Network.GCP.Project); err != nil {
			log.Warn("gcp-firewall-logs network provider unavailable; network_drops disabled", "err", err)
		} else {
			tools = append(tools, investigate.NetworkDropsTool{Network: nw})
			log.Info("network provider enabled", "provider", config.NetworkGCPFirewallLogs, "project", cfg.Network.GCP.Project)
		}
	case "":
		// network signal disabled (default)
	default:
		log.Warn("unknown network provider; network_drops disabled", "provider", cfg.Network.Provider)
	}
	// Read-only cluster access (Flux controller logs + pod status + events), when a
	// cluster is reachable. The same reader backs all three tools.
	if cs := kubeClientset(log); cs != nil {
		reader := cluster.New(cs)
		tools = append(tools,
			investigate.ControllerLogsTool{Logs: reader},
			investigate.PodStatusTool{Kube: reader},
			investigate.KubeEventsTool{Kube: reader},
		)
	}
	// Cloud context (AWS): CloudTrail "what changed" + EC2/ASG/EKS health. Opt-in.
	if cfg.Cloud.Provider == "aws" {
		if cl, err := awscloud.New(ctx, cfg.Cloud.Region, cfg.Cloud.ClusterName); err != nil {
			log.Warn("aws cloud provider unavailable; cloud tools disabled", "err", err)
		} else {
			tools = append(tools, investigate.CloudWhatChangedTool{Cloud: cl}, investigate.CloudResourceHealthTool{Cloud: cl})
			log.Info("cloud provider enabled", "provider", "aws", "region", cfg.Cloud.Region)
		}
	}
	return model, tools, recall, cat
}

// kubeClientset builds a read-only clientset for pod-log access, or nil when no
// cluster is reachable (e.g. local runs without a kubeconfig).
func kubeClientset(log *slog.Logger) *kubernetes.Clientset {
	restCfg, err := restConfig()
	if err != nil {
		return nil
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Warn("clientset unavailable; controller_logs disabled", "err", err)
		return nil
	}
	return cs
}

// gitOpsFromKube builds the GitOps provider from the ambient kubeconfig (best-effort).
func gitOpsFromKube(cfg *config.Config, log *slog.Logger) providers.GitOpsProvider {
	restCfg, err := restConfig()
	if err != nil {
		log.Warn("no kube client; what-changed disabled", "err", err)
		return nil
	}
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		log.Warn("dynamic client unavailable; what-changed disabled", "err", err)
		return nil
	}
	return buildGitOps(cfg, dc, log)
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
	if !modelConfigured(cfg) {
		return fmt.Errorf("investigate requires a configured model (set config.model)")
	}
	// Progress logs go to stderr; the findings go to stdout.
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	ctx := context.Background()

	model, tools, recall, _ := buildModelAndTools(ctx, cfg, gitOpsFromKube(cfg, log), nil, log)
	var result *providers.Investigation
	li := &investigate.LoopInvestigator{
		Model: model, Tools: tools, Recall: recall, Actions: action.New(cfg.Actions), Log: log, Verify: true,
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

// outcomeKind labels an outcome-ledger open as a recall (cache hit) or a fresh finding.
func outcomeKind(recalled bool) string {
	if recalled {
		return "recall"
	}
	return "fresh"
}

// readyFunc gates readiness on leadership AND a warm catalog. When a catalog is
// configured, the leader must NOT advertise ready until its knowledge base is
// loaded and warm — otherwise it would serve incident traffic blind. This
// distinguishes the two ways buildCatalog returns a nil catalog:
//
//   - configured but the load failed (configured=true, cat=nil): stay 503. A
//     static catalog has no syncer to recover, so the pod stays not-ready and the
//     misconfiguration surfaces loudly instead of silently serving with no KB.
//   - not configured at all (configured=false, cat=nil): no catalog gate;
//     readiness is pure leadership.
//
// A configured-but-not-yet-warm catalog (git-sync NewEmpty, cat!=nil &&
// !Ready()) is also held at 503 until its first successful sync.
func readyFunc(leader func() bool, cat *catalog.Catalog, configured bool) func() bool {
	return func() bool {
		if configured && (cat == nil || !cat.Ready()) {
			return false
		}
		if cat != nil && !cat.Ready() {
			return false
		}
		return leader()
	}
}

// buildInvestigator returns the LLM ReAct investigator when a model is configured,
// otherwise the read-only LogInvestigator. It also returns the catalog (nil when
// no model is configured or no catalog is wired).
func buildInvestigator(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, approvals *action.Approvals, auto *action.Auto, metrics *telemetry.Metrics, ledger *outcome.Ledger, log *slog.Logger) (investigate.Investigator, *catalog.Catalog) {
	if !modelConfigured(cfg) {
		log.Info("no model configured; using log-only investigator")
		return investigate.LogInvestigator{Log: log}, nil
	}
	model, tools, recall, cat := buildModelAndTools(ctx, cfg, gp, metrics, log)
	if recall != nil {
		recall.Metrics = metrics
		recall.Log = log
		recall.Outcome = ledger // outcome-driven decay (serve path); *outcome.Ledger satisfies OutcomeStats
	}
	log.Info("using LLM investigator", "provider", modelProvider(cfg), "model", cfg.Model.Model, "tools", len(tools))
	notifier := buildNotifier(cfg, log)
	log.Info("delivery notifiers", "count", notifier.Len())
	cur := buildCurator(cfg, buildForgeTokenSource(cfg, log), cat, metrics, log)
	actions := action.New(cfg.Actions)
	if actions.Enabled() {
		log.Info("action policy enabled", "mode", string(actions.Mode()))
	}
	return &investigate.LoopInvestigator{
		Model:                     model,
		Tools:                     tools,
		Log:                       log,
		Actions:                   actions,
		Recall:                    recall,
		Verify:                    true, // adversarial review of root causes before delivery/curation
		Metrics:                   metrics,
		ModelProvider:             cfg.Model.Provider,
		MaxSteps:                  cfg.Investigation.MaxSteps,
		MaxToolOutputBytes:        cfg.Investigation.MaxToolOutputBytes,
		MaxTokensPerInvestigation: cfg.Investigation.MaxTokensPerInvestigation,
		Timeout:                   cfg.Investigation.Timeout.Std(),
		OnComplete: func(found providers.Investigation) {
			// Record the outcome "open" first: this investigation happened for an
			// incident, with the answer we used (recall vs fresh). A matching
			// resolved-alert webhook later stamps whether it actually resolved.
			// Skip sources without an alert fingerprint (GitOps watch, reinvestigate
			// poller) — they could never be matched by a resolved-alert webhook.
			fps := found.Fingerprints
			if len(fps) == 0 && found.Fingerprint != "" {
				fps = []string{found.Fingerprint}
			}
			if len(fps) > 0 {
				kind := outcomeKind(found.Recalled)
				now := time.Now()
				// The deterministic dedup fingerprint (resource+cause) is the curated PR's
				// stable resolution-join key: it is the same value draftKBEntry stamps into
				// the PR body, so a later resolve matches the PR regardless of the LLM's
				// re-worded title. Computed once for the whole batch.
				dupFP := curator.DupFingerprint(found)
				// One open per constituent fingerprint (coalesced batches fan out), so
				// every alert's resolve webhook can later match this investigation.
				for _, fp := range fps {
					if err := ledger.Open(outcome.Event{
						Fingerprint:    fp,
						DupFingerprint: dupFP,
						Kind:           kind,
						Entry:          found.RecalledEntry,
						Title:          found.Title,
						Resource:       found.Resource.Ref(),
						At:             now,
					}); err != nil {
						log.Warn("outcome ledger open failed", "fingerprint", fp, "err", err)
					}
					if metrics != nil {
						metrics.OutcomesOpened.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
					}
				}
			}
			// Post-investigation action handling, by mode. The loop has already
			// filtered found.Actions to the envelope (rung 1). auto and approvals are
			// mutually exclusive (one is nil unless that mode is configured).
			switch {
			case auto != nil:
				// Rung 3: evaluate + execute eligible actions (reversible/confidence/
				// rate/kill-switch gated); descriptions are annotated with the outcome.
				found.Actions = auto.Run(ctx, found)
			case approvals != nil:
				// Rung 2: register envelope-compliant actions for human approval; annotate
				// with how to approve (curl) and the ApprovalID (Slack buttons).
				for i := range found.Actions {
					id := approvals.Register(found.Actions[i])
					found.Actions[i].ApprovalID = id
					found.Actions[i].Description = fmt.Sprintf("%s — approve: POST /actions/%s/approve", found.Actions[i].Description, id)
				}
				if len(found.Actions) > 0 {
					log.Info("actions registered for approval", "count", len(found.Actions))
				}
			}
			log.Info("findings",
				"confidence", found.Confidence, "root_causes", len(found.RootCauses), "unresolved", len(found.Unresolved))
			// Curate first so the delivered message can link to the KB issue/PR.
			if cur != nil {
				if ref, err := cur.Curate(context.Background(), found); err != nil {
					log.Error("curate findings", "err", err)
				} else if ref.URL != "" {
					found.CuratedURL = ref.URL
					log.Info("curated", "url", ref.URL)
				}
			}
			if err := notifier.Deliver(context.Background(), found); err != nil {
				log.Error("deliver findings", "err", err)
			}
		},
	}, cat
}

// startGitOpsFailureWatch drains Flux WatchFailures into the queue. Failures are
// debounced (when a positive window is configured): the investigation is enqueued
// only after the failure has persisted — re-checked still Ready=False via the
// provider's ResourceStatus — filtering reconcile-churn transients that would
// otherwise drive confident-but-wrong root causes.
func startGitOpsFailureWatch(ctx context.Context, cfg *config.Config, q investigate.Enqueuer, gp providers.GitOpsProvider, metrics *telemetry.Metrics, log *slog.Logger) {
	events, err := gp.WatchFailures(ctx)
	if err != nil {
		log.Warn("gitops-failure watch disabled", "err", err)
		return
	}
	deb := buildFailureDebouncer(cfg, gp, metrics, log)
	log.Info("watching gitops failures", "engine", gitopsEngine(cfg),
		"debounce", cfg.Triggers.GitOpsFailures.DebounceWindow())
	go investigate.DrainFailures(ctx, events, q, trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std()), deb, log)
}

// buildFailureDebouncer wires a Debouncer whose "still failing?" re-check reads
// the workload's CURRENT Ready condition via GitOpsInspector.ResourceStatus. When
// the engine offers no inspector, the predicate is nil and the debouncer just
// waits out the window before enqueuing (still filtering instantly-clearing
// flaps). A zero window makes Debounce enqueue immediately.
func buildFailureDebouncer(cfg *config.Config, gp providers.GitOpsProvider, metrics *telemetry.Metrics, log *slog.Logger) *investigate.Debouncer {
	var check investigate.StillFailing
	if insp, ok := gp.(providers.GitOpsInspector); ok {
		check = func(ctx context.Context, w providers.Workload) (bool, error) {
			rs, err := insp.ResourceStatus(ctx, w)
			if err != nil {
				return false, err
			}
			// A recovered (Ready=True) or vanished resource is no longer worth
			// investigating; anything else (False/Unknown) is still failing.
			if rs.NotFound || rs.Ready == "True" {
				return false, nil
			}
			return true, nil
		}
	}
	return investigate.NewDebouncer(cfg.Triggers.GitOpsFailures.DebounceWindow(), check, log).WithMetrics(metrics)
}

// gitopsEngine returns the configured GitOps engine, defaulting to flux.
func gitopsEngine(cfg *config.Config) string {
	if cfg.GitOps.Engine == "argocd" {
		return "argocd"
	}
	return "flux"
}

// buildGitOps builds the GitOps provider for the configured engine (flux default).
func buildGitOps(cfg *config.Config, dc dynamic.Interface, log *slog.Logger) providers.GitOpsProvider {
	differ := &whatchanged.Differ{}
	if gitopsEngine(cfg) == "argocd" {
		log.Info("gitops engine", "engine", "argocd")
		return argocd.New(argocd.NewDynamicReader(dc), differ)
	}
	log.Info("gitops engine", "engine", "flux")
	return flux.New(flux.NewDynamicReader(dc), differ)
}

// restConfig builds a Kubernetes REST config from in-cluster config, falling back
// to the local kubeconfig (respecting $KUBECONFIG, then ~/.kube/config).
func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// setMemoryLimitFromCgroup sets GOMEMLIMIT to ~90% of the cgroup v2 memory limit
// so the Go GC respects the container's memory cap — keeping the heap under a soft
// ceiling and returning memory to the OS under pressure — instead of letting RSS
// grow across investigations until the cgroup OOM-kills the process. No-op when
// GOMEMLIMIT is set explicitly, or there is no cgroup memory limit.
func setMemoryLimitFromCgroup(log *slog.Logger) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return // an explicit operator override wins
	}
	b, err := os.ReadFile("/sys/fs/cgroup/memory.max") // cgroup v2 (EKS)
	if err != nil {
		return
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return // unlimited
	}
	cgroupMax, err := strconv.ParseInt(s, 10, 64)
	if err != nil || cgroupMax <= 0 {
		return
	}
	limit := cgroupMax / 10 * 9 // 90%: leave headroom for non-heap (stacks, bleve, runtime)
	debug.SetMemoryLimit(limit)
	log.Info("GOMEMLIMIT set from cgroup", "cgroup_max_bytes", cgroupMax, "gomemlimit_bytes", limit)
}
