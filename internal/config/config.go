// SPDX-License-Identifier: Apache-2.0

// Package config defines RunLore's configuration, including provider wiring and
// the trigger policy that decides which incidents start an investigation.
//
// The trigger policy is what keeps RunLore from firing on every alert: it filters
// incidents by environment, severity, namespace/team/label, and dedups still-firing
// alerts — controlling noise, relevance, and LLM cost.
package config

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/providers"
)

// Config is the top-level RunLore configuration (loaded from YAML).
type Config struct {
	GitOps   GitOps        `yaml:"gitops"` // engine selection (flux default | argocd)
	Triggers TriggerPolicy `yaml:"triggers"`

	// Sources is the per-source enablement map: a key under `sources.<name>`
	// enables that source adapter, and its value is the adapter's own raw config
	// (decoded lazily by each adapter's Build). Presence is enablement — e.g.
	// `sources.alertmanager: {}` turns on the Alertmanager webhook source. This
	// keeps adding a source from requiring a central-struct edit. The webhook
	// auth token stays server-level (server.webhook_token_env).
	Sources map[string]yaml.Node `yaml:"sources"`

	Actions ActionPolicy `yaml:"actions"` // read-only by default; the upper rungs of the autonomy ladder
	Forge   Forge        `yaml:"forge"`   // git-forge auth (GitHub App) for diff access + curation
	Curate  Curate       `yaml:"curate"`  // Phase-2 backlog groomer settings
	Model   Model        `yaml:"model"`   // optional; when BaseURL is set, serve uses the LLM investigator
	Notify  Notify       `yaml:"notify"`  // chat delivery for findings
	Catalog Catalog      `yaml:"catalog"` // OKF knowledge catalog
	Outcome Outcome      `yaml:"outcome"` // learning-loop outcome ledger

	LeaderElection LeaderElection `yaml:"leader_election"` // HA: only the leader investigates

	Metrics MetricsConfig `yaml:"metrics"` // PromQL backend (VictoriaMetrics/Prometheus) for query_metrics
	Logs    LogsConfig    `yaml:"logs"`    // LogsQL backend (VictoriaLogs) for query_logs
	Network Network       `yaml:"network"` // network-flow data source (pluggable, CNI-agnostic); empty Provider disables it
	Cloud   Cloud         `yaml:"cloud"`   // cloud-side context (AWS); empty Provider disables it
	MCP     MCP           `yaml:"mcp"`     // external MCP servers whose tools the agent may call (opt-in)

	Server ServerConfig `yaml:"server"` // HTTP ingress (webhook authentication)

	Investigation Investigation `yaml:"investigation"` // coalescing + rate-limit + per-investigation token controls
	Telemetry     Telemetry     `yaml:"telemetry"`     // OpenTelemetry metrics
	Logging       Logging       `yaml:"logging"`       // structured-logging format + verbosity
}

// Logging configures the structured logger. Format selects human-readable text
// (default, for local CLI) or JSON (for in-cluster log aggregation). Level sets
// verbosity. Both are overridable at startup via RUNLORE_LOG_FORMAT /
// RUNLORE_LOG_LEVEL (see internal/logging).
type Logging struct {
	Format string `yaml:"format"` // "text" (default) | "json"
	Level  string `yaml:"level"`  // "debug" | "info" (default) | "warn" | "error"
}

// Endpoint is a backend base URL with optional auth; empty URL disables the
// corresponding tool. Auth follows the secrets-by-indirection convention used
// elsewhere (model.api_key_env, forge.*_env): the config stores the NAME of an
// env var, never the secret itself, and the value is read at runtime.
type Endpoint struct {
	URL string `yaml:"url"`

	// TokenEnv names an env var holding a bearer token for the backend. When set
	// and the var is non-empty, requests carry "Authorization: Bearer <token>".
	// Empty (default) ⇒ no Authorization header (unchanged, keyless behaviour).
	TokenEnv string `yaml:"token_env"`

	// Headers are static request headers added to every backend request — e.g. a
	// tenant header for a multi-tenant VictoriaMetrics/VictoriaLogs instance
	// ("X-Scope-OrgID: <tenant>"). Empty (default) ⇒ no extra headers.
	Headers map[string]string `yaml:"headers"`
}

// Metrics backend flavors for config.metrics.flavor. The flavor unlocks
// backend-specific query guidance (VictoriaMetrics also speaks MetricsQL, a PromQL
// superset). Empty ⇒ auto-detect at startup (probe /api/v1/status/buildinfo),
// failing safe to generic Prometheus behaviour when the probe can't identify the
// backend — no MetricsQL claims are made unless the backend is known to be VM.
const (
	MetricsFlavorPrometheus     = "prometheus"      // generic Prometheus HTTP API only
	MetricsFlavorVictoriaMetric = "victoriametrics" // also accepts MetricsQL (PromQL superset)
)

// MetricsConfig is the metrics backend endpoint plus an OPTIONAL flavor override.
// The endpoint keys (url/token_env/headers) are inlined so the existing
// `metrics: {url: …}` shape is unchanged; Flavor is a new opt-in sub-key. Empty
// Flavor ⇒ auto-detect (see MetricsFlavor*), which fails safe to plain Prometheus.
type MetricsConfig struct {
	Endpoint `yaml:",inline"`

	// Flavor optionally pins the backend flavor instead of auto-detecting it:
	// "victoriametrics" enables MetricsQL query guidance; "prometheus" (or an unknown
	// value) keeps generic behaviour. Empty ⇒ probe at startup.
	Flavor string `yaml:"flavor"`
}

// LogsConfig is the logs backend endpoint plus the OPTIONAL collector field-naming
// convention. The endpoint keys (url/token_env/headers) are inlined so the existing
// `logs: {url: …}` shape is unchanged; Fields is a new opt-in sub-key that lets an
// operator whose collector labels logs differently (e.g. Loki-style `namespace`
// instead of `kubernetes.pod_namespace`) retarget every logs query and the renderer
// WITHOUT a code change. Empty Fields ⇒ the shipped VictoriaLogs/vector convention.
type LogsConfig struct {
	Endpoint `yaml:",inline"`

	Fields LogFields `yaml:"fields"`
}

// LogFields names the collector's log-schema fields. Every value defaults (via
// Resolved) to EXACTLY the string the code hardcoded before this was configurable,
// so an unset `logs.fields` is a no-op — the maintainer's test cluster keeps
// working. Override only the field(s) your collector renames.
type LogFields struct {
	// ContainerField / NamespaceField / PodField are the STREAM label names used to
	// build a `{k=v}` selector (query_logs) and to derive the compact pod/container
	// identity in the renderer. Defaults: kubernetes.container_name /
	// kubernetes.pod_namespace / kubernetes.pod_name.
	ContainerField string `yaml:"container_field"`
	NamespaceField string `yaml:"namespace_field"`
	PodField       string `yaml:"pod_field"`

	// LevelField is the severity field query_logs filters on (after unpack_json) and
	// that the error-summary histogram splits by. Defaults: log.level.
	LevelField string `yaml:"level_field"`

	// UnpackPipe is the LogsQL pipe that promotes JSON body fields to top-level
	// fields so LevelField becomes filterable. Default: unpack_json. Set to a
	// different pipe (or leave empty to disable) if your logs are already flat.
	UnpackPipe string `yaml:"unpack_pipe"`
}

// Default log-field convention: the VictoriaLogs + vector kubernetes-metadata
// layout RunLore shipped with. Each Resolved default MUST equal one of these so an
// unset config reproduces the previous hardcoded behaviour exactly.
const (
	defaultLogContainerField = "kubernetes.container_name"
	defaultLogNamespaceField = "kubernetes.pod_namespace"
	defaultLogPodField       = "kubernetes.pod_name"
	defaultLogLevelField     = "log.level"
	defaultLogUnpackPipe     = "unpack_json"
)

