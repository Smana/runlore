// SPDX-License-Identifier: Apache-2.0

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
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
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

// Ref renders the workload as "namespace/name", or just "namespace" when the
// name is unknown (common for alert-triggered investigations), or "" when the
// namespace is unknown too. It is the canonical form used for structural recall
// matching, curated-entry resources, and outcome-ledger attribution.
func (w Workload) Ref() string {
	if w.Namespace == "" {
		return ""
	}
	if w.Name == "" {
		return w.Namespace
	}
	return w.Namespace + "/" + w.Name
}

// reDeployPod matches a volatile pod-name suffix: a Deployment pod is
// <name>-<rs-hash>-<pod-hash>, e.g. "harbor-registry-59598dbd57-ltkzw". The suffix
// names one ephemeral pod, not the controller family it belongs to.
var reDeployPod = regexp.MustCompile(`-[a-f0-9]{8,10}-[a-z0-9]{5}$`) // <name>-<rs-hash>-<pod-hash>

// NormalizeWorkloadName strips a trailing pod-name hash so a per-pod name reduces
// to its controller family: a Deployment pod (<name>-<rs-hash>-<pod-hash>) and a
// DaemonSet/StatefulSet-revision pod (<name>-<5-char hash containing a digit>)
// both collapse to <name>. Names without such a suffix are returned unchanged, so
// real trailing words (e.g. "redis-cache") are preserved. It is idempotent.
//
// This is the single source of truth for pod-hash normalization. It is shared by
// the curator dedup path (curator.DupFingerprint / IncidentKey — CORE-681, so the
// same incident on a different pod dedupes to one KB entry) AND the instant-recall
// structural gate (investigate.resourceAgrees), so a pod-scoped alert carrying the
// volatile hash still matches the normalized workload stored on a KB entry. Homed
// here — not in curator — because both packages already import providers, which
// owns the Workload type; investigate must not import curator (no cycle).
func NormalizeWorkloadName(name string) string {
	if m := reDeployPod.FindString(name); m != "" {
		return name[:len(name)-len(m)]
	}
	if i := strings.LastIndexByte(name, '-'); i >= 0 {
		suf := name[i+1:]
		if len(suf) == 5 && strings.ContainsAny(suf, "0123456789") && isAlnum(suf) {
			return name[:i]
		}
	}
	return name
}

