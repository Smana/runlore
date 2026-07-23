// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logs/victorialogs"
	"github.com/Smana/runlore/internal/mcp"
	"github.com/Smana/runlore/internal/metrics/prometheus"
	"github.com/Smana/runlore/internal/network/awsvpc"
	"github.com/Smana/runlore/internal/network/gcpfirewall"
	"github.com/Smana/runlore/internal/network/hubble"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	awscloud "github.com/Smana/runlore/internal/providers/cloud/aws"
	"github.com/Smana/runlore/internal/providers/cluster"
	"github.com/Smana/runlore/internal/sourcerepo"
	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/whatchanged"
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
				StaleAfter:           cfg.Catalog.InstantRecall.StaleAfter.Std(),
			}
			log.Info("instant recall enabled",
				"min_score", cfg.Catalog.InstantRecall.MinScore,
				"margin_gap", cfg.Catalog.InstantRecall.MarginGap, "solo_floor", cfg.Catalog.InstantRecall.SoloFloor,
				"outcome_prior", cfg.Catalog.InstantRecall.OutcomePrior, "outcome_floor", cfg.Catalog.InstantRecall.OutcomeFloor,
				"stale_after", cfg.Catalog.InstantRecall.StaleAfter.Std())
			// LLM reranker (opt-in): replace the corpus-dependent BM25-magnitude fire gate
			// with a CALIBRATED match-confidence gate. Route the one cheap call to the
			// verify tier (cheaper/faster) when configured, else the main model — mirroring
			// how verifyFindings picks its model. Metrics/Log are set here so the reranker
			// is fully wired from the shared builder (serve + investigate).
			if cfg.Catalog.InstantRecall.RerankEnabled() {
				rerankModel := model
				verifyTier := BuildVerifyModel(cfg)
				if verifyTier != nil {
					rerankModel = verifyTier
				}
				recall.Rerank = &investigate.Reranker{
					Model:     rerankModel,
					Threshold: cfg.Catalog.InstantRecall.RerankThreshold,
					K:         cfg.Catalog.InstantRecall.RerankK,
					MinScore:  cfg.Catalog.InstantRecall.RerankMinScore,
					Metrics:   metrics,
					Log:       log,
				}
				log.Info("instant-recall reranker enabled (calibrated-confidence gate replaces the BM25 solo_floor)",
					"threshold", cfg.Catalog.InstantRecall.RerankThreshold,
					"k", cfg.Catalog.InstantRecall.RerankK,
					"min_score", cfg.Catalog.InstantRecall.RerankMinScore,
					"verify_tier_model", verifyTier != nil)
			}
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
		warnIfBackendUnreachable(ctx, log, "metrics", cfg.Metrics.URL)
		m := prometheus.NewWithAuth(cfg.Metrics.URL, cfg.Metrics.TokenEnv, cfg.Metrics.Headers)
		// Backend flavor (P4): a config override wins; otherwise probe buildinfo once at
		// startup. VictoriaMetrics unlocks description-only MetricsQL guidance on the
		// query tools. Detection fails safe to FlavorUnknown (generic Prometheus, no
		// MetricsQL claims) if the probe can't identify the backend.
		flavor := prometheus.Flavor(cfg.Metrics.Flavor)
		if flavor != prometheus.FlavorUnknown {
			m.WithFlavor(flavor)
		} else {
			flavor = m.DetectFlavor(ctx)
		}
		vm := flavor == prometheus.FlavorVictoriaMetrics
		log.Info("metrics backend flavor", "flavor", string(m.Flavor()), "metricsql", vm)
		tools = append(tools,
			investigate.QueryMetricsTool{Metrics: m, MetricsQL: vm},
			investigate.QueryMetricsRangeTool{Metrics: m, MetricsQL: vm},
			// discover_metrics turns a "no series matched" into a recoverable step by
			// listing the metric names / label values that actually exist for a selector.
			investigate.DiscoverMetricsTool{Metrics: m},
		)
	}
	if cfg.Logs.URL != "" {
		warnIfBackendUnreachable(ctx, log, "logs", cfg.Logs.URL)
		// Resolve the OPTIONAL collector field convention once; an unset config yields
		// the shipped defaults, so this is a no-op unless logs.fields is set.
		lf := cfg.Logs.Fields.Resolved()
		lg := victorialogs.NewWithAuth(cfg.Logs.URL, cfg.Logs.TokenEnv, cfg.Logs.Headers).WithLevelField(lf.LevelField)
		tools = append(tools,
			// query_logs reads the same raw pod logs pod_logs does, so it shares the
			// pod_log_namespaces allowlist (L2 confinement) and honours the field
			// convention (L1). The incident namespace is injected per-investigation by
			// the loop (scopeTools), exactly like pod_logs.
			investigate.QueryLogsTool{
				Logs: lg,
				Fields: investigate.LogFields{
					ContainerField: lf.ContainerField,
					NamespaceField: lf.NamespaceField,
					PodField:       lf.PodField,
					LevelField:     lf.LevelField,
					UnpackPipe:     lf.UnpackPipe,
				},
				AllowedNamespaces: cfg.Investigation.PodLogNamespaces,
			},
			// logs_error_summary (error volume histogram + top messages) and
			// discover_log_fields (real field names) both degrade gracefully when the
			// backend lacks the analytics/field capability, so they are safe to always
			// register whenever a logs backend is configured.
			investigate.LogsErrorSummaryTool{Logs: lg},
			investigate.DiscoverLogFieldsTool{Logs: lg},
		)
	}
	// Network-flow data source (the network_drops tool). Pluggable and CNI-agnostic:
	// no provider is enabled by default. The selected provider must match the cluster's
	// environment (Cilium Hubble, AWS VPC Flow Logs, or GCP Firewall Logs).
	switch cfg.Network.Provider {
	case config.NetworkHubble:
		if cfg.Network.Hubble.URL != "" {
			tools = append(tools, investigate.NetworkDropsTool{Network: hubble.New(cfg.Network.Hubble.URL, cfg.Network.Hubble.TLS)})
			log.Info("network provider enabled", "provider", config.NetworkHubble, "url", cfg.Network.Hubble.URL, "tls", cfg.Network.Hubble.TLS)
			if cfg.Network.URL != "" {
				log.Warn("config.network.url is deprecated; set config.network.provider=hubble and config.network.hubble.url")
			}
		}
	case config.NetworkAWSVPCFlowLogs:
		// Resolve the optional custom field-index map: nil → v2 default layout.
		var flowFieldIndex map[string]int
		if cfg.Network.AWS.FlowFormat == "custom" && len(cfg.Network.AWS.FlowFields) > 0 {
			flowFieldIndex = cfg.Network.AWS.FlowFields
		}
		if nw, err := awsvpc.New(ctx, cfg.Network.AWS.Region, cfg.Network.AWS.LogGroup, flowFieldIndex); err != nil {
			log.Warn("aws-vpc-flow-logs network provider unavailable; network_drops disabled", "err", err)
		} else {
			tools = append(tools, investigate.NetworkDropsTool{Network: nw})
			log.Info("network provider enabled", "provider", config.NetworkAWSVPCFlowLogs, "log_group", cfg.Network.AWS.LogGroup, "flow_format", cfg.Network.AWS.FlowFormat)
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
	// cluster is reachable. The same reader backs all three tools — and, when present,
	// the fused incident_timeline below (as its KubeReader/EventWindower source).
	var kubeReader providers.KubeReader
	if cs := KubeClientset(log); cs != nil {
		cr := cluster.New(cs)
		kubeReader = cr
		tools = append(tools, clusterTools(cr, cfg)...)
	}
	// Cloud context (AWS): CloudTrail "what changed" + EC2/ASG/EKS health. Opt-in.
	var cloudProvider providers.CloudProvider
	if cfg.Cloud.Provider == "aws" {
		if cl, err := awscloud.New(ctx, cfg.Cloud.Region, cfg.Cloud.ClusterName); err != nil {
			log.Warn("aws cloud provider unavailable; cloud tools disabled", "err", err)
		} else {
			cloudProvider = cl
			tools = append(tools, investigate.CloudWhatChangedTool{Cloud: cl}, investigate.CloudResourceHealthTool{Cloud: cl})
			log.Info("cloud provider enabled", "provider", "aws", "region", cfg.Cloud.Region)
		}
	}
	// source_diff (source-repo whitelist): read the code change behind an image
	// or module version bump. Registered only when the operator listed repos.
	tools = appendSourceDiffTool(cfg, tools, log)
	// incident_timeline (P1) fuses the timestamped facts from the providers above into
	// ONE chronologically-sorted view. Register it only when at least one contributing
	// source is wired (GitOps changes, cloud control-plane changes, or kube events) —
	// with none, the tool would have nothing to correlate. It fans out to whichever of
	// its (optional) sources are present and skips the rest gracefully.
	if gp != nil || cloudProvider != nil || kubeReader != nil {
		tools = append(tools, investigate.IncidentTimelineTool{GitOps: gp, Kube: kubeReader, Cloud: cloudProvider})
		log.Info("incident_timeline enabled", "gitops", gp != nil, "cloud", cloudProvider != nil, "kube", kubeReader != nil)
	}
	// workload_ownership (G4) walks a failing pod's ownerReferences to its top
	// controller and names the owning GitOps object from its tracking labels, then
	// surfaces live-vs-GitOps drift (the "someone kubectl-edited it" cause what_changed
	// can't see). It needs BOTH an OwnerWalker (the cluster reader supports it) AND a
	// GitOpsInspector for the authoritative engine drift verdict — the inspector is
	// optional (nil ⇒ the tool degrades to the last-applied fallback), but the walker is
	// required, so registration is gated on the walker being present.
	if ow, ok := kubeReader.(providers.OwnerWalker); ok {
		var insp providers.GitOpsInspector
		if gp != nil {
			insp, _ = gp.(providers.GitOpsInspector)
		}
		tools = append(tools, investigate.WorkloadOwnershipTool{Kube: ow, Inspector: insp})
		log.Info("workload_ownership enabled", "inspector", insp != nil)
	}
	tools = appendMCPTools(ctx, cfg, log, tools)
	return model, tools, recall, cat
}

// clusterReader is the read-only cluster capability backing the cluster tools: it is
// both a LogReader (controller_logs, pod_logs) and a KubeReader (pod_status,
// kube_events). *cluster.Reader satisfies it; narrowing to an interface lets the
// engine-gating be unit-tested with a fake and no live cluster.
type clusterReader interface {
	providers.LogReader
	providers.KubeReader
}

// clusterTools assembles the read-only cluster tools (controller_logs + pod_logs +
// pod_status + kube_events) from a cluster reader. controller_logs enumerates the Flux
// controllers in flux-system, so it is a dead/misleading tool on an ArgoCD deployment:
// it is registered ONLY when the configured GitOps engine is Flux (the default). Making
// registration a function of the known engine capability keeps the gate in one testable
// place. See GitopsEngine.
func clusterTools(reader clusterReader, cfg *config.Config) []investigate.Tool {
	var tools []investigate.Tool
	if GitopsEngine(cfg) == "flux" {
		tools = append(tools, investigate.ControllerLogsTool{Logs: reader})
	}
	tools = append(tools,
		// pod_logs streams raw pod logs (secrets/PII) to the LLM, so it is
		// constrained at the app layer to the incident namespace plus this
		// operator allowlist. The per-incident namespace is set by the loop.
		investigate.PodLogsTool{Logs: reader, AllowedNamespaces: cfg.Investigation.PodLogNamespaces},
		investigate.PodStatusTool{Kube: reader},
		investigate.KubeEventsTool{Kube: reader},
	)
	return tools
}

// appendSourceDiffTool registers source_diff when the operator listed
// source_repos.allow patterns. The allowlist is the security boundary (the
// model can only reach listed repos); auth reuses the forge GitHub App token
// exactly like what_changed; mirrors live under a "source" subdir of the
// gitops mirror root so source-repo and GitOps mirrors never contend.
func appendSourceDiffTool(cfg *config.Config, tools []investigate.Tool, log *slog.Logger) []investigate.Tool {
	if len(cfg.SourceRepos.Allow) == 0 {
		return tools
	}
	allow, err := sourcerepo.New(cfg.SourceRepos.Allow)
	if err != nil {
		// Config.Validate() already rejects bad patterns at load; this guard
		// only protects callers that skipped validation. Loud, not fatal.
		log.Warn("source_repos: invalid allowlist; source_diff disabled", "err", err)
		return tools
	}
	// Confine the GitHub App token to the forge's own host: source_diff clone
	// URLs are model-chosen across the whole allowlist, so without this a
	// github.com token would be transmitted to any other allowlisted host (e.g.
	// a gitlab.com repo). Off-host repos clone anonymously.
	sd := &whatchanged.Differ{
		TokenSource: BuildForgeTokenSource(cfg, log),
		TokenHost:   githubGitHost(cfg.Forge.GitHubAPIURL),
	}
	if cfg.GitOps.Mirror.IsEnabled() {
		base := cfg.GitOps.Mirror.Dir
		if base == "" {
			base = filepath.Join(os.TempDir(), "runlore-mirrors")
		}
		if mc, merr := whatchanged.NewMirrorCache(filepath.Join(base, "source"), cfg.GitOps.Mirror.Max); merr != nil {
			log.Warn("source_repos: mirror cache unavailable; falling back to clone-per-call", "err", merr)
		} else {
			sd.Mirrors = mc
		}
	}
	log.Info("source_diff enabled", "allow", cfg.SourceRepos.Allow, "token_host", sd.TokenHost)
	return append(tools, investigate.SourceDiffTool{Source: sd, Allow: allow})
}

// githubGitHost derives the git host the GitHub App token is valid for from the
// configured GitHub API URL. Default/empty ⇒ github.com. A GitHub Enterprise
// API URL (https://ghe.example.com/api/v3) shares its host with the git remote,
// so the host is returned as-is; the public api.github.com maps to github.com.
func githubGitHost(apiURL string) string {
	if apiURL == "" {
		return "github.com"
	}
	u, err := url.Parse(apiURL)
	if err != nil || u.Hostname() == "" {
		return "github.com"
	}
	if h := strings.ToLower(u.Hostname()); h != "api.github.com" {
		return h
	}
	return "github.com"
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
		allowed := map[string]bool{}
		for _, tn := range s.Tools {
			allowed[tn] = true
		}
		advertised := map[string]bool{}
		var skipped []string
		added := 0
		for _, rt := range remote {
			advertised[rt.Name] = true
			if len(allowed) > 0 && !allowed[rt.Name] {
				skipped = append(skipped, rt.Name)
				continue
			}
			tl := mcp.NewTool(c, rt)
			if have[tl.Name()] {
				log.Warn("mcp: skipping tool (name collision)", "server", s.Name, "tool", tl.Name())
				continue
			}
			have[tl.Name()] = true
			tools = append(tools, tl)
			added++
		}
		if len(skipped) > 0 {
			log.Info("mcp: tools excluded by allowlist", "server", s.Name, "skipped", skipped)
		}
		for tn := range allowed {
			if !advertised[tn] {
				log.Warn("mcp: allowlisted tool not advertised by server (typo?)", "server", s.Name, "tool", tn)
			}
		}
		log.Info("mcp: registered server tools", "server", s.Name, "tools", added)
	}
	return tools
}

