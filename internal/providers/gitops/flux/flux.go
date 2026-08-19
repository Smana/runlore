// SPDX-License-Identifier: Apache-2.0

// Package flux implements providers.GitOpsProvider for Flux: it reads Flux
// Kustomizations and their GitRepository sources from the cluster and emits
// engine-agnostic Changes, each diffable through whatchanged.Differ.
package flux

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

// kustomization is the minimal Flux Kustomization data the provider needs.
type kustomization struct {
	Name, Namespace string
	Path            string // spec.path
	TargetNamespace string // spec.targetNamespace (where the applied workloads land)
	SourceKind      string // spec.sourceRef.kind (GitRepository | OCIRepository | Bucket | ExternalArtifact)
	SourceName      string // spec.sourceRef.name
	SourceNamespace string // spec.sourceRef.namespace (defaults to the Kustomization namespace)
	Revision        string // status.lastAppliedRevision
	ReadyStatus     string // status.conditions[type=Ready].status ("True"/"False"/"Unknown")
	ReadyReason     string
	ReadyMessage    string
	// ReadyTime is the Ready condition's lastTransitionTime — the reconcile time,
	// used as the Change.When fallback when the commit time can't be resolved.
	ReadyTime time.Time
}

// gitRepository is the minimal Flux GitRepository data the provider needs.
type gitRepository struct {
	Name, Namespace string
	URL             string // spec.url
}

// KustomizationEvent is a single watch event for a Kustomization.
type KustomizationEvent struct {
	Kustomization kustomization
}

// helmRelease is the minimal Flux HelmRelease data the failure watcher needs: its
// identity and Ready condition. Unlike kustomization it carries no source/path/
// revision — a HelmRelease failure is reported for investigation, not Git-diffed
// through the Kustomization Change path.
type helmRelease struct {
	Name, Namespace string
	ReadyStatus     string // status.conditions[type=Ready].status ("True"/"False"/"Unknown")
	ReadyReason     string
	ReadyMessage    string
}

// HelmReleaseEvent is a single watch event for a HelmRelease.
type HelmReleaseEvent struct {
	HelmRelease helmRelease
}

// Reader is the cluster-read surface the provider depends on. The dynamic
// client-go implementation lives in dynamic.go; tests use a fake.
type Reader interface {
	ListKustomizations(ctx context.Context) ([]kustomization, error)
	GetGitRepository(ctx context.Context, namespace, name string) (gitRepository, error)
	// SourceRevision returns a source's current synced revision
	// (status.artifact.revision) for a GitRepository/OCIRepository/Bucket/
	// ExternalArtifact, used to find a failing Kustomization's HEAD.
	SourceRevision(ctx context.Context, kind, namespace, name string) (string, error)
	WatchKustomizations(ctx context.Context) (<-chan KustomizationEvent, error)
	// WatchHelmReleases watches all Flux HelmReleases (runlore#306): a stuck/failed
	// HelmRelease goes Ready=False without any Kustomization flipping, so it is
	// invisible to WatchKustomizations.
	WatchHelmReleases(ctx context.Context) (<-chan HelmReleaseEvent, error)
	// GetResource fetches one object by kind/namespace/name (kinds in kindToGVR).
	GetResource(ctx context.Context, kind, namespace, name string) (*unstructured.Unstructured, error)
	// ListEvents returns recent Event lines for an involved object.
	ListEvents(ctx context.Context, namespace, name, kind string) ([]string, error)
}

// Provider implements providers.GitOpsProvider for Flux.
type Provider struct {
	reader Reader
	differ *whatchanged.Differ
}

// New builds a Flux provider from a Reader and a Differ.
func New(reader Reader, differ *whatchanged.Differ) *Provider {
	return &Provider{reader: reader, differ: differ}
}

