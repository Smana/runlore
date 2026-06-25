// Package argocd implements providers.GitOpsProvider for Argo CD: it reads Argo CD
// Applications from the cluster and emits engine-agnostic Changes (diffable via
// whatchanged.Differ) and failure events — the same contract as the flux package.
package argocd

import (
	"context"
	"strings"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

// application is the minimal Argo CD Application data the provider needs.
type application struct {
	Name, Namespace string
	RepoURL         string // spec.source.repoURL
	Path            string // spec.source.path
	Revision        string // status.sync.revision (current)
	PrevRevision    string // previous status.history revision (diff range start)
	HealthStatus    string // status.health.status
	SyncStatus      string // status.sync.status
	Message         string // status.operationState.message (failure context)
}

// ApplicationEvent is a single watch event for an Application.
type ApplicationEvent struct {
	Application application
}

// Reader is the cluster-read surface the provider depends on. The dynamic
// client-go implementation lives in dynamic.go; tests use a fake.
type Reader interface {
	ListApplications(ctx context.Context) ([]application, error)
	WatchApplications(ctx context.Context) (<-chan ApplicationEvent, error)
}

// Provider implements providers.GitOpsProvider for Argo CD.
type Provider struct {
	reader Reader
	differ *whatchanged.Differ
}

// New builds an Argo CD provider from a Reader and a Differ.
func New(reader Reader, differ *whatchanged.Differ) *Provider {
	return &Provider{reader: reader, differ: differ}
}

// Changes lists Argo CD Applications and emits a Change per app: its source repo +
// path, the deployed revision (ToRev), and the previously deployed revision
// (FromRev, from the rollout history) when available.
//
// NOTE (v1 scope): the window is accepted but not yet used to filter by deploy
// time; multi-source Applications (spec.sources) use only spec.source for now.
func (p *Provider) Changes(ctx context.Context, _ providers.TimeWindow, sel providers.Selector) ([]providers.Change, error) {
	apps, err := p.reader.ListApplications(ctx)
	if err != nil {
		return nil, err
	}
	var changes []providers.Change
	for _, a := range apps {
		if sel.Namespace != "" && a.Namespace != sel.Namespace {
			continue
		}
		if sel.Name != "" && a.Name != sel.Name {
			continue
		}
		if a.RepoURL == "" || a.Revision == "" {
			continue // not enough to locate a source diff
		}
		changes = append(changes, mapApplication(a))
	}
	return changes, nil
}

// Diff resolves a Change's diff via the Differ. ctx is threaded into the Differ so
// a hung clone/patch is cancellable (per-investigation deadline).
func (p *Provider) Diff(ctx context.Context, c providers.Change) (providers.Diff, error) {
	return p.differ.ForChange(ctx, c)
}

// WatchFailures watches Argo CD Applications and emits a FailureEvent whenever one
// is Degraded. The returned channel closes when the watch ends or ctx is done.
func (p *Provider) WatchFailures(ctx context.Context) (<-chan providers.FailureEvent, error) {
	src, err := p.reader.WatchApplications(ctx)
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
				a := ev.Application
				if a.HealthStatus != "Degraded" {
					continue
				}
				fe := providers.FailureEvent{
					Workload: providers.Workload{Kind: "Application", Name: a.Name, Namespace: a.Namespace},
					Engine:   providers.EngineArgoCD,
					Reason:   a.HealthStatus,
					Message:  a.Message,
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

// mapApplication builds an engine-agnostic Change from an Application.
func mapApplication(a application) providers.Change {
	return providers.Change{
		Workload: providers.Workload{Kind: "Application", Name: a.Name, Namespace: a.Namespace},
		Engine:   providers.EngineArgoCD,
		Type:     providers.ChangeSync,
		Source:   providers.SourceRef{RepoURL: a.RepoURL, Path: a.Path},
		FromRev:  parseRevision(a.PrevRevision),
		ToRev:    parseRevision(a.Revision),
	}
}

// parseRevision extracts the commit SHA from a revision string. Argo CD revisions
// are usually bare SHAs; tolerate "<ref>@sha1:<sha>" and "<ref>/<sha>" too.
func parseRevision(rev string) string {
	if i := strings.LastIndex(rev, ":"); i >= 0 {
		return rev[i+1:]
	}
	if i := strings.LastIndex(rev, "/"); i >= 0 {
		return rev[i+1:]
	}
	return rev
}

var _ providers.GitOpsProvider = (*Provider)(nil)
