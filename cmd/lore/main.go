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
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
	"github.com/Smana/runlore/internal/server"
	"github.com/Smana/runlore/internal/trigger"
	"github.com/Smana/runlore/internal/whatchanged"
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

	queue := investigate.NewQueue(investigate.LogInvestigator{Log: log}, log)
	go queue.Run(ctx)

	// Best-effort GitOps-failure watch: only if enabled and a cluster is reachable.
	if cfg.Triggers.GitOpsFailures.Enabled {
		startGitOpsFailureWatch(ctx, cfg, queue, log)
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

// startGitOpsFailureWatch builds a dynamic client and drains Flux WatchFailures
// into the queue. Failures here are logged, not fatal — webhook-only serving
// continues if no cluster is reachable.
func startGitOpsFailureWatch(ctx context.Context, cfg *config.Config, q investigate.Enqueuer, log *slog.Logger) {
	client, err := dynamicClient()
	if err != nil {
		log.Warn("gitops-failure watch disabled: no kube client", "err", err)
		return
	}
	provider := flux.New(flux.NewDynamicReader(client), &whatchanged.Differ{})
	events, err := provider.WatchFailures(ctx)
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