// Resolved returns the field convention with every unset value filled from the
// shipped defaults, so callers can use the result without repeating the fallbacks.
// UnpackPipe is deliberately allowed to be explicitly empty (already-flat logs), so
// only an unset (zero) LogFields as a whole restores the default pipe — an operator
// who sets any field but leaves unpack_pipe empty still gets the default pipe unless
// they had a fully-zero struct. To keep the "any override" case simple, an empty
// UnpackPipe here falls back to the default; disabling it is out of scope for v1.
func (f LogFields) Resolved() LogFields {
	if f.ContainerField == "" {
		f.ContainerField = defaultLogContainerField
	}
	if f.NamespaceField == "" {
		f.NamespaceField = defaultLogNamespaceField
	}
	if f.PodField == "" {
		f.PodField = defaultLogPodField
	}
	if f.LevelField == "" {
		f.LevelField = defaultLogLevelField
	}
	if f.UnpackPipe == "" {
		f.UnpackPipe = defaultLogUnpackPipe
	}
	return f
}

// Cloud configures the cloud context provider. Auth is in-cluster identity (EKS
// Pod Identity / IRSA) via the AWS SDK's default credential chain — no static keys.
// Empty Provider disables the cloud tools (default — cloud is opt-in).
type Cloud struct {
	Provider    string `yaml:"provider"`     // "" (disabled) | "aws"
	Region      string `yaml:"region"`       // e.g. eu-west-3 (default: AWS_REGION / IMDS)
	ClusterName string `yaml:"cluster_name"` // EKS cluster name, scopes nodegroup/ASG queries
}

// MCP configures outbound connections to external MCP servers whose tools the
// investigation loop may call. Empty Servers disables it (the default — MCP is opt-in).
type MCP struct {
	Servers []MCPServer `yaml:"servers"`
}

// MCPServer is one external MCP server reachable over streamable-HTTP.
type MCPServer struct {
	Name     string `yaml:"name"` // identifier; namespaces its tools as name__tool
	Endpoint `yaml:",inline"`
}

// Network provider identifiers for config.network.provider. The network signal is
// PLUGGABLE and assumes no particular CNI — pick the one matching your environment.
const (
	NetworkHubble          = "hubble"            // Cilium Hubble Relay (requires the Cilium CNI)
	NetworkAWSVPCFlowLogs  = "aws-vpc-flow-logs" // AWS VPC Flow Logs via CloudWatch Logs (any AWS VPC; CNI-agnostic)
	NetworkGCPFirewallLogs = "gcp-firewall-logs" // GCP Firewall Rules Logging via Cloud Logging (any GCP VPC; CNI-agnostic)
)

// Network configures the network-flow data source backing the network_drops tool.
// The signal is pluggable and CNI-agnostic: RunLore does NOT assume Cilium (or any
// particular CNI). Empty Provider disables the tool (the default — network is opt-in).
type Network struct {
	Provider string     `yaml:"provider"` // "" (disabled) | hubble | aws-vpc-flow-logs | gcp-firewall-logs
	Hubble   HubbleCfg  `yaml:"hubble"`   // when provider=hubble
	AWS      AWSFlowCfg `yaml:"aws"`      // when provider=aws-vpc-flow-logs
	GCP      GCPFlowCfg `yaml:"gcp"`      // when provider=gcp-firewall-logs

	// URL keeps the pre-pluggable `network: {url: ...}` shape working: a bare url with
	// no provider is treated as Hubble (with a deprecation warning at wiring time).
	URL string `yaml:"url"`
}

// HubbleCfg configures the Cilium Hubble Relay network provider.
type HubbleCfg struct {
	URL string `yaml:"url"` // Hubble Relay gRPC address (host:port), e.g. hubble-relay.kube-system:80

	// TLS enables encrypted transport to the Hubble Relay endpoint. Default
	// (false) keeps the existing plaintext/insecure behaviour so the maintainer's
	// test cluster (Cilium/Hubble Relay, currently plaintext) keeps connecting
	// without any config change.
	TLS bool `yaml:"tls"` // false (default) = insecure/plaintext; true = TLS (credentials.NewTLS)
}

// AWSFlowCfg configures the AWS VPC Flow Logs network provider. Auth is in-cluster
// identity (EKS Pod Identity / IRSA) via the AWS default credential chain.
type AWSFlowCfg struct {
	Region   string `yaml:"region"`    // AWS region (default: AWS_REGION / IMDS)
	LogGroup string `yaml:"log_group"` // CloudWatch Logs group that receives the VPC Flow Logs (required)

	// FlowFormat selects the VPC Flow Logs field layout. Default (empty / "v2")
	// uses the standard v2 default format (14 fields). Set to "custom" and
	// configure FlowFields when the log group was created with a custom field
	// list; custom-format log groups silently return no results under "v2" because
	// the positional field assumptions no longer hold.
	FlowFormat string `yaml:"flow_format"` // "" | "v2" (default) | "custom"

	// FlowFields maps flow field names to their 0-based column index in the
	// space-delimited log record. Only consulted when FlowFormat is "custom".
	// Required keys: srcaddr, dstaddr, srcport, dstport, protocol.
	// Example for a custom format omitting the first two standard fields:
	//   flow_fields: {srcaddr: 1, dstaddr: 2, srcport: 3, dstport: 4, protocol: 5}
	FlowFields map[string]int `yaml:"flow_fields"`
}

// GCPFlowCfg configures the GCP Firewall Rules Logging network provider. Auth is
// Workload Identity / Application Default Credentials.
type GCPFlowCfg struct {
	Project string `yaml:"project"` // GCP project ID (default: ADC / metadata server)
}

// UnmarshalYAML decodes the Network block and applies the legacy back-compat mapping:
// a bare `network: {url: ...}` (the old Hubble-only shape) becomes provider=hubble.
func (n *Network) UnmarshalYAML(value *yaml.Node) error {
	type raw Network // avoid recursing into this method
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*n = Network(r)
	if n.Provider == "" && n.URL != "" {
		n.Provider = NetworkHubble
		if n.Hubble.URL == "" {
			n.Hubble.URL = n.URL
		}
	}
	return nil
}

// ServerConfig configures the HTTP ingress.
type ServerConfig struct {
	// WebhookTokenEnv names an env var holding a shared secret required on
	// POST /webhook/alertmanager as "Authorization: Bearer <token>". Empty leaves
	// the webhook unauthenticated (rejected by Validate when actions.mode=auto).
	WebhookTokenEnv string `yaml:"webhook_token_env"`
}

// Investigation holds cost/throughput controls on the alert→investigation→LLM path.
type Investigation struct {
	Coalesce                  Coalesce  `yaml:"coalesce"`
	RateLimit                 RateLimit `yaml:"rate_limit"`
	MaxSteps                  int       `yaml:"max_steps"`                    // 0 ⇒ loop default (20)
	MaxToolOutputBytes        int       `yaml:"max_tool_output_bytes"`        // unset/0 ⇒ bounded default (32768); -1 ⇒ unlimited
	MaxTokensPerInvestigation int       `yaml:"max_tokens_per_investigation"` // unset/0 ⇒ bounded default (100000); -1 ⇒ unlimited
	Timeout                   Duration  `yaml:"timeout"`                      // per-investigation deadline; 0 ⇒ default (10m) via applyDefaults
	ToolTimeout               Duration  `yaml:"tool_timeout"`                 // per-TOOL-call timeout so one hung tool can't eat the budget; 0 ⇒ default (60s) at construction

	// RecurrenceCooldown (opt-in, 0 = off) suppresses re-investigating a trigger
	// whose previous investigation completed less than this long ago, concluded
	// (verdict ≠ inconclusive), and has no standing 👎 feedback. Without it a
	// still-firing alert re-investigates on every Alertmanager repeat_interval and
	// a persistently-failing GitOps resource on every informer resync (~10m).
	// Requires outcome.ledger_path (the gate reads the ledger's trigger index).
	RecurrenceCooldown Duration `yaml:"recurrence_cooldown"`

	// Compaction selects how mid-loop history compaction treats the tool outputs it
	// elides once the estimate crosses the compaction target. "" / "elide" is the
	// default: drop their bodies for short markers (lossy). "summarize" first asks a
	// model (the verify-tier model when configured, else the main model) for one
	// compact factual digest of the batch and keeps that in place of the markers,
	// falling back to plain elision on any summarizer error/refusal/truncation.
	Compaction string `yaml:"compaction"` // "" | "elide" (default) | "summarize"

	// PodLogNamespaces lists extra namespaces (beyond the incident's own) that
	// pod_logs may read controller/crash logs from. pod_logs streams raw pod logs
	// (which carry secrets/PII) to the external LLM, so the model is constrained to
	// the incident namespace plus this allowlist at the application layer — not just
	// by Kubernetes RBAC. Set this to match the Helm rbac.controllerLogNamespaces
	// (e.g. [flux-system]); empty means the incident namespace only.
	PodLogNamespaces []string `yaml:"pod_log_namespaces"`

	// ProgressUpdates opts into interim progress notifications during a long
	// investigation. Off by default (zero behaviour change, zero extra model calls).
	ProgressUpdates ProgressUpdates `yaml:"progress_updates"`
}

