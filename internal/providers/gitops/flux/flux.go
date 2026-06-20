// Package flux implements providers.GitOpsProvider for Flux: it reads Flux
// Kustomizations and their GitRepository sources from the cluster and emits
// engine-agnostic Changes, each diffable through whatchanged.Differ.
package flux

import (
	"context"
	"strings"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

// kustomization is the minimal Flux Kustomization data the provider needs.
type kustomization struct {
	Name, Namespace string
	Path            string // spec.path
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

// Reader is the cluster-read surface the provider depends on. The dynamic
// client-go implementation lives in dynamic.go; tests use a fake.
type Reader interface {
	ListKustomizations(ctx context.Context) ([]kustomization, error)
	GetGitRepository(ctx context.Context, namespace, name string) (gitRepository, error)
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
		if k.Path == "" || k.Revision == "" || k.SourceName == "" {
			continue // not enough to locate a source diff
		}
		key := k.SourceNamespace + "/" + k.SourceName
		url, ok := urlCache[key]
		if !ok {
			gr, err := p.reader.GetGitRepository(ctx, k.SourceNamespace, k.SourceName)
			if err != nil {
				return nil, err
			}
			url = gr.URL
			urlCache[key] = url
		}
		if url == "" {
			continue
		}
		changes = append(changes, mapKustomization(k, url))
	}
	return changes, nil
}

// Diff resolves a Change's diff via the Differ.
func (p *Provider) Diff(_ context.Context, c providers.Change) (providers.Diff, error) {
	return p.differ.ForChange(c)
}

// WatchFailures is not implemented yet (next plan: watch Kustomization
// Ready=False / source FetchFailed). It returns a closed channel.
func (p *Provider) WatchFailures(context.Context) (<-chan providers.FailureEvent, error) {
	ch := make(chan providers.FailureEvent)
	close(ch)
	return ch, nil
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

// compile-time check that Provider satisfies the contract.
var _ providers.GitOpsProvider = (*Provider)(nil)
