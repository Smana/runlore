package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logs/victorialogs"
	"github.com/Smana/runlore/internal/mcp"
	"github.com/Smana/runlore/internal/metrics/prometheus"
	"github.com/Smana/runlore/internal/network/awsvpc"
	"github.com/Smana/runlore/internal/network/gcpfirewall"
	"github.com/Smana/runlore/internal/network/hubble"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	awscloud "github.com/Smana/runlore/internal/providers/cloud/aws"
	"github.com/Smana/runlore/internal/providers/cluster"
	"github.com/Smana/runlore/internal/telemetry"
)

// BuildModelAndTools assembles the model, investigation tools, and the instant-recall
// short-circuit from config + the GitOps provider. Shared by serve and investigate.
func BuildModelAndTools(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, metrics *telemetry.Metrics, log *slog.Logger) (providers.ModelProvider, []investigate.Tool, *investigate.Recall, *catalog.Catalog) {
	apiKey := ""
	if cfg.Model.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Model.APIKeyEnv)
	}
	model := BuildModel(cfg, apiKey)
	forgeTok := BuildForgeTokenSource(cfg, log)
	var tools []investigate.Tool
	if gp != nil {
		tools = append(tools, investigate.WhatChangedTool{GitOps: gp})
		// Deep read-only Flux introspection (status/events + dependency tree), when
		// the GitOps provider supports it (Flux does).
		if insp, ok := gp.(providers.GitOpsInspector); ok {
			tools = append(tools, investigate.GitOpsStatusTool{Inspector: insp}, investigate.GitOpsTreeTool{Inspector: insp})
		}
	}
	var recall *investigate.Recall
	cat := BuildCatalog(ctx, cfg, forgeTok, metrics, log)
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
			// Hybrid (cosine-gated) recall — opt-in, and only effective once the catalog
			// actually built vectors (an embedder was configured + embedding succeeded).
			if cfg.Catalog.InstantRecall.Hybrid {
				recall.Hybrid = cat
				recall.HybridMinScore = cfg.Catalog.InstantRecall.HybridMinScore
				recall.HybridMarginGap = cfg.Catalog.InstantRecall.HybridMarginGap
				if recall.HybridMinScore == 0 {
					recall.HybridMinScore = 0.80
				}
				if recall.HybridMarginGap == 0 {
					recall.HybridMarginGap = 0.05
				}
				log.Info("hybrid recall enabled (EXPERIMENTAL — tune cosine thresholds via the instant-recall eval)",
					"hybrid_min_score", recall.HybridMinScore, "hybrid_margin_gap", recall.HybridMarginGap,
					"vectors_ready", cat.HasVectors())
			}
		}
	}
	if cfg.Metrics.URL != "" {
		m := prometheus.NewWithAuth(cfg.Metrics.URL, cfg.Metrics.TokenEnv, cfg.Metrics.Headers)
		tools = append(tools,
			investigate.QueryMetricsTool{Metrics: m},
			investigate.QueryMetricsRangeTool{Metrics: m},
		)
	}
	if cfg.Logs.URL != "" {
		tools = append(tools, investigate.QueryLogsTool{Logs: victorialogs.NewWithAuth(cfg.Logs.URL, cfg.Logs.TokenEnv, cfg.Logs.Headers)})
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
	if cs := KubeClientset(log); cs != nil {
		reader := cluster.New(cs)
		tools = append(tools,
			investigate.ControllerLogsTool{Logs: reader},
			// pod_logs streams raw pod logs (secrets/PII) to the LLM, so it is
			// constrained at the app layer to the incident namespace plus this
			// operator allowlist. The per-incident namespace is set by the loop.
			investigate.PodLogsTool{Logs: reader, AllowedNamespaces: cfg.Investigation.PodLogNamespaces},
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
	tools = appendMCPTools(ctx, cfg, log, tools)
	return model, tools, recall, cat
}

// appendMCPTools discovers tools from each configured MCP server and appends them
// (namespaced, read-only) to the loop's tool set. A server that fails initialize/list is
// logged and skipped — RunLore continues with the built-in tools. A namespaced name that
// collides with an already-registered tool is skipped (built-ins win).
func appendMCPTools(ctx context.Context, cfg *config.Config, log *slog.Logger, tools []investigate.Tool) []investigate.Tool {
	if len(cfg.MCP.Servers) == 0 {
		return tools
	}
	have := map[string]bool{}
	for _, t := range tools {
		have[t.Name()] = true
	}
	for _, s := range cfg.MCP.Servers {
		apiKey := ""
		if s.TokenEnv != "" {
			apiKey = os.Getenv(s.TokenEnv)
		}
		c := mcp.NewClient(s.Name, s.URL, apiKey, s.Headers, log)
		if err := c.Initialize(ctx); err != nil {
			log.Warn("mcp: skipping server (initialize failed)", "server", s.Name, "err", err)
			continue
		}
		remote, err := c.ListTools(ctx)
		if err != nil {
			log.Warn("mcp: skipping server (tools/list failed)", "server", s.Name, "err", err)
			continue
		}
		added := 0
		for _, rt := range remote {
			tl := mcp.NewTool(c, rt)
			if have[tl.Name()] {
				log.Warn("mcp: skipping tool (name collision)", "server", s.Name, "tool", tl.Name())
				continue
			}
			have[tl.Name()] = true
			tools = append(tools, tl)
			added++
		}
		log.Info("mcp: registered server tools", "server", s.Name, "tools", added)
	}
	return tools
}

// defaultToolTimeout bounds a single tool call when investigation.tool_timeout is
// unset (0). It keeps a hung/slow provider (a stuck git clone, an unresponsive
// metrics/logs endpoint) from consuming the whole per-investigation budget while
// still allowing legitimately slow queries (log scans, range PromQL) to finish.
const defaultToolTimeout = 60 * time.Second

// BuildInvestigator returns the LLM ReAct investigator when a model is configured,
// otherwise the read-only LogInvestigator. It also returns the catalog (nil when
// no model is configured or no catalog is wired).
func BuildInvestigator(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, approvals *action.Approvals, auto *action.Auto, metrics *telemetry.Metrics, ledger *outcome.Ledger, log *slog.Logger) (investigate.Investigator, *catalog.Catalog, error) {
	if !ModelConfigured(cfg) {
		log.Info("no model configured; using log-only investigator")
		return investigate.LogInvestigator{Log: log}, nil, nil
	}
	model, tools, recall, cat := BuildModelAndTools(ctx, cfg, gp, metrics, log)
	if recall != nil {
		recall.Metrics = metrics
		recall.Log = log
		recall.Outcome = ledger // outcome-driven decay (serve path); *outcome.Ledger satisfies OutcomeStats
	}
	log.Info("using LLM investigator", "provider", ModelProvider(cfg), "model", cfg.Model.Model, "tools", len(tools))
	notifier, err := BuildNotifier(cfg, log)
	if err != nil {
		return nil, nil, err
	}
	log.Info("delivery notifiers", "count", notifier.Len())
	cur := BuildCurator(cfg, BuildForgeTokenSource(cfg, log), cat, metrics, log)
	actions := action.New(cfg.Actions)
	if actions.Enabled() {
		log.Info("action policy enabled", "mode", string(actions.Mode()))
	}
	// Per-tool timeout: default to 60s when unset (0) so one hung tool can't eat the
	// whole per-investigation budget; an explicit config value flows through as-is.
	toolTimeout := cfg.Investigation.ToolTimeout.Std()
	if toolTimeout == 0 {
		toolTimeout = defaultToolTimeout
	}
	return &investigate.LoopInvestigator{
		Model:                     model,
		VerifyModel:               BuildVerifyModel(cfg),
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
		Compaction:                cfg.Investigation.Compaction,
		Timeout:                   cfg.Investigation.Timeout.Std(),
		ToolTimeout:               toolTimeout,
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
				kind := OutcomeKind(found.Recalled)
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
			} else {
				log.Info("delivered findings",
					"confidence", found.Confidence, "root_causes", len(found.RootCauses), "curated_url", found.CuratedURL)
			}
		},
	}, cat, nil
}

// BuildFailureDebouncer wires a Debouncer whose "still failing?" re-check reads
// the workload's CURRENT Ready condition via GitOpsInspector.ResourceStatus. When
// the engine offers no inspector, the predicate is nil and the debouncer just
// waits out the window before enqueuing (still filtering instantly-clearing
// flaps). A zero window makes Debounce enqueue immediately.
func BuildFailureDebouncer(cfg *config.Config, gp providers.GitOpsProvider, metrics *telemetry.Metrics, log *slog.Logger) *investigate.Debouncer {
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
