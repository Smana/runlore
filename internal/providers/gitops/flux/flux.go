// Package flux implements providers.GitOpsProvider for Flux: it reads Flux
// Kustomizations and their GitRepository sources from the cluster and emits
// engine-agnostic Changes, each diffable through whatchanged.Differ.
package flux

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

// kustomization is the minimal Flux Kustomization data the provider needs.
type kustomization struct {
	Name, Namespace string
	Path            string // spec.path
	SourceKind      string // spec.sourceRef.kind (GitRepository | OCIRepository | Bucket | ExternalArtifact)
	SourceName      string // spec.sourceRef.name
	SourceNamespace string // spec.sourceRef.namespace (defaults to the Kustomization namespace)
	Revision        string // status.lastAppliedRevision
	ReadyStatus     string // status.conditions[type=Ready].status ("True"/"False"/"Unknown")
	ReadyReason     string
	ReadyMessage    string
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

// Reader is the cluster-read surface the provider depends on. The dynamic
// client-go implementation lives in dynamic.go; tests use a fake.
type Reader interface {
	ListKustomizations(ctx context.Context) ([]kustomization, error)
	GetGitRepository(ctx context.Context, namespace, name string) (gitRepository, error)
	WatchKustomizations(ctx context.Context) (<-chan KustomizationEvent, error)
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
// meaning "the change introduced by ToRev" — resolved at diff time.
//
// NOTE (v1 scope): the window is accepted but not yet used to filter by commit
// time; each Change reflects the current applied revision. Git-log-based
// time-windowing is a follow-up.
func (p *Provider) Changes(ctx context.Context, _ providers.TimeWindow, sel providers.Selector) ([]providers.Change, error) {
	ks, err := p.reader.ListKustomizations(ctx)
	if err != nil {
		return nil, err
	}
	urlCache := map[string]string{}
	var changes []providers.Change
	for _, k := range ks {
		if sel.Namespace != "" && k.Namespace != sel.Namespace {
			continue
		}
		if sel.Name != "" && k.Name != sel.Name {
			continue
		}
		if k.Revision == "" || k.SourceName == "" {
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
				continue // a GitRepository change with no path can't be located for a diff
			}
			key := k.SourceNamespace + "/" + k.SourceName
			cached, ok := urlCache[key]
			if !ok {
				gr, err := p.reader.GetGitRepository(ctx, k.SourceNamespace, k.SourceName)
				if err != nil {
					continue // source not resolvable (missing/transient) — skip, don't abort
				}
				cached = gr.URL
				urlCache[key] = cached
			}
			if cached == "" {
				continue
			}
			url = cached
		}
		changes = append(changes, mapKustomization(k, url))
	}
	return changes, nil
}

// Diff resolves a Change's diff via the Differ. Changes from a non-Git source
// (OCIRepository/Bucket/ExternalArtifact) carry no Git URL — there is no Git diff
// to show, so return an empty diff rather than failing.
func (p *Provider) Diff(_ context.Context, c providers.Change) (providers.Diff, error) {
	if c.Source.RepoURL == "" {
		return providers.Diff{}, nil
	}
	return p.differ.ForChange(c)
}

// WatchFailures watches Flux Kustomizations and emits a FailureEvent whenever one
// is Ready=False (a failed/blocked reconcile). The returned channel closes when
// the watch ends or ctx is done.
func (p *Provider) WatchFailures(ctx context.Context) (<-chan providers.FailureEvent, error) {
	src, err := p.reader.WatchKustomizations(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan providers.FailureEvent)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-src:
				if !ok {
					return
				}
				k := ev.Kustomization
				if k.ReadyStatus != "False" {
					continue
				}
				fe := providers.FailureEvent{
					Workload: providers.Workload{Kind: "Kustomization", Name: k.Name, Namespace: k.Namespace},
					Engine:   providers.EngineFlux,
					Reason:   k.ReadyReason,
					Message:  k.ReadyMessage,
				}
				select {
				case out <- fe:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// mapKustomization builds an engine-agnostic Change from a Kustomization + its
// resolved source URL.
func mapKustomization(k kustomization, repoURL string) providers.Change {
	return providers.Change{
		Workload: providers.Workload{Kind: "Kustomization", Name: k.Name, Namespace: k.Namespace},
		Engine:   providers.EngineFlux,
		Type:     providers.ChangeSync,
		Source:   providers.SourceRef{RepoURL: repoURL, Path: k.Path},
		ToRev:    parseRevision(k.Revision),
		// FromRev empty: diff the change introduced by ToRev.
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

// ResourceStatus returns a Flux/K8s object's Ready condition, key spec refs
// (sourceRef, dependsOn, url) and recent Events — the "why is it failing" lens.
// A missing object is reported via NotFound (often the cascade root), not an error.
func (p *Provider) ResourceStatus(ctx context.Context, w providers.Workload) (providers.ResourceStatus, error) {
	rs := providers.ResourceStatus{Workload: w, Refs: map[string]string{}}
	u, err := p.reader.GetResource(ctx, w.Kind, w.Namespace, w.Name)
	if apierrors.IsNotFound(err) {
		rs.NotFound = true
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
	if apierrors.IsNotFound(err) {
		node.NotFound = true
		return node
	}
	if err != nil {
		return node // unknown kind / transient — leave Ready empty, keep walking siblings
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