// ProgressUpdates configures opt-in interim progress notifications: a long
// investigation (up to 20 steps) is otherwise silent until the final message.
// Off by default. When enabled, the loop emits one ping every EverySteps steps
// to any notifier that supports it (Slack first); a ping is best-effort and never
// fails the investigation.
type ProgressUpdates struct {
	Enabled    bool `yaml:"enabled"`
	EverySteps int  `yaml:"every_steps"` // emit a ping every N steps; 0 ⇒ default 5 (applyDefaults). Must be > 0 when enabled.
}

// Coalesce folds correlated incidents into one investigation.
type Coalesce struct {
	Enabled           bool     `yaml:"enabled"`
	Debounce          Duration `yaml:"debounce"`
	MaxWait           Duration `yaml:"max_wait"`
	MaxBatch          int      `yaml:"max_batch"`
	Cooldown          Duration `yaml:"cooldown"`
	CorrelationLabels []string `yaml:"correlation_labels"` // empty ⇒ AM groupKey, else namespace+label values
}

// RateLimit caps investigation starts per sliding window.
type RateLimit struct {
	// MaxPerWindow caps investigation STARTS per Window. nil (unset) defaults to
	// 30 (see applyDefaults) — a cost-DoS guard, since per-incident spend is
	// bounded but the count of incidents was not. An EXPLICIT 0 preserves the
	// pre-default unlimited behavior for configs that opted into it.
	MaxPerWindow *int     `yaml:"max_per_window"`
	Window       Duration `yaml:"window"`
	MaxRequeues  int      `yaml:"max_requeues"` // drop a key after this many backoff requeues
}

// Telemetry configures OpenTelemetry metrics export.
type Telemetry struct {
	MetricsEnabled bool   `yaml:"metrics_enabled"` // serve OTel metrics on GET /metrics (Prometheus exposition)
	OTLPEndpoint   string `yaml:"otlp_endpoint"`   // optional OTLP push (phase-2); empty ⇒ scrape-only
}

// LeaderElection configures high availability. When enabled, replicas elect a
// leader via a Lease; only the leader runs the informer watch + investigation
// queue and reports ready (so the Service routes webhooks to it). Run >1 replica
// for failover. Disabled by default (single-replica / local).
type LeaderElection struct {
	Enabled bool   `yaml:"enabled"`
	Name    string `yaml:"name"` // Lease name (default "runlore-leader")
}

// Catalog configures the OKF knowledge catalog read by the agent. Provide either
// a mounted Dir (e.g. a ConfigMap) or a Git repo to sync (which closes the
// read/write loop — the curator's merged PRs flow back into what the agent reads).
type Catalog struct {
	Dir           string        `yaml:"dir"` // OKF bundle path (mounted ConfigMap, or the git-sync mirror)
	Git           CatalogGit    `yaml:"git"`
	InstantRecall InstantRecall `yaml:"instant_recall"`
}

// Outcome configures the learning-loop outcome ledger.
type Outcome struct {
	LedgerPath string `yaml:"ledger_path"` // append-only JSONL path (e.g. the git-sync mirror PV); empty disables
	// MaxEvents bounds the JSONL before it is compacted on load (startup / leadership
	// Reload): older paired events are folded into a checkpoint record so the file stops
	// growing forever. A pointer for a three-state knob: nil (key absent) ⇒ a generous
	// default (outcome.DefaultMaxEvents); an explicit 0 ⇒ compaction disabled.
	MaxEvents *int `yaml:"max_events"`
}

// InstantRecall short-circuits the investigation loop when the catalog has a
// high-confidence match for the symptom. Off by default; MinScore is the BM25
// relevance floor (tune for your catalog).
type InstantRecall struct {
	Enabled              bool    `yaml:"enabled"`
	MinScore             float64 `yaml:"min_score"`              // similarity floor for the top hit
	MarginGap            float64 `yaml:"margin_gap"`             // top hit must beat the runner-up by at least this
	SoloFloor            float64 `yaml:"solo_floor"`             // confident bar when there is only one hit (higher than MinScore)
	RequireWorkloadMatch bool    `yaml:"require_workload_match"` // true = exact namespace+workload (also disables scopeless matching); false = namespace-level agreement is enough
	OutcomePrior         float64 `yaml:"outcome_prior"`          // Beta prior strength for outcome decay
	OutcomeFloor         float64 `yaml:"outcome_floor"`          // reject a recall when the outcome factor drops below this

	// Rerank adds an LLM reranking stage to the recall short-circuit: it ranks the
	// top-K structurally-agreeing candidates against the incident with ONE cheap
	// model call and gates the fire on the reranker's CALIBRATED match confidence
	// (RerankThreshold) instead of the corpus-dependent BM25 magnitude
	// (SoloFloor/MarginGap). This is the principled gate: an enriched real-corpus
	// BM25 score is ~0.1–1.2 (an order of magnitude below the default SoloFloor 4.0),
	// so the magnitude gate only fires where the operator hand-tuned solo_floor to
	// their corpus, whereas a calibrated confidence needs no per-corpus tuning —
	// measured 0/11 → 11/11 fire at default thresholds with perfect precision.
	//
	// Three-state (*bool): **nil (unset) ⇒ ON** whenever instant_recall is enabled,
	// so it works out of the box; explicit `false` disables it (the BM25-magnitude
	// gate is then used, byte-for-byte unchanged); explicit `true` is the same as
	// unset. Use RerankEnabled(). Routes to model.verify (cheaper/faster) when
	// configured, else the main model.
	Rerank          *bool   `yaml:"rerank"`           // nil/true ⇒ reranker ON (default); false ⇒ off (legacy BM25-magnitude gate)
	RerankThreshold float64 `yaml:"rerank_threshold"` // calibrated match-confidence bar to short-circuit (default 0.7; corpus-independent)
	RerankK         int     `yaml:"rerank_k"`         // max structurally-agreeing candidates ranked in one call (default 5; bounded for cost)
	RerankMinScore  float64 `yaml:"rerank_min_score"` // trivial retrieval-score floor below which retrieval found nothing plausible → skip the paid call (cost guard; default 0.1)

	// Hybrid switches recall to fused BM25 + embedding retrieval, gated on COSINE
	// similarity instead of the BM25 score above. Requires model.embeddings to be
	// configured (else recall stays BM25). EXPERIMENTAL — tune the cosine thresholds
	// against the instant-recall eval before relying on it; the defaults are
	// conservative placeholders, not measured values.
	Hybrid          bool    `yaml:"hybrid"`            // enable hybrid (cosine-gated) recall
	HybridMinScore  float64 `yaml:"hybrid_min_score"`  // cosine floor for the top hit (default 0.80)
	HybridMarginGap float64 `yaml:"hybrid_margin_gap"` // cosine margin over the runner-up (default 0.05)
}