// Changes lists Flux Kustomizations and emits a Change per workload: its source
// repo + path and the currently applied revision (ToRev). FromRev is left empty,
// meaning "the change introduced by ToRev" — resolved at diff time. Each Change's
// When is set to the commit time of ToRev (falling back to the Ready-condition
// reconcile time) so the change can be aligned against symptom timestamps (B1).
//
// Namespace resolution (B2): in the default Flux bootstrap every Kustomization
// lives in flux-system, so a caller asking for a workload's namespace (e.g.
// "harbor") would match nothing on the Kustomization's OWN metadata.namespace.
// matchesNamespace therefore accepts EITHER the object's namespace OR its
// spec.targetNamespace (where the applied workloads land). When the namespace
// filter still yields zero AND a name is given, we retry name-matched across all
// namespaces and flag it in the result so "no changes" can never be a silent false
// negative (mirrors GetResource's flux-system-then-all-namespaces fallback).
//
// TimeWindow (G3): when w is non-zero, each diffable-Git Kustomization emits a Change
// per source revision that landed IN the window (git-log on the resolved source,
// newest-first, capped at whatchanged.MaxWindowRevisions), so "what changed over the last N hours"
// surfaces the whole timeline — not just the current applied revision. A zero/unset
// window falls back to the single-latest-revision behavior. The enumeration is bounded
// (cap + committer-time short-circuit) so a wide window can't explode the output.
func (p *Provider) Changes(ctx context.Context, w providers.TimeWindow, sel providers.Selector) ([]providers.Change, error) {
	changes, _, err := p.ChangesLookup(ctx, w, sel)
	return changes, err
}

// ChangesLookup implements providers.ChangesLookupReporter: the same enumeration as
// Changes, plus what an empty result established. Four of the five `continue`s in
// changesFor below already know which case they hit; this stops throwing that away.
func (p *Provider) ChangesLookup(ctx context.Context, w providers.TimeWindow, sel providers.Selector) ([]providers.Change, providers.Lookup, error) {
	ks, err := p.reader.ListKustomizations(ctx)
	if err != nil {
		return nil, providers.Lookup{Reason: providers.LookupFailed}, err
	}
	scope := sel.Namespace
	if scope == "" {
		scope = providers.AllNamespaces
	}
	changes, lk := p.changesFor(ctx, w, ks, func(k kustomization) bool {
		return matchesNamespace(k, sel.Namespace) && (sel.Name == "" || k.Name == sel.Name)
	})
	lk.Scopes = []string{scope}
	// B2 fallback: the namespace filter found nothing, but a named object might live
	// in another namespace (the flux-system bootstrap layout). Retry by name across
	// every namespace rather than returning a false-negative "no changes".
	if len(changes) == 0 && sel.Name != "" {
		changes, lk = p.changesFor(ctx, w, ks, func(k kustomization) bool { return k.Name == sel.Name })
		lk.Scopes = []string{scope, providers.AllNamespaces}
		if scope == providers.AllNamespaces {
			lk.Scopes = []string{providers.AllNamespaces}
		}
	}
	return changes, lk, nil
}

