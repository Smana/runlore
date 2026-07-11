// SPDX-License-Identifier: Apache-2.0

// Package argocd implements providers.GitOpsProvider for Argo CD: it reads Argo CD
// Applications from the cluster and emits engine-agnostic Changes (diffable via
// whatchanged.Differ) and failure events — the same contract as the flux package.
package argocd

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

// application is the minimal Argo CD Application data the provider needs. Source
// fields hold the FIRST source for a multi-source app (spec.sources[]), falling
// back from the singular spec.source — see applicationFromUnstructured.
type application struct {
	Name, Namespace string
	DestNamespace   string    // spec.destination.namespace (where the workloads land)
	RepoURL         string    // spec.source.repoURL (or spec.sources[0].repoURL)
	Path            string    // spec.source.path (or spec.sources[0].path)
	Revision        string    // status.sync.revision (or status.sync.revisions[0])
	PrevRevision    string    // previous status.history revision (diff range start)
	DeployedAt      time.Time // status.history[last].deployedAt (the change→symptom time anchor)
	HealthStatus    string    // status.health.status
	SyncStatus      string    // status.sync.status
	OperationPhase  string    // status.operationState.phase (Failed/Error => sync failed)
	Message         string    // status.operationState.message (failure context)
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
	// GetApplication fetches one Application as unstructured (deep inspection needs
	// status.conditions + status.resources, which the minimal `application` omits). A
	// NotFound error is returned verbatim so callers can distinguish "missing".
	GetApplication(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error)
	// ListEvents returns recent Event lines for an involved object.
	ListEvents(ctx context.Context, namespace, name, kind string) ([]string, error)
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
// Multi-source Applications (spec.sources[]) are supported: the first source +
// first revision back the Change (see applicationFromUnstructured). An app that
// still resolves no diffable source is logged at Debug and skipped, so the blind
// spot is observable rather than silent.
//
// Namespace resolution (B2): an Application lives in the argocd namespace but
// deploys into spec.destination.namespace, so a query keyed on the workload
// namespace must match EITHER. When the filter still yields zero and a name is
// given, we retry name-matched across all namespaces so "no changes" is never a
// silent false negative — mirroring the flux provider.
//
// NOTE (v1 scope): the window is accepted but not yet used to filter by deploy
// time; each Change reflects the current synced revision.
func (p *Provider) Changes(ctx context.Context, _ providers.TimeWindow, sel providers.Selector) ([]providers.Change, error) {
	apps, err := p.reader.ListApplications(ctx)
	if err != nil {
		return nil, err
	}
	changes := changesFor(apps, func(a application) bool {
		return matchesNamespace(a, sel.Namespace) && (sel.Name == "" || a.Name == sel.Name)
	})
	if len(changes) == 0 && sel.Name != "" {
		changes = changesFor(apps, func(a application) bool { return a.Name == sel.Name })
	}
	return changes, nil
}

// changesFor maps the Applications accepted by keep into engine-agnostic Changes.
func changesFor(apps []application, keep func(application) bool) []providers.Change {
	var changes []providers.Change
	for _, a := range apps {
		if !keep(a) {
			continue
		}
		if a.RepoURL == "" || a.Revision == "" {
			slog.Debug("argocd: skipping application with no diffable source",
				"application", a.Namespace+"/"+a.Name,
				"hasRepoURL", a.RepoURL != "", "hasRevision", a.Revision != "")
			continue // not enough to locate a source diff
		}
		changes = append(changes, mapApplication(a))
	}
	return changes
}

// matchesNamespace reports whether an Application belongs to ns, accepting EITHER
// its own metadata.namespace OR its spec.destination.namespace (B2). Empty matches all.
func matchesNamespace(a application, ns string) bool {
	return ns == "" || a.Namespace == ns || a.DestNamespace == ns
}

// Diff resolves a Change's diff via the Differ. ctx is threaded into the Differ so
// a hung clone/patch is cancellable (per-investigation deadline).
func (p *Provider) Diff(ctx context.Context, c providers.Change) (providers.Diff, error) {
	return p.differ.ForChange(ctx, c)
}

// WatchFailures watches Argo CD Applications and emits a FailureEvent when an app
// is health-Degraded OR its last sync operation FAILED (operationState.phase ∈
// {Failed, Error}). The sync-operation check catches a failed reconcile that has
// not (yet) manifested as Degraded health — a signal the health-only check missed.
// The returned channel closes when the watch ends or ctx is done.
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
				reason, failed := failureReason(ev.Application)
				if !failed {
					continue
				}
				a := ev.Application
				fe := providers.FailureEvent{
					Workload: providers.Workload{Kind: "Application", Name: a.Name, Namespace: a.Namespace},
					Engine:   providers.EngineArgoCD,
					Reason:   reason,
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

// failureReason classifies an Application as failing or not. It fires on
// health-Degraded and on a failed sync operation (operationState.phase ∈
// {Failed, Error}); the returned reason names which condition fired (health takes
// precedence in the reason string). It deliberately does NOT fire on OutOfSync,
// which is the steady state of any app with auto-sync off or mid-drift, not a
// failure.
func failureReason(a application) (reason string, failed bool) {
	if a.HealthStatus == "Degraded" {
		return a.HealthStatus, true
	}
	if a.OperationPhase == "Failed" || a.OperationPhase == "Error" {
		return "Sync" + a.OperationPhase, true
	}
	return "", false
}

// mapApplication builds an engine-agnostic Change from an Application. When carries
// the last deploy time (status.history[last].deployedAt) so the change can be
// aligned against symptom timestamps (B1).
func mapApplication(a application) providers.Change {
	return providers.Change{
		Workload: providers.Workload{Kind: "Application", Name: a.Name, Namespace: a.Namespace},
		Engine:   providers.EngineArgoCD,
		Type:     providers.ChangeSync,
		When:     a.DeployedAt,
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

var (
	_ providers.GitOpsProvider  = (*Provider)(nil)
	_ providers.GitOpsInspector = (*Provider)(nil)
)