// RerankEnabled reports whether the instant-recall LLM reranker should run. It is
// ON by default whenever instant_recall is enabled (nil ⇒ on) — the calibrated,
// corpus-independent gate is what makes recall fire out of the box; only an
// explicit `rerank: false` falls back to the legacy BM25-magnitude gate.
func (ir InstantRecall) RerankEnabled() bool {
	return ir.Enabled && (ir.Rerank == nil || *ir.Rerank)
}

// CatalogGit configures periodic Git sync of the catalog into Dir.
type CatalogGit struct {
	URL      string   `yaml:"url"`       // repo to clone/pull; empty disables git-sync
	Branch   string   `yaml:"branch"`    // default "main"
	Interval Duration `yaml:"interval"`  // re-sync period (default 5m)
	TokenEnv string   `yaml:"token_env"` // env var with a read token (empty = anonymous/public)
}

// Model configures the LLM used for investigation. Provider selects the wire
// protocol: "openai" (default, OpenAI-compatible: vLLM/Ollama/OpenAI) or
// "anthropic" (native Messages API). When unconfigured, serve uses the log-only
// investigator.
type Model struct {
	Provider  string `yaml:"provider"`    // "openai" (default) | "anthropic" | "gemini"
	BaseURL   string `yaml:"base_url"`    // OpenAI: required; Anthropic/Gemini: optional (built-in default endpoint). Must be https when api_key_env is set on a public host (validated).
	Model     string `yaml:"model"`       // model name
	APIKeyEnv string `yaml:"api_key_env"` // env var holding the API key (empty = keyless)
	// MaxTokens caps the model's output (generated) tokens per request. 0 = use the
	// 8192 default. Streaming providers send it (Anthropic max_tokens, OpenAI
	// max_tokens, Gemini generationConfig.maxOutputTokens); a too-low value truncates.
	MaxTokens int `yaml:"max_tokens"`
	// Effort opts into deeper model reasoning per request. Provider-specific
	// vocabulary, validated at startup: anthropic low|medium|high|max (sent as
	// output_config.effort), openai minimal|low|medium|high (sent as
	// reasoning_effort). Empty = omitted from requests (today's behavior). Not
	// supported for provider gemini (its thinkingConfig has replay semantics —
	// thought signatures — the provider-agnostic history can't carry). Models that
	// don't support the knob return a 400, which is classified permanent.
	Effort string `yaml:"effort"`
	// Thinking opts into adaptive extended thinking, sent as thinking:{type:"adaptive"}.
	// Only value is "adaptive"; only supported for provider anthropic (the client
	// replays the signed thinking blocks across the tool loop). Empty = omitted from
	// requests (today's behavior, byte-for-byte). Validated at startup; any other value
	// or any non-anthropic provider is a clear config error. Interacts with effort:
	// both may be set (effort is soft guidance for how much thinking Claude does).
	// Give max_tokens headroom — thinking consumes output tokens.
	Thinking string `yaml:"thinking"`
	// Verify optionally routes the adversarial verify pass to a cheaper/faster model;
	// unset fields inherit from the parent above (so `verify: {model: <cheap>}` reuses
	// the same provider/endpoint/key). Absent ⇒ verify runs on the main model.
	Verify *ModelOverride `yaml:"verify"`
	// Embeddings optionally configures an OpenAI-compatible /embeddings endpoint used
	// for hybrid recall (instant_recall.hybrid). Unset ⇒ BM25-only recall.
	Embeddings *Embeddings `yaml:"embeddings"`
	// Pricing optionally sets token rates so RunLore can estimate and report a
	// per-investigation cost. Unset ⇒ token totals are reported without a dollar
	// figure. Rates are validated non-negative.
	Pricing *Pricing `yaml:"pricing"`
}

// Pricing sets model token rates in USD per MILLION tokens, used to estimate a
// per-investigation cost. All rates are optional and default to 0; a rate must be
// non-negative.
type Pricing struct {
	InputUSDPerMTok       float64 `yaml:"input_usd_per_mtok"`
	OutputUSDPerMTok      float64 `yaml:"output_usd_per_mtok"`
	CachedInputUSDPerMTok float64 `yaml:"cached_input_usd_per_mtok"`
}