// toPricing converts the optional config pricing to the loop's Pricing carrier
// (nil ⇒ nil, i.e. unpriced — token totals are reported without a cost).
func toPricing(p *config.Pricing) *investigate.Pricing {
	if p == nil {
		return nil
	}
	return &investigate.Pricing{
		InputUSDPerMTok:       p.InputUSDPerMTok,
		OutputUSDPerMTok:      p.OutputUSDPerMTok,
		CachedInputUSDPerMTok: p.CachedInputUSDPerMTok,
	}
}

// kbMatchScore returns the BM25 bar the kb-match visibility signal
// (Investigation.MatchedKnowledge — the "📚 Matches known runbook" block) uses to decide
// a full investigation's kb_search hit is a clear known-runbook match worth surfacing.
// kb_search runs in the SAME corpus/query-dependent BM25 score regime as instant recall,
// so we borrow the operator's CONFIGURED recall SoloFloor (recall's most conservative
// single-hit bar) instead of a hardcoded constant: a cluster that tunes solo_floor DOWN
// for its sub-1.0 label-derived alert-query scores then gets a correspondingly low
// visibility bar, rather than the signal silently never firing (live-found). A nil recall
// (instant recall disabled) ⇒ 0, so the loop's tracker falls back to its historical 4.0
// default (investigate.kbClearMatchScoreDefault) and behaviour is unchanged.
func kbMatchScore(recall *investigate.Recall) float64 {
	if recall == nil {
		return 0
	}
	return recall.SoloFloor
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
	// Assign via a concrete-nil check so a disabled curator (BuildCurator returns a
	// nil *curator.Curator) stays a nil interface — not a non-nil typed-nil that would
	// pass onInvestigationComplete's `cur != nil` guard and panic on Curate.
	var curOrNil investigationCurator
	if cur != nil {
		// Fingerprint-dedup matches become recovery evidence for contested entries
		// (👎 recovery, N5). A disabled ledger (no outcome.ledger_path) no-ops inside
		// Confirm, so this wiring is unconditional.
		cur.Confirmations = ledger
		curOrNil = cur
	}
	actions := action.New(cfg.Actions)
	if actions.Enabled() {
		log.Info("action policy enabled", "mode", string(actions.Mode()))
	}
	// Recurrence cooldown (opt-in): suppress re-investigating a trigger the agent
	// conclusively answered moments ago. Validate already requires a ledger with a
	// non-zero cooldown; Enabled() guards the disabled-ledger edge regardless.
	var recurrence *investigate.RecurrenceGate
	if d := cfg.Investigation.RecurrenceCooldown.Std(); d > 0 && ledger.Enabled() {
		recurrence = &investigate.RecurrenceGate{Outcome: ledger, Cooldown: d}
		log.Info("recurrence cooldown enabled", "cooldown", d)
	}
	// Per-tool timeout: default to 60s when unset (0) so one hung tool can't eat the
	// whole per-investigation budget; an explicit config value flows through as-is.
	toolTimeout := cfg.Investigation.ToolTimeout.Std()
	if toolTimeout == 0 {
		toolTimeout = defaultToolTimeout
	}
	// Interim progress notifications (opt-in). Wire the loop's callback to any
	// notifier that implements the ProgressNotifier capability (Multi type-asserts
	// per sink); delivery is best-effort — a failing ping is logged and swallowed,
	// never failing the investigation. Left unset (nil, 0) when disabled ⇒ the loop
	// emits nothing and makes zero extra model calls.
	var onProgress func(providers.ProgressUpdate)
	progressEverySteps := 0
	if pu := cfg.Investigation.ProgressUpdates; pu.Enabled {
		progressEverySteps = pu.EverySteps
		onProgress = func(up providers.ProgressUpdate) {
			// A progress ping must never fail an investigation: swallow its errors.
			if err := notifier.DeliverProgress(context.Background(), up); err != nil {
				log.Warn("progress notification failed", "err", err)
			}
		}
		log.Info("interim progress updates enabled", "every_steps", progressEverySteps)
	}
	// Per-investigation cost reporting (optional). The verify override inherits the
	// main pricing when it sets none (whole-struct or() inherit), so a cheaper verify
	// model is costed correctly.
	mainPricing := toPricing(cfg.Model.Pricing)
	verifyPricing := mainPricing
	if v := cfg.Model.Verify; v != nil && v.Pricing != nil {
		verifyPricing = toPricing(v.Pricing)
	}
	// A nil *catalog.Catalog must stay a nil INTERFACE here: assigning a typed-nil
	// *catalog.Catalog to prior would make prior != nil true (interface holds a
	// non-nil type descriptor even with a nil pointer), and onInvestigationComplete's
	// guard would then call FindFingerprint on a nil receiver and panic.
	var prior priorEntryFinder
	if cat != nil {
		prior = cat
	}
	return &investigate.LoopInvestigator{
		Model:                     model,
		VerifyModel:               BuildVerifyModel(cfg),
		Pricing:                   mainPricing,
		VerifyPricing:             verifyPricing,
		Tools:                     tools,
		Log:                       log,
		Actions:                   actions,
		Recall:                    recall,
		Recurrence:                recurrence,
		Verify:                    true, // adversarial review of root causes before delivery/curation
		Metrics:                   metrics,
		ModelProvider:             cfg.Model.Provider,
		MaxSteps:                  cfg.Investigation.MaxSteps,
		MaxToolOutputBytes:        cfg.Investigation.MaxToolOutputBytes,
		MaxTokensPerInvestigation: cfg.Investigation.MaxTokensPerInvestigation,
		Compaction:                cfg.Investigation.Compaction,
		Timeout:                   cfg.Investigation.Timeout.Std(),
		ToolTimeout:               toolTimeout,
		KBMatchScore:              kbMatchScore(recall), // visibility bar tracks the configured recall floor
		OnProgress:                onProgress,
		ProgressEverySteps:        progressEverySteps,
		OnComplete: func(found providers.Investigation) {
			onInvestigationComplete(ctx, found, ledger, prior, curOrNil, notifier, auto, approvals, metrics, log)
		},
	}, cat, nil
}

