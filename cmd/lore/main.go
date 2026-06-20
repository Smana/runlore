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
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/investigate"
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

	// Build the (best-effort) dynamic client once: used by both the GitOps-failure
	// watch and the what-changed tool.
	var fluxProvider *flux.Provider
	if client, err := dynamicClient(); err != nil {
		log.Warn("no kube client; GitOps features disabled", "err", err)
	} else {
		fluxProvider = flux.New(flux.NewDynamicReader(client), &whatchanged.Differ{})
	}

	inv := buildInvestigator(cfg, fluxProvider, log)
	queue := investigate.NewQueue(inv, log)
	go queue.Run(ctx)

	if cfg.Triggers.GitOpsFailures.Enabled && fluxProvider != nil {
		startGitOpsFailureWatch(ctx, cfg, queue, fluxProvider, log)
	}

	srv := server.New(cfg, queue, log)
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		_ = httpSrv.Shutdown(context.Background())
	}()
	log.Info("runlore serving", "addr", *addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
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
func buildInvestigator(cfg *config.Config, fp *flux.Provider, log *slog.Logger) investigate.Investigator {
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
	if cfg.Catalog.Dir != "" {
		if cat, err := catalog.New(cfg.Catalog.Dir); err != nil {
			log.Warn("catalog disabled", "dir", cfg.Catalog.Dir, "err", err)
		} else {
			log.Info("catalog loaded", "dir", cfg.Catalog.Dir, "entries", cat.Len())
			tools = append(tools, investigate.KBSearchTool{Catalog: cat})
		}
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

// dynamicClient builds a dynamic client from in-cluster config, falling back to
// the default kubeconfig.
func dynamicClient() (dynamic.Interface, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		restCfg, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return nil, err
		}
	}
	return dynamic.NewForConfig(restCfg)
}
