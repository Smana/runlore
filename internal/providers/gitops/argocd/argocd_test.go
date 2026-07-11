// SPDX-License-Identifier: Apache-2.0

package argocd

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

func TestParseRevision(t *testing.T) {
	cases := map[string]string{
		"abc123":           "abc123", // bare SHA (Argo CD default)
		"main@sha1:def456": "def456",
		"main/abc789":      "abc789",
		"":                 "",
	}
	for in, want := range cases {
		if got := parseRevision(in); got != want {
			t.Errorf("parseRevision(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeReader struct {
	apps       []application
	events     []ApplicationEvent
	obj        *unstructured.Unstructured // returned by GetApplication
	notFound   bool                       // GetApplication returns a NotFound error
	eventLines []string                   // returned by ListEvents
}

func (f fakeReader) ListApplications(context.Context) ([]application, error) { return f.apps, nil }
func (f fakeReader) WatchApplications(context.Context) (<-chan ApplicationEvent, error) {
	ch := make(chan ApplicationEvent, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}
func (f fakeReader) GetApplication(_ context.Context, _, name string) (*unstructured.Unstructured, error) {
	if f.notFound {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "applications"}, name)
	}
	return f.obj, nil
}
func (f fakeReader) ListEvents(context.Context, string, string, string) ([]string, error) {
	return f.eventLines, nil
}

func TestProviderChanges(t *testing.T) {
	r := fakeReader{apps: []application{
		{Name: "harbor", Namespace: "argocd", RepoURL: "https://github.com/org/repo", Path: "apps/harbor", Revision: "newsha", PrevRevision: "oldsha"},
		{Name: "incomplete", Namespace: "argocd", RepoURL: "", Revision: "x"}, // skipped: no repoURL
	}}
	p := New(r, &whatchanged.Differ{})
	changes, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change (incomplete skipped), got %d", len(changes))
	}
	c := changes[0]
	if c.Engine != providers.EngineArgoCD || c.Type != providers.ChangeSync {
		t.Fatalf("unexpected engine/type: %+v", c)
	}
	if c.Workload != (providers.Workload{Kind: "Application", Name: "harbor", Namespace: "argocd"}) {
		t.Fatalf("unexpected workload: %+v", c.Workload)
	}
	if c.Source != (providers.SourceRef{RepoURL: "https://github.com/org/repo", Path: "apps/harbor"}) {
		t.Fatalf("unexpected source: %+v", c.Source)
	}
	if c.FromRev != "oldsha" || c.ToRev != "newsha" {
		t.Fatalf("unexpected revs: from=%q to=%q", c.FromRev, c.ToRev)
	}
}

func TestProviderChangesMultiSource(t *testing.T) {
	// A multi-source app (its source fields populated from spec.sources[0] by the
	// reader) must yield a Change, not be silently dropped.
	r := fakeReader{apps: []application{
		{Name: "multi", Namespace: "argocd", RepoURL: "https://github.com/org/manifests", Path: "apps/multi", Revision: "newsha", PrevRevision: "oldsha"},
	}}
	p := New(r, &whatchanged.Differ{})
	changes, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change for multi-source app, got %d", len(changes))
	}
	c := changes[0]
	if c.Source != (providers.SourceRef{RepoURL: "https://github.com/org/manifests", Path: "apps/multi"}) ||
		c.FromRev != "oldsha" || c.ToRev != "newsha" {
		t.Fatalf("unexpected multi-source change: %+v", c)
	}
}

// TestProviderChangesDestNamespace covers B1+B2: an Application living in argocd but
// deploying into destination namespace "harbor" must be found when queried by that
// namespace, and When must carry the last deploy time.
func TestProviderChangesDestNamespace(t *testing.T) {
	deployedAt := time.Date(2026, 7, 1, 14, 2, 0, 0, time.UTC)
	r := fakeReader{apps: []application{{
		Name: "harbor", Namespace: "argocd", DestNamespace: "harbor",
		RepoURL: "https://github.com/org/repo", Path: "apps/harbor",
		Revision: "newsha", PrevRevision: "oldsha", DeployedAt: deployedAt,
	}}}
	p := New(r, &whatchanged.Differ{})
	changes, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{Namespace: "harbor"})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Workload.Name != "harbor" {
		t.Fatalf("destination-namespace query did not resolve the owning Application: %+v", changes)
	}
	if !changes[0].When.Equal(deployedAt) {
		t.Fatalf("When = %v, want deployedAt %v", changes[0].When, deployedAt)
	}
}

