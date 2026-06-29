// Package config defines RunLore's configuration, including provider wiring and
// the trigger policy that decides which incidents start an investigation.
//
// The trigger policy is what keeps RunLore from firing on every alert: it filters
// incidents by environment, severity, namespace/team/label, and dedups still-firing
// alerts — controlling noise, relevance, and LLM cost.
package config

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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

	Metrics Endpoint `yaml:"metrics"` // PromQL backend (VictoriaMetrics/Prometheus) for query_metrics
	Logs    Endpoint `yaml:"logs"`    // LogsQL backend (VictoriaLogs) for query_logs
	Network Network  `yaml:"network"` // network-flow data source (pluggable, CNI-agnostic); empty Provider disables it
	Cloud   Cloud    `yaml:"cloud"`   // cloud-side context (AWS); empty Provider disables it

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

// Endpoint is a backend base URL; empty disables the corresponding tool.
type Endpoint struct {
	URL string `yaml:"url"`
}

// Cloud configures the cloud context provider. Auth is in-cluster identity (EKS
// Pod Identity / IRSA) via the AWS SDK's default credential chain — no static keys.
// Empty Provider disables the cloud tools (default — cloud is opt-in).
type Cloud struct {
	Provider    string `yaml:"provider"`     // "" (disabled) | "aws"
	Region      string `yaml:"region"`       // e.g. eu-west-3 (default: AWS_REGION / IMDS)
	ClusterName string `yaml:"cluster_name"` // EKS cluster name, scopes nodegroup/ASG queries
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
}

// AWSFlowCfg configures the AWS VPC Flow Logs network provider. Auth is in-cluster
// identity (EKS Pod Identity / IRSA) via the AWS default credential chain.
type AWSFlowCfg struct {
	Region   string `yaml:"region"`    // AWS region (default: AWS_REGION / IMDS)
	LogGroup string `yaml:"log_group"` // CloudWatch Logs group that receives the VPC Flow Logs (required)
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
	MaxToolOutputBytes        int       `yaml:"max_tool_output_bytes"`        // 0 ⇒ unlimited
	MaxTokensPerInvestigation int       `yaml:"max_tokens_per_investigation"` // 0 ⇒ unlimited
	Timeout                   Duration  `yaml:"timeout"`                      // per-investigation deadline; 0 ⇒ default (10m) via applyDefaults
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
	MaxPerWindow int      `yaml:"max_per_window"` // 0 ⇒ unlimited
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
}

// InstantRecall short-circuits the investigation loop when the catalog has a
// high-confidence match for the symptom. Off by default; MinScore is the BM25
// relevance floor (tune for your catalog).
type InstantRecall struct {
	Enabled              bool    `yaml:"enabled"`
	MinScore             float64 `yaml:"min_score"`              // similarity floor for the top hit
	MarginGap            float64 `yaml:"margin_gap"`             // top hit must beat the runner-up by at least this
	SoloFloor            float64 `yaml:"solo_floor"`             // confident bar when there is only one hit (higher than MinScore)
	RequireWorkloadMatch bool    `yaml:"require_workload_match"` // true = exact namespace+workload; false = namespace-level agreement is enough
	OutcomePrior         float64 `yaml:"outcome_prior"`          // Beta prior strength for outcome decay
	OutcomeFloor         float64 `yaml:"outcome_floor"`          // reject a recall when the outcome factor drops below this

	// Hybrid switches recall to fused BM25 + embedding retrieval, gated on COSINE
	// similarity instead of the BM25 score above. Requires model.embeddings to be
	// configured (else recall stays BM25). EXPERIMENTAL — tune the cosine thresholds
	// against the instant-recall eval before relying on it; the defaults are
	// conservative placeholders, not measured values.
	Hybrid          bool    `yaml:"hybrid"`            // enable hybrid (cosine-gated) recall
	HybridMinScore  float64 `yaml:"hybrid_min_score"`  // cosine floor for the top hit (default 0.80)
	HybridMarginGap float64 `yaml:"hybrid_margin_gap"` // cosine margin over the runner-up (default 0.05)
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
	BaseURL   string `yaml:"base_url"`    // OpenAI: required; Anthropic/Gemini: optional (built-in default endpoint)
	Model     string `yaml:"model"`       // model name
	APIKeyEnv string `yaml:"api_key_env"` // env var holding the API key (empty = keyless)
	// Verify optionally routes the adversarial verify pass to a cheaper/faster model;
	// unset fields inherit from the parent above (so `verify: {model: <cheap>}` reuses
	// the same provider/endpoint/key). Absent ⇒ verify runs on the main model.
	Verify *ModelOverride `yaml:"verify"`
	// Embeddings optionally configures an OpenAI-compatible /embeddings endpoint used
	// for hybrid recall (instant_recall.hybrid). Unset ⇒ BM25-only recall.
	Embeddings *Embeddings `yaml:"embeddings"`
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
}

// MatrixNotify configures Matrix delivery.
type MatrixNotify struct {
	Homeserver     string `yaml:"homeserver"`
	RoomID         string `yaml:"room_id"`
	AccessTokenEnv string `yaml:"access_token_env"` // env var holding the access token
}

// GitOps selects the GitOps engine RunLore reads (what-changed + failure watch).
type GitOps struct {
	Engine string `yaml:"engine"` // "flux" (default) | "argocd"
}

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

// Validate enforces cross-field invariants after loading — fail-closed defaults
// for the autonomy ladder: enabling execution requires the controls that bound
// it. Returns an error that should abort startup.
func (c *Config) Validate() error {
	switch c.Actions.Mode {
	case "", ActionOff, ActionSuggest:
		return nil // read-only-ish: nothing to execute
	case ActionApprove, ActionAuto:
		// Both executing rungs require the control/kill-switch token (fail closed).
		if c.Actions.ApprovalTokenEnv == "" {
			return fmt.Errorf("actions.mode=%s requires actions.approval_token_env (control/kill-switch endpoints fail closed without it)", c.Actions.Mode)
		}
		if c.Actions.Mode == ActionApprove {
			return nil
		}
		// auto-only: unattended execution additionally needs audit, an authenticated
		// webhook, and bounded gates.
		if c.Actions.AuditLogPath == "" {
			return fmt.Errorf("actions.mode=auto requires actions.audit_log_path (auto-execution must be audited)")
		}
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
