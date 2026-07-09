// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // G108: pprof is opt-in (RUNLORE_PPROF) and bound to 127.0.0.1 only, never the Service
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/coalesce"
	"github.com/Smana/runlore/internal/config"
	fluxexec "github.com/Smana/runlore/internal/executor/flux"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logging"
	_ "github.com/Smana/runlore/internal/notify/webhook" // self-registers the generic outgoing-webhook notifier
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
	"github.com/Smana/runlore/internal/server"
	"github.com/Smana/runlore/internal/source"
	_ "github.com/Smana/runlore/internal/source/alertmanager" // self-registers the alertmanager webhook source
	_ "github.com/Smana/runlore/internal/source/gitops"       // self-registers the gitops-failure watcher source
	"github.com/Smana/runlore/internal/source/pagerduty"
	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/trigger"
)

// RunServe runs the in-cluster agent: it loads config, builds the investigation
// pipeline (kube/GitOps/model/catalog/actions), runs leader election, and serves
// the webhook + readiness endpoints until SIGTERM, then drains gracefully. version
// is injected at build time and stays in package main; it is passed through for the
// build-info gauge.
func RunServe(version string, args []string) error {
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
	SetMemoryLimitFromCgroup(log) // make the GC respect the container memory cap

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
	if restCfg, err := RestConfig(); err != nil {
		log.Warn("no kube client; GitOps features + leader election disabled", "err", err)
	} else {
		if dc, derr := dynamic.NewForConfig(restCfg); derr != nil {
			log.Warn("dynamic client unavailable; GitOps features disabled", "err", derr)
		} else {
			gitops = BuildGitOps(cfg, dc, log)
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
	aud, auditClose, aerr := BuildAuditor(cfg, log)
	if aerr != nil {
		return aerr
	}
	defer auditClose()
	// Audit every cluster mutation at the single Execute seam (both rungs go through it).
	execForActions := executor
	if executor != nil {
		execForActions = action.NewAuditedExecutor(executor, aud)
	}
	approvals := BuildApprovals(cfg, execForActions, aud, log)
	auto := BuildAuto(cfg, execForActions, aud, log)
	slackSigningSecret := os.Getenv(cfg.Notify.Slack.SigningSecretEnv)
	webhookToken := os.Getenv(cfg.Server.WebhookTokenEnv)
	if err := RequireWebhookAuth(cfg, webhookToken); err != nil {
		return err
	}
	// The PagerDuty source authenticates its own webhook via X-PagerDuty-Signature
	// (not the shared bearer token), so guard it separately on the same fail-closed
	// policy: an enabled source with a configured model must carry a signing secret.
	pdSecret, pdEnabled := pagerduty.Secret(cfg.Sources)
	if err := RequirePagerDutyAuth(cfg, pdEnabled, pdSecret); err != nil {
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

	// max_events is a three-state knob: unset (nil) ⇒ the generous default; explicit 0 ⇒
	// compaction disabled; N ⇒ compact when the ledger exceeds N events.
	maxEvents := outcome.DefaultMaxEvents
	if cfg.Outcome.MaxEvents != nil {
		maxEvents = *cfg.Outcome.MaxEvents
	}
	ledger, err := outcome.NewWithMaxEvents(cfg.Outcome.LedgerPath, maxEvents)
	if err != nil {
		return fmt.Errorf("outcome ledger: %w", err)
	}
	if cfg.Outcome.LedgerPath != "" {
		log.Info("outcome ledger enabled", "path", cfg.Outcome.LedgerPath, "max_events", maxEvents)
	}

	inv, cat, err := BuildInvestigator(ctx, cfg, gitops, approvals, auto, metrics, ledger, log)
	if err != nil {
		return fmt.Errorf("build investigator: %w", err)
	}
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
		out := func(batch []investigate.Request) {
			rep := batch[0]
			if len(batch) > 1 {
				rep.Message = coalesce.Summarize(batch)
			}
			// Record every constituent fingerprint so each alert's resolve webhook
			// matches an open (a single incident stays one fingerprint).
			var fps []string
			for _, r := range batch {
				if r.Fingerprint != "" {
					fps = append(fps, r.Fingerprint)
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

	reinv := BuildReinvestigator(ctx, cfg, gitops, metrics, log)

	// failureDedup is created ONCE (process scope), not per leadership term, so it
	// survives leader flaps: the gitops informer's initial-LIST replay of every
	// still-failing workload on each re-acquire is then suppressed instead of
	// re-investigated (GO-P1A). Deduper is safe for concurrent use across terms.
	failureDedup := trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std())

	// Source registry: build the enabled sources (webhook + watcher) and the shared
	// ingest pipeline. Webhook sources are mounted by the server and ingest through
	// the pipeline (policy → dedup → enqueue); watcher sources are run by RunWatchers.
	// Alerts enqueue via the coalescer when enabled, else straight to the queue.
	var alertEnq investigate.Enqueuer = queue
	if cz != nil {
		alertEnq = cz
	}
	// resolve records a resolved alert into the outcome ledger (+ metrics), mirroring
	// the legacy server.go resolved-alert handling.
	resolve := func(fp string, at time.Time) {
		if ep, ok, rerr := ledger.Resolve(fp, at); rerr != nil {
			log.Warn("outcome ledger resolve failed", "fingerprint", fp, "err", rerr)
		} else if ok && metrics != nil {
			metrics.IncidentsResolved.Add(ctx, 1)
			metrics.IncidentResolutionSeconds.Record(ctx, ep.Duration.Seconds())
			if ep.Kind == "recall" {
				metrics.RecallOutcome.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "resolved")))
			}
		}
	}
	built, err := source.BuildEnabled(source.Deps{Cfg: cfg, GitOps: gitops, Log: log, Raw: cfg.Sources})
	if err != nil {
		return fmt.Errorf("build sources: %w", err)
	}
	pipe := source.NewPipeline(cfg, alertEnq, resolve, log).WithMetrics(metrics).WithContext(workCtx)
	if w := cfg.Triggers.Incidents.Debounce.Std(); w > 0 {
		log.Info("incident debounce enabled",
			"window", w, "note", "firing alerts held; dropped if resolved within the window")
	}

	// startWork runs the leader-only loops (investigation queue + failure watch +
	// re-investigate poller), scoped to a context cancelled when leadership is lost.
	startWork := func(workCtx context.Context) {
		// Re-sync the outcome ledger's cached aggregate from the shared file on every
		// (re-)acquisition of leadership. In multi-replica HA the file may have grown
		// while another replica led and this process was a follower; the cache is built
		// incrementally and would otherwise be stale, corrupting recall-decay. A failed
		// reload must NOT crash the leader — log and keep serving the prior cache.
		if rerr := ledger.Reload(); rerr != nil {
			log.Warn("outcome ledger reload on leadership failed; serving prior cache", "err", rerr)
		}
		go queue.Run(workCtx)
		// The gitops watcher source is built only when sources.gitops.enabled, so
		// RunWatchers no-ops when no watcher source is present.
		if gitops != nil {
			deb := BuildFailureDebouncer(cfg, gitops, metrics, log)
			for _, b := range built {
				if b.Desc.Kind == source.Watcher {
					log.Info("watching gitops failures",
						"engine", GitopsEngine(cfg), "debounce", cfg.Triggers.GitOpsFailures.DebounceWindow())
					break
				}
			}
			source.RunWatchers(workCtx, built, queue, failureDedup, deb, log)
		}
		if reinv != nil {
			log.Info("re-investigate poller enabled", "label", investigate.ReinvestigateLabel)
			go reinv.Poll(workCtx, 2*time.Minute)
		}
		// Opt-in Matrix reaction listener — leader-only like the pollers above, so an
		// HA deployment records each 👍/👎 exactly once, into the leader's ledger.
		if mfb := BuildMatrixFeedback(cfg, ledger, log); mfb != nil {
			log.Info("matrix feedback reactions enabled", "room", cfg.Notify.Matrix.RoomID)
			go mfb.Run(workCtx)
		}
	}

	var leader atomic.Bool
	useLE := cfg.LeaderElection.Enabled && clientset != nil
	if useLE {
		go RunLeaderElection(workCtx, cfg, clientset, &leader, log, startWork)
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
	// Opt-in 👍/👎 feedback: wire the recorder ONLY when the option is on AND the
	// ledger persists (Validate already requires ledger_path + signing secret with
	// the option, and Enabled() guards the typed-nil/disabled-ledger cases) — so
	// with the option off the endpoint behaves exactly as before.
	if cfg.Notify.Slack.FeedbackButtons && ledger.Enabled() {
		acts.Feedback = ledger
		log.Info("slack feedback buttons enabled", "endpoint", "/slack/interactions")
	}
	srv := server.New(ReadyFunc(leader.Load, cat, CatalogExpected(cfg)), acts, built, pipe, metricsHandler, log)
	if cz != nil {
		go cz.Run(workCtx, cfg.Investigation.Coalesce.Debounce.Std()/2)
	}
	httpSrv := NewHTTPServer(*addr, srv.Handler())
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
