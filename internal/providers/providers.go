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

// Supported GitOps engines. EngineAWS marks a non-GitOps change from the cloud
// control plane (CloudTrail), so cloud events join the same "what changed" model.
const (
	EngineFlux   Engine = "flux"
	EngineArgoCD Engine = "argocd"
	EngineAWS    Engine = "aws"
)

// ChangeType classifies a detected change.
type ChangeType string

// Change types detected on the cluster.
const (
	ChangeSync      ChangeType = "sync"       // a reconcile/sync applied a new revision
	ChangeChartBump ChangeType = "chart-bump" // a Helm chart version changed
	ChangeImageBump ChangeType = "image-bump" // a container image tag changed
	ChangeDrift     ChangeType = "drift"      // observed state diverged from desired
	ChangeCloudAPI  ChangeType = "cloud-api"  // a mutating cloud control-plane call (CloudTrail)
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

// Action describes a remediation the agent can propose and (at the upper autonomy
// rungs, after approval) execute. Op names a concrete, reversible operation an
// Executor can run; an empty Op is a suggestion only.
type Action struct {
	Name        string
	Description string
	Op          string // executable operation: suspend | resume | reconcile (empty = suggestion only)
	Target      Workload
	Mutating    bool   // true for any cluster write
	Reversible  bool   // a rollback/suspend is reversible; a delete may not be
	BlastRadius int    // number of workloads affected
	ApprovalID  string // runtime: set when registered for approval; drives Slack approve/reject buttons
}

// OpSafety is the server-derived safety metadata for an executable action op.
type OpSafety struct {
	Reversible bool
	Blast      int
}

// Ops is the canonical registry of executable remediation operations and their
// server-authoritative safety metadata. The action gate (internal/action) derives
// reversibility/blast from this — never from model output — and the executor
// (internal/executor/flux) runs only ops listed here. One entry per op is the
// single source of truth that keeps the gate and the executor from drifting.
var Ops = map[string]OpSafety{
	"suspend":   {Reversible: true, Blast: 1},
	"resume":    {Reversible: true, Blast: 1},
	"reconcile": {Reversible: true, Blast: 1},
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

// ResourceStatus is a read-only snapshot of a GitOps/Kubernetes object's health,
// used to investigate WHY a resource is failing (not just that it is).
type ResourceStatus struct {
	Workload Workload
	NotFound bool              // the object does not exist (often the cascade root)
	Ready    string            // Ready condition status: "True"/"False"/"Unknown"/""
	Reason   string            // Ready condition reason
	Message  string            // Ready condition message
	Refs     map[string]string // key spec references (e.g. sourceRef, dependsOn)
	Events   []string          // recent Event lines (type/reason/message)
}

// DepNode is a node in a GitOps dependency tree (dependsOn + sourceRef edges),
// used to find the ROOT failing resource behind a dependency cascade.
type DepNode struct {
	Workload Workload
	NotFound bool
	Ready    string // Ready condition status
	Reason   string
	Children []DepNode
}

// GitOpsInspector is optional read-only deep introspection for an investigation:
// a resource's status/refs/events and its dependency tree. Not every engine
// implements it (Flux does); consumers type-assert for it.
type GitOpsInspector interface {
	// ResourceStatus returns conditions, key refs, and recent Events for one object.
	ResourceStatus(ctx context.Context, w Workload) (ResourceStatus, error)
	// DependencyTree walks dependsOn/sourceRef edges to surface the root failure.
	DependencyTree(ctx context.Context, w Workload) (DepNode, error)
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

// LogReader reads recent pod logs from the cluster (read-only), backing the
// controller_logs investigation tool. Implemented with client-go CoreV1 GetLogs.
type LogReader interface {
	// PodLogs returns recent log lines from pods matching labelSelector in
	// namespace, bounded to the last sinceMinutes (0 = no lower bound).
	PodLogs(ctx context.Context, namespace, labelSelector string, sinceMinutes int) (LogResult, error)
}

// PodStatus is a pod's high-level health: phase, ready count, and per-container
// waiting/terminated reasons — the pod-level signals (CreateContainerConfigError,
// ImagePullBackOff, CrashLoopBackOff, …) that never reach logs because the
// container never started.
type PodStatus struct {
	Name    string
	Phase   string
	Ready   string   // "1/2"
	Healthy bool     // Running/Succeeded with all containers ready and no waiting reasons
	Reasons []string // e.g. "registry: CreateContainerConfigError: couldn't find key username in Secret …"
}

// KubeEvent is a normalized Kubernetes Event — surfaces causes that live in the
// event stream, not logs or status (FailedScheduling, FailedMount, …).
type KubeEvent struct {
	Type    string // Normal | Warning
	Reason  string
	Object  string // Kind/Name
	Message string
	Count   int32
}

// KubeReader reads read-only pod status and Kubernetes Events for incident triage,
// backing the pod_status / kube_events tools. Implemented with client-go CoreV1.
type KubeReader interface {
	// PodStatuses returns pod health in a namespace, optionally narrowed by a label
	// selector (empty = all pods).
	PodStatuses(ctx context.Context, namespace, labelSelector string) ([]PodStatus, error)
	// Events returns recent Events in a namespace; objectName "" = all objects;
	// warnOnly restricts to Warning events.
	Events(ctx context.Context, namespace, objectName string, warnOnly bool) ([]KubeEvent, error)
}

// CloudProvider abstracts read-only cloud-side context for an incident. It adds
// the AWS-layer "what changed" lens (mutating control-plane events) and cloud
// resource health (instances/ASGs/nodegroups) that the in-cluster signals can't see.
//
// Implemented with native cloud SDKs (aws-sdk-go-v2) and in-cluster identity
// (EKS Pod Identity / IRSA) — not Steampipe and not a bundled CLI (both break the
// single-binary property). Steampipe / cloud MCP servers stay optional MCP
// extensions. Cloud is opt-in (config.cloud.provider).
type CloudProvider interface {
	// CloudChanges returns recent mutating cloud control-plane events (AWS:
	// CloudTrail) in the window, normalized to the engine-agnostic Change model so
	// they join the same "what changed" timeline as GitOps diffs.
	CloudChanges(ctx context.Context, sel Selector, w TimeWindow) ([]Change, error)
	// ResourceHealth returns cloud-side state/health for resources backing the
	// selector (EC2 instance status, ASG capacity/activities, EKS nodegroup), as
	// normalized lines for the model.
	ResourceHealth(ctx context.Context, sel Selector, w TimeWindow) (LogResult, error)
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

// CuratedIssue is a minimal view of a curated KB issue, used by the re-investigate
// loop to re-run and post results back.
type CuratedIssue struct {
	Number int
	Title  string
	Body   string
	Labels []string
}

// ReinvestForge lists curated issues flagged for re-investigation and posts results
// back to them. RunLore polls the forge (outbound) — it receives no inbound GitHub
// webhooks — so a human checking the "reinvestigate" label triggers a fresh run.
type ReinvestForge interface {
	// ListIssuesByLabel returns open issues carrying the given label.
	ListIssuesByLabel(ctx context.Context, label string) ([]CuratedIssue, error)
	// Comment posts a comment on an issue.
	Comment(ctx context.Context, number int, body string) error
	// ReplaceLabel removes one label and adds another (lifecycle transition);
	// either side may be empty to only add or only remove.
	ReplaceLabel(ctx context.Context, number int, remove, add string) error
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
	Actions    []Action // proposed remediations (autonomy ladder; never executed at rung "suggest")
	CuratedURL string   // runtime: KB issue/PR the curator opened, linked in delivery (set after curation)
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
