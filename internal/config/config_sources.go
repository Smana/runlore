// SPDX-License-Identifier: Apache-2.0

package config

import (
	"cmp"
	"path"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

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

// Logs backend providers for config.logs.provider. Unlike the metrics backends
// (which share the Prometheus HTTP API and differ only in dialect), these
// backends have entirely different query APIs, so this selects the CLIENT, not
// a flavor of one. Empty ⇒ auto-detect at startup (internal/logs.Detect: Loki
// answers /loki/api/v1/status/buildinfo; Elasticsearch/OpenSearch answer GET /
// with a version document; VictoriaLogs answers neither), failing safe to
// victorialogs — the provider RunLore shipped with — so an unreachable backend
// at startup reproduces today's behaviour exactly. These values MUST equal
// internal/logs.Provider* exactly (see that package's comment) — they are also
// the ids internal/docsguard reflects a docs/integrations page's
// `integration: {kind: logs, id: ...}` claim against.
const (
	LogsProviderVictoriaLogs  = "victorialogs"  // LogsQL over /select/logsql/* (shipped default)
	LogsProviderLoki          = "loki"          // LogQL over /loki/api/v1/*
	LogsProviderElasticsearch = "elasticsearch" // classic _search DSL (Lucene query_string) over /<index>/_search
	LogsProviderOpenSearch    = "opensearch"    // same _search DSL as Elasticsearch — a fork, not a different API
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

	// Provider optionally pins the logs backend implementation instead of
	// auto-detecting it: "loki", "elasticsearch", "opensearch", or
	// "victorialogs" (see LogsProvider*). Empty ⇒ probe once at startup,
	// failing safe to victorialogs.
	Provider string `yaml:"provider"`

	// Index is the Elasticsearch/OpenSearch index pattern to search (e.g.
	// "logs-*"). Ignored by victorialogs/loki. Empty ⇒ "logs-*" (the
	// Filebeat/ECS convention).
	Index string `yaml:"index"`

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
	// Elasticsearch/OpenSearch have no parser-pipe concept — ignored for them.
	UnpackPipe string `yaml:"unpack_pipe"`

	// TimestampField is the Elasticsearch/OpenSearch field the range filter,
	// sort, and date_histogram aggregation key on. Ignored by victorialogs/loki
	// (both have a built-in time concept, no configurable field name). Default:
	// @timestamp (the ECS convention).
	TimestampField string `yaml:"timestamp_field"`

	// MessageField is the Elasticsearch/OpenSearch field TopMessages
	// aggregates over. Ignored by victorialogs/loki. Default: message (the ECS
	// convention) — which ships `text`-only with no keyword sub-field by
	// default, so TopMessages transparently falls back to client-side
	// aggregation unless this is pointed at an aggregatable field (e.g. a
	// `message.keyword` multi-field your index template adds).
	MessageField string `yaml:"message_field"`
}

// Default log-field convention: the VictoriaLogs + vector kubernetes-metadata
// layout RunLore shipped with. These values MUST NOT change — an unset config
// has to reproduce the previous hardcoded behaviour exactly.
var defaultLogFields = LogFields{
	ContainerField: "kubernetes.container_name",
	NamespaceField: "kubernetes.pod_namespace",
	PodField:       "kubernetes.pod_name",
	LevelField:     "log.level",
	UnpackPipe:     "unpack_json",
}

// Default log-field convention for Loki: the promtail/Grafana-Alloy stream-label
// layout plus Loki 3.x's auto-detected severity (detected_level is structured
// metadata, filterable WITHOUT a parser stage — hence no default unpack pipe, so
// UnpackPipe stays as-set here, empty meaning no parser stage).
// An operator on Loki 2.x (no detected_level) overrides logs.fields, e.g.
// {level_field: level, unpack_pipe: logfmt}.
var defaultLokiFields = LogFields{
	ContainerField: "container",
	NamespaceField: "namespace",
	PodField:       "pod",
	LevelField:     "detected_level",
}

// Default log-field convention for Elasticsearch/OpenSearch: the ECS
// (Elastic Common Schema) layout Filebeat and most ECS-compliant collectors
// ship. Both distributions get the SAME defaults — they are the same wire
// format under a fork, not two different conventions. UnpackPipe has no default
// (and stays unused): these backends have no parser-pipe concept.
var defaultECSFields = LogFields{
	ContainerField: "kubernetes.container.name",
	NamespaceField: "kubernetes.namespace",
	PodField:       "kubernetes.pod.name",
	LevelField:     "log.level",
	TimestampField: "@timestamp",
	MessageField:   "message",
}

// Resolved returns the field convention with every unset value filled from the
// shipped defaults, so callers can use the result without repeating the fallbacks.
// UnpackPipe is deliberately allowed to be explicitly empty (already-flat logs), so
// only an unset (zero) LogFields as a whole restores the default pipe — an operator
// who sets any field but leaves unpack_pipe empty still gets the default pipe unless
// they had a fully-zero struct. To keep the "any override" case simple, an empty
// UnpackPipe here falls back to the default; disabling it is out of scope for v1.
func (f LogFields) Resolved() LogFields {
	return f.withDefaults(defaultLogFields)
}

// ResolvedFor resolves the field convention for a specific logs provider: Loki
// gets Loki-appropriate defaults, Elasticsearch/OpenSearch get ECS defaults;
// anything else (victorialogs, "") keeps Resolved()'s shipped VictoriaLogs
// behaviour. Explicitly-set fields always win.
func (f LogFields) ResolvedFor(provider string) LogFields {
	switch provider {
	case LogsProviderLoki:
		return f.withDefaults(defaultLokiFields)
	case LogsProviderElasticsearch, LogsProviderOpenSearch:
		return f.withDefaults(defaultECSFields)
	default:
		return f.Resolved()
	}
}

// withDefaults fills every unset field of f from d and returns the result, so a
// convention only has to declare the values it defines: a field d leaves empty
// (Loki/ECS have no UnpackPipe, VictoriaLogs no timestamp/message field) is left
// exactly as the operator set it.
func (f LogFields) withDefaults(d LogFields) LogFields {
	f.ContainerField = cmp.Or(f.ContainerField, d.ContainerField)
	f.NamespaceField = cmp.Or(f.NamespaceField, d.NamespaceField)
	f.PodField = cmp.Or(f.PodField, d.PodField)
	f.LevelField = cmp.Or(f.LevelField, d.LevelField)
	f.UnpackPipe = cmp.Or(f.UnpackPipe, d.UnpackPipe)
	f.TimestampField = cmp.Or(f.TimestampField, d.TimestampField)
	f.MessageField = cmp.Or(f.MessageField, d.MessageField)
	return f
}

// Cloud provider identifiers for config.cloud.provider. Cloud context is opt-in;
// an empty provider disables the cloud tools entirely.
const (
	CloudAWS = "aws" // CloudTrail what-changed + EC2/ASG/EKS health; auth is EKS Pod Identity / IRSA
	CloudGCP = "gcp" // Cloud Audit Logs what-changed + GKE/MIG/Compute health; auth is GKE Workload Identity
)

// Cloud configures the cloud context provider. Auth is always in-cluster identity —
// EKS Pod Identity / IRSA on AWS, a GKE Workload Identity direct principal binding on
// GCP — never a static key. Empty Provider disables the cloud tools (default).
//
// The AWS fields are flat for back-compatibility with every deployment that predates
// a second cloud; GCP is nested, matching the per-provider blocks Network already
// uses in this file. New clouds nest.
type Cloud struct {
	Provider    string `yaml:"provider"`     // "" (disabled) | "aws" | "gcp"
	Region      string `yaml:"region"`       // AWS only: e.g. eu-west-3 (default: AWS_REGION / IMDS)
	ClusterName string `yaml:"cluster_name"` // AWS only: EKS cluster name, scopes nodegroup/ASG queries

	GCP GCPCloudCfg `yaml:"gcp"` // when provider=gcp
}

// GCPCloudCfg configures the GCP cloud context provider. EVERY field is optional: on
// GKE all three are resolved from the metadata server, falling back to the cluster's
// own node objects, so `cloud: {provider: gcp}` is a complete configuration.
//
// Set them only to override that resolution — a cluster whose metadata server does
// not expose the cluster attributes, or a deployment reading a project other than the
// one it runs in.
type GCPCloudCfg struct {
	Project     string `yaml:"project"`      // GCP project id (default: metadata server)
	Location    string `yaml:"location"`     // cluster region OR zone (default: metadata server)
	ClusterName string `yaml:"cluster_name"` // GKE cluster name (default: metadata server)
}

// MCP configures outbound connections to external MCP servers whose tools the
// investigation loop may call. Empty Servers disables it (the default — MCP is opt-in).
type MCP struct {
	Servers []MCPServer `yaml:"servers"`

	// RequireAllowlist refuses startup unless EVERY server declares a tools
	// allowlist — deny-by-default for operators who treat remote MCP servers
	// as untrusted. Default false: a listed server exposes all its tools,
	// matching pre-allowlist behavior.
	RequireAllowlist bool `yaml:"require_allowlist"`
}

// MCPServer is one external MCP server reachable over streamable-HTTP.
type MCPServer struct {
	Name     string `yaml:"name"` // identifier; namespaces its tools as name__tool
	Endpoint `yaml:",inline"`

	// Tools is an exact-name allowlist of remote tools to expose to the model
	// (pre-namespacing names, as advertised by tools/list). Empty ⇒ every
	// advertised tool (unless mcp.require_allowlist). Enforced at discovery:
	// a tool outside the list is never registered, so it cannot be called.
	Tools []string `yaml:"tools"`
}

// SourceRepos gates the source_diff investigation tool: an allowlist of
// application/module source repos the loop may clone and diff when a change
// names a version bump (image tag, module ref). Empty (the default) ⇒ the
// tool is not registered — no new required config. Patterns are
// host/org/repo with per-segment globs ("github.com/acme/*"); matching is the
// server-side security boundary (see internal/sourcerepo). Private GitHub
// repos authenticate with the forge GitHub App installation token
// (contents:read); private non-GitHub hosts are not supported yet.
type SourceRepos struct {
	Allow []string `yaml:"allow"`
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
	// ApplyDefaults) is distinguishable from an explicit `debounce: 0` (fire
	// immediately). A plain Duration can't tell the two apart — both are the zero
	// value — which is why `debounce: 0` used to be silently clobbered to 60s.
	Debounce *Duration `yaml:"debounce"`
}

// DebounceWindow is the GitOps-failure debounce window. nil (unset) reads as 0
// here, but ApplyDefaults fills an unset trigger with 60s; an explicit 0
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
	// A pointer, so an unset key (nil ⇒ 60s default, applied in ApplyDefaults) is
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
	// A pointer, so an unset key (nil ⇒ true, applied in ApplyDefaults) is
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
// ApplyDefaults fills an unset trigger with 60s; an explicit 0 means investigate
// immediately on every fire. NOTE: the hold never applies to a critical alert —
// see investigate.Request.IsCritical and source.incidentDebouncer.Hold.
func (t IncidentTrigger) DebounceWindow() time.Duration {
	if t.Debounce == nil {
		return 0
	}
	return t.Debounce.Std()
}

// CancelQueuedOnResolveEnabled reports whether a queued investigation is dropped
// when its alert resolves first. nil (unset) reads as false here, but ApplyDefaults
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