// Embeddings configures an OpenAI-compatible /embeddings endpoint (vLLM/Ollama/
// OpenAI) for hybrid catalog recall — served by the same kind of endpoint as the
// model; keyless when APIKeyEnv is empty.
type Embeddings struct {
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// ModelOverride is a partial Model used to route the verify pass to a cheaper model;
// empty fields inherit from the parent Model.
type ModelOverride struct {
	Provider  string `yaml:"provider"`
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`
	// MaxTokens overrides the parent's effective output-token cap for the verify pass;
	// 0 inherits the parent's effective value.
	MaxTokens int `yaml:"max_tokens"`
	// Effort overrides the parent's effort for the verify pass; empty inherits the
	// parent's value (same vocabulary and validation as model.effort).
	Effort string `yaml:"effort"`
	// Thinking overrides the parent's thinking mode for the verify pass; empty inherits
	// the parent's value (same vocabulary and validation as model.thinking). Note the
	// verify pass always forces a tool_choice, so the Anthropic client drops thinking
	// for that request anyway — this knob only affects any non-forced verify calls.
	Thinking string `yaml:"thinking"`
	// Pricing overrides the parent's token rates for the verify pass (a cheaper
	// verify model has its own cost); nil inherits model.pricing.
	Pricing *Pricing `yaml:"pricing"`
}

// Notify configures where investigation findings are delivered.
type Notify struct {
	Slack  SlackNotify          `yaml:"slack"`
	Matrix MatrixNotify         `yaml:"matrix"`
	Extra  map[string]yaml.Node `yaml:",inline"` // notify.<name> blocks for registered (non-built-in) notifiers
}

// SlackNotify configures Slack delivery and (for rung-2 actions) interactive
// approve/reject buttons. Delivery uses either an incoming webhook (WebhookURLEnv)
// or a bot token posting to a channel via chat.postMessage (BotTokenEnv+Channel);
// if both are set, the bot token wins.
type SlackNotify struct {
	WebhookURLEnv    string   `yaml:"webhook_url_env"`    // env var holding the incoming-webhook URL
	BotTokenEnv      string   `yaml:"bot_token_env"`      // env var holding a bot token (xoxb-…) for chat.postMessage
	Channel          string   `yaml:"channel"`            // channel ID or name to post to (required with bot_token_env)
	SigningSecretEnv string   `yaml:"signing_secret_env"` // env var with the Slack signing secret (verifies button clicks)
	ApproverIDs      []string `yaml:"approver_ids"`       // Slack user IDs allowed to approve actions (empty = no Slack approvals)
	// FeedbackButtons (opt-in, default off) renders 👍/👎 buttons on investigation
	// messages; clicks are recorded in the outcome ledger and weigh the recalled
	// entry's trust like resolve signals do. Requires exposing POST
	// /slack/interactions to Slack (an interactivity Request URL — the same
	// endpoint approve-mode buttons use) plus signing_secret_env and
	// outcome.ledger_path; Validate fails loud when either is missing.
	FeedbackButtons bool `yaml:"feedback_buttons"`
}

// MatrixNotify configures Matrix delivery.
type MatrixNotify struct {
	Homeserver     string `yaml:"homeserver"`
	RoomID         string `yaml:"room_id"`
	AccessTokenEnv string `yaml:"access_token_env"` // env var holding the access token
	// FeedbackReactions (opt-in, default off) records 👍/👎 reactions on RunLore's
	// investigation messages into the outcome ledger, where they weigh recalled-
	// entry trust exactly like Slack's feedback buttons. Unlike Slack, nothing is
	// exposed: reactions arrive over the client-server /sync long-poll — an
	// OUTBOUND request authenticated by the access token above. Requires the three
	// notifier fields and outcome.ledger_path; Validate fails loud otherwise.
	FeedbackReactions bool `yaml:"feedback_reactions"`
}

// GitOps selects the GitOps engine RunLore reads (what-changed + failure watch).
type GitOps struct {
	Engine string       `yaml:"engine"` // "flux" (default) | "argocd"
	Mirror GitOpsMirror `yaml:"mirror"` // persistent clone mirror for what_changed
}

// GitOpsMirror configures the persistent per-repo bare mirror backing
// what_changed clones. Enabled by default (nil/true): a mirror only ever
// falls back to the previous clone-per-call behavior on error.
type GitOpsMirror struct {
	Enabled *bool  `yaml:"enabled"` // nil/true ⇒ on; false ⇒ clone per call (legacy)
	Dir     string `yaml:"dir"`     // mirror root; "" ⇒ <tmp>/runlore-mirrors (ephemeral; point at a PV to persist across restarts)
	Max     int    `yaml:"max"`     // max mirrors kept (LRU by mtime); 0 ⇒ 10
}

// IsEnabled reports whether the mirror cache is on (nil ⇒ default on).
func (m GitOpsMirror) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// TriggerPolicy decides what RunLore reacts to.
type TriggerPolicy struct {
	Incidents      IncidentTrigger      `yaml:"incidents"`       // primary trigger
	GitOpsFailures GitOpsFailureTrigger `yaml:"gitops_failures"` // secondary trigger
}

// GitOpsFailureTrigger holds the GitOps-failure-driven investigation POLICY.
// Enablement now lives under `sources.gitops.enabled`; this struct keeps only the
// debounce window. Debounce delays an investigation until the failure has persisted
// for that window (re-checked still Ready=False), filtering reconcile-churn
// transients that would otherwise produce confident-but-wrong root causes. A zero
// Debounce fires immediately on every Ready=False (the original behavior).
type GitOpsFailureTrigger struct {
	// Debounce is a pointer so an unset key (nil ⇒ 60s default, applied in
	// applyDefaults) is distinguishable from an explicit `debounce: 0` (fire
	// immediately). A plain Duration can't tell the two apart — both are the zero
	// value — which is why `debounce: 0` used to be silently clobbered to 60s.
	Debounce *Duration `yaml:"debounce"`
}

// DebounceWindow is the GitOps-failure debounce window. nil (unset) reads as 0
// here, but applyDefaults fills an unset trigger with 60s; an explicit 0
// means fire immediately on every Ready=False.
func (g GitOpsFailureTrigger) DebounceWindow() time.Duration {
	if g.Debounce == nil {
		return 0
	}
	return g.Debounce.Std()
}

// IncidentTrigger holds the incident/alert MATCH policy. Enablement of the
// alertmanager source now lives under `sources.alertmanager`; this struct is purely
// the match/ignore/dedup criteria applied to admitted alerts.
type IncidentTrigger struct {
	Match  IncidentMatch `yaml:"match"`  // must match to investigate
	Ignore IncidentMatch `yaml:"ignore"` // excludes even if Match passes
	Dedup  Dedup         `yaml:"dedup"`
	// Debounce is the pre-investigation hold for a firing alert: after admission
	// (match + dedup) RunLore waits this long and investigates only if the alert
	// is still active — i.e. no matching Alertmanager `resolved` webhook arrived
	// within the window. It filters self-resolving alerts (e.g. a
	// KubeDaemonSetRolloutStuck during a Karpenter node-churn cycle) that would
	// otherwise burn a full investigation on noise. It composes with `coalesce`
	// (which batches the survivors afterwards) and `dedup` (which still suppresses
	// re-fires before the hold begins).
	//
	// A pointer, so an unset key (nil ⇒ 60s default, applied in applyDefaults) is
	// distinguishable from an explicit `debounce: 0` (investigate immediately, on
	// every fire) — mirroring gitops_failures.debounce.
	//
	// It defaults ON because the hold is not merely a cost saver: an alert that
	// self-heals is still investigated without it, and its `resolved` webhook then
	// credits the recalled entry's resolve rate in the outcome ledger — trust earned
	// on a resolution the diagnosis had nothing to do with. Holding self-resolving
	// alerts back keeps that evidence out of the ledger in the first place.
	Debounce *Duration `yaml:"debounce"`
	// CancelQueuedOnResolve drops a QUEUED — accepted but not yet started —
	// investigation when the matching Alertmanager `resolved` webhook arrives
	// first. It extends Debounce past the hold window: without it, a fire→resolve
	// sequence whose firing already passed into the investigation queue still burns
	// a full paid investigation.
	//
	// A pointer, so an unset key (nil ⇒ true, applied in applyDefaults) is
	// distinguishable from an explicit `cancel_queued_on_resolve: false` — mirroring
	// Debounce.
	//
	// It defaults ON, and it is what makes the critical carve-out affordable. Debounce
	// deliberately does NOT hold a critical alert (investigate.Request.IsCritical: a
	// debounce must never delay the first look at a critical page), so on a default
	// install — whose trigger matches `severity: [critical]` exclusively — the hold
	// filters nothing. This does: a critical that self-heals before its investigation
	// STARTS is dropped from the queue when its `resolved` webhook lands. Same
	// noise/cost/ledger saving as the hold, at zero added latency, because nothing is
	// waited on — the cancel only ever races an investigation that has not begun.
	//
	// Boundaries: an IN-FLIGHT investigation is never cancelled, and a coalesced
	// multi-alert batch is not cancelled on one member's resolve (see
	// investigate.Queue.CancelByFingerprint). Set it to false to keep the post-hoc
	// answer to "why did it fire?" even after self-resolution.
	CancelQueuedOnResolve *bool `yaml:"cancel_queued_on_resolve"`
}

// DebounceWindow is the incident debounce hold. nil (unset) reads as 0 here, but
// applyDefaults fills an unset trigger with 60s; an explicit 0 means investigate
// immediately on every fire. NOTE: the hold never applies to a critical alert —
// see investigate.Request.IsCritical and source.incidentDebouncer.Hold.
func (t IncidentTrigger) DebounceWindow() time.Duration {
	if t.Debounce == nil {
		return 0
	}
	return t.Debounce.Std()
}

// CancelQueuedOnResolveEnabled reports whether a queued investigation is dropped
// when its alert resolves first. nil (unset) reads as false here, but applyDefaults
// fills an unset trigger with true; an explicit `false` is left untouched. Mirrors
// DebounceWindow.
func (t IncidentTrigger) CancelQueuedOnResolveEnabled() bool {
	return t.CancelQueuedOnResolve != nil && *t.CancelQueuedOnResolve
}

// IncidentMatch is a set of matchers ANDed together; empty fields match anything.
type IncidentMatch struct {
	Severity    []string          `yaml:"severity"`    // e.g. [critical]
	Environment []string          `yaml:"environment"` // e.g. [prod]
	Namespaces  []string          `yaml:"namespaces"`  // glob patterns
	AlertNames  []string          `yaml:"alertnames"`  // glob patterns
	Labels      map[string]string `yaml:"labels"`      // arbitrary label matchers
}

// Dedup suppresses re-investigation of a still-firing alert within Window.
type Dedup struct {
	Window Duration `yaml:"window"`
}

// MatchFields reports whether an incident passes this trigger policy: matched by
// Match and not excluded by a non-empty Ignore. Enablement is the source's job
// (sources.alertmanager); matching here is purely criteria. title is the alertname.
func (t IncidentTrigger) MatchFields(title, severity, environment, namespace string, labels map[string]string) bool {
	if !t.Match.matchFields(severity, environment, namespace, title, labels) {
		return false
	}
	if !t.Ignore.isEmpty() && t.Ignore.matchFields(severity, environment, namespace, title, labels) {
		return false
	}
	return true
}

// matchFields reports whether the incident satisfies every non-empty criterion.
func (m IncidentMatch) matchFields(severity, environment, namespace, alertname string, labels map[string]string) bool {
	// Severity is matched case-insensitively: Alertmanager labels arrive with
	// arbitrary casing (Critical, CRITICAL), and a config of `severity: [critical]`
	// must still match — otherwise RunLore silently goes deaf. This also keeps the
	// trigger consistent with the coalescer's EqualFold("critical") fast-path.
	if len(m.Severity) > 0 && !containsFold(m.Severity, severity) {
		return false
	}
	if len(m.Environment) > 0 && !slices.Contains(m.Environment, environment) {
		return false
	}
	if len(m.Namespaces) > 0 && !globAny(m.Namespaces, namespace) {
		return false
	}
	if len(m.AlertNames) > 0 && !globAny(m.AlertNames, alertname) {
		return false
	}
	for k, v := range m.Labels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// isEmpty reports whether no criteria are set.
func (m IncidentMatch) isEmpty() bool {
	return len(m.Severity) == 0 && len(m.Environment) == 0 &&
		len(m.Namespaces) == 0 && len(m.AlertNames) == 0 && len(m.Labels) == 0
}

// containsFold reports whether s equals any element of vals under Unicode case
// folding (the case-insensitive counterpart of slices.Contains).
func containsFold(vals []string, s string) bool {
	for _, v := range vals {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func globAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}

// Duration is a time.Duration that unmarshals from a Go duration string ("30m").
type Duration time.Duration

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a duration string such as "30m" or "1h30m".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// ActionMode controls how far RunLore may go when acting on the cluster.
// Default (zero value) is read-only.
type ActionMode string

// Action modes, from read-only to autonomous.
const (
	ActionOff     ActionMode = "off"     // read-only (default): no action tools registered
	ActionSuggest ActionMode = "suggest" // propose a command/PR; never execute
	ActionApprove ActionMode = "approve" // execute only after explicit human approval
	ActionAuto    ActionMode = "auto"    // EXPERIMENTAL/frozen (FEAT-1): execute in-envelope, no click — not for prod
)

// ActionPolicy gates cluster-mutating actions — the upper rungs of the autonomy
// ladder. v1 ships ActionOff; the type exists so active tools can be added later
// behind a gate without re-architecting (see docs/design.md §9, "Act").
type ActionPolicy struct {
	Mode             ActionMode  `yaml:"mode"`               // off | suggest | approve | auto (experimental, frozen)
	Allow            ActionAllow `yaml:"allow"`              // envelope, enforced even in approve/auto
	RequireApproval  bool        `yaml:"require_approval"`   // force a human click for gated actions
	ApprovalTokenEnv string      `yaml:"approval_token_env"` // env var with a shared secret for the approval endpoints
	AuditLogPath     string      `yaml:"audit_log_path"`     // append-only, hash-chained action audit log (required for auto)
	Auto             AutoPolicy  `yaml:"auto"`               // rung-3 unattended-execution safety controls
}

// AutoPolicy bounds unattended execution (mode "auto"). Even within these, auto
// only ever runs REVERSIBLE actions, and every decision is audited + delivered.
type AutoPolicy struct {
	DryRun        bool     `yaml:"dry_run"`        // log "would execute" without executing
	MinConfidence float64  `yaml:"min_confidence"` // only auto-execute when the investigation is at least this confident
	MaxPerWindow  int      `yaml:"max_per_window"` // rate limit; 0 = unlimited (not recommended)
	Window        Duration `yaml:"window"`         // rate-limit window (default 1h)
}

// ActionAllow bounds what may be acted on, even in approve/auto modes.
// Irreversibility is the trip-wire for mandatory human approval.
type ActionAllow struct {
	ReversibleOnly      bool     `yaml:"reversible_only"`      // never auto-apply irreversible actions
	Namespaces          []string `yaml:"namespaces"`           // allowlist of target namespaces; empty = no executable target permitted
	ProtectedNamespaces []string `yaml:"protected_namespaces"` // never an action target (added to the built-ins flux-system, kube-system)
	MaxBlastRadius      int      `yaml:"max_blast_radius"`     // cap on affected workloads
	Kinds               []string `yaml:"kinds"`                // resource kinds that may be acted on
}

// Enabled reports whether any cluster-mutating action is permitted.
func (a ActionPolicy) Enabled() bool {
	return a.Mode != "" && a.Mode != ActionOff
}

// isPrivateHost reports whether host is a loopback / in-cluster / private endpoint
// where sending a key over plain http is acceptable. Pure — no DNS — so config
// validation stays deterministic and network-free. IP literals are classified by
// range; hostnames by well-known private forms. Anything else is treated as public.
func isPrivateHost(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" {
		return true
	}
	if !strings.Contains(h, ".") {
		return true // single-label in-cluster service name, e.g. "vllm"
	}
	for _, suf := range []string{".local", ".internal", ".svc", ".cluster.local"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

// checkSecureKeyEndpoint rejects a base_url that would send an API key in cleartext.
// A key is "present" when apiKeyEnv is non-empty; an empty base_url uses the provider's
// built-in (https) default and is always fine. http is allowed only to a private host.
func checkSecureKeyEndpoint(urlField, keyField, baseURL, apiKeyEnv string) error {
	if apiKeyEnv == "" || baseURL == "" {
		return nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL (%s is set): %w", urlField, keyField, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isPrivateHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s is %q with an API key (%s set) on a public host — the key would be sent in cleartext; use https or a loopback/in-cluster endpoint", urlField, baseURL, keyField)
	default:
		return fmt.Errorf("%s must be an http(s) URL when %s is set, got scheme %q", urlField, keyField, u.Scheme)
	}
}

// effortLevels is the per-provider vocabulary of the opt-in model.effort knob.
// Unknown provider names use the "openai" set (NewModelClient routes them to the
// OpenAI-compatible client). Gemini is deliberately absent: its thinkingConfig has
// replay semantics (thought signatures) the provider-agnostic message history
// can't carry, so effort is rejected there rather than half-supported.
var effortLevels = map[string]map[string]bool{
	"anthropic": {"low": true, "medium": true, "high": true, "max": true},
	"openai":    {"minimal": true, "low": true, "medium": true, "high": true},
}

// ValidateEffort checks an effective (provider, effort) pair against the
// per-provider effort vocabulary, so callers that build a ModelProvider outside
// config.Load (e.g. the eval model-comparison runner) share one source of truth.
// field names the setting in the returned error. Empty effort is always valid.
func ValidateEffort(field, provider, effort string) error {
	return validateEffort(field, provider, effort)
}

// validateEffort checks an effective (provider, effort) pair against the
// per-provider vocabulary. Empty effort is always valid (the knob is opt-in and
// omitted from requests); an empty provider defaults to the OpenAI-compatible
// wire protocol, mirroring NewModelClient.
func validateEffort(field, provider, effort string) error {
	if effort == "" {
		return nil
	}
	if provider == "gemini" {
		return fmt.Errorf("%s: effort is not supported for provider gemini yet", field)
	}
	levels, ok := effortLevels[provider]
	if !ok {
		levels = effortLevels["openai"] // "", vllm, ollama, … → OpenAI-compatible client
	}
	if !levels[effort] {
		valid := make([]string, 0, len(levels))
		for l := range levels {
			valid = append(valid, l)
		}
		slices.Sort(valid)
		return fmt.Errorf("%s: %q is not a valid effort for this provider (valid: %s, or empty to omit)",
			field, effort, strings.Join(valid, "|"))
	}
	return nil
}

// validateThinking checks an effective (provider, thinking) pair. Empty thinking is
// always valid (the knob is opt-in and omitted from requests). Only "adaptive" is a
// valid mode, and only for provider anthropic — the thinking-block replay contract
// (signed blocks carried verbatim across the tool loop) is Anthropic-specific, so any
// other provider is rejected with a clear error rather than silently ignored.
func validateThinking(field, provider, thinking string) error {
	if thinking == "" {
		return nil
	}
	if provider != "anthropic" {
		p := provider
		if p == "" {
			p = "openai"
		}
		return fmt.Errorf("%s: thinking is only supported for provider anthropic (got %q)", field, p)
	}
	if thinking != "adaptive" {
		return fmt.Errorf("%s: %q is not a valid thinking mode (valid: adaptive, or empty to omit)", field, thinking)
	}
	return nil
}

// validatePricing rejects a negative token rate (which would produce a negative
// cost). A nil *Pricing (unconfigured) is valid.
func validatePricing(field string, p *Pricing) error {
	if p == nil {
		return nil
	}
	if p.InputUSDPerMTok < 0 || p.OutputUSDPerMTok < 0 || p.CachedInputUSDPerMTok < 0 {
		return fmt.Errorf("%s: rates must be >= 0 (input=%g output=%g cached_input=%g)",
			field, p.InputUSDPerMTok, p.OutputUSDPerMTok, p.CachedInputUSDPerMTok)
	}
	return nil
}

// Validate enforces cross-field invariants after loading — fail-closed defaults
// for the autonomy ladder: enabling execution requires the controls that bound
// it. Returns an error that should abort startup.
func (c *Config) Validate() error {
	// Reject a nonsensical output-token cap before it reaches a provider request. 0
	// means "use the default"; a negative value is always a misconfiguration.
	if c.Model.MaxTokens < 0 {
		return fmt.Errorf("model.max_tokens must be >= 0 (0 = use the default), got %d", c.Model.MaxTokens)
	}
	if c.Model.Verify != nil && c.Model.Verify.MaxTokens < 0 {
		return fmt.Errorf("model.verify.max_tokens must be >= 0 (0 = inherit), got %d", c.Model.Verify.MaxTokens)
	}
	// Effort is validated against the provider it will actually be sent to. The
	// verify override resolves its EFFECTIVE provider and effort first (inherit-
	// when-empty, mirroring BuildVerifyModel's or() semantics), so an inherited
	// parent effort that is invalid for the override's provider fails at startup
	// rather than as a per-request 400.
	if err := validateEffort("model.effort", c.Model.Provider, c.Model.Effort); err != nil {
		return err
	}
	// Thinking is validated against the provider it will actually be sent to, mirroring
	// effort (and BuildVerifyModel's or() inherit-when-empty semantics).
	if err := validateThinking("model.thinking", c.Model.Provider, c.Model.Thinking); err != nil {
		return err
	}
	if v := c.Model.Verify; v != nil {
		prov := v.Provider
		if prov == "" {
			prov = c.Model.Provider
		}
		eff := v.Effort
		if eff == "" {
			eff = c.Model.Effort
		}
		if err := validateEffort("model.verify.effort (or inherited model.effort)", prov, eff); err != nil {
			return err
		}
		think := v.Thinking
		if think == "" {
			think = c.Model.Thinking
		}
		if err := validateThinking("model.verify.thinking (or inherited model.thinking)", prov, think); err != nil {
			return err
		}
	}
	// Interim progress updates: a non-positive cadence while enabled is a
	// misconfiguration (applyDefaults fills an unset 0 with 5, so only an explicit
	// negative reaches here). Validate fail-loud rather than silently never pinging.
	if c.Investigation.ProgressUpdates.Enabled && c.Investigation.ProgressUpdates.EverySteps <= 0 {
		return fmt.Errorf("investigation.progress_updates.every_steps must be > 0 when enabled, got %d", c.Investigation.ProgressUpdates.EverySteps)
	}
	// gitops.mirror.max caps the persistent what_changed mirror count; 0 means the
	// applyDefaults value (10). A negative cap is always a misconfiguration.
	if c.GitOps.Mirror.Max < 0 {
		return fmt.Errorf("gitops.mirror.max must be >= 0 (0 = use the default 10), got %d", c.GitOps.Mirror.Max)
	}
	// Pricing rates must be non-negative (a negative rate would report a negative
	// cost). Cover the main model and the verify override (which carries its own).
	if err := validatePricing("model.pricing", c.Model.Pricing); err != nil {
		return err
	}
	if v := c.Model.Verify; v != nil {
		if err := validatePricing("model.verify.pricing", v.Pricing); err != nil {
			return err
		}
	}
	// Reject a negative rate-limit budget: applyDefaults fills an unset nil with 30,
	// so only an explicit negative reaches here — fail loud rather than silently
	// treating it as unlimited.
	if mpw := c.Investigation.RateLimit.MaxPerWindow; mpw != nil && *mpw < 0 {
		return fmt.Errorf("investigation.rate_limit.max_per_window must be >= 0 (0 = unlimited), got %d", *mpw)
	}
	// Reject a negative per-tool timeout: time.ParseDuration accepts negative values
	// which silently disable the feature (fails the > 0 guard in runTool) rather than
	// setting the intended timeout. 0 means "use the default (60s)".
	if c.Investigation.ToolTimeout.Std() < 0 {
		return fmt.Errorf("investigation.tool_timeout must be >= 0 (0 = use the default 60s), got %v", c.Investigation.ToolTimeout.Std())
	}
	// Reject an unknown compaction mode at startup rather than silently defaulting a
	// typo to lossy elision. Empty means the default (elide).
	switch c.Investigation.Compaction {
	case "", "elide", "summarize":
	default:
		return fmt.Errorf("investigation.compaction %q is invalid (want elide|summarize; empty = elide)", c.Investigation.Compaction)
	}
	// Reject a cleartext API key over a public endpoint (the key would be sent in the
	// clear, and is the enabler for a redirect-based key leak). Cover the main model,
	// a verify override that targets its own endpoint, and embeddings. Loopback /
	// in-cluster hosts are exempt.
	if err := checkSecureKeyEndpoint("model.base_url", "model.api_key_env", c.Model.BaseURL, c.Model.APIKeyEnv); err != nil {
		return err
	}
	if v := c.Model.Verify; v != nil {
		// Resolve the effective endpoint and key mirroring BuildVerifyModel's or() semantics:
		// use the override value if set, else fall back to the parent. This catches the case
		// where a verify override supplies its own key but inherits an insecure parent base_url.
		base := v.BaseURL
		if base == "" {
			base = c.Model.BaseURL
		}
		key := v.APIKeyEnv
		if key == "" {
			key = c.Model.APIKeyEnv
		}
		if err := checkSecureKeyEndpoint("model.verify.base_url", "model.verify.api_key_env (or inherited model.api_key_env)", base, key); err != nil {
			return err
		}
	}
	if e := c.Model.Embeddings; e != nil {
		if err := checkSecureKeyEndpoint("model.embeddings.base_url", "model.embeddings.api_key_env", e.BaseURL, e.APIKeyEnv); err != nil {
			return err
		}
	}
	seenMCP := map[string]bool{}
	for i, s := range c.MCP.Servers {
		if s.Name == "" || s.URL == "" {
			return fmt.Errorf("mcp.servers[%d]: name and url are required", i)
		}
		if strings.Contains(s.Name, "__") || strings.ContainsAny(s.Name, " \t") {
			return fmt.Errorf("mcp.servers[%d]: name %q must not contain '__' or whitespace", i, s.Name)
		}
		if seenMCP[s.Name] {
			return fmt.Errorf("mcp.servers[%d]: duplicate server name %q", i, s.Name)
		}
		seenMCP[s.Name] = true
		if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			scheme := ""
			if err == nil {
				scheme = u.Scheme
			}
			return fmt.Errorf("mcp.servers[%s].url: scheme must be http or https, got %q", s.Name, scheme)
		}
		if err := checkSecureKeyEndpoint("mcp.servers["+s.Name+"].url", "mcp.servers["+s.Name+"].token_env", s.URL, s.TokenEnv); err != nil {
			return err
		}
	}
	// Curation verdict gate: reject an unknown verdict in forge.skip_verdicts at
	// startup rather than silently ignoring a typo (which would leave benign findings
	// drafting PRs into the review queue). Empty is valid — every verdict is eligible.
	for i, v := range c.Forge.SkipVerdicts {
		if !providers.ValidVerdict(providers.Verdict(v)) {
			return fmt.Errorf("forge.skip_verdicts[%d]: unknown verdict %q (want no_action|action_suggested|action_required|inconclusive)", i, v)
		}
	}
	// The recurrence cooldown reads the outcome ledger's trigger index; without a
	// ledger it would silently never suppress — fail loud instead. A negative
	// duration is always a misconfiguration (mirrors tool_timeout).
	if d := c.Investigation.RecurrenceCooldown.Std(); d < 0 {
		return fmt.Errorf("investigation.recurrence_cooldown must be >= 0 (0 = off), got %v", d)
	} else if d > 0 && c.Outcome.LedgerPath == "" {
		return fmt.Errorf("investigation.recurrence_cooldown requires outcome.ledger_path (the suppression gate reads the ledger's per-trigger index)")
	}
	// Feedback buttons are click-driven: enabling them without the pieces a click
	// needs would either accept unsigned requests (never) or render buttons whose
	// clicks silently vanish (a lie to the on-call). Fail loud at startup instead.
	if c.Notify.Slack.FeedbackButtons {
		if c.Notify.Slack.SigningSecretEnv == "" {
			return fmt.Errorf("notify.slack.feedback_buttons requires notify.slack.signing_secret_env: clicks arrive on the exposed POST /slack/interactions endpoint and must be signature-verified")
		}
		if c.Outcome.LedgerPath == "" {
			return fmt.Errorf("notify.slack.feedback_buttons requires outcome.ledger_path: ratings are recorded in the outcome ledger")
		}
	}
	// Same fail-loud contract for the Matrix reaction listener: without the
	// notifier fields it would sync nothing, without the ledger it would record
	// nowhere — both silent lies to whoever enabled the option.
	if c.Notify.Matrix.FeedbackReactions {
		m := c.Notify.Matrix
		if m.Homeserver == "" || m.RoomID == "" || m.AccessTokenEnv == "" {
			return fmt.Errorf("notify.matrix.feedback_reactions requires homeserver, room_id and access_token_env (the reaction listener long-polls the configured room)")
		}
		if c.Outcome.LedgerPath == "" {
			return fmt.Errorf("notify.matrix.feedback_reactions requires outcome.ledger_path: ratings are recorded in the outcome ledger")
		}
	}
	// Instant-recall reranker (opt-in): its knobs are only meaningful when enabled.
	// applyDefaults fills unset (0) values, so only an explicitly out-of-range setting
	// reaches here — fail loud rather than silently gating on a nonsensical threshold.
	// A threshold in (0,1] is a calibrated PROBABILITY (the reranker returns 0.0–1.0);
	// a value >1 or <=0 could never fire (or always fire), defeating the gate.
	if c.Catalog.InstantRecall.RerankEnabled() {
		ir := c.Catalog.InstantRecall
		if ir.RerankThreshold <= 0 || ir.RerankThreshold > 1 {
			return fmt.Errorf("catalog.instant_recall.rerank_threshold must be in (0,1] (a calibrated match confidence), got %g", ir.RerankThreshold)
		}
		if ir.RerankK < 1 {
			return fmt.Errorf("catalog.instant_recall.rerank_k must be >= 1 (candidates to rank), got %d", ir.RerankK)
		}
		if ir.RerankMinScore < 0 {
			return fmt.Errorf("catalog.instant_recall.rerank_min_score must be >= 0 (retrieval-score cost floor), got %g", ir.RerankMinScore)
		}
	}
	switch c.Actions.Mode {
	case "", ActionOff, ActionSuggest:
		return nil // read-only-ish: nothing to execute
	case ActionApprove, ActionAuto:
		// Both executing rungs require the control/kill-switch token (fail closed).
		if c.Actions.ApprovalTokenEnv == "" {
			return fmt.Errorf("actions.mode=%s requires actions.approval_token_env (control/kill-switch endpoints fail closed without it)", c.Actions.Mode)
		}
		// Both executing rungs mutate the cluster, so both must be audited: the hash
		// chain is verified fail-closed on open (see internal/app.BuildAuditor). Without
		// an audit_log_path approve would silently fall back to a Nop auditor — no chain,
		// no verify, no fail-closed — yet it still executes cluster mutations.
		if c.Actions.AuditLogPath == "" {
			return fmt.Errorf("actions.mode=%s requires actions.audit_log_path (an executing rung must be audited)", c.Actions.Mode)
		}
		if c.Actions.Mode == ActionApprove {
			return nil
		}
		// auto-only: unattended execution additionally needs an authenticated webhook
		// and bounded gates.
		if c.Server.WebhookTokenEnv == "" {
			return fmt.Errorf("actions.mode=auto requires server.webhook_token_env (the alert webhook must be authenticated)")
		}
		if c.Actions.Auto.MinConfidence <= 0 {
			return fmt.Errorf("actions.mode=auto requires actions.auto.min_confidence > 0")
		}
		if c.Actions.Auto.MaxPerWindow <= 0 {
			return fmt.Errorf("actions.mode=auto requires actions.auto.max_per_window > 0 (unbounded auto-execution is unsafe)")
		}
		if len(c.Actions.Allow.Namespaces) == 0 {
			return fmt.Errorf("actions.mode=auto requires actions.allow.namespaces (target-namespace allowlist)")
		}
		return nil
	default:
		return fmt.Errorf("unknown actions.mode %q (want off|suggest|approve|auto)", c.Actions.Mode)
	}
}

// Curate configures the Phase-2 backlog groomer (lore curate).
type Curate struct {
	StaleAfter          Duration `yaml:"stale_after"`          // close unprotected KB PRs idle longer than this; 0 disables (default 720h)
	RecurrenceThreshold int      `yaml:"recurrence_threshold"` // open a knowledge-gap issue after this many unresolved occurrences of a pattern; 0 ⇒ default 3
}

// Forge holds git-forge authentication and the curation target repo.
type Forge struct {
	GitHubApp     GitHubApp `yaml:"github_app"`
	KBRepo        string    `yaml:"kb_repo"`        // "owner/name" — the catalog repo for curation
	BaseBranch    string    `yaml:"base_branch"`    // PR target branch (default "main")
	GitHubAPIURL  string    `yaml:"github_api_url"` // override for GHES/tests (default https://api.github.com)
	DupScore      float64   `yaml:"dup_score"`      // file-time catalog BM25 dedup threshold (default 5.0)
	MinConfidence float64   `yaml:"min_confidence"` // file-time quality gate: min overall confidence (default 0.75)
	// SkipVerdicts lists investigation verdicts that must NOT draft a KB PR — the
	// finding is still delivered to chat, but no repo artifact is created. Values are
	// validated against the verdict enum (no_action|action_suggested|action_required|
	// inconclusive). Empty (default) draws no distinction: every verdict is eligible,
	// preserving pre-gate behaviour. Recommended production value: ["no_action"].
	SkipVerdicts []string `yaml:"skip_verdicts"`
}

// GitHubApp holds GitHub App credentials. The private key mints 1-hour
// installation tokens (no long-lived PAT); it is referenced from a Secret, never
// inlined. Required permissions: contents:read (diff), and on the KB repo
// issues:write + pull_requests:write + contents:write (curation).
type GitHubApp struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyRef  string `yaml:"private_key_ref"` // Secret name/key (e.g. via External Secrets)
	PrivateKeyEnv  string `yaml:"private_key_env"` // v1: env var holding the PEM private key
}