// changesFor maps the Kustomizations accepted by keep into engine-agnostic Changes,
// resolving each source URL (cached per source) and populating When. With a non-zero
// window it expands each diffable-Git Kustomization into one Change per in-window
// source revision (G3); otherwise it emits the single current-revision Change.
func (p *Provider) changesFor(ctx context.Context, w providers.TimeWindow, ks []kustomization, keep func(kustomization) bool) ([]providers.Change, providers.Lookup) {
	urlCache := map[string]string{}
	var changes []providers.Change
	// LookupAbsent until a Kustomization matches; a match then skipped for being
	// undiffable escalates to LookupUndiffable (the object exists — a Kustomization with
	// no applied revision is often the answer); a source read REFUSED by RBAC escalates
	// further to LookupDenied and is never downgraded, because a search that never ran
	// must not be reported as one that came back empty. That precedence is the whole
	// point: #503 was a denial rendered as an absence.
	lk := providers.Lookup{Reason: providers.LookupAbsent}
	undiffable := func() {
		if lk.Reason != providers.LookupDenied {
			lk.Reason = providers.LookupUndiffable
		}
	}
	for _, k := range ks {
		if !keep(k) {
			continue
		}
		if k.Revision == "" || k.SourceName == "" {
			undiffable()
			continue // nothing applied yet / no source to attribute the change to
		}
		isGit := k.SourceKind == "" || k.SourceKind == "GitRepository"
		// Only a GitRepository source has a Git URL we can diff. Other source kinds
		// (OCIRepository, Bucket, ExternalArtifact — e.g. ArtifactGenerator output)
		// still produced a change worth reporting; emit it without a diffable URL
		// rather than erroring the whole lookup (which is what made "what changed"
		// fail on ArtifactGenerator-based GitOps).
		url := ""
		if isGit {
			if k.Path == "" {
				undiffable()
				continue // a GitRepository change with no path can't be located for a diff
			}
			key := k.SourceNamespace + "/" + k.SourceName
			cached, ok := urlCache[key]
			if !ok {
				gr, err := p.reader.GetGitRepository(ctx, k.SourceNamespace, k.SourceName)
				if err != nil {
					// A Forbidden here is NOT a missing source: the read never happened, so
					// reporting it as "nothing with a resolvable source" is #503's mechanism
					// (a denial collapsed into an absence) reappearing on this path. It was a
					// bare `continue` before, which is exactly how that bug shipped last time.
					if apierrors.IsForbidden(err) {
						lk.Reason = providers.LookupDenied
					} else {
						undiffable()
					}
					continue // source not resolvable (missing/transient/refused) — skip, don't abort
				}
				cached = gr.URL
				urlCache[key] = cached
			}
			if cached == "" {
				undiffable()
				continue
			}
			url = cached
		}
		fromRev, toRev := revisionRange(ctx, p.reader, k)
		// G3: with a window set and a diffable Git source, emit a Change per revision
		// that landed in the window (newest-first, capped). Fall back to the single
		// current-revision Change when the window is zero, the source isn't Git, or
		// the enumeration yields nothing (clone/log failure — never abort the lookup).
		if wchanges := p.windowChanges(ctx, w, k, url, isGit, toRev); len(wchanges) > 0 {
			changes = append(changes, wchanges...)
			continue
		}
		c := mapKustomization(k, url, fromRev, toRev)
		c.When = p.changeTime(ctx, url, toRev, k.ReadyTime)
		changes = append(changes, c)
	}
	return changes, lk
}

// windowChanges expands a diffable-Git Kustomization into one Change per source
// revision that landed within w (newest-first, capped by whatchanged.MaxWindowRevisions). Each
// Change diffs the revision against its parent (FromRev="") and carries the commit's
// time as When, so the model sees the whole in-window timeline instead of only the
// current applied revision (G3). It returns nil — so the caller keeps today's
// single-revision behavior — when the window is zero, the source is not diffable Git,
// no Differ is wired, or the enumeration finds/​resolves nothing.
func (p *Provider) windowChanges(ctx context.Context, w providers.TimeWindow, k kustomization, url string, isGit bool, toRev string) []providers.Change {
	if p.differ == nil || !isGit || url == "" || toRev == "" || (w.Start.IsZero() && w.End.IsZero()) {
		return nil
	}
	revs, err := p.differ.RevisionsInWindow(ctx, url, toRev, k.Path, w, whatchanged.MaxWindowRevisions)
	if err != nil || len(revs) == 0 {
		return nil // clone/log failure or nothing in window — fall back to current revision
	}
	out := make([]providers.Change, 0, len(revs))
	for _, r := range revs {
		c := mapKustomization(k, url, "", r.SHA)
		c.When = r.When
		out = append(out, c)
	}
	return out
}

// matchesNamespace reports whether a Kustomization belongs to ns, accepting EITHER
// its own metadata.namespace OR its spec.targetNamespace. An empty ns matches all.
// This is the B2 fix: in the standard bootstrap the Kustomization lives in
// flux-system but applies workloads into targetNamespace, so a query keyed on the
// workload namespace must resolve to the owning object.
func matchesNamespace(k kustomization, ns string) bool {
	return ns == "" || k.Namespace == ns || k.TargetNamespace == ns
}

