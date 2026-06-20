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

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logs/victorialogs"
	"github.com/Smana/runlore/internal/metrics/prometheus"
	openai "github.com/Smana/runlore/internal/model/openai"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
	"github.com/Smana/runlore/internal/server"
	"github.com/Smana/runlore/internal/trigger"
	"github.com/Smana/runlore/internal/whatchanged"

	github "github.com/Smana/runlore/internal/forge/github"
)

var version = "0.0.0-dev"

const usage = `lore — the RunLore SRE agent

Usage:
  lore investigate [--alert <name>] [--since <dur>]   investigate an alert/symptom (on-demand)
  lore serve [--config <path>] [--addr <addr>]        run the in-cluster agent (react to incidents)
  lore catalog sync                                   sync + index the knowledge catalog
  lore eval                                           replay past incidents, score root-cause identification
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
	case "investigate", "catalog", "eval":
		fmt.Printf("lore %s: not yet implemented (scaffold). See docs/design.md\n", os.Args[1])
		os.Exit(1)
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
		fluxProvider *flux.Provider
		clientset    *kubernetes.Clientset
	)
	if restCfg, err := restConfig(); err != nil {
		log.Warn("no kube client; GitOps features + leader election disabled", "err", err)
	} else {
		if dc, derr := dynamic.NewForConfig(restCfg); derr != nil {
			log.Warn("dynamic client unavailable; GitOps features disabled", "err", derr)
		} else {
			fluxProvider = flux.New(flux.NewDynamicReader(dc), &whatchanged.Differ{})
		}
		if cs, cerr := kubernetes.NewForConfig(restCfg); cerr != nil {
			log.Warn("clientset unavailable; leader election disabled", "err", cerr)
		} else {
			clientset = cs
		}
	}

	inv := buildInvestigator(ctx, cfg, fluxProvider, log)
	queue := investigate.NewQueue(inv, log)

	// startWork runs the leader-only loops (investigation queue + failure watch),
	// scoped to a context cancelled when leadership is lost.
	startWork := func(workCtx context.Context) {
		go queue.Run(workCtx)
		if cfg.Triggers.GitOpsFailures.Enabled && fluxProvider != nil {
			startGitOpsFailureWatch(workCtx, cfg, queue, fluxProvider, log)
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
	srv := server.New(cfg, queue, leader.Load, log)
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

// buildCatalog returns the kb_search backing store, or nil when no catalog is
// configured. With a Git URL it starts a background syncer (running on every
// replica, so a failover standby is already warm) that re-indexes after each pull;
// otherwise it loads a static mounted directory once.
func buildCatalog(ctx context.Context, cfg *config.Config, log *slog.Logger) catalog.Searcher {
	if cfg.Catalog.Git.URL != "" {
		dir := cfg.Catalog.Dir
		if dir == "" {
			dir = "/var/lib/runlore/catalog"
		}
		cat := catalog.NewEmpty()
		var token catalog.TokenFunc
		if env := cfg.Catalog.Git.TokenEnv; env != "" {
			if t := os.Getenv(env); t != "" {
				token = func(context.Context) (string, error) { return t, nil }
			}
		}
		syncer := &catalog.Syncer{URL: cfg.Catalog.Git.URL, Branch: cfg.Catalog.Git.Branch, Dir: dir, Token: token, Log: log}
		go syncer.Run(ctx, cfg.Catalog.Git.Interval.Std(), func() {
			if err := cat.Reload(dir); err != nil {
				log.Warn("catalog reload failed", "dir", dir, "err", err)
				return
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

// buildNotifier assembles the configured chat notifiers (best-effort fan-out).
func buildNotifier(cfg *config.Config, log *slog.Logger) *notify.Multi {
	var ns []providers.Notifier
	if env := cfg.Notify.Slack.WebhookURLEnv; env != "" {
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

// buildCurator returns a Curator when a GitHub App + KB repo are configured, else nil.
func buildCurator(cfg *config.Config, log *slog.Logger) *curator.Curator {
	ga := cfg.Forge.GitHubApp
	if ga.AppID == 0 || ga.InstallationID == 0 || ga.PrivateKeyEnv == "" || cfg.Forge.KBRepo == "" {
		return nil
	}
	pemData := os.Getenv(ga.PrivateKeyEnv)
	if pemData == "" {
		log.Warn("curator disabled: empty private key env", "env", ga.PrivateKeyEnv)
		return nil
	}
	key, err := github.ParsePrivateKey(pemData)
	if err != nil {
		log.Warn("curator disabled: bad private key", "err", err)
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
	ts := github.NewAppTokenSource(cfg.Forge.GitHubAPIURL, ga.AppID, ga.InstallationID, key)
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, ts.Token)
	log.Info("curator enabled", "repo", cfg.Forge.KBRepo)
	return &curator.Curator{Issues: client, MinConfidencePR: 0.75, Log: log}
}

// buildInvestigator returns the LLM ReAct investigator when a model is configured,
// otherwise the read-only LogInvestigator.
func buildInvestigator(ctx context.Context, cfg *config.Config, fp *flux.Provider, log *slog.Logger) investigate.Investigator {
	if cfg.Model.BaseURL == "" {
		log.Info("no model configured; using log-only investigator")
		return investigate.LogInvestigator{Log: log}
	}
	apiKey := ""
	if cfg.Model.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Model.APIKeyEnv)
	}
	model := openai.New(cfg.Model.BaseURL, cfg.Model.Model, apiKey)
	var tools []investigate.Tool
	if fp != nil {
		tools = append(tools, investigate.WhatChangedTool{GitOps: fp})
	}
	if cat := buildCatalog(ctx, cfg, log); cat != nil {
		tools = append(tools, investigate.KBSearchTool{Catalog: cat})
	}
	if cfg.Metrics.URL != "" {
		tools = append(tools, investigate.QueryMetricsTool{Metrics: prometheus.New(cfg.Metrics.URL)})
	}
	if cfg.Logs.URL != "" {
		tools = append(tools, investigate.QueryLogsTool{Logs: victorialogs.New(cfg.Logs.URL)})
	}
	log.Info("using LLM investigator", "model", cfg.Model.Model, "tools", len(tools))
	notifier := buildNotifier(cfg, log)
	log.Info("delivery notifiers", "count", notifier.Len())
	cur := buildCurator(cfg, log)
	return &investigate.LoopInvestigator{
		Model: model,
		Tools: tools,
		Log:   log,
		OnComplete: func(found providers.Investigation) {
			log.Info("findings",
				"confidence", found.Confidence, "root_causes", len(found.RootCauses), "unresolved", len(found.Unresolved))
			if err := notifier.Deliver(context.Background(), found); err != nil {
				log.Error("deliver findings", "err", err)
			}
			if cur != nil {
				if ref, err := cur.Curate(context.Background(), found); err != nil {
					log.Error("curate findings", "err", err)
				} else if ref.URL != "" {
					log.Info("curated", "url", ref.URL)
				}
			}
		},
	}
}

// startGitOpsFailureWatch drains Flux WatchFailures into the queue.
func startGitOpsFailureWatch(ctx context.Context, cfg *config.Config, q investigate.Enqueuer, fp *flux.Provider, log *slog.Logger) {
	events, err := fp.WatchFailures(ctx)
	if err != nil {
		log.Warn("gitops-failure watch disabled", "err", err)
		return
	}
	log.Info("watching gitops failures (Flux Kustomizations)")
	go investigate.DrainFailures(ctx, events, q, trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std()))
}

// restConfig builds a Kubernetes REST config from in-cluster config, falling back
// to the default kubeconfig.
func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}
