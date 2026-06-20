// Package providers defines the pluggable backend contracts RunLore is built on.
//
// Every backend the agent touches is an interface, so the investigation loop and
// the knowledge entries are written against engine-agnostic types (notably Change),
// never against Flux/ArgoCD/VictoriaMetrics/Prometheus directly.
//
// Core providers are built-in (direct clients) so the binary is self-contained;
// MCP is the extension layer for additional, optional tools.
//
// This file is the architecture contract. Method bodies live in sub-packages.
package providers

import (
	"context"
	"time"
)

// ---- engine-agnostic "what changed" model ------------------------------------

// Engine identifies a GitOps engine.
type Engine string

// Supported GitOps engines.
const (
	EngineFlux   Engine = "flux"
	EngineArgoCD Engine = "argocd"
)

// ChangeType classifies a detected change.
type ChangeType string

// Change types detected on the cluster.
const (
	ChangeSync      ChangeType = "sync"       // a reconcile/sync applied a new revision
	ChangeChartBump ChangeType = "chart-bump" // a Helm chart version changed
	ChangeImageBump ChangeType = "image-bump" // a container image tag changed
	ChangeDrift     ChangeType = "drift"      // observed state diverged from desired
)

// Workload identifies a Kubernetes object.
type Workload struct {
	Kind      string
	Name      string
	Namespace string
}

// SourceRef points at the Git source + path backing a change.
type SourceRef struct {
	RepoURL string
	Path    string
}

// Change is the engine-agnostic unit of "what changed". Flux and ArgoCD both
// reduce to: revision history + a Git diff between two deployed revisions.
type Change struct {
	Workload    Workload
	Engine      Engine
	Type        ChangeType
	When        time.Time
	FromRev     string
	ToRev       string
	Source      SourceRef
	ManagedBy   string     // the Kustomization/Application/ResourceSet that owns it
	BlastRadius []Workload // resources affected by the change
	DiffRef     string     // opaque handle resolvable via GitOpsProvider.Diff
}

// Diff is a unified diff scoped to a workload's path.
type Diff struct {
	Files []FileDiff
}

// FileDiff is the unified-diff patch for a single file.
type FileDiff struct {
	Path  string
	Patch string
}

// FailureEvent is a normalized GitOps failure signal used as a React trigger.
type FailureEvent struct {
	Workload Workload
	Engine   Engine
	Reason   string
	Message  string
	When     time.Time
}

// Action describes a cluster-mutating operation a provider could expose (e.g. a
// rollback, a reconcile, a scale). In v1 no actions are registered — RunLore is
// read-only. The metadata exists so active tools can be added later behind
// config.ActionPolicy (the autonomy ladder) without re-architecting.
type Action struct {
	Name        string
	Description string
	Target      Workload
	Mutating    bool // true for any cluster write
	Reversible  bool // a rollback is reversible; a delete may not be
	BlastRadius int  // number of workloads affected
}

// TimeWindow is a [Start, End] interval.
type TimeWindow struct {
	Start time.Time
	End   time.Time
}

// Selector narrows a query to a subset of workloads.
type Selector struct {
	Namespace string
	Kind      string
	Name      string
}

// ---- provider interfaces -----------------------------------------------------

// GitOpsProvider abstracts Flux/ArgoCD: the "what changed" spine + failure triggers.
type GitOpsProvider interface {
	// Changes returns the ranked change timeline in a window (the spine).
	Changes(ctx context.Context, w TimeWindow, sel Selector) ([]Change, error)
	// Diff returns the actual Git diff for a change, scoped to its source path.
	Diff(ctx context.Context, c Change) (Diff, error)
	// WatchFailures emits normalized GitOps failure events as a React trigger.
	WatchFailures(ctx context.Context) (<-chan FailureEvent, error)
}

// MetricsProvider abstracts VictoriaMetrics/Prometheus (both speak PromQL).
type MetricsProvider interface {
	Query(ctx context.Context, promql string, at time.Time) (Samples, error)
	QueryRange(ctx context.Context, promql string, w TimeWindow, step time.Duration) (Matrix, error)
}

// LogsProvider abstracts the logs backend (VictoriaLogs now; Loki etc. later).
type LogsProvider interface {
	Query(ctx context.Context, query string, w TimeWindow) (LogResult, error)
}