// changeTime resolves the Change.When for a Kustomization (B1): the ToRev commit's
// committer timestamp — the moment the change landed in Git — falling back to the
// Ready-condition reconcile time when the commit can't be resolved (no diffable
// URL, clone failure, or unresolvable rev). Zero when neither is available.
func (p *Provider) changeTime(ctx context.Context, url, toRev string, readyTime time.Time) time.Time {
	if url != "" && toRev != "" && p.differ != nil {
		if t, err := p.differ.CommitTime(ctx, url, toRev); err == nil && !t.IsZero() {
			return t
		}
	}
	return readyTime // reconcile time (or zero)
}

// revisionRange picks the (FromRev, ToRev) a Change should diff. For a healthy
// Kustomization it is ("", lastApplied) — the change introduced by the applied
// revision. For a FAILING one (Ready != True) whose source HEAD has moved past
// lastApplied, it is (lastApplied, HEAD) — a best-effort attempt to span the
// breaking commit for the case where Flux held lastAppliedRevision at the last-good
// revision (e.g. an apply failure).
//
// This is only a heuristic. On a health-check *failure* Flux applies the manifest —
// advancing lastAppliedRevision to (or past) the breaking commit — before the health
// gate fails, so the change is at/behind lastApplied and this forward range can miss
// it entirely. That case is recovered downstream: Differ.ForChange falls back to the
// newest commit that actually touched the resource's path (RunLore #239). The source
// read is best-effort — on any error (or when HEAD == lastApplied) we fall back to
// the single-revision range.
func revisionRange(ctx context.Context, reader Reader, k kustomization) (fromRev, toRev string) {
	lastApplied := parseRevision(k.Revision)
	if k.ReadyStatus == "True" || k.ReadyStatus == "" {
		return "", lastApplied
	}
	head, err := reader.SourceRevision(ctx, k.SourceKind, k.SourceNamespace, k.SourceName)
	if err != nil {
		return "", lastApplied // can't resolve HEAD — keep today's behavior
	}
	headRev := parseRevision(head)
	if headRev == "" || headRev == lastApplied {
		return "", lastApplied // no real gap to span
	}
	return lastApplied, headRev
}

// Diff resolves a Change's diff via the Differ. Changes from a non-Git source
// (OCIRepository/Bucket/ExternalArtifact) carry no Git URL — there is no Git diff
// to show, so return an empty diff rather than failing. ctx is threaded into the
// Differ so a hung clone/patch on a large monorepo is cancellable (per-investigation
// deadline).
func (p *Provider) Diff(ctx context.Context, c providers.Change) (providers.Diff, error) {
	if c.Source.RepoURL == "" {
		return providers.Diff{}, nil
	}
	return p.differ.ForChange(ctx, c)
}

// WatchFailures watches Flux Kustomizations AND HelmReleases and emits a
// FailureEvent whenever one is Ready=False (a failed/blocked reconcile). Both are
// watched because a HelmRelease can fail on its own (image pull, failed hook,
// exhausted install retries) without any Kustomization flipping — that class of
// failure was previously invisible to the autonomous gitops-watch (runlore#306).
// The two streams are merged onto one channel, which closes when both watches end
// or ctx is done.
func (p *Provider) WatchFailures(ctx context.Context) (<-chan providers.FailureEvent, error) {
	ksrc, err := p.reader.WatchKustomizations(ctx)
	if err != nil {
		return nil, err
	}
	hsrc, err := p.reader.WatchHelmReleases(ctx)
	if err != nil {
		return nil, err
	}

	out := make(chan providers.FailureEvent)
	// emit sends fe unless ctx is cancelled; returns false to stop the caller loop.
	emit := func(fe providers.FailureEvent) bool {
		select {
		case out <- fe:
			return true
		case <-ctx.Done():
			return false
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	// Kustomization failures.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ksrc:
				if !ok {
					return
				}
				k := ev.Kustomization
				if k.ReadyStatus != "False" {
					continue
				}
				if !emit(providers.FailureEvent{
					Workload: providers.Workload{Kind: "Kustomization", Name: k.Name, Namespace: k.Namespace},
					Engine:   providers.EngineFlux,
					Reason:   k.ReadyReason,
					Message:  k.ReadyMessage,
				}) {
					return
				}
			}
		}
	}()
	// HelmRelease failures (runlore#306).
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-hsrc:
				if !ok {
					return
				}
				h := ev.HelmRelease
				if h.ReadyStatus != "False" {
					continue
				}
				if !emit(providers.FailureEvent{
					Workload: providers.Workload{Kind: "HelmRelease", Name: h.Name, Namespace: h.Namespace},
					Engine:   providers.EngineFlux,
					Reason:   h.ReadyReason,
					Message:  h.ReadyMessage,
				}) {
					return
				}
			}
		}
	}()
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, nil
}

