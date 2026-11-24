// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"time"
)

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
	// LookupUndiffable — objects matching the request WERE found, but none carries enough
	// to locate a source (no applied revision, no sourceRef, or a source that did not
	// resolve), so no repository could be searched. Distinct from LookupAbsent because the
	// object exists: a Kustomization that has never reconciled is a prime suspect, and
	// reporting it as absent hides the very thing the caller is looking for.
	LookupUndiffable LookupReason = "undiffable"
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

// ChangesLookupReporter is an optional capability: a GitOps provider that says what an
// EMPTY Changes result established. Consumers type-assert for it exactly like
// GitOpsInspector.
//
// It exists because an empty change list has several causes and only the provider can tell
// them apart — it is the code doing the skipping. Nothing matched the selector, objects
// matched but none was diffable, or a source read was refused by RBAC and never ran: the
// first is about the request, the second is about an object that EXISTS, and the third is
// not about the cluster at all. Collapsing them is how runlore#503 turned a limit of a tool
// into a claim about the world, and what_changed reproduced it one layer down by answering
// "no changes found for the given selector" to all of them.
//
// Changes() stays on the interface and delegates here, so a provider that does not
// implement this keeps working and every existing caller and test double is unaffected.
type ChangesLookupReporter interface {
	// ChangesLookup returns the same changes as Changes plus, when that list is EMPTY, a
	// Lookup recording what the enumeration established. The Lookup is meaningless when
	// changes is non-empty — the caller has data and needs no explanation of it.
	ChangesLookup(ctx context.Context, w TimeWindow, sel Selector) ([]Change, Lookup, error)
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

// GitOpsFailureLister is an optional capability for one-shot listing of failing
// GitOps objects without starting watch informers (used by `lore status`).
type GitOpsFailureLister interface {
	// ListFailures returns failing GitOps objects, optionally scoped to namespace
	// (empty lists across all namespaces).
	ListFailures(ctx context.Context, namespace string) ([]FailureEvent, error)
}
