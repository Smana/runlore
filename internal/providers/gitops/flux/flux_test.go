package flux

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
	// srcRevs maps "kind/namespace/name" → the source's current artifact
	// revision (status.artifact.revision). Absent entries return an error.
	srcRevs map[string]string
}

func (f fakeReader) ListKustomizations(context.Context) ([]kustomization, error) { return f.ks, nil }
func (f fakeReader) GetGitRepository(_ context.Context, ns, name string) (gitRepository, error) {
	if gr, ok := f.grs[ns+"/"+name]; ok {
		return gr, nil
	}
	return gitRepository{}, fmt.Errorf("gitrepository %s/%s not found", ns, name)
}
func (f fakeReader) SourceRevision(_ context.Context, kind, ns, name string) (string, error) {
	if rev, ok := f.srcRevs[kind+"/"+ns+"/"+name]; ok {
		return rev, nil
	}
	return "", fmt.Errorf("source %s/%s/%s revision not found", kind, ns, name)
}
func (f fakeReader) WatchKustomizations(context.Context) (<-chan KustomizationEvent, error) {
	ch := make(chan KustomizationEvent)
	close(ch)
	return ch, nil
}
func (f fakeReader) GetResource(context.Context, string, string, string) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f fakeReader) ListEvents(context.Context, string, string, string) ([]string, error) {
	return nil, nil
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

// TestProviderChangesFailingKustomizationSpansGap covers the core bug: a failing
// Kustomization keeps status.lastAppliedRevision pinned to the last HEALTHY commit
// (Flux does NOT advance it on a health-check failure). Keying "what changed" on
// lastAppliedRevision alone diffs the pre-break commit and misses the breaking one.
// For a failing Kustomization whose source HEAD differs from lastApplied, the Change
// must span lastApplied..HEAD; otherwise behavior is unchanged.
func TestProviderChangesFailingKustomizationSpansGap(t *testing.T) {
	const url = "https://github.com/org/repo"
	cases := []struct {
		name        string
		readyStatus string
		lastApplied string // status.lastAppliedRevision
		srcHead     string // source status.artifact.revision (current synced HEAD)
		wantFrom    string
		wantTo      string
	}{
		{
			// The bug repro: failing health check, HEAD (B) is past lastApplied (A).
			name:        "failing with source ahead spans lastApplied..HEAD",
			readyStatus: "False", lastApplied: "main@sha1:aaa", srcHead: "main@sha1:bbb",
			wantFrom: "aaa", wantTo: "bbb",
		},
		{
			// Healthy: keep today's behavior exactly (diff the change introduced by ToRev).
			name:        "healthy keeps single-rev behavior",
			readyStatus: "True", lastApplied: "main@sha1:aaa", srcHead: "main@sha1:bbb",
			wantFrom: "", wantTo: "aaa",
		},
		{
			// Failing but source HEAD == lastApplied: no real gap, no false span.
			name:        "failing with source equal to lastApplied keeps single-rev behavior",
			readyStatus: "False", lastApplied: "main@sha1:aaa", srcHead: "main@sha1:aaa",
			wantFrom: "", wantTo: "aaa",
		},
		{
			// Ready != True (the spec's "failing" predicate) also covers Unknown
			// (e.g. reconciling/progressing): span the gap so an in-flight breaking
			// commit past the last-applied one is surfaced rather than missed.
			name:        "ready-unknown with source ahead spans lastApplied..HEAD",
			readyStatus: "Unknown", lastApplied: "main@sha1:aaa", srcHead: "main@sha1:bbb",
			wantFrom: "aaa", wantTo: "bbb",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fakeReader{
				ks: []kustomization{{
					Name: "apps", Namespace: "flux-system", Path: "./apps",
					SourceKind: "GitRepository", SourceName: "flux-system", SourceNamespace: "flux-system",
					Revision: tc.lastApplied, ReadyStatus: tc.readyStatus,
				}},
				grs:     map[string]gitRepository{"flux-system/flux-system": {URL: url}},
				srcRevs: map[string]string{"GitRepository/flux-system/flux-system": tc.srcHead},
			}
			p := New(r, &whatchanged.Differ{})
			changes, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{})
			if err != nil {
				t.Fatalf("Changes: %v", err)
			}
			if len(changes) != 1 {
				t.Fatalf("want 1 change, got %d", len(changes))
			}
			if changes[0].FromRev != tc.wantFrom || changes[0].ToRev != tc.wantTo {
				t.Fatalf("revs: from=%q to=%q, want from=%q to=%q",
					changes[0].FromRev, changes[0].ToRev, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestProviderChangesExternalArtifactSource(t *testing.T) {
	// A Kustomization sourced from an ExternalArtifact (ArtifactGenerator output)
	// must still produce a change — without erroring on a missing GitRepository,
	// which is what broke "what changed" on ArtifactGenerator-based GitOps.
	r := fakeReader{
		ks: []kustomization{
			{Name: "crossplane-configuration", Namespace: "flux-system", Path: ".",
				SourceKind: "ExternalArtifact", SourceName: "infra-artifact", SourceNamespace: "flux-system",
				Revision: "sha256:abc"},
		},
		grs: map[string]gitRepository{}, // no GitRepository exists — must NOT cause an error
	}
	p := New(r, &whatchanged.Differ{})
	changes, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{})
	if err != nil {
		t.Fatalf("Changes must not error on a non-Git source: %v", err)
	}
	if len(changes) != 1 || changes[0].Workload.Name != "crossplane-configuration" {
		t.Fatalf("expected 1 change for the ExternalArtifact-sourced Kustomization, got %+v", changes)
	}
	if changes[0].Source.RepoURL != "" {
		t.Fatalf("non-Git source should have no RepoURL, got %q", changes[0].Source.RepoURL)
	}
	// And its diff is an empty (no-op) result, not an error.
	d, derr := p.Diff(context.Background(), changes[0])
	if derr != nil || len(d.Files) != 0 {
		t.Fatalf("expected empty diff for non-Git source, got files=%d err=%v", len(d.Files), derr)
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
