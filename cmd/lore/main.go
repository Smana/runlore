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
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

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
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/eval"
	fluxexec "github.com/Smana/runlore/internal/executor/flux"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logs/victorialogs"
	"github.com/Smana/runlore/internal/metrics/prometheus"
	anthropic "github.com/Smana/runlore/internal/model/anthropic"
	gemini "github.com/Smana/runlore/internal/model/gemini"
	openai "github.com/Smana/runlore/internal/model/openai"
	"github.com/Smana/runlore/internal/network/hubble"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/providers/cluster"
	"github.com/Smana/runlore/internal/providers/gitops/argocd"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
	"github.com/Smana/runlore/internal/server"
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
  lore eval [--config <path>] [--cases <dir>]         replay incident cases, score RCA identification
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
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

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

	inv := buildInvestigator(ctx, cfg, gitops, approvals, auto, log)
	queue := investigate.NewQueue(inv, log)
	reinv := buildReinvestigator(ctx, cfg, gitops, log)

	// startWork runs the leader-only loops (investigation queue + failure watch +
	// re-investigate poller), scoped to a context cancelled when leadership is lost.
	startWork := func(workCtx context.Context) {
		go queue.Run(workCtx)
		if cfg.Triggers.GitOpsFailures.Enabled && gitops != nil {
			startGitOpsFailureWatch(workCtx, cfg, queue, gitops, log)
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
	srv := server.New(cfg, queue, leader.Load, acts, log)
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
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
func buildCatalog(ctx context.Context, cfg *config.Config, forgeTok forgeToken, log *slog.Logger) *catalog.Catalog {
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
		return cat
	}
	return nil
}

// runEval replays recorded incident cases through the investigation loop and
// reports the RCA-identification rate. Requires a configured model.
func runEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	casesDir := fs.String("cases", "examples/eval", "directory of eval cases")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !modelConfigured(cfg) {
		return fmt.Errorf("eval requires a configured model (set config.model)")
	}
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
	rep := runner.Run(context.Background(), cases)
	for _, res := range rep.Results {
		status := "PASS"
		if !res.Pass {
			status = "FAIL"
		}
		fmt.Printf("%-4s  %-32s  confidence=%.2f", status, res.Name, res.Confidence)
		if len(res.Missing) > 0 {
			fmt.Printf("  missing: %s", strings.Join(res.Missing, ", "))
		}
		fmt.Println()
	}
	fmt.Printf("\nRCA identified: %d/%d (%.0f%%)\n", rep.Passed(), len(rep.Results), rep.RCARate()*100)
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
// limited, kill-switchable, and audited. Recommend dry_run before going live.
func buildAuto(cfg *config.Config, exec action.Executor, aud audit.Auditor, log *slog.Logger) *action.Auto {
	if cfg.Actions.Mode != config.ActionAuto {
		return nil
	}
	if exec == nil {
		log.Warn("auto-execution disabled: no cluster executor available")
		return nil
	}
	a := cfg.Actions.Auto
	log.Warn("rung-3 AUTO execution ENABLED — reversible actions execute WITHOUT human approval",
		"dry_run", a.DryRun, "min_confidence", a.MinConfidence, "max_per_window", a.MaxPerWindow, "window", a.Window.Std().String())
	au := action.NewAuto(exec, a, action.New(cfg.Actions), aud, log)
	// NewAuto starts paused (fail closed by construction across cold start / failover);
	// surface that to the operator, who resumes via the authenticated /actions/resume.
	log.Warn("rung-3 auto starts PAUSED (kill-switch engaged) — POST /actions/resume to begin auto-execution")
	return au
}

// buildCurator returns a Curator when the GitHub App token + KB repo are
// configured, else nil.
func buildCurator(cfg *config.Config, token forgeToken, log *slog.Logger) *curator.Curator {
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
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, github.TokenFunc(token))
	log.Info("curator enabled", "repo", cfg.Forge.KBRepo)
	return &curator.Curator{Issues: client, MinConfidencePR: 0.75, Log: log}
}

