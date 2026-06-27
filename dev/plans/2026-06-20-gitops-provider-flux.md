# GitOpsProvider (Flux) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the **`Changes` half of the what-changed spine** for **Flux Kustomizations** — a `providers.GitOpsProvider` that reads Flux state from the cluster and emits `[]providers.Change` (source repo + path + current revision), each diffable through the existing `whatchanged.Differ`.

**Architecture:** The provider reads Flux CRDs as **`unstructured`** objects through a small **`Reader`** interface (so the version-robust, hard-to-fake `client-go` plumbing is isolated from the pure mapping logic, which is unit-tested against a fake `Reader`). `Changes()` is pure — it emits a `Change` with `ToRev` (the applied revision) and an **empty `FromRev`**, meaning "diff the change introduced by `ToRev`". The clone + parent-resolution lives in `whatchanged` (one place; the natural home for the review's shallow-clone/cache optimization). A `dynamicReader` implements `Reader` over `client-go`'s dynamic client.

**Tech Stack:** Go 1.26, `github.com/go-git/go-git/v5` (already present), `k8s.io/client-go` + `k8s.io/apimachinery` (new). Contract: `internal/providers/providers.go` (`GitOpsProvider`, `Change`, `SourceRef`, `Workload`, `EngineFlux`, `ChangeSync`, `Diff`, `FailureEvent`, `TimeWindow`, `Selector`).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/whatchanged/differ.go` *(modify)* | Refactor to `diffCommits`; add `RemoteFromParent`; `ForChange` handles empty `FromRev` |
| `internal/whatchanged/differ_test.go` *(modify)* | Tests for `RemoteFromParent` + empty-`FromRev` `ForChange` |
| `internal/providers/gitops/flux/flux.go` *(create)* | `Provider`, `Reader` interface, types, `parseRevision`, mapping, `Changes`/`Diff`/`WatchFailures` |
| `internal/providers/gitops/flux/flux_test.go` *(create)* | `parseRevision` + `Changes` (fake `Reader`) tests |
| `internal/providers/gitops/flux/dynamic.go` *(create)* | `dynamicReader` — `Reader` over `client-go` dynamic client |
| `internal/providers/gitops/flux/dynamic_test.go` *(create)* | dynamic fake-client test |
| `go.mod` / `go.sum` *(modify)* | add `client-go`, `apimachinery` |

---

## Task 1: `whatchanged` — diff a revision against its parent

**Files:**
- Modify: `internal/whatchanged/differ.go`
- Test: `internal/whatchanged/differ_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/whatchanged/differ_test.go`:

```go
func TestRemoteFromParent(t *testing.T) {
	dir, _, v2 := buildRepo(t)
	// v2 is the second commit; its parent is v1. Diffing the change introduced by
	// v2, scoped to apps/harbor, must yield exactly that file's delta.
	d, err := (&Differ{}).RemoteFromParent(dir, v2.String(), "apps/harbor")
	if err != nil {
		t.Fatalf("RemoteFromParent: %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected diff: %v", paths(d.Files))
	}
	if !strings.Contains(d.Files[0].Patch, "+version: 1.15.0") {
		t.Fatalf("patch missing expected delta:\n%s", d.Files[0].Patch)
	}
}

func TestForChangeEmptyFromRev(t *testing.T) {
	dir, _, v2 := buildRepo(t)
	c := providers.Change{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Engine:   providers.EngineFlux,
		Type:     providers.ChangeSync,
		ToRev:    v2.String(), // FromRev intentionally empty
		Source:   providers.SourceRef{RepoURL: dir, Path: "apps/harbor"},
	}
	d, err := (&Differ{}).ForChange(c)
	if err != nil {
		t.Fatalf("ForChange (empty FromRev): %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected diff: %v", paths(d.Files))
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/whatchanged/ -run 'RemoteFromParent|ForChangeEmpty' -v`
Expected: FAIL — `RemoteFromParent` undefined; `ForChange` errors on empty `FromRev`.

- [ ] **Step 3: Refactor `diffRevisions` to `diffCommits`, add `RemoteFromParent`, update `ForChange`**

In `internal/whatchanged/differ.go`, replace the `diffRevisions` function with a commit-level core + a thin resolver:

```go
// diffRevisions resolves two revisions and returns their path-scoped diff.
func diffRevisions(repo *git.Repository, fromRev, toRev, scope string) (providers.Diff, error) {
	from, err := resolveCommit(repo, fromRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", fromRev, err)
	}
	to, err := resolveCommit(repo, toRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", toRev, err)
	}
	return diffCommits(from, to, scope)
}

// diffCommits returns the path-scoped unified diff between two commits.
// scope is a path prefix matched on segment boundaries; "" includes every file.
func diffCommits(from, to *object.Commit, scope string) (providers.Diff, error) {
	patch, err := from.Patch(to)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("patch: %w", err)
	}
	var out providers.Diff
	for _, fp := range patch.FilePatches() {
		path := filePatchPath(fp)
		if !underScope(path, scope) {
			continue
		}
		var buf bytes.Buffer
		if err := diff.NewUnifiedEncoder(&buf, diff.DefaultContextLines).Encode(singleFilePatch{fp}); err != nil {
			return providers.Diff{}, fmt.Errorf("encode %s: %w", path, err)
		}
		out.Files = append(out.Files, providers.FileDiff{Path: path, Patch: buf.String()})
	}
	return out, nil
}
```

Add `RemoteFromParent` after `Remote`:

```go
// RemoteFromParent clones url and returns the path-scoped diff of the change
// introduced by rev (rev against its first parent). A root commit (no parent)
// yields an empty diff.
//
// NOTE (perf): like Remote, this does a full in-memory clone per call. When the
// GitOpsProvider drives this across many changes, add a shallow fetch or a
// per-repo clone cache here (see docs/plans review note).
func (d *Differ) RemoteFromParent(url, rev, scope string) (providers.Diff, error) {
	repo, err := git.Clone(memory.NewStorage(), nil, &git.CloneOptions{URL: url, Auth: d.auth()})
	if err != nil {
		return providers.Diff{}, fmt.Errorf("clone %s: %w", url, err)
	}
	to, err := resolveCommit(repo, rev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", rev, err)
	}
	if to.NumParents() == 0 {
		return providers.Diff{}, nil // root commit: nothing to diff against
	}
	from, err := to.Parent(0)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("parent of %q: %w", rev, err)
	}
	return diffCommits(from, to, scope)
}
```

Replace `ForChange` so an empty `FromRev` means "diff the change introduced by `ToRev`":

```go
// ForChange resolves the diff for a detected Change by cloning its source repo
// and scoping to the workload's path. With both revisions it diffs FromRev..ToRev;
// with only ToRev it diffs the change introduced by ToRev (against its parent).
func (d *Differ) ForChange(c providers.Change) (providers.Diff, error) {
	if c.ToRev == "" {
		return providers.Diff{}, fmt.Errorf("change %s/%s: missing to revision", c.Workload.Namespace, c.Workload.Name)
	}
	if c.FromRev == "" {
		return d.RemoteFromParent(c.Source.RepoURL, c.ToRev, c.Source.Path)
	}
	return d.Remote(c.Source.RepoURL, c.FromRev, c.ToRev, c.Source.Path)
}
```

- [ ] **Step 4: Run to verify the new + existing tests pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/whatchanged/ -v`
Expected: PASS — the new `RemoteFromParent`/`ForChangeEmptyFromRev` tests plus all existing ones (`TestForChange` still passes: it sets a non-empty `FromRev`).

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/whatchanged/
git commit -m "feat(whatchanged): diff a revision against its parent (RemoteFromParent + empty-FromRev ForChange)"
```

---

## Task 2: Flux provider core (pure mapping + `Reader` interface)

**Files:**
- Create: `internal/providers/gitops/flux/flux.go`
- Test: `internal/providers/gitops/flux/flux_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/providers/gitops/flux/flux_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -v`
Expected: FAIL — package `flux` / `New` / types undefined.

- [ ] **Step 3: Implement the provider core**

Create `internal/providers/gitops/flux/flux.go`:

```go
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
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -v`
Expected: PASS (`TestParseRevision`, `TestProviderChanges`, `TestProviderChangesSelector`).

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/providers/gitops/flux/
git commit -m "feat(gitops/flux): pure Kustomization->Change mapping behind a Reader interface"
```

---

## Task 3: `dynamicReader` — read Flux via client-go dynamic client

**Files:**
- Create: `internal/providers/gitops/flux/dynamic.go`
- Test: `internal/providers/gitops/flux/dynamic_test.go`
- Modify: `go.mod` (add `client-go`, `apimachinery`)

- [ ] **Step 1: Add the dependencies**

Run: `cd /home/smana/Sources/runlore && go get k8s.io/client-go@latest k8s.io/apimachinery@latest`
Expected: `go.mod` gains `k8s.io/client-go` and `k8s.io/apimachinery`.

- [ ] **Step 2: Write the failing test**

Create `internal/providers/gitops/flux/dynamic_test.go`:

```go
package flux

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestDynamicReader(t *testing.T) {
	ksObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "apps", "namespace": "flux-system"},
		"spec": map[string]any{
			"path":      "./apps",
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"},
		},
		"status": map[string]any{"lastAppliedRevision": "main@sha1:abc123"},
	}}
	grObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "GitRepository",
		"metadata":   map[string]any{"name": "flux-system", "namespace": "flux-system"},
		"spec":       map[string]any{"url": "https://github.com/org/repo"},
	}}

	gvrToListKind := map[schema.GroupVersionResource]string{
		kustomizationGVR: "KustomizationList",
		gitRepositoryGVR: "GitRepositoryList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, ksObj, grObj)
	r := NewDynamicReader(client)

	ks, err := r.ListKustomizations(context.Background())
	if err != nil {
		t.Fatalf("ListKustomizations: %v", err)
	}
	if len(ks) != 1 {
		t.Fatalf("want 1 kustomization, got %d", len(ks))
	}
	got := ks[0]
	if got.Name != "apps" || got.Namespace != "flux-system" || got.Path != "./apps" ||
		got.SourceName != "flux-system" || got.SourceNamespace != "flux-system" || got.Revision != "main@sha1:abc123" {
		t.Fatalf("unexpected kustomization: %+v", got)
	}

	gr, err := r.GetGitRepository(context.Background(), "flux-system", "flux-system")
	if err != nil {
		t.Fatalf("GetGitRepository: %v", err)
	}
	if gr.URL != "https://github.com/org/repo" {
		t.Fatalf("unexpected url: %q", gr.URL)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -run TestDynamicReader -v`
Expected: FAIL — `NewDynamicReader` / GVRs undefined.

- [ ] **Step 4: Implement `dynamicReader`**

Create `internal/providers/gitops/flux/dynamic.go`:

```go
package flux

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Flux CRD resources (v1).
var (
	kustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	gitRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
)

// dynamicReader reads Flux CRDs as unstructured objects via the dynamic client.
type dynamicReader struct {
	client dynamic.Interface
}

// NewDynamicReader builds a Reader backed by a client-go dynamic client.
func NewDynamicReader(client dynamic.Interface) Reader {
	return &dynamicReader{client: client}
}

func (r *dynamicReader) ListKustomizations(ctx context.Context) ([]kustomization, error) {
	list, err := r.client.Resource(kustomizationGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list kustomizations: %w", err)
	}
	out := make([]kustomization, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, kustomizationFromUnstructured(&list.Items[i]))
	}
	return out, nil
}

func (r *dynamicReader) GetGitRepository(ctx context.Context, namespace, name string) (gitRepository, error) {
	u, err := r.client.Resource(gitRepositoryGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return gitRepository{}, fmt.Errorf("get gitrepository %s/%s: %w", namespace, name, err)
	}
	url, _, _ := unstructured.NestedString(u.Object, "spec", "url")
	return gitRepository{Name: name, Namespace: namespace, URL: url}, nil
}

func kustomizationFromUnstructured(u *unstructured.Unstructured) kustomization {
	path, _, _ := unstructured.NestedString(u.Object, "spec", "path")
	srcName, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "name")
	srcNamespace, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "namespace")
	rev, _, _ := unstructured.NestedString(u.Object, "status", "lastAppliedRevision")
	namespace := u.GetNamespace()
	if srcNamespace == "" {
		srcNamespace = namespace // sourceRef.namespace defaults to the Kustomization namespace
	}
	return kustomization{
		Name:            u.GetName(),
		Namespace:       namespace,
		Path:            path,
		SourceName:      srcName,
		SourceNamespace: srcNamespace,
		Revision:        rev,
	}
}
```

- [ ] **Step 5: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -run TestDynamicReader -v`
Expected: PASS.

> If the fake constructor name/signature differs in the resolved `client-go` version (the dynamic fake API has shifted across releases), adjust the test's client construction to the installed version's equivalent — `NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)` is correct for recent `client-go`. Keep the `Reader` behaviour and assertions identical.

- [ ] **Step 6: Tidy, full gate, commit**

Run:
```bash
cd /home/smana/Sources/runlore
go mod tidy
go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l . && golangci-lint run ./...
```
Expected: all clean, `0 issues`.

```bash
git add internal/providers/gitops/flux/ go.mod go.sum
git commit -m "feat(gitops/flux): dynamic client-go Reader for Kustomizations + GitRepositories"
```

---

## What this plan delivers

A Flux `providers.GitOpsProvider`: `Changes()` reads Kustomizations + their GitRepository sources from the cluster (via the dynamic client) and emits engine-agnostic `Change`s (source repo + path + applied revision), each diffable through `Differ.ForChange` (which now resolves the change introduced by `ToRev`). Combined with the diff engine, this is the first end-to-end **"what changed on this GitOps-managed cluster, and what was the actual delta."**

## Next plans (not in this plan)

1. **`WatchFailures`** — watch Kustomization `Ready=False` / GitRepository `FetchFailed` (the React trigger).
2. **Flux HelmRelease** changes (`status.history` chart bumps) + **ArgoCD** `Application.status.history`.
3. **Time-windowing + ranking** — git-log over the path to build a windowed, blast-radius-ranked change timeline.
4. **`Remote` shallow-clone / per-repo clone cache** (the review's perf note) — once `Changes`+`Diff` drive cloning at scale.

---

## Self-Review

- **Spec coverage:** Implements `providers.GitOpsProvider` for Flux Kustomizations (`Changes` real; `Diff` delegates to the Differ; `WatchFailures` an explicit, documented stub). The compile-time `var _ providers.GitOpsProvider = (*Provider)(nil)` guards the contract. Deferred items are named follow-ups. ✅
- **Placeholder scan:** Every step has complete, compilable code. The two transitional notes (dynamic-fake version latitude; `WatchFailures` stub) are explicit design decisions, not missing work. ✅
- **Type consistency:** `kustomization`/`gitRepository`/`Reader` are defined in Task 2 and implemented in Task 3; `parseRevision`, `mapKustomization`, `kustomizationGVR`/`gitRepositoryGVR` consistent. `RemoteFromParent`/`diffCommits`/`ForChange` (Task 1) align with the existing `whatchanged` types and are consumed by `Provider.Diff`. Revision parsing handles all documented Flux/ArgoCD formats. ✅
