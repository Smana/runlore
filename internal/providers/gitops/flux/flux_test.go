package flux

import (
	"context"
	"fmt"
	"testing"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

func TestParseRevision(t *testing.T) {
	cases := map[string]string{
		"main@sha1:abc123":   "abc123", // Flux v1
		"v1.0.0@sha1:def456": "def456", // tag
		"main@sha256:beef":   "beef",   // sha256 digests
		"main/abc789":        "abc789", // legacy Flux
		"abc123":             "abc123", // bare sha (ArgoCD-style)
		"":                   "",
	}
	for in, want := range cases {
		if got := parseRevision(in); got != want {
			t.Errorf("parseRevision(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeReader is an in-memory Reader for testing the mapping logic.
type fakeReader struct {
	ks  []kustomization
	grs map[string]gitRepository // key: namespace/name
}

func (f fakeReader) ListKustomizations(context.Context) ([]kustomization, error) { return f.ks, nil }
func (f fakeReader) GetGitRepository(_ context.Context, ns, name string) (gitRepository, error) {
	if gr, ok := f.grs[ns+"/"+name]; ok {
		return gr, nil
	}
	return gitRepository{}, fmt.Errorf("gitrepository %s/%s not found", ns, name)
}
func (f fakeReader) WatchKustomizations(context.Context) (<-chan KustomizationEvent, error) {
	ch := make(chan KustomizationEvent)
	close(ch)
	return ch, nil
}

func TestProviderChanges(t *testing.T) {
	r := fakeReader{
		ks: []kustomization{
			{Name: "apps", Namespace: "flux-system", Path: "./apps", SourceName: "flux-system", SourceNamespace: "flux-system", Revision: "main@sha1:abc123"},
			{Name: "incomplete", Namespace: "flux-system", Path: "", SourceName: "flux-system", SourceNamespace: "flux-system", Revision: "main@sha1:zzz"}, // skipped: no path
		},
		grs: map[string]gitRepository{"flux-system/flux-system": {URL: "https://github.com/org/repo"}},
	}
	p := New(r, &whatchanged.Differ{})
	changes, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change (incomplete skipped), got %d", len(changes))
	}
	c := changes[0]
	if c.Engine != providers.EngineFlux || c.Type != providers.ChangeSync {
		t.Fatalf("unexpected engine/type: %+v", c)
	}
	if c.Workload != (providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}) {
		t.Fatalf("unexpected workload: %+v", c.Workload)
	}
	if c.Source != (providers.SourceRef{RepoURL: "https://github.com/org/repo", Path: "./apps"}) {
		t.Fatalf("unexpected source: %+v", c.Source)
	}
	if c.ToRev != "abc123" || c.FromRev != "" {
		t.Fatalf("unexpected revs: from=%q to=%q", c.FromRev, c.ToRev)
	}
}

func TestProviderChangesSelector(t *testing.T) {
	r := fakeReader{
		ks: []kustomization{
			{Name: "apps", Namespace: "flux-system", Path: "./apps", SourceName: "flux-system", SourceNamespace: "flux-system", Revision: "main@sha1:a"},
			{Name: "infra", Namespace: "infra", Path: "./infra", SourceName: "flux-system", SourceNamespace: "flux-system", Revision: "main@sha1:b"},
		},
		grs: map[string]gitRepository{"flux-system/flux-system": {URL: "https://github.com/org/repo"}},
	}
	p := New(r, &whatchanged.Differ{})
	changes, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{Namespace: "infra"})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Workload.Name != "infra" {
		t.Fatalf("selector did not filter to infra: %v", changes)
	}
}

// streamReader is a fakeReader that also serves a fixed watch stream.
type streamReader struct {
	fakeReader
	events []KustomizationEvent
}

func (s streamReader) WatchKustomizations(_ context.Context) (<-chan KustomizationEvent, error) {
	ch := make(chan KustomizationEvent, len(s.events))
	for _, e := range s.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestWatchFailures(t *testing.T) {
	r := streamReader{events: []KustomizationEvent{
		{Kustomization: kustomization{Name: "ok", Namespace: "flux-system", ReadyStatus: "True"}},
		{Kustomization: kustomization{Name: "bad", Namespace: "apps", ReadyStatus: "False", ReadyReason: "BuildFailed", ReadyMessage: "boom"}},
		{Kustomization: kustomization{Name: "progressing", Namespace: "apps", ReadyStatus: "Unknown"}},
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
		t.Fatalf("want 1 failure event (only Ready=False), got %d", len(got))
	}
	e := got[0]
	if e.Engine != providers.EngineFlux || e.Workload.Name != "bad" || e.Workload.Kind != "Kustomization" ||
		e.Reason != "BuildFailed" || e.Message != "boom" {
		t.Fatalf("unexpected failure event: %+v", e)
	}
}