// investigationCurator opens a KB issue/PR for an investigation's findings.
// Satisfied by *curator.Curator; abstracted so onInvestigationComplete's ordering
// is unit-testable with a stub that need not talk to a forge.
type investigationCurator interface {
	Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error)
}

// priorEntryFinder is the catalog's exact-identity lookup used to quote the
// merged KB entry on a recurring incident (implemented by *catalog.Catalog).
// Narrowed to an interface so the completion pipeline is testable without an index.
type priorEntryFinder interface {
	FindFingerprint(fp string) (catalog.Entry, bool)
}

// warnIfBackendUnreachable probes a configured metrics/logs backend once at startup
// and logs a loud WARN if it can't be reached. Without this the failure is SILENT and
// insidious: a NetworkPolicy that blocks egress (or a wrong URL) doesn't stop startup —
// the agent runs, but every metrics/logs tool call hangs until it times out mid-
// investigation, starving the analysis of those signals (and burning the per-
// investigation deadline half-blind). Any HTTP response — even 401/404 — counts as
// reachable; only a connection error or timeout is a problem. Best-effort and
// non-fatal: the probe never blocks startup beyond its short timeout.
func warnIfBackendUnreachable(ctx context.Context, log *slog.Logger, kind, rawURL string) {
	if log == nil || rawURL == "" {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return // a malformed URL is caught by config validation, not here
	}
	// Use the SSRF-guarded egress client (redirect guard + credential stripping) with a
	// bounded timeout — never http.DefaultClient (unbounded, no redirect guard). The
	// request context still carries the 3s probe deadline above.
	resp, err := httpx.SecureClient(3 * time.Second).Do(req)
	if err != nil {
		log.Warn("configured "+kind+" backend is UNREACHABLE — investigations will run WITHOUT it "+
			"(check the NetworkPolicy egress to this endpoint's port, or the URL)",
			"kind", kind, "url", rawURL, "err", err)
		return
	}
	_ = resp.Body.Close()
}