// mapKustomization builds an engine-agnostic Change from a Kustomization, its
// resolved source URL, and the (fromRev, toRev) range computed by revisionRange.
// A healthy Kustomization carries an empty fromRev (diff the change introduced by
// toRev); a failing one whose source HEAD moved past lastApplied spans the gap.
func mapKustomization(k kustomization, repoURL, fromRev, toRev string) providers.Change {
	return providers.Change{
		Workload: providers.Workload{Kind: "Kustomization", Name: k.Name, Namespace: k.Namespace},
		Engine:   providers.EngineFlux,
		Type:     providers.ChangeSync,
		Source:   providers.SourceRef{RepoURL: repoURL, Path: k.Path},
		FromRev:  fromRev,
		ToRev:    toRev,
	}
}

// parseRevision extracts the commit SHA from a Flux/ArgoCD revision string:
// Flux v1 "<ref>@sha1:<sha>" / "<ref>@sha256:<sha>", legacy "<ref>/<sha>", or a
// bare "<sha>".
func parseRevision(rev string) string {
	if i := strings.LastIndex(rev, ":"); i >= 0 {
		return rev[i+1:]
	}
	if i := strings.LastIndex(rev, "/"); i >= 0 {
		return rev[i+1:]
	}
	return rev
}

// GitOpsEngine reports the engine this provider speaks, so a consumer can describe the
// deployment from the provider it was handed instead of from the unvalidated
// gitops.engine string. It is the same fact every Change already carries.
func (p *Provider) GitOpsEngine() providers.Engine { return providers.EngineFlux }

// ResourceStatus returns a Flux/K8s object's Ready condition, key spec refs
// (sourceRef, dependsOn, url) and recent Events — the "why is it failing" lens.
// A read that returns no object is reported via NotFound + Lookup, not an error, so the
// caller can say what the negative established rather than assert the object is absent.
func (p *Provider) ResourceStatus(ctx context.Context, w providers.Workload) (providers.ResourceStatus, error) {
	rs := providers.ResourceStatus{Workload: w, Refs: map[string]string{}}
	u, err := p.reader.GetResource(ctx, w.Kind, w.Namespace, w.Name)
	if lk, ok := providers.LookupOf(err); ok {
		// A kind this provider cannot resolve is a scope limit, not a negative about the
		// cluster, so it stays an ERROR — which the loop already reads as missing data.
		if lk.Reason == providers.LookupUnresolvable {
			return rs, err
		}
		rs.NotFound, rs.Lookup = true, lk
		return rs, nil
	}
	if apierrors.IsNotFound(err) {
		rs.NotFound, rs.Lookup = true, providers.Lookup{Reason: providers.LookupAbsent}
		return rs, nil
	}
	if err != nil {
		return rs, err
	}
	rs.Ready, rs.Reason, rs.Message = readyCondition(u)
	if ref := sourceRef(u, w.Namespace); ref != "" {
		rs.Refs["sourceRef"] = ref
	}
	if deps := dependsOn(u, w.Kind, w.Namespace); len(deps) > 0 {
		names := make([]string, 0, len(deps))
		for _, d := range deps {
			names = append(names, d.Namespace+"/"+d.Name)
		}
		rs.Refs["dependsOn"] = strings.Join(names, ",")
	}
	if url, ok, _ := unstructured.NestedString(u.Object, "spec", "url"); ok && url != "" {
		rs.Refs["url"] = url
	}
	rs.Events, _ = p.reader.ListEvents(ctx, w.Namespace, w.Name, w.Kind) // best-effort
	return rs, nil
}