// NetworkProvider abstracts network observability (Hubble now).
type NetworkProvider interface {
	Drops(ctx context.Context, sel Selector, w TimeWindow) (LogResult, error)
}

// CloudProvider abstracts cloud-side context for an incident (managed-DB status,
// instance/node health, load-balancer/target health, recent cloud events).
//
// Phase 2+: implemented with native cloud SDKs (aws-sdk-go-v2, google-cloud-go,
// azure-sdk-for-go) and in-cluster identity (IRSA/Pod Identity, GKE/Azure Workload
// Identity) — not Steampipe and not a bundled cloud CLI (both add heavy deps and
// break the single-binary property). Steampipe / cloud MCP servers stay available
// as optional MCP extensions. No cloud provider ships in v1.
type CloudProvider interface {
	// Context returns cloud-side signals relevant to a workload/incident window.
	Context(ctx context.Context, sel Selector, w TimeWindow) (LogResult, error)
}

// ModelProvider abstracts the LLM (Anthropic | OpenAI-compatible: vLLM/Ollama).
type ModelProvider interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// Notifier delivers an investigation to a destination. Pluggable: Slack and
// Matrix first; PagerDuty and incident.io later. Several notifiers can be wired
// at once (e.g. chat for humans + an incident platform for the on-call record).
type Notifier interface {
	Deliver(ctx context.Context, inv Investigation) error
}

// IssueProvider opens/updates issues & PRs for confidence-routed curation
// (GitHub now; GitLab later). All writes target the git forge, never the cluster.
//
// GitHub auth is a GitHub App (fine-grained permissions, short-lived 1h
// installation tokens) — the same App also provides the git access used by the
// what-changed spine. GitLab falls back to a scoped access token.
type IssueProvider interface {
	OpenIssue(ctx context.Context, inv Investigation) (Ref, error)
	OpenPR(ctx context.Context, entry KBEntry) (Ref, error)
}

// ---- payloads ----------------------------------------------------------------

// Sample is one instant metric value with its labels.
type Sample struct {
	Metric map[string]string
	Value  float64
	Time   time.Time
}

// Point is a single (time, value) in a range series.
type Point struct {
	Time  time.Time
	Value float64
}

// Series is a labeled time series (range query).
type Series struct {
	Metric map[string]string
	Points []Point
}

// LogLine is one normalized log entry (engine-agnostic).
type LogLine struct {
	Time    time.Time
	Message string
	Fields  map[string]string
}

// Samples is an instant-vector result.
type Samples []Sample

// Matrix is a range-query result.
type Matrix []Series

// LogResult is a logs/network query result.
type LogResult []LogLine

// Investigation is the structured output contract of an investigation.
type Investigation struct {
	Title      string
	RootCauses []Hypothesis
	Changes    []Change
	Unresolved []string // honest: what the agent could not determine
	Confidence float64
}

// Hypothesis is one ranked root-cause candidate with its evidence.
type Hypothesis struct {
	Summary         string
	Confidence      float64
	ChangeRef       string
	Evidence        []string
	SuggestedAction string // reversible-first
	Reversible      bool
}

// KBEntry is an OKF knowledge entry the curator drafts from an investigation.
type KBEntry struct {
	Type        string // e.g. Incident | Playbook | Postmortem
	Title       string
	Description string
	Resource    string
	Tags        []string
	Body        string // markdown
}

// Ref is a URL handle to a created issue or PR.
type Ref struct{ URL string }

// CompletionRequest / CompletionResponse are the minimal LLM exchange types.
type CompletionRequest struct {
	System   string
	Messages []Message
	Tools    []ToolSpec
}

// Message is one turn in an LLM exchange.
type Message struct {
	Role       string // system | user | assistant | tool
	Content    string
	ToolCalls  []ToolCall // assistant turn requesting tools
	ToolCallID string     // tool turn: the call this answers
}

// ToolSpec describes a tool offered to the model.
type ToolSpec struct {
	Name        string
	Description string
	Schema      string // JSON Schema
}

// CompletionResponse is the model's reply (text and/or tool calls).
type CompletionResponse struct {
	Text      string
	ToolCalls []ToolCall
}

// ToolCall is a model request to invoke a tool.
type ToolCall struct {
	ID   string
	Name string
	Args string // JSON
}
