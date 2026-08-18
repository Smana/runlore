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
	"errors"
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

// EntryResourceRef narrows a rendered workload ref to the single, whitespace-free
// value RunLore's own merge gate accepts in a knowledge-base entry's `resource:`
// frontmatter — kbvalidate rejects any resource containing " \t\r\n" outright.
//
// It exists because Ref() renders whatever is in Workload.Name, and on a curated
// or captured finding that is MODEL-WRITTEN free text: submit_findings fills
// affected_resource (internal/investigate/tools.go), and a finding covering
// several objects routinely arrives as a list — "essentials, monitoring,
// argocd-app-of-apps" — which Ref() renders as "argocd/essentials, monitoring,
// argocd-app-of-apps". Written verbatim, that value fails the gate, so the entry's
// pull request can never be merged. On the thread-capture path that is the worst
// possible shape of failure: the human is told the write succeeded and the
// announcement fires, because both are 1:1 with the LANDED FORGE WRITE, which did
// land — only the merge is impossible, and nothing surfaces it.
//
// Narrowing, not dropping: `resource` is the structural-recall index (see
// investigate's resourceAgrees), so an entry with no resource is reachable
// lexically only. The first listed object is the one the namespace was rendered
// against and is a real object, so keeping it preserves the index. Nothing is
// lost — the full list still reaches the entry BODY verbatim.
//
// Whitespace-free input is returned EXACTLY as given — the single-field early
// return below exists for that promise alone. It is what lets every caller say
// that no entry which merges today is written differently, and it is why the
// punctuation trim is guarded rather than unconditional: an unguarded trim also
// rewrites "argocd/app," and "a;b;", which are whitespace-free, clear the gate
// today, and are none of this function's business.
//
// So it deliberately does NOT split a comma-joined list that carries no
// whitespace ("a,b,c"): that value merges today, and quietly rewriting what an
// existing entry is indexed under is a bigger change than closing a gate defect.
//
// KNOWN CONSEQUENCE: two different multi-object findings that happen to lead with
// the same object now write the SAME `resource`, while curator.DupFingerprint
// keeps them distinct (it hashes the full, un-narrowed Ref()). So the frontmatter
// index can collide where the dedup identity does not — a new class, and stated
// here rather than left to be discovered. It is strictly better than what it
// replaces, where neither entry could be merged at all, and the entry body still
// distinguishes them for a reader.
func EntryResourceRef(ref string) string {
	fields := strings.Fields(ref)
	if len(fields) == 0 {
		return ""
	}
	if len(fields) == 1 {
		return fields[0]
	}
	// Trailing list punctuation is what the split left behind ("argocd/essentials,"),
	// not part of the name; a trailing "/" would leave a ref with an empty name half.
	return strings.TrimRight(fields[0], ",;/")
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
// reversibility/blast from this — never from model output — and the per-engine
// executors (internal/executor/flux, internal/executor/argocd) run only ops
// listed here. Op names are engine-neutral; the executor for the configured
// gitops.engine translates them (Flux: spec.suspend / requestedAt annotation;
// Argo CD: pause/restore spec.syncPolicy.automated / refresh annotation). One
// entry per op is the single source of truth that keeps the gate and the
// executors from drifting.
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

// LookupReason says what a read that returned no object actually ESTABLISHED, which is
// not always "the object is absent".
//
// The dynamic client reports a 404 identically whether a served kind has no object of
// that name or the API serves no such kind at all; an RBAC-refused cluster-wide search
// never ran; and a kind the provider has no mapping for never reached the API at all.
// Collapsing those into one "not found" is how runlore#503 turned a limit of the tool
// into a claim about the cluster. A provider knows which case it hit, so it says so.
type LookupReason string

const (
	// LookupAbsent — the kind is served, the search ran, and no object of that name came
	// back from any scope searched. The only reason that is genuinely about the object.
	LookupAbsent LookupReason = "absent"
	// LookupKindNotServed — the API serves no such resource type, so no object of that
	// kind could have been returned. Says nothing about the named object.
	LookupKindNotServed LookupReason = "kind-not-served"
	// LookupDenied — a search the answer would otherwise claim to have made was refused
	// by RBAC and never ran. The object may sit in a scope this agent cannot read.
	LookupDenied LookupReason = "denied"
	// LookupUnresolvable — this provider has no mapping for the kind, so no request was
	// ever issued. A statement about the provider's scope, not about the cluster.
	LookupUnresolvable LookupReason = "unresolvable"
	// LookupFailed — the read errored for some other reason, so nothing was established.
	LookupFailed LookupReason = "failed"
)

// AllNamespaces is the Lookup scope name for a completed cluster-wide search by name.
const AllNamespaces = "all namespaces"

// Lookup records what a name lookup DID, so a tool can report the lookup instead of
// asserting a conclusion the lookup does not support.
type Lookup struct {
	Reason LookupReason
	// Scopes are the search scopes the provider actually COMPLETED, in order: a
	// namespace name, or AllNamespaces. A scope that was skipped or refused is not
	// listed — naming a search that never ran is the false half of #503's message,
	// which claimed the namespace, flux-system and all namespaces unconditionally.
	Scopes []string
}

// LookupError is a failed read that knows what it established. Providers return it so a
// caller can report the lookup rather than infer a verdict from a bare 404; the
// underlying API error is wrapped, so apierrors.IsNotFound and friends still work.
type LookupError struct {
	Lookup Lookup
	Err    error
}

func (e *LookupError) Error() string {
	if e.Err == nil {
		return string(e.Lookup.Reason)
	}
	return string(e.Lookup.Reason) + ": " + e.Err.Error()
}

func (e *LookupError) Unwrap() error { return e.Err }

// LookupOf recovers the Lookup a provider attached to a failed read, reporting false
// for a nil error or one that carries no such record.
func LookupOf(err error) (Lookup, bool) {
	var le *LookupError
	if errors.As(err, &le) {
		return le.Lookup, true
	}
	return Lookup{}, false
}

// ResourceStatus is a read-only snapshot of a GitOps/Kubernetes object's health,
// used to investigate WHY a resource is failing (not just that it is).
type ResourceStatus struct {
	Workload Workload
	// NotFound reports that the read returned no object. Lookup says what that
	// ESTABLISHED — read them together, because "not found" on its own does not
	// distinguish an absent object from an unserved kind or a denied search.
	NotFound bool
	Lookup   Lookup
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
	// Lookup is set whenever this node's own read returned no object — absent, unserved
	// kind, denied, unresolvable kind, or a failed read. A zero Lookup means the object
	// WAS read, so a renderer has to consult it before printing a Ready state: a node
	// whose read failed used to render as "(Ready=unknown)", which asserts it exists.
	Lookup   Lookup
	Ready    string // Ready condition status
	Reason   string
	Children []DepNode
}

// GitOpsEngineReporter is an optional capability: a GitOps provider naming the engine it
// speaks. Consumers type-assert for it exactly like GitOpsInspector.
//
// It exists so a consumer can source the engine from the provider it was actually handed
// rather than from gitops.engine, which nothing validates — "argo" and "ArgoCD" both fall
// through to flux — and which therefore cannot support a statement about the deployment.
// Both providers already tag every Change they emit with their Engine; this exposes the
// same fact before there is a Change to read.
type GitOpsEngineReporter interface {
	GitOpsEngine() Engine
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

// ResourceSpecOutcome distinguishes the ways a spec read can end. They are
// SEPARATE values because conflating them is what made gitops_resource_status
// dangerous: it answered "the object genuinely does not exist" for a kind it never
// supported, and a model reasoned from that as evidence of absence. Only
// ResourceAbsent is evidence about the cluster's contents.
type ResourceSpecOutcome string

// The outcomes of a resource spec read.
const (
	ResourceFound         ResourceSpecOutcome = "found"          // the object was read
	ResourceAbsent        ResourceSpecOutcome = "absent"         // the server says this OBJECT does not exist
	ResourceForbidden     ResourceSpecOutcome = "forbidden"      // RBAC denied the read; says NOTHING about existence
	ResourceKindUnknown   ResourceSpecOutcome = "kind_unknown"   // this cluster serves no such kind; says NOTHING about existence
	ResourceKindAmbiguous ResourceSpecOutcome = "kind_ambiguous" // several API groups serve this kind; nothing was read
	ResourceRefused       ResourceSpecOutcome = "refused"        // this agent refuses the kind by policy (Secret)
)

// ResourceSpecQuery identifies the object to read.
//
// Kind is BARE ("VMServiceScrape") because that is what a model has: it reads the
// kind off an alert or a manifest, not a fully-qualified resource. Group is the
// optional disambiguator for the case where a bare Kind is served by more than one
// API group — Event (core and events.k8s.io) and NetworkPolicy (networking.k8s.io
// and crd.projectcalico.org) on any cluster running Calico. Without it, an
// ambiguity would be a dead end: the reader refuses to guess, and the caller would
// have no way to say which one it meant.
type ResourceSpecQuery struct {
	Kind      string
	Name      string
	Namespace string
	// Group narrows resolution to one API group ("" means "no preference", and
	// "core" is accepted as a spelling of the core group's empty name).
	Group string
}

// ResourceSpec is one Kubernetes object's desired and observed state, as YAML.
//
// Spec and Status are rendered rather than typed because the point is to read
// ARBITRARY kinds — including CRDs the binary has never heard of — so there is no
// Go type to unmarshal into.
type ResourceSpec struct {
	// Query echoes the request NORMALIZED to what was actually read: the Kind in
	// the casing the server serves it under, the Group that answered, and NO
	// namespace for a cluster-scoped kind. Rendering the request back verbatim
	// would state a caller's mistake as fact — "StorageClass made-up-ns/fast".
	Query   ResourceSpecQuery
	Outcome ResourceSpecOutcome
	// APIVersion the object was actually read at, so a reader can tell which of
	// several served versions answered.
	APIVersion string
	Spec       string // .spec as YAML ("" when the kind has no spec, e.g. ConfigMap)
	Status     string // .status as YAML ("" when absent)
	// Detail carries the server's own message for a non-found outcome, so a denial
	// reads as a denial rather than being flattened into "not found".
	Detail string
}

// ResourceSpecReader reads one object's spec/status by kind, name and namespace.
//
// It exists because every other reader here answers "what is happening" while a large
// class of incidents is "the spec says X and reality is Y": a Service selector matching
// no pods, a scrape CR targeting an absent namespace, a NetworkPolicy with no egress to
// kube-dns, an HPA on a metric that never reports. Those were previously only inferable
// from consequences — see the investigation that concluded a VMServiceScrape had been
// deleted when its namespaceSelector simply pointed at a namespace that did not exist.
//
// Implementations MUST refuse Secret outright — both before AND after resolution, so a
// kind that folds to "secret" cannot slip past the pre-check — and MUST report RBAC
// denials as ResourceForbidden rather than as absence.
type ResourceSpecReader interface {
	ResourceSpec(ctx context.Context, q ResourceSpecQuery) (ResourceSpec, error)
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

// ThreadNotifier is the optional capability of carrying a conversation back into
// a delivered notification's thread. A notifier that cannot — an incoming
// webhook, the generic HTTP sink — simply does not implement it, and thread
// interaction is unavailable on that transport. Same contract as an unset data
// source disabling its tool: no capability is ever faked.
type ThreadNotifier interface {
	Notifier
	// ReplyInThread posts text into the thread rooted at root, in channel. Both
	// handles are transport-specific opaque strings (Slack: thread_ts and a
	// channel id).
	ReplyInThread(ctx context.Context, root, channel, text string) error
	// Transport names the chat system this notifier replies on ("slack",
	// "matrix"). It is what lets a deployment running several transports route
	// each thread's reply back to the system the human is actually in — the
	// alternative, picking the first notifier that can reply, silently answers
	// one transport's threads on another.
	Transport() string
}

// KBRoute names how a knowledge-base write landed on the forge. The values
// mirror the labels the thread responder already reports on its
// ThreadNotesWritten counter, so an announcement and the metric describing the
// same write cannot disagree about which route it took.
type KBRoute string

// The routes a knowledge-base write can take.
const (
	// KBRouteComment added the note as a comment on a KB pull request SOMEONE
	// ELSE drafted — the curator's. There the note is review feedback on another
	// author's draft: a human reconciles it at merge time, and until they do it
	// is deliberately not part of the entry.
	KBRouteComment KBRoute = "comment"
	// KBRouteOpenPR opened a new standalone pull request for the note.
	KBRouteOpenPR KBRoute = "open_pr"
	// KBRouteAppend appended the note to the ENTRY FILE of a standalone note PR
	// RunLore itself opened earlier in the same thread. It is a route of its own
	// rather than a second spelling of KBRouteComment because the two land
	// somewhere materially different: what this route writes is inside the entry
	// the catalog gains when the PR merges, and what a comment writes is not.
	KBRouteAppend KBRoute = "append"
)

// KBDelivery says WHERE a KBUpdate announcement is to be delivered. It is set
// once from configuration (config.AnnounceMode) and read by every sink.
//
// # A sink that is not the originating transport falls back to the channel
//
// Only ONE sink can ever deliver into the thread a note was typed in: the
// transport that thread lives on. A Matrix room cannot reply into a Slack
// thread, an incoming-webhook Slack cannot reply into any thread, and an HTTP
// webhook has no thread at all. The choice for those sinks is fall back to the
// channel, or skip them; this contract is FALL BACK, and the sinks that cannot
// thread-route are the reason.
//
// Skipping would make thread-routing silently un-configure an operator's other
// sinks. The echo KBDeliverThread exists to remove is specific to the
// originating transport, where the thread already lives inside the channel the
// announcement would post to; a Matrix room that never saw the Slack thread has
// no echo to remove, and the announcement is the only way it learns anything at
// all. Dropping it there would answer "stop repeating yourself in one channel"
// with "stop telling the other rooms", which is a different feature and a worse
// one. The same argument holds harder for a webhook sink feeding an index or an
// incident tool: it wants the record either way.
//
// The fallback is also what makes a missing handle safe. A KBUpdate whose Root
// or Channel is empty — a thread context rebuilt after a restart, an origin that
// was never a thread — cannot be replied into by anyone, and the write it
// describes has already landed. Falling back to the channel reports it; skipping
// would lose an announcement for a write that really happened, which is the
// failure this whole path is built to avoid.
type KBDelivery string

// Where an announcement lands.
const (
	// KBDeliverChannel posts to each sink's own configured channel or room and
	// nowhere else.
	//
	// It is the ZERO VALUE deliberately: a KBUpdate built without deciding
	// behaves exactly as every announcement did before routing existed, so no
	// producer that predates this field can change destination by omission.
	KBDeliverChannel KBDelivery = ""
	// KBDeliverThread delivers into the originating thread where the sink can
	// (see above), and to the sink's channel where it cannot.
	KBDeliverThread KBDelivery = "thread"
	// KBDeliverBoth delivers into the originating thread AND to every sink's
	// channel.
	KBDeliverBoth KBDelivery = "both"
)

// IntoThread reports whether this delivery wants the originating thread. It does
// NOT report that a given sink can honour that — only the sink knows whether it
// is the originating transport and holds both handles.
func (d KBDelivery) IntoThread() bool { return d == KBDeliverThread || d == KBDeliverBoth }

// KBUpdate announces that a knowledge-base write ALREADY LANDED on the forge. It
// is emitted after the fact and describes what was written — it is never a
// request to write, and a consumer failing to deliver it changes nothing about
// the entry, which is already on the forge.
//
// It names its origin (Transport + Root + Channel) so a consumer can tell which
// thread produced it — and so the transport that already replied in that thread
// can avoid announcing the same write to the same people twice, or deliver into
// that thread rather than beside it (see Delivery).
//
// It also names WHO WROTE the note as distinct from whose message produced it:
// on the freeform route RunLore's own chat model drafted the text and the human
// only prompted it. See ModelDrafted — a consumer that renders Author without
// consulting it will attribute model prose to a named engineer.
//
// # Untrusted fields
//
// Note, Title and Author are UNTRUSTED. They are secret-redacted upstream
// (redact.Secrets) and length-capped, but they remain model- or human-authored
// text: on the model-drafted route RunLore's own chat model wrote Note, and
// Author is whatever the chat transport reported. Every implementer MUST escape
// them with its transport's escaper before rendering — unescaped they can inject
// links and mass-ping a channel (Slack <!channel>, Matrix @room), the same
// hazard ProgressUpdate.Interim carries.
//
// URL, Root and Channel are untrusted for the same reason at one remove: they
// come back from the forge and from the chat transport rather than from RunLore,
// which is why the thread reply already wraps the URL in thread.Untrusted.
// Transport, Route, PR, At and Delivery are RunLore's own values and need no
// escaping.
type KBUpdate struct {
	// Transport is the chat system the note came from ("slack", "matrix"),
	// matching ThreadNotifier.Transport; "" when the write had no thread origin.
	Transport string
	// Root is the opaque thread-root handle on that transport (Slack: thread_ts).
	Root string
	// Channel is where that thread lives (Slack: channel id; Matrix: room id) —
	// the second half of the handle ReplyInThread needs, since a thread root
	// alone does not say which channel to post it in. "" when the write had no
	// thread origin, or when the origin was rebuilt without one.
	Channel string
	// Delivery says where this announcement is to be delivered. It is a routing
	// instruction rather than a fact about the write, and the only field here
	// that is: everything else describes what landed on the forge.
	Delivery KBDelivery
	// Route is how the write landed (comment on an open PR, or a new PR).
	Route KBRoute
	// PR is the forge pull/merge request number the knowledge landed in; 0 when
	// the forge URL did not name one.
	PR int
	// URL is the forge URL of that pull request.
	URL string
	// Title is the knowledge entry's title. Set on the KBRouteOpenPR route,
	// which generates one; empty on KBRouteComment, which adds to an entry that
	// was already titled. UNTRUSTED.
	Title string
	// Author is the human whose thread message produced the note, as the chat
	// transport reported it. Provenance, not proof of identity. UNTRUSTED.
	//
	// It names whose message PRODUCED the note, which is not the same as who
	// wrote its words — see ModelDrafted, which every renderer must consult
	// before presenting Note as this person's own.
	Author string
	// ModelDrafted reports that RunLore's own chat model wrote Note from Author's
	// message, rather than Author having typed it after an explicit "note:".
	//
	// It is the announcement's half of a distinction every other surface already
	// makes, and the reason it has to exist HERE rather than be inferred: a sink
	// receives this struct and nothing else. Without it the event carried
	// {author: "alice", note: "<the model's text>"} with no way to tell the two
	// routes apart, so a chat sink rendered "By alice in a slack thread" over
	// prose alice never wrote and a webhook receiver stored it as her statement.
	// The knowledge base's merge gate reads exactly that signal — a human
	// attestation is what a reviewer weighs a note by — so filing model prose
	// under a named engineer is the specific failure the thread reply's
	// openedWith, NoteBody's "@alice did not write it" heading and
	// conceptDescription's leading provenance clause were each written to close.
	//
	// TRUSTED: RunLore sets it from the note's own provenance (thread.Note.
	// DraftedFrom), it is never reported by a chat system, and it is a boolean
	// rather than text, so no renderer escapes it. It is nonetheless the field
	// most likely to be forgotten by a renderer, which is why the announcement
	// surfaces have tests asserting the two routes cannot render identically.
	ModelDrafted bool
	// Note is the note text AS WRITTEN to the forge — post-redaction and
	// post-cap, never the caller's raw input, so an announcement cannot leak
	// what the entry itself masked. UNTRUSTED.
	Note string
	// At is when the write landed.
	At time.Time
}

// KBUpdateNotifier is an OPTIONAL capability a Notifier may implement to receive
// KBUpdate announcements, so a knowledge-base write reaches every configured
// destination rather than only the thread that produced it.
//
// It is separate from Notifier — and, unlike ThreadNotifier, does not embed it —
// for the same reason ProgressNotifier is: a KB-update announcement is not an
// Investigation, and widening Notifier would force every existing sink to grow a
// method it has no use for. Consumers type-assert for it (see notify.Multi) and
// skip the sinks that do not implement it.
//
// Delivery is best-effort by contract: a failure is logged and swallowed, never
// propagated. The write being announced has already landed on the forge, so a
// broadcast must never be able to report it as failed or roll it back.
type KBUpdateNotifier interface {
	DeliverKBUpdate(ctx context.Context, up KBUpdate) error
}

// CurationForge is the forge surface the curator's file-time gate needs: open a
// drafted PR, list open KB PRs (dedup), and comment to coalesce duplicates.
type CurationForge interface {
	OpenPR(ctx context.Context, entry KBEntry) (Ref, error)
	ListPRsByLabel(ctx context.Context, label string) ([]CuratedIssue, error)
	// CommentOnPR posts a comment on the PULL REQUEST / MERGE REQUEST numbered
	// `number`. See the scoped-comment note on ReinvestForge.CommentOnIssue for
	// why the artifact kind is in the method name rather than left to the forge.
	CommentOnPR(ctx context.Context, number int, body string) error
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
	// CommentOnIssue posts a comment on the ISSUE numbered `number`.
	//
	// The artifact kind is part of the method NAME, not a runtime guess by the
	// forge, because on GitLab a bare number does not identify one artifact:
	// merge requests and issues have INDEPENDENT iid sequences, both starting at
	// 1, so in a busy KB project issue #3 and merge request !3 both exist and are
	// unrelated. A forge that had to infer the kind (say, "try MRs first, fall
	// back to issues on 404") would post the re-investigation findings onto a
	// random merge request, get a 200 back, and leave no trace of the misroute.
	// GitHub has no such ambiguity — issues and PRs share one number sequence and
	// one comments endpoint — so both of its scoped methods are the same call;
	// the split exists so the COMPILER, not a code reviewer, enforces the
	// distinction on the forges where it is load-bearing.
	CommentOnIssue(ctx context.Context, number int, body string) error
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

// Conclusive reports whether v is an ANSWER — the investigation reached a
// judgement the on-call can act on (or deliberately not act on). It is the ONE
// definition of "conclusive": the outcome ledger folds its per-trigger standing
// answer with it, and TriggerRecurrence.Concluded re-checks stored verdicts through
// it, so the writer and the reader cannot disagree. inconclusive is not an answer,
// and neither is "" (a pre-verdict ledger event, or a reply the parser could not
// normalize) — both mean "we still owe a real answer".
func (v Verdict) Conclusive() bool {
	switch v {
	case VerdictNoAction, VerdictActionSuggested, VerdictActionRequired:
		return true
	}
	return false
}

// ValidVerdict reports whether v is one of the model-facing enum values; the
// parser normalizes anything else to "" so formatters can safely omit it. Defined
// in terms of Conclusive so the three actionability verdicts are enumerated once —
// a fifth verdict is then one switch to update, not two adjacent ones.
func ValidVerdict(v Verdict) bool {
	return v.Conclusive() || v == VerdictInconclusive
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
	Actions                []Action // proposed remediations (autonomy ladder; never executed at rung "suggest")
	CuratedURL             string   // runtime: KB issue/PR the curator opened, linked in delivery (set after curation)
	// CurateError is the reason the curator could not write this finding to the KB, set
	// only when a write was ATTEMPTED and failed. It exists because an empty CuratedURL is
	// ambiguous: it is also the normal state for a finding below curate.min_confidence or
	// carrying a skip_verdicts verdict, so "no KB link" cannot tell a human whether the
	// learning loop is working. Rendered by notify.Format AND by the Slack card's footer
	// (notify.summaryBlocks), both through notify.curateFailureReason; never written to
	// the KB body.
	//
	// One of exactly two free-text fields on this struct stamped AFTER
	// investigate.redactInvestigation — the other is Prior (Cause/Resolution) — because
	// both are produced by the OnComplete pipeline rather than inside the loop. Each is
	// therefore redacted at its assignment (app.onInvestigationComplete) rather than by
	// the reflection walk, and neither is on redactionSkipField: both are untrusted free
	// text, the opposite of what that list is for. Everything else the pipeline stamps
	// past the chokepoint is a number, a time, or a server-derived identifier already on
	// that list (CuratedURL, PrevCuratedURL, Action.ApprovalID).
	CurateError   string
	Fingerprint   string      // originating alert fingerprint; for outcome-ledger attribution
	Fingerprints  []string    // coalesced batch fingerprints; one outcome open is recorded per entry
	TriggerKey    string      // deterministic incident identity set at trigger time (alerts: host-invariant per-class key from curator.IncidentKey; GitOps: failing resource+condition). curator.DupFingerprint prefers it so reworded re-investigations (#137) AND the same alert on a different pod/node (CORE-681) still dedupe
	RecalledEntry string      // when Recalled: the catalog entry Path that was matched
	Verified      bool        // true when the adversarial verify pass ran and a root cause survived it
	Usage         UsageTotals // per-investigation model token/cost accounting (loop + verify); surfaced to humans + metrics, never written to the curated KB body
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

// UnaccountedInconclusive reports whether inv claims it could not determine the
// cause while giving no account of what blocked it: no root cause, no question for
// a human, no data gap. That payload contradicts itself — an honest "I could not
// determine" always has something in one of those three channels — and it is what
// the model produces when it reaches for `inconclusive` to mean "this is already
// known" (#471, and again live on 2026-08-18).
//
// It lives HERE, on the contract, because two sides consult it and must not
// disagree: the loop says it out loud at the source, where the payload is still
// attributable to the model call that made it, and the notifier acts on it at
// delivery, where a card that neither explains itself nor shows evidence is what
// the on-call actually has to read. Same reasoning as Verdict.Conclusive — one
// definition, so the writer and the reader cannot drift apart.
//
// RuledOut is deliberately not one of the three: eliminating hypotheses says what
// the cause is NOT, which is neither a finding nor a reason the run could not reach
// one. The three channels here are the ones that answer "so what happened?" or
// "why don't you know?".
func (inv Investigation) UnaccountedInconclusive() bool {
	return inv.Verdict == VerdictInconclusive &&
		len(inv.RootCauses) == 0 && len(inv.Unresolved) == 0 && len(inv.DataGaps) == 0
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
//
// Cause and Resolution are KB body text of external origin (whatever a human wrote
// during review, verbatim), and they are stamped PAST investigate.redactInvestigation
// — so app.onInvestigationComplete secret-redacts them at the assignment. EntryPath
// is a catalog path and stays verbatim.
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
	// ExtraLabels are additional FORGE labels (GitHub/GitLab issue/PR labels)
	// OpenPR appends to its standard lifecycle labels ("runlore", "triggered").
	// Unlike Tags above — which renders into the committed OKF entry's
	// frontmatter and never reaches the forge — ExtraLabels never renders into
	// the entry file; it exists purely so a later pass can recognise and
	// exclude specific PRs from auto-closing (e.g. a standalone operator note
	// — see internal/curate's isOperatorNote). Additive only: OpenPR must
	// APPEND these alongside the lifecycle labels, never replace them with it.
	// Empty/nil for an ordinary curated finding.
	ExtraLabels []string
	Body        string   // markdown
	Fingerprint string   // deterministic dedup fingerprint (see curator.DupFingerprint)
	Confidence  float64  // overall investigation confidence; queryable extension frontmatter (0 = unset)
	Provenance  []string // distinct causing-change refs; queryable extension frontmatter
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
//
// An ERROR return is not necessarily the zero value. A provider reports token
// counts DURING the stream — Anthropic's input usage arrives on the first event
// of all — so a completion that fails after generating (and being billed for)
// thousands of tokens already knows what it cost. Every client in this repo
// therefore returns that cost alongside the error: Usage carries whatever the
// fold observed before it failed, and Attempts how many upstream requests the
// failed exchange took. A caller charging a spend budget bills those; charging
// zero would make a flaky endpoint look free while the bill grows.
//
// EVERY OTHER FIELD MUST BE TREATED AS UNUSABLE when err != nil. Text,
// ToolCalls, StopReason, Truncated and Opaque describe a reply that did not
// happen: the clients here zero them on an error return, and no caller may read
// them off a failed call regardless.
type CompletionResponse struct {
	Text      string
	ToolCalls []ToolCall
	// Usage is the provider-reported token count for this completion. Zero when
	// the provider omits it (older endpoints, or a provider that does not report
	// it), and zero on a failure that happened before the provider reported
	// anything — callers treat the zero value as "unknown", not "zero tokens".
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
	// Attempts is how many upstream HTTP requests this completion actually cost:
	// 1 normally, more when the client retried a transient failure (a network
	// error, 429 or 5xx) before this one succeeded. Usage describes only the
	// attempt that returned a body, so a caller charging a spend budget needs
	// this to bill the ones that failed — a provider bills every request it
	// accepted. 0 when the client does not report it (see AttemptsOf on the
	// error path): callers read 0 as "unknown", i.e. at least one.
	Attempts int
}

// CostOnly reduces a response to what the exchange COST — its Usage and
// Attempts — dropping every field that describes a reply. It is what a client
// returns alongside an error: the token counts the provider reported before the
// failure are real and billed, while a half-folded Text or an unterminated tool
// call is not an answer and must never reach a caller. See CompletionResponse.
func (r CompletionResponse) CostOnly() CompletionResponse {
	return CompletionResponse{Usage: r.Usage, Attempts: r.Attempts}
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
	InputTokens  int `json:"input_tokens"`  // total prompt/input tokens billed, INCLUDING any served from cache (normalized across providers)
	OutputTokens int `json:"output_tokens"` // generated/output tokens in the reply
	// CachedInputTokens is the subset of InputTokens that was a cache READ (Anthropic
	// cache_read_input_tokens, OpenAI prompt_tokens_details.cached_tokens, Gemini
	// cachedContentTokenCount) — the saving. 0 when the provider reports none.
	CachedInputTokens int `json:"cached_input_tokens"`
	// CacheWriteTokens is input tokens WRITTEN to the cache this request (Anthropic
	// cache_creation_input_tokens, billed ~1.25x). 0 for providers that don't report it.
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// Total is the billable token count for one completion: input plus output.
// Neither cache field is added — both CachedInputTokens and CacheWriteTokens
// are subsets of InputTokens after normalization (the Anthropic client, the
// only one reporting a cache write, sets InputTokens to input + cache_read +
// cache_creation), so adding either would double-count. It returns int64
// because every consumer accumulates or reports in that width.
//
// A zero Usage totals 0, which callers must not read as "this call was free":
// see CompletionResponse.Usage on the zero value meaning "unknown".
func (u Usage) Total() int64 {
	return int64(u.InputTokens) + int64(u.OutputTokens)
}

// EstimateTokens approximates a request's input size (~4 chars/token) over
// everything that actually goes over the wire: the system prompt, the full tool
// specs (name + description + JSON Schema), and the message history — including
// the assistant tool-call JSON (m.ToolCalls[].Args). Counting only m.Content
// systematically under-estimates a tool-heavy request. It ignores JSON envelope
// and role overhead, so it stays a mild under-estimate of the true wire size,
// but the right order of magnitude.
//
// It lives here because two callers need a request's size where Usage cannot
// supply one, for opposite reasons: internal/investigate sizes the NEXT request
// against its per-investigation budget, BEFORE any response exists (and
// calibrates this heuristic against provider-reported usage afterwards), while
// internal/thread charges its chat budget an estimate precisely when a
// completion came back with no usage at all — CompletionResponse.Usage's zero
// value meaning "unknown", not "zero tokens".
func EstimateTokens(system string, msgs []Message, tools []ToolSpec) int {
	n := len(system)
	for _, t := range tools {
		n += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Args)
		}
	}
	return n / 4
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