// buildReinvestigator returns a poller that re-runs KB issues labelled
// "reinvestigate" and posts the fresh findings back, or nil when the forge isn't
// configured. RunLore polls the forge (outbound) — it has no inbound webhooks.
func buildReinvestigator(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, log *slog.Logger) *investigate.Reinvestigator {
	token := buildForgeTokenSource(cfg, log)
	if token == nil || cfg.Forge.KBRepo == "" {
		return nil
	}
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		return nil
	}
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, cfg.Forge.BaseBranch, github.TokenFunc(token))
	model, tools, recall := buildModelAndTools(ctx, cfg, gp, log)
	run := func(ctx context.Context, req investigate.Request) (providers.Investigation, error) {
		var res providers.Investigation
		var got bool
		li := &investigate.LoopInvestigator{
			Model: model, Tools: tools, Recall: recall, Verify: true, Log: log,
			OnComplete: func(inv providers.Investigation) { res, got = inv, true },
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
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

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
	if err := syncer.Sync(context.Background()); err != nil {
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
func buildModelAndTools(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, log *slog.Logger) (providers.ModelProvider, []investigate.Tool, *investigate.Recall) {
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
	if cat := buildCatalog(ctx, cfg, forgeTok, log); cat != nil {
		tools = append(tools, investigate.KBSearchTool{Catalog: cat})
		if cfg.Catalog.InstantRecall.Enabled {
			recall = &investigate.Recall{Catalog: cat, MinScore: cfg.Catalog.InstantRecall.MinScore}
			log.Info("instant recall enabled", "min_score", cfg.Catalog.InstantRecall.MinScore)
		}
	}
	if cfg.Metrics.URL != "" {
		tools = append(tools, investigate.QueryMetricsTool{Metrics: prometheus.New(cfg.Metrics.URL)})
	}
	if cfg.Logs.URL != "" {
		tools = append(tools, investigate.QueryLogsTool{Logs: victorialogs.New(cfg.Logs.URL)})
	}
	if cfg.Network.URL != "" {
		tools = append(tools, investigate.NetworkDropsTool{Network: hubble.New(cfg.Network.URL)})
	}
	// Read-only controller-log access (Flux controllers), when a cluster is reachable.
	if cs := kubeClientset(log); cs != nil {
		tools = append(tools, investigate.ControllerLogsTool{Logs: cluster.New(cs)})
	}
	return model, tools, recall
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
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx := context.Background()

	model, tools, recall := buildModelAndTools(ctx, cfg, gitOpsFromKube(cfg, log), log)
	var result *providers.Investigation
	li := &investigate.LoopInvestigator{
		Model: model, Tools: tools, Recall: recall, Actions: action.New(cfg.Actions), Log: log, Verify: true,
		OnComplete: func(inv providers.Investigation) { result = &inv },
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

// buildInvestigator returns the LLM ReAct investigator when a model is configured,
// otherwise the read-only LogInvestigator.
func buildInvestigator(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, approvals *action.Approvals, auto *action.Auto, log *slog.Logger) investigate.Investigator {
	if !modelConfigured(cfg) {
		log.Info("no model configured; using log-only investigator")
		return investigate.LogInvestigator{Log: log}
	}
	model, tools, recall := buildModelAndTools(ctx, cfg, gp, log)
	log.Info("using LLM investigator", "provider", modelProvider(cfg), "model", cfg.Model.Model, "tools", len(tools))
	notifier := buildNotifier(cfg, log)
	log.Info("delivery notifiers", "count", notifier.Len())
	cur := buildCurator(cfg, buildForgeTokenSource(cfg, log), log)
	actions := action.New(cfg.Actions)
	if actions.Enabled() {
		log.Info("action policy enabled", "mode", string(actions.Mode()))
	}
	return &investigate.LoopInvestigator{
		Model:   model,
		Tools:   tools,
		Log:     log,
		Actions: actions,
		Recall:  recall,
		Verify:  true, // adversarial review of root causes before delivery/curation
		OnComplete: func(found providers.Investigation) {
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
	}
}

// startGitOpsFailureWatch drains Flux WatchFailures into the queue.
func startGitOpsFailureWatch(ctx context.Context, cfg *config.Config, q investigate.Enqueuer, gp providers.GitOpsProvider, log *slog.Logger) {
	events, err := gp.WatchFailures(ctx)
	if err != nil {
		log.Warn("gitops-failure watch disabled", "err", err)
		return
	}
	log.Info("watching gitops failures", "engine", gitopsEngine(cfg))
	go investigate.DrainFailures(ctx, events, q, trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std()), log)
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