// DependencyTree walks a resource's dependsOn + sourceRef edges, returning the
// tree with each node's Ready state so the root failure (a not-Ready or missing
// node) is visible. Best-effort: child read errors don't abort the walk.
func (p *Provider) DependencyTree(ctx context.Context, w providers.Workload) (providers.DepNode, error) {
	return p.depNode(ctx, w, map[string]bool{}), nil
}

func (p *Provider) depNode(ctx context.Context, w providers.Workload, seen map[string]bool) providers.DepNode {
	node := providers.DepNode{Workload: w}
	key := w.Kind + "/" + w.Namespace + "/" + w.Name
	if seen[key] {
		return node // cycle guard
	}
	seen[key] = true
	u, err := p.reader.GetResource(ctx, w.Kind, w.Namespace, w.Name)
	if lk, ok := providers.LookupOf(err); ok {
		// Every negative is recorded, including the ones that are NOT about the object:
		// an unresolvable kind (a sourceRef into a kind this provider has no mapping for
		// — this repo's own eval walks into an ArtifactGenerator) used to fall through to
		// "leave Ready empty", which renders as "(Ready=unknown)" and asserts a node
		// nobody looked up.
		node.NotFound = lk.Reason == providers.LookupAbsent
		node.Lookup = lk
		return node
	}
	if apierrors.IsNotFound(err) {
		node.NotFound = true
		node.Lookup = providers.Lookup{Reason: providers.LookupAbsent}
		return node
	}
	if err != nil {
		node.Lookup = providers.Lookup{Reason: providers.LookupFailed}
		return node // transient — keep walking siblings, but do not claim the node exists
	}
	node.Ready, node.Reason, _ = readyCondition(u)
	for _, dep := range dependsOn(u, w.Kind, w.Namespace) {
		node.Children = append(node.Children, p.depNode(ctx, dep, seen))
	}
	if src, ok := sourceRefWorkload(u, w.Namespace); ok {
		node.Children = append(node.Children, p.depNode(ctx, src, seen))
	}
	return node
}

// sourceRefWorkload reads spec.sourceRef as a Workload (namespace defaulting to the
// parent's). ok is false when there is no sourceRef.kind to follow.
func sourceRefWorkload(u *unstructured.Unstructured, defaultNS string) (providers.Workload, bool) {
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "kind")
	if kind == "" {
		return providers.Workload{}, false
	}
	name, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "name")
	ns, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "namespace")
	if ns == "" {
		ns = defaultNS
	}
	return providers.Workload{Kind: kind, Name: name, Namespace: ns}, true
}

// sourceRef renders "kind/namespace/name" of spec.sourceRef, or "".
func sourceRef(u *unstructured.Unstructured, defaultNS string) string {
	name, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "name")
	if name == "" {
		return ""
	}
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "kind")
	ns, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "namespace")
	if ns == "" {
		ns = defaultNS
	}
	return fmt.Sprintf("%s/%s/%s", kind, ns, name)
}

// dependsOn reads spec.dependsOn (same-kind references); namespace defaults to the
// parent's.
func dependsOn(u *unstructured.Unstructured, parentKind, defaultNS string) []providers.Workload {
	raw, found, _ := unstructured.NestedSlice(u.Object, "spec", "dependsOn")
	if !found {
		return nil
	}
	var out []providers.Workload
	for _, d := range raw {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		ns, _ := m["namespace"].(string)
		if ns == "" {
			ns = defaultNS
		}
		out = append(out, providers.Workload{Kind: parentKind, Name: name, Namespace: ns})
	}
	return out
}

// compile-time check that Provider satisfies the contracts.
var (
	_ providers.GitOpsProvider  = (*Provider)(nil)
	_ providers.GitOpsInspector = (*Provider)(nil)
)