func isAlnum(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
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
//
// LabelValues is metric/label discovery: it answers "what exists?" so the agent
// never dead-ends on a guessed metric name. A query that matches nothing returns
// an empty result with no hint of the real names a workload exports; LabelValues
// scopes to a matcher + window so it stays cheap on a big TSDB. Metric-name
// discovery uses the label "__name__".
type MetricsProvider interface {
	Query(ctx context.Context, promql string, at time.Time) (Samples, error)
	QueryRange(ctx context.Context, promql string, w TimeWindow, step time.Duration) (Matrix, error)
	// LabelValues lists the values a label takes across the series that match the
	// given matchers (PromQL selectors, e.g. `{namespace="apps"}`), within the
	// window. label "__name__" enumerates metric names. matchers may be empty
	// (whole-TSDB), though callers should scope it so it stays cheap.
	LabelValues(ctx context.Context, label string, matchers []string, w TimeWindow) ([]string, error)
}

// LogsProvider abstracts the logs backend (VictoriaLogs now; Loki etc. later).
type LogsProvider interface {
	Query(ctx context.Context, query string, w TimeWindow) (LogResult, error)
}

// Bucket is one time-bucket of a log-hits histogram: how many lines matched in
// [Time, Time+step). Level is the per-level series label when the backend split
// hits by severity ("" for a single, unsplit series).
type Bucket struct {
	Time  time.Time
	Level string
	Count int64
}

// MsgCount is one dominant log message and its occurrence stats over a window:
// how many lines collapsed to it (after numeric normalization) and the first→last
// span it covered — the "what is flooding the logs" summary.
type MsgCount struct {
	Message string
	Count   int64
	First   time.Time
	Last    time.Time
}

// LogFields is an OPTIONAL discovery capability a LogsProvider may implement:
// the list of field names present in the logs a query matches (with per-field hit
// counts) — the log-side analogue of MetricsProvider.LabelValues. It answers "the
// query returned nothing / the schema I assumed is wrong — what fields do these
// logs ACTUALLY have?" so the agent recovers instead of dead-ending on a guessed
// collector schema. Consumers type-assert for it; VictoriaLogs implements it via
// /select/logsql/field_names.
type LogFields interface {
	// FieldNames returns the field names present in the logs matching query over
	// the window, each with its occurrence count, most-frequent first.
	FieldNames(ctx context.Context, query string, w TimeWindow) ([]FieldCount, error)
}

// FieldCount is one log field name and how many matching lines carried it.
type FieldCount struct {
	Name string
	Hits int64
}

// LogStats is an OPTIONAL analytics capability a LogsProvider may implement:
// error-volume-over-time (Hits) and top-messages-by-count (TopMessages). It is
// separate from LogsProvider so the analytics surface never widens the core
// contract — consumers type-assert for it exactly like GitOpsInspector, and a
// backend that cannot serve analytics (or a future Loki client) simply omits it,
// letting the tool fall back gracefully. VictoriaLogs implements it via
// /select/logsql/hits and a `stats by (_msg)` pipe.
type LogStats interface {
	// Hits returns the match count per step-sized bucket over the window; the
	// backend may split into per-level series (Bucket.Level set) or return a
	// single unsplit series.
	Hits(ctx context.Context, query string, w TimeWindow, step time.Duration) ([]Bucket, error)
	// TopMessages returns up to k dominant messages (numeric tokens collapsed so
	// near-identical lines group), each with its count and first→last span.
	TopMessages(ctx context.Context, query string, w TimeWindow, k int) ([]MsgCount, error)
}

// NetworkProvider abstracts network observability (Hubble now).
type NetworkProvider interface {
	Drops(ctx context.Context, sel Selector, w TimeWindow) (LogResult, error)
}

// LogReader reads recent pod logs from the cluster (read-only), backing the
// controller_logs investigation tool. Implemented with client-go CoreV1 GetLogs.
type LogReader interface {
	// PodLogs returns recent log lines from the pods selected by q, each line
	// prefixed with its pod name.
	PodLogs(ctx context.Context, q PodLogQuery) (LogResult, error)
}

// PodLogQuery selects pods and a log window for LogReader.PodLogs. It mirrors the
// optional-field shape of corev1.PodLogOptions so a new knob (e.g. a container
// name) doesn't break the interface and every caller.
type PodLogQuery struct {
	Namespace     string // required
	LabelSelector string // empty = all pods in the namespace
	SinceMinutes  int    // 0 = no lower bound
	Previous      bool   // read the last-terminated container (crash output) instead of the running one
	Container     string // empty = all of the pod's containers (the reader iterates them); set to scope to one
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
	// PodIP/NodeName/HostIP bridge a network_drops IP back to a pod: a VPC/Hubble
	// drop names an IP, and only pod_status can tie that IP to a workload. All three
	// are already on the corev1.Pod object, so surfacing them costs no extra API call
	// (B8, CORE-707). Empty when the pod hasn't been scheduled/assigned an IP yet.
	PodIP    string
	NodeName string
	HostIP   string
	// Time anchors (K1): pod_status was the only cluster tool with no notion of
	// WHEN. Restarts is the summed container RestartCount (how many times the pod
	// has looped); CreatedAt is the pod's creation time (its age); the
	// LastTerminated* pair is the last-terminated container's start/finish, so a
	// crash loop can be tied to a change/deploy time. All zero-valued when the
	// signal is absent (a fresh, never-restarted pod), and rendered only then.
	Restarts               int
	CreatedAt              time.Time
	LastTerminatedStarted  time.Time
	LastTerminatedFinished time.Time
}

// KubeEvent is a normalized Kubernetes Event — surfaces causes that live in the
// event stream, not logs or status (FailedScheduling, FailedMount, …).
type KubeEvent struct {
	Type     string // Normal | Warning
	Reason   string
	Object   string // Kind/Name
	Message  string
	Count    int32
	LastSeen time.Time // when the event last fired; zero when the API omitted it
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

// EventWindower is an optional KubeReader extension (K2): it adds a time window so
// the newest in-window events are actually returned in a busy namespace, where a
// single un-windowed page can miss them. It is a SEPARATE interface (not a new
// Events parameter) to keep KubeReader.Events arity stable for existing callers and
// fakes; kube_events type-asserts for it and falls back to Events when absent.
type EventWindower interface {
	// EventsSince behaves like KubeReader.Events but drops events older than
	// sinceMinutes (0 = no lower bound, equivalent to Events).
	EventsSince(ctx context.Context, namespace, objectName string, warnOnly bool, sinceMinutes int) ([]KubeEvent, error)
}

// OwnerLink is one hop in a resource's ownerReferences chain, e.g. a Pod owned by
// a ReplicaSet owned by a Deployment. Kind/Name/Namespace are engine-agnostic K8s
// identifiers — no Flux/ArgoCD types leak through.
type OwnerLink struct {
	Kind      string
	Name      string
	Namespace string
}

// OwnerChain is the resolved ownerReferences walk from a starting object (a Pod)
// up to its TOP controller, plus the GitOps object that manages that controller.
// It answers "a pod is failing — WHICH GitOps object owns it, and did its live
// state drift from what GitOps applied?" without the model guessing by name (G4).
//
// Engine-agnostic: ManagedByKind/ManagedByName name the owning Kustomization/
// HelmRelease (Flux) or Application (ArgoCD) as plain strings; Engine records which
// GitOps engine's tracking labels resolved it. Drift, when non-nil, is the live-vs-
// GitOps drift verdict for the owning object (see DriftVerdict).
type OwnerChain struct {
	// Chain is the ownerReferences hops, start (the pod) FIRST, top controller LAST.
	Chain []OwnerLink
	// Top is the top controller (Deployment/StatefulSet/DaemonSet/Job); zero-valued
	// Kind when the start object had no controller owner (a bare pod).
	Top OwnerLink
	// Engine is the GitOps engine whose tracking labels named the owner ("flux"/
	// "argocd"), or "" when no tracking label was found on the top controller.
	Engine Engine
	// ManagedByKind/ManagedByName name the owning GitOps object (e.g. Kustomization
	// "harbor", Application "harbor"); "" when no tracking label was found.
	ManagedByKind      string
	ManagedByNamespace string
	ManagedByName      string
	// Drift is the generic last-applied-configuration drift signal computed while
	// walking (a manual `kubectl edit` on the top controller). nil when the signal
	// was absent (no last-applied annotation) or the live spec matched it. The
	// authoritative GitOps-engine verdict (Argo OutOfSync / Flux not-Ready) is layered
	// on separately by the caller via GitOpsInspector — this is the cheap fallback.
	Drift *DriftVerdict
}

// DriftVerdict states whether a live object drifted from what GitOps applied, and by
// which signal. Signal is one of: "argocd-outofsync" (Argo's own OutOfSync verdict),
// "flux-not-ready-drift" (a Flux object not-Ready with a drift/reconcile reason), or
// "last-applied-configuration" (live spec differs from the kubectl.kubernetes.io/
// last-applied-configuration annotation — a manual kubectl-apply edit). Detail is a
// short human-readable summary; it never carries a full diff (out of scope).
type DriftVerdict struct {
	Drifted bool
	Signal  string
	Detail  string
}

// OwnerWalker is an OPTIONAL KubeReader extension (G4): it walks a resource's
// ownerReferences up to its top controller and names the owning GitOps object from
// the controller's Flux/ArgoCD tracking labels, and surfaces the generic last-applied-
// configuration drift signal on that controller. It is SEPARATE from KubeReader (not
// a new KubeReader method) so KubeReader's arity stays stable for existing callers
// and fakes; the workload_ownership tool type-asserts for it exactly like
// EventWindower/GitOpsInspector, and gracefully degrades when it is absent.
type OwnerWalker interface {
	// WorkloadOwnership resolves the owner chain for the pods selected by (namespace,
	// labelSelector). It picks the first matching pod (or an explicit podName when
	// set), walks Pod → ReplicaSet → Deployment (or StatefulSet/DaemonSet/Job), reads
	// the top controller's tracking labels to name the owning GitOps object, and
	// computes the last-applied-configuration drift signal on the top controller.
	WorkloadOwnership(ctx context.Context, namespace, labelSelector, podName string) (OwnerChain, error)
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

// ProgressUpdate is a lightweight interim status ping for a still-running
// investigation. It is NOT an Investigation (there are no findings yet) — it
// exists so a long (up to 20-step) investigation is not silent until the final
// message. Interim is model-derived text that may quote tool output; the loop
// redacts it (redact.Secrets) before it leaves, and notifiers must still escape
// it as untrusted like any other model text.
type ProgressUpdate struct {
	Title     string         // incident title (untrusted alert text)
	Step      int            // current step (1-based)
	MaxSteps  int            // step ceiling
	ToolsUsed map[string]int // investigation tool name → call count so far
	Interim   string         // model's latest interim assistant text (already secret-redacted), if any
}

// ProgressNotifier is an OPTIONAL capability a Notifier may implement to receive
// interim progress pings during a long investigation. It is separate from
// Notifier so a progress ping (not an Investigation) never widens the Notifier
// contract: the app type-asserts for it and wires progress delivery only to the
// notifiers that support it (Slack first; Matrix/webhook may no-op for now).
// Delivery of a progress ping is best-effort — a failure is logged and swallowed,
// never failing the investigation.
type ProgressNotifier interface {
	DeliverProgress(ctx context.Context, up ProgressUpdate) error
}

// CurationForge is the forge surface the curator's file-time gate needs: open a
// drafted PR, list open KB PRs (dedup), and comment to coalesce duplicates.
type CurationForge interface {
	OpenPR(ctx context.Context, entry KBEntry) (Ref, error)
	ListPRsByLabel(ctx context.Context, label string) ([]CuratedIssue, error)
	Comment(ctx context.Context, number int, body string) error
}

// CuratedIssue is a minimal view of a curated KB issue, used by the re-investigate
// loop to re-run and post results back.
type CuratedIssue struct {
	Number    int
	Title     string
	Body      string
	Labels    []string
	UpdatedAt time.Time // forge last-update time; used by the curate lifecycle sweep
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

// TruncationLine is the sentinel appended when a logs/flow query stops at its cap
// with more entries upstream, so the model knows the view is partial. It carries no
// Time or Fields, so it cannot be mistaken for a real entry. Every capping provider
// (Hubble/AWS VPC/GCP firewall flow sources, VictoriaLogs) emits this one line.
func TruncationLine(limit int64) LogLine {
	return LogLine{
		Message: fmt.Sprintf("… results truncated at %d (more matched — narrow the query or shorten the window)", limit),
	}
}

// Samples is an instant-vector result.
type Samples []Sample

// Matrix is a range-query result.
type Matrix []Series

// LogResult is a logs/network query result.
type LogResult []LogLine

// Verdict classifies an investigation's actionability for the humans reading the
// notification — the "do I need to do anything?" answer, separate from confidence
// (how sure the model is) and severity (how the alert was labelled).
type Verdict string

// The model-facing verdict vocabulary; submit_findings requires one of these.
const (
	VerdictNoAction        Verdict = "no_action"        // benign / self-healed / synthetic; nothing to do
	VerdictActionSuggested Verdict = "action_suggested" // a human should follow the suggested next steps
	VerdictActionRequired  Verdict = "action_required"  // live impact; act promptly
	VerdictInconclusive    Verdict = "inconclusive"     // could not be determined with available data
)

// ValidVerdict reports whether v is one of the model-facing enum values; the
// parser normalizes anything else to "" so formatters can safely omit it.
func ValidVerdict(v Verdict) bool {
	switch v {
	case VerdictNoAction, VerdictActionSuggested, VerdictActionRequired, VerdictInconclusive:
		return true
	}
	return false
}

// Investigation is the structured output contract of an investigation.
type Investigation struct {
	Title      string
	RootCauses []Hypothesis
	Changes    []Change
	Unresolved []string // honest: what the agent could not determine
	Verdict    Verdict  // model-classified actionability; "" when the model omitted it (rendered nowhere)
	RuledOut   []string // hypotheses considered and rejected, one line each with the disproving evidence
	DataGaps   []string // signals that could not be obtained (tool errors, missing metrics, truncation) — a data limitation, not a question for a human
	Confidence float64
	Recalled   bool     // true when produced by instant recall (a KB cache hit); the curator skips re-curating it
	Resource   Workload // the workload the investigation identified as affected; defaults to the originating alert workload when none was named (stored on curated entries for structural recall)
	// AlertResource is the workload the ORIGINATING ALERT fired on, stamped verbatim
	// from the Request and never touched by the model. It is NOT Resource: Resource is
	// where the fault was FOUND, which the investigation routinely refines to a deeper
	// object (an alert on the HelmRelease tooling/harbor resolving to the pod
	// tooling/harbor-registry). Recall, however, matches by the resource an ALERT
	// carries — so an entry indexed only by the fault locus is unreachable from the
	// alert that would surface it. Persisting both is what closes that gap.
	AlertResource Workload
	// Trigger-time facts stamped verbatim from the Request for the notification's
	// metadata block. The model never sees or sets them; empty for sources that lack them.
	Severity    string    // alert severity label at trigger time
	Environment string    // deployment environment (prod/staging/…)
	Cluster     string    // alert "cluster" label
	Tenant      string    // alert "tenant" label
	AlertName   string    // triggering alert name (labels["alertname"]); "" for non-alert sources
	StartedAt   time.Time // incident start (alert startsAt / failure time)
	// InvestigationStartedAt is when RUNLORE began investigating — distinct from StartedAt,
	// which is when the INCIDENT began. The two can be far apart: a request waits out
	// debounce/coalescing, then queues behind the single sequential worker and any
	// rate-limit backoff before the loop starts. Stamped by the loop at its delivery
	// chokepoint (never by the model) and carried onto the outcome-ledger open, where it is
	// the exact bound on resolve-before-open pairing (see outcome.resolvesSince) — the open
	// itself is stamped at COMPLETION, so without this the pairing window is unknowable.
	InvestigationStartedAt time.Time
	Actions                []Action    // proposed remediations (autonomy ladder; never executed at rung "suggest")
	CuratedURL             string      // runtime: KB issue/PR the curator opened, linked in delivery (set after curation)
	Fingerprint            string      // originating alert fingerprint; for outcome-ledger attribution
	Fingerprints           []string    // coalesced batch fingerprints; one outcome open is recorded per entry
	TriggerKey             string      // deterministic incident identity set at trigger time (alerts: host-invariant per-class key from curator.IncidentKey; GitOps: failing resource+condition). curator.DupFingerprint prefers it so reworded re-investigations (#137) AND the same alert on a different pod/node (CORE-681) still dedupe
	RecalledEntry          string      // when Recalled: the catalog entry Path that was matched
	Verified               bool        // true when the adversarial verify pass ran and a root cause survived it
	Usage                  UsageTotals // per-investigation model token/cost accounting (loop + verify); surfaced to humans + metrics, never written to the curated KB body
	// Recurrence facts stamped at completion from the outcome ledger's per-TriggerKey
	// index (never seen by the model). They describe PRIOR investigations of the same
	// TriggerKey; this run's own open is recorded after they are read.
	Occurrences    int             // Nth recorded investigation of this TriggerKey (1 = first); 0 = unknown/ledger disabled
	LastOccurrence time.Time       // when the previous occurrence was investigated
	PrevCuratedURL string          // the previous occurrence's KB link, for the "same conclusion as before" pointer
	Prior          *PriorKnowledge // what the merged KB entry already says about this recurring incident; nil when unknown (see PriorKnowledge)
	// MatchedKnowledge is the single strongest PRE-EXISTING knowledge-base entry that
	// this investigation's kb_search calls matched at clear-match strength — the visible
	// proof that RunLore already had documented knowledge for the incident. It is stamped
	// by the ReAct loop (never by the model) and is DISTINCT from Prior: Prior reports
	// RECURRENCE ("this exact incident, investigated N times before", from the outcome
	// ledger), whereas MatchedKnowledge reports that a FULL investigation reused a known
	// runbook/entry even on a first sighting. nil when no kb_search hit cleared the bar.
	// Notifiers render it only when Prior == nil (the recurrence block already covers the
	// "seen before" case — don't double-render).
	MatchedKnowledge *MatchedEntry
}

// MatchedEntry is the strongest pre-existing catalog entry an investigation's
// kb_search calls matched at clear-match strength. It closes a live visibility gap:
// when a full investigation's kb_search found a known runbook and used it, the
// delivered notification previously gave NO sign RunLore already had knowledge for
// the incident (the "Seen before"/Prior block only fires on a ledger recurrence, not
// when a full loop reuses a known entry). Path + Title always populate; URL only when
// a web link is cheaply derivable (else the notifier shows Path). Score is the BM25
// relevance of the matching hit — recorded so the clear-match bar can be tuned from
// live data, like the recall thresholds.
type MatchedEntry struct {
	Path  string  // catalog path of the matched entry (bundle-relative)
	Title string  // entry title, for the human-facing line
	URL   string  // web link to the entry when cheaply derivable; "" ⇒ notifier shows Path
	Score float64 // BM25 relevance score of the matching kb_search hit
}

// PriorKnowledge is what the knowledge base already says about a recurring
// incident: excerpts of the merged entry's Cause and (human-reviewed)
// Resolution sections, plus the entry's recall track record from the outcome
// ledger. Stamped at completion — never seen by the model — and only on FRESH
// investigations of a recurring TriggerKey whose merged entry is findable by
// dup-fingerprint; nil otherwise, so notifiers fall back to the counter+link.
type PriorKnowledge struct {
	Cause      string // excerpt of the merged entry's "## Cause" section
	Resolution string // excerpt of "## Resolution" — carries the human's review edits, the payoff of curation
	EntryPath  string // catalog path of the merged entry
	Recalls    int    // times the entry answered an incident via instant recall
	Resolved   int    // recalls followed by an incident-resolved signal
}

// UsageTotals aggregates model token usage over a whole investigation: every
// model call summed — the ReAct loop, the adversarial verify pass, and any recall
// verification. It is carried on Investigation so notifiers and metrics can
// surface usage/cost without re-reading provider internals. Zero when no model
// call reported usage (e.g. a pure recall short-circuit).
type UsageTotals struct {
	ModelCalls        int     // number of model completions made
	InputTokens       int     // total input/prompt tokens, INCLUDING any served from cache (mirrors Usage.InputTokens)
	OutputTokens      int     // total generated/output tokens
	CachedInputTokens int     // subset of InputTokens that was a cache read (the saving)
	CostUSD           float64 // estimated cost; meaningful only when Priced (model.pricing configured)
	Priced            bool    // pricing was configured, so CostUSD is populated (may legitimately be 0)
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
	Type        string // OKF type, one of the validator vocabulary: Incident | Playbook | Concept
	Title       string
	Description string
	Resource    string
	// AlertResource is the resource the originating ALERT fired on, when it differs
	// from Resource (the fault locus). Recall matches by alert resource; without this
	// an entry whose fault sits deeper than its alert is permanently unrecallable.
	AlertResource string
	Tags          []string
	Body          string   // markdown
	Fingerprint   string   // deterministic dedup fingerprint (see curator.DupFingerprint)
	Confidence    float64  // overall investigation confidence; queryable extension frontmatter (0 = unset)
	Provenance    []string // distinct causing-change refs; queryable extension frontmatter
	// Reviewer context, rendered in the PR BODY only — never in the committed
	// entry file (renderEntry ignores these), so the catalog and validator are
	// untouched. Related is the draft-time BM25 neighborhood; the recurrence
	// facts mirror Investigation.Occurrences/PrevCuratedURL.
	Related        []RelatedEntry
	Occurrences    int
	PrevCuratedURL string
}

// RelatedEntry is a nearby catalog entry surfaced to the KB PR reviewer so
// "is this a duplicate / what do we already know?" is answerable in the PR.
type RelatedEntry struct {
	Path     string // bundle-relative entry path (the forge renders the web link)
	Title    string
	Resource string  // affected resource, when the entry names one
	Score    float64 // BM25 score at draft time (corpus-relative — a hint, not a ranking guarantee)
}

// Ref is a URL handle to a created issue or PR.
type Ref struct{ URL string }

// CompletionRequest / CompletionResponse are the minimal LLM exchange types.
type CompletionRequest struct {
	System   string
	Messages []Message
	Tools    []ToolSpec
	// ToolChoice optionally names one tool from Tools that the model MUST call on
	// this turn ("" = provider default: the model chooses freely between prose and
	// any tool). Set it on structured-output turns — submit_verdicts, submit_review,
	// submit_grade, and the post-budget-nudge submit_findings — where a prose reply
	// is never acceptable; leave it empty on normal investigation steps so the model
	// keeps the freedom to pick tools or answer.
	ToolChoice string
}

// Message is one turn in an LLM exchange.
type Message struct {
	Role       string // system | user | assistant | tool
	Content    string
	ToolCalls  []ToolCall // assistant turn requesting tools
	ToolCallID string     // tool turn: the call this answers
	// Opaque is provider-specific content the client must replay verbatim; produced
	// and consumed only by the same provider, empty otherwise. The loop carries it
	// from a completion's CompletionResponse.Opaque onto the assistant turn it stores
	// in history, so the same provider can prepend it on the next request. Currently
	// the Anthropic client uses it to replay signed adaptive-thinking blocks; other
	// providers ignore it.
	Opaque json.RawMessage
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
	// Usage is the provider-reported token count for this completion. Zero when
	// the provider omits it (older endpoints, or a provider that does not report
	// it) — callers treat the zero value as "unknown", not "zero tokens".
	Usage Usage
	// Truncated is true when the provider stopped because it hit the output-token
	// ceiling (Anthropic stop_reason "max_tokens", OpenAI finish_reason "length",
	// Gemini finishReason "MAX_TOKENS"). It distinguishes a cut-off answer from a
	// complete one, so the loop need not treat a truncated reply as final.
	Truncated bool
	// StopReason is the provider's raw turn-termination reason, normalized to the
	// provider's own vocabulary (Anthropic stop_reason, OpenAI finish_reason, Gemini
	// finishReason). It is empty when the provider omits it. Refused() interprets it;
	// the loop uses Refused() rather than matching strings itself.
	StopReason string
	// Opaque is provider-specific content the client must replay verbatim; produced
	// and consumed only by the same provider, empty otherwise. The loop copies it onto
	// the assistant Message it appends to history so the same provider can replay it on
	// the next request. The Anthropic client serializes completed adaptive-thinking
	// (and redacted_thinking) blocks — in order, with their signatures — into it;
	// OpenAI/Gemini leave it empty and ignore it.
	Opaque json.RawMessage
}

// refusalStopReasons is the set of stop reasons (across providers, lower-cased) that
// mean the model declined the request on safety/policy grounds rather than producing
// an answer. Anthropic emits "refusal"; OpenAI "content_filter"; Gemini "SAFETY",
// "PROHIBITED_CONTENT", "BLOCKLIST", "SPII".
var refusalStopReasons = map[string]bool{
	"refusal":            true,
	"content_filter":     true,
	"safety":             true,
	"prohibited_content": true,
	"blocklist":          true,
	"spii":               true,
}

// Refused reports whether the model declined the request on safety/policy grounds
// (a successful response with no usable answer) rather than terminating normally.
// The comparison is case-insensitive so a provider's casing (e.g. Gemini's "SAFETY")
// does not matter. The loop treats a refusal as a first-class unresolved outcome.
func (r CompletionResponse) Refused() bool {
	return refusalStopReasons[strings.ToLower(r.StopReason)]
}

// Usage is the provider-reported token accounting for one completion.
type Usage struct {
	InputTokens  int // total prompt/input tokens billed, INCLUDING any served from cache (normalized across providers)
	OutputTokens int // generated/output tokens in the reply
	// CachedInputTokens is the subset of InputTokens that was a cache READ (Anthropic
	// cache_read_input_tokens, OpenAI prompt_tokens_details.cached_tokens, Gemini
	// cachedContentTokenCount) — the saving. 0 when the provider reports none.
	CachedInputTokens int
	// CacheWriteTokens is input tokens WRITTEN to the cache this request (Anthropic
	// cache_creation_input_tokens, billed ~1.25x). 0 for providers that don't report it.
	CacheWriteTokens int
}

// ToolCall is a model request to invoke a tool.
type ToolCall struct {
	ID   string
	Name string
	Args string // JSON
}

const fingerprintMarkerPrefix = "<!-- runlore-fingerprint: "

// FingerprintMarker renders a hidden PR-body marker carrying the dedup fingerprint,
// so an open PR's fingerprint is recoverable from the PR listing without fetching
// file contents. It returns "" for an empty fingerprint so callers may append it
// unconditionally.
func FingerprintMarker(fp string) string {
	if fp == "" {
		return ""
	}
	return fingerprintMarkerPrefix + fp + " -->"
}

// ParseFingerprintMarker extracts the fingerprint from a PR body, or "" if absent.
func ParseFingerprintMarker(body string) string {
	i := strings.Index(body, fingerprintMarkerPrefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(fingerprintMarkerPrefix):]
	j := strings.Index(rest, " -->")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}