// TestProviderChangesNamespaceNameFallback: querying a name in a namespace the app
// neither lives in nor targets still resolves it by name across namespaces (B2).
func TestProviderChangesNamespaceNameFallback(t *testing.T) {
	r := fakeReader{apps: []application{
		{Name: "harbor", Namespace: "argocd", DestNamespace: "harbor", RepoURL: "u", Revision: "a"},
	}}
	p := New(r, &whatchanged.Differ{})
	changes, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{Namespace: "elsewhere", Name: "harbor"})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Workload.Name != "harbor" {
		t.Fatalf("name fallback across namespaces failed: %+v", changes)
	}
}

func TestProviderChangesSelector(t *testing.T) {
	r := fakeReader{apps: []application{
		{Name: "harbor", Namespace: "argocd", RepoURL: "u", Revision: "a"},
		{Name: "infra", Namespace: "argocd", RepoURL: "u", Revision: "b"},
	}}
	p := New(r, &whatchanged.Differ{})
	changes, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{Name: "infra"})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Workload.Name != "infra" {
		t.Fatalf("selector did not filter to infra: %v", changes)
	}
}

func TestWatchFailures(t *testing.T) {
	r := fakeReader{events: []ApplicationEvent{
		{Application: application{Name: "ok", Namespace: "argocd", HealthStatus: "Healthy"}},
		{Application: application{Name: "bad", Namespace: "apps", HealthStatus: "Degraded", Message: "container crash"}},
		{Application: application{Name: "prog", Namespace: "apps", HealthStatus: "Progressing"}},
		// healthy app whose last sync OPERATION failed — must still trigger.
		{Application: application{Name: "syncfail", Namespace: "apps", HealthStatus: "Healthy", OperationPhase: "Failed", Message: "bad manifest"}},
		// OutOfSync alone is NOT a failure (auto-sync off / mid-drift steady state).
		{Application: application{Name: "drift", Namespace: "apps", HealthStatus: "Healthy", SyncStatus: "OutOfSync"}},
	}}
	p := New(r, &whatchanged.Differ{})
	ch, err := p.WatchFailures(context.Background())
	if err != nil {
		t.Fatalf("WatchFailures: %v", err)
	}
	got := map[string]providers.FailureEvent{}
	for e := range ch {
		got[e.Workload.Name] = e
	}
	if len(got) != 2 {
		t.Fatalf("want 2 failure events (Degraded + Sync Failed), got %d: %+v", len(got), got)
	}
	if e := got["bad"]; e.Engine != providers.EngineArgoCD || e.Workload.Kind != "Application" ||
		e.Reason != "Degraded" || e.Message != "container crash" {
		t.Fatalf("unexpected degraded failure event: %+v", e)
	}
	if e := got["syncfail"]; e.Reason != "SyncFailed" || e.Message != "bad manifest" {
		t.Fatalf("unexpected sync-failed failure event: %+v", e)
	}
}

func TestFailureReason(t *testing.T) {
	cases := []struct {
		name       string
		app        application
		wantReason string
		wantFailed bool
	}{
		{"healthy", application{HealthStatus: "Healthy"}, "", false},
		{"progressing", application{HealthStatus: "Progressing"}, "", false},
		{"degraded", application{HealthStatus: "Degraded"}, "Degraded", true},
		{"sync-failed", application{HealthStatus: "Healthy", OperationPhase: "Failed"}, "SyncFailed", true},
		{"sync-error", application{HealthStatus: "Healthy", OperationPhase: "Error"}, "SyncError", true},
		{"sync-running", application{HealthStatus: "Healthy", OperationPhase: "Running"}, "", false},
		{"outofsync-not-failure", application{HealthStatus: "Healthy", SyncStatus: "OutOfSync"}, "", false},
		// health-Degraded takes precedence in the reason even if a sync also failed.
		{"degraded-precedence", application{HealthStatus: "Degraded", OperationPhase: "Failed"}, "Degraded", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, failed := failureReason(tc.app)
			if failed != tc.wantFailed || reason != tc.wantReason {
				t.Errorf("failureReason(%+v) = (%q,%v), want (%q,%v)", tc.app, reason, failed, tc.wantReason, tc.wantFailed)
			}
		})
	}
}
