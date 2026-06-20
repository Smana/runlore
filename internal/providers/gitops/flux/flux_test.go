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