// onInvestigationComplete runs the post-investigation pipeline once the loop returns
// findings: stamp recurrence facts, curate, record the outcome open, handle actions,
// then deliver. Extracted from BuildInvestigator's OnComplete closure so the ordering
// (which the outcome open and recurrence pointers depend on) is unit-testable.
//
// Order matters: recurrence facts are read BEFORE this run's own open is recorded,
// and curate runs BEFORE the open so the open durably carries the KB link.
func onInvestigationComplete(ctx context.Context, found providers.Investigation, ledger *outcome.Ledger, prior priorEntryFinder, cur investigationCurator, notifier *notify.Multi, auto *action.Auto, approvals *action.Approvals, metrics *telemetry.Metrics, log *slog.Logger) {
	// Recurrence facts BEFORE recording this run's own open, so the count and
	// "previous" pointer describe prior investigations only. Occurrences returns the
	// total opens recorded so far for this TriggerKey; this run adds one, hence n+1
	// (and 1 the first time a key with no prior opens is seen).
	if n, last, url := ledger.Occurrences(found.TriggerKey); n > 0 {
		found.Occurrences = n + 1
		found.LastOccurrence = last
		found.PrevCuratedURL = url
	} else if found.TriggerKey != "" {
		found.Occurrences = 1
	}
	// The dedup fingerprint is this incident's deterministic identity: the merged
	// KB entry carries it in frontmatter and the ledger opens below stamp it —
	// computed once for both uses.
	dupFP := curator.DupFingerprint(found)
	// Prior knowledge: on a RECURRING fresh investigation, quote what the merged
	// KB entry already says (cause + human-reviewed resolution + recall track
	// record) so the on-call reads the previous answer in the notification
	// instead of clicking through to the forge. Recalls are excluded — the
	// recalled entry IS the answer being delivered. Best-effort by construction:
	// no merged entry, empty sections, or a ledger error leave Prior nil and the
	// notification falls back to the counter+link it already carries.
	if found.Occurrences > 1 && !found.Recalled && prior != nil {
		if e, ok := prior.FindFingerprint(dupFP); ok {
			cause, resolution := e.Section("Cause"), e.Section("Resolution")
			if cause != "" || resolution != "" {
				pk := &providers.PriorKnowledge{Cause: cause, Resolution: resolution, EntryPath: e.Path}
				if counts, err := ledger.OpenCounts(); err == nil {
					agg := counts[e.Path]
					pk.Recalls, pk.Resolved = agg.Recalls, agg.Resolved
				}
				found.Prior = pk
			}
		}
	}
	// A recall already carries Prior (cause + resolution, stamped by recalledInvestigation
	// from the matched entry); enrich it with the entry's recall track record so the
	// "⚡ instant recall" block can show the resolve rate — the outcome-ledger signal that
	// makes the cache hit trustworthy. Read before this run's own open is recorded, so the
	// rate describes prior recalls only.
	if found.Recalled && found.Prior != nil && found.RecalledEntry != "" {
		if counts, err := ledger.OpenCounts(); err == nil {
			agg := counts[found.RecalledEntry]
			found.Prior.Recalls, found.Prior.Resolved = agg.Recalls, agg.Resolved
		}
	}
	// Curate BEFORE recording the outcome open — the open event is the durable record
	// of THIS investigation and must carry the KB link that recurrence pointers and
	// the learning loop later read back; delivery also links to the KB issue/PR. A
	// curate failure is non-fatal (log + continue) so opens are still recorded and the
	// findings are still delivered.
	if cur != nil {
		if ref, err := cur.Curate(context.Background(), found); err != nil {
			log.Error("curate findings", "err", err)
		} else if ref.URL != "" {
			found.CuratedURL = ref.URL
			log.Info("curated", "url", ref.URL)
		}
	}
	// Record the outcome "open": this investigation happened for an incident, with the
	// answer we used (recall vs fresh) and the KB link curate just produced. A matching
	// resolved-alert webhook later stamps whether it actually resolved. Skip sources
	// without an alert fingerprint (GitOps watch, reinvestigate poller) — they could
	// never be matched by a resolved-alert webhook.
	fps := found.Fingerprints
	if len(fps) == 0 && found.Fingerprint != "" {
		fps = []string{found.Fingerprint}
	}
	if len(fps) > 0 {
		kind := OutcomeKind(found.Recalled)
		now := time.Now()
		// One open per constituent fingerprint (coalesced batches fan out), so
		// every alert's resolve webhook can later match this investigation.
		for i, fp := range fps {
			// The per-fingerprint opens exist for resolve-webhook pairing, but the
			// TriggerKey occurrence index (ledger.byTrigger) counts investigations,
			// not constituents — and it is rebuilt from a raw replay of the file
			// (loadLocked), which has no notion of batches. Stamping the TriggerKey on
			// every open of an N-alert coalesced batch would therefore inflate
			// Occurrences by N, both live and again on every restart; first-open-only
			// keeps the count right by construction on both paths. CuratedURL/Verdict
			// stay on every open (the index ignores TriggerKey-less events, and a
			// complete per-fingerprint record keeps future readers from seeing gaps).
			triggerKey := ""
			if i == 0 {
				triggerKey = found.TriggerKey
			}
			// Whether a resolve signal can ever arrive for this open: true for sources
			// with a resolve channel (Alertmanager/PagerDuty — real fingerprints), false
			// for sources that never emit one (GitOps/reinvestigate — synthetic derived
			// fingerprints). A non-resolvable recall open is recorded for recurrence but
			// kept out of recall decay (see outcome.applyOpenLocked).
			resolvable := !outcome.Derived(fp)
			if err := ledger.Open(outcome.Event{
				Fingerprint:    fp,
				DupFingerprint: dupFP,
				Kind:           kind,
				Entry:          found.RecalledEntry,
				Title:          found.Title,
				Resource:       found.Resource.Ref(),
				TriggerKey:     triggerKey,
				CuratedURL:     found.CuratedURL,
				Verdict:        string(found.Verdict),
				Resolvable:     &resolvable,
				At:             now,
				// When the investigation BEGAN (`now` above is its COMPLETION). The gap is the
				// enqueue→open latency — queue wait behind the single worker, rate-limit
				// backoff, then the run — and it is the exact window in which a resolve may
				// legitimately land before this open (see outcome.resolvesSince).
				StartedAt: found.InvestigationStartedAt,
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
	if err := notifier.Deliver(context.Background(), found); err != nil {
		log.Error("deliver findings", "err", err)
	} else {
		log.Info("delivered findings",
			"confidence", found.Confidence, "root_causes", len(found.RootCauses), "curated_url", found.CuratedURL)
	}
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
