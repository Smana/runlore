package argocd

import (
	"context"
	"testing"

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
	apps   []application
	events []ApplicationEvent
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
	}}
	p := New(r, &whatchanged.Differ{})
	ch, err := p.WatchFailures(context.Background())
	if err != nil {
		t.Fatalf("WatchFailures: %v", err)
	}
	var got []providers.FailureEvent
	for e := range ch {
		got = append(got, e)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 failure event (only Degraded), got %d", len(got))
	}
	e := got[0]
	if e.Engine != providers.EngineArgoCD || e.Workload.Name != "bad" || e.Workload.Kind != "Application" ||
		e.Reason != "Degraded" || e.Message != "container crash" {
		t.Fatalf("unexpected failure event: %+v", e)
	}
}
