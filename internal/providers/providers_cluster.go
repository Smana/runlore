// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"time"
)

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

// ResourceListQuery identifies the objects to enumerate.
//
// It is deliberately the same shape as ResourceSpecQuery minus Name: the caller knows a
// Kind and a place to look, and that is exactly the situation Name cannot be supplied in.
type ResourceListQuery struct {
	Kind      string
	Namespace string
	// Group narrows resolution to one API group ("" means "no preference", and "core"
	// is accepted as a spelling of the core group's empty name).
	Group string
	// LabelSelector optionally narrows the listing, in the standard
	// "key=value,key2=value2" form. Empty lists everything of the kind.
	LabelSelector string
}

// ResourceListItem is one object's identity in a listing.
//
// Identity ONLY — no spec, no status. The listing answers "what exists here"; reading any
// one of them is resource_spec's job, and returning specs here would blow the tool-output
// budget on objects the caller has not asked about yet.
type ResourceListItem struct {
	Name      string
	Namespace string // empty for a cluster-scoped kind
}

// ResourceList is the set of objects of one kind in one place.
//
// Outcome reuses ResourceSpecOutcome because the taxonomy is the same and the distinctions
// are the same ones that matter: a FORBIDDEN listing is not an empty namespace, and an
// unknown kind is not an absent object.
//
// The load-bearing difference from ResourceSpec: ResourceFound with zero Items IS
// meaningful evidence — "this cluster serves the kind, this agent may read it, and there
// are none here". That is the answer resource_spec structurally cannot give, because it
// can only ask about names somebody already guessed.
type ResourceList struct {
	// Query echoes the request NORMALIZED to what was actually listed — the Kind in the
	// casing the server serves it under, and no namespace for a cluster-scoped kind.
	Query      ResourceListQuery
	Outcome    ResourceSpecOutcome
	APIVersion string
	Items      []ResourceListItem
	// Truncated reports that the server had more objects than were returned, so an
	// absent name in Items is not evidence the object does not exist.
	Truncated bool
	// Detail carries the server's own message for a non-found outcome.
	Detail string
}

// ResourceLister enumerates the objects of one kind in one namespace.
//
// It exists because resource_spec can only read a name somebody already knows, and a large
// class of incidents is "which object is doing this to me" — which NetworkPolicy denies
// this flow, which ValidatingWebhookConfiguration rejects this apply, which ScaledObject
// governs this Deployment. Without enumeration a model can only guess names: a real
// investigation spent 32 of its 58 tool calls guessing CiliumNetworkPolicy names, never
// found the object, and published a card whose own Data gaps section said "resource_spec
// reads named objects only (not lists)". It had confirmed the denial and still could not
// name the policy.
//
// Implementations MUST apply the same refusals as ResourceSpecReader (Secret by kind AND
// by resolved resource name) and MUST report RBAC denials as ResourceForbidden rather than
// as an empty listing — an empty listing is evidence of absence and a denial is not.
type ResourceLister interface {
	ResourceList(ctx context.Context, q ResourceListQuery) (ResourceList, error)
}

// ResourceReader is the pair of reads one dynamic client plus discovery can serve:
// enumerate a kind, then read one of the objects by name.
//
// They are one interface because they are one capability with one set of prerequisites,
// and because registering the by-name half alone is the failure mode this pairing exists
// to prevent — a model with resource_spec and no lister guesses names, and guesses wrong.
type ResourceReader interface {
	ResourceSpecReader
	ResourceLister
}

// KindScoper answers whether a bare Kubernetes Kind is namespaced ON THIS CLUSTER.
//
// It exists so namespaced-ness can be carried as DATA (Workload.Scope) instead of
// guessed downstream from the kind's name. The guess cannot be made correct: any
// operator may ship a CRD under any Kind, and several ship namespaced CRDs spelled
// exactly like cluster-scoped or non-Kubernetes things — ACK's DBInstance, DBCluster,
// CacheCluster, Nodegroup, TargetGroup, LaunchTemplate and LoadBalancer, Crossplane
// v2's namespaced managed resources, HyperShift's NodePool against Karpenter's. The
// API server's own discovery already knows, and the process already reads it.
//
// The contract's three-valued answer is the load-bearing part. An implementation MUST
// return ScopeUnknown — never a guess and never an error — when the cluster serves no
// such kind, when two API groups serve it and disagree, or when the kind is empty:
// callers degrade to their prior behaviour on unknown, and any other answer would turn
// "nobody knows" into a fact. An error is reserved for "discovery itself failed", and
// carries ScopeUnknown with it.
type KindScoper interface {
	KindScope(ctx context.Context, kind string) (ResourceScope, error)
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
