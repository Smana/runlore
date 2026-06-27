# What-Changed Spine — Git Revision Diffing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `internal/whatchanged` — given a Git repo and two revisions, produce the **path-scoped unified diff** of what changed (the "actual landed delta"), returned as a `providers.Diff`. This is the differentiating core of RunLore's what-changed spine.

**Architecture:** A `Differ` over [`go-git`](https://github.com/go-git/go-git). A pure core (`diffRevisions`) operates on an opened `*git.Repository` and is unit-tested against temp repos built in-test; thin `Local` (already-cloned path) and `Remote` (in-memory clone, GitHub-App-token auth) wrappers feed it; `ForChange` maps a `providers.Change` (its `Source` + `FromRev`/`ToRev`) to its `Diff`. No Kubernetes here — the Flux/ArgoCD revision-history reading that *produces* the from/to revisions is a separate follow-up plan that calls this engine.

**Tech Stack:** Go 1.26, `github.com/go-git/go-git/v5`. Existing contract: `providers.Diff` / `providers.FileDiff` / `providers.Change` in `internal/providers/providers.go`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/whatchanged/differ.go` *(create)* | `Differ`, the `diffRevisions` core, `Local`/`Remote`/`ForChange` |
| `internal/whatchanged/differ_test.go` *(create)* | temp-repo fixtures + diff tests |
| `go.mod` / `go.sum` *(modify)* | add `go-git/v5` |

---

## Task 1: go-git dependency + the diff core (`Local`)

**Files:**
- Modify: `go.mod` (add dependency)
- Create: `internal/whatchanged/differ.go`
- Test: `internal/whatchanged/differ_test.go`

- [ ] **Step 1: Add the go-git dependency**

Run: `cd /home/smana/Sources/runlore && go get github.com/go-git/go-git/v5@latest`
Expected: `go.mod` gains `require github.com/go-git/go-git/v5 ...`.

- [ ] **Step 2: Write the failing test**

Create `internal/whatchanged/differ_test.go`:

```go
package whatchanged

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// buildRepo creates a temp git repo with two commits and returns the repo dir
// and the two commit hashes. v1 adds two files; v2 changes both.
func buildRepo(t *testing.T) (dir string, v1, v2 plumbing.Hash) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string, sec int64) plumbing.Hash {
		h, err := wt.Commit(msg, &git.CommitOptions{
			Author: &object.Signature{Name: "t", Email: "t@x", When: time.Unix(sec, 0)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	write("apps/harbor/values.yaml", "version: 1.14.0\n")
	write("other/app.yaml", "x: 1\n")
	v1 = commit("v1", 1000)

	write("apps/harbor/values.yaml", "version: 1.15.0\ndatabase:\n  runMigrations: true\n")
	write("other/app.yaml", "x: 2\n")
	v2 = commit("v2", 2000)
	return dir, v1, v2
}

func TestLocalScoped(t *testing.T) {
	dir, v1, v2 := buildRepo(t)
	d, err := (&Differ{}).Local(dir, v1.String(), v2.String(), "apps/harbor")
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if len(d.Files) != 1 {
		t.Fatalf("want 1 scoped file, got %d (%v)", len(d.Files), paths(d.Files))
	}
	f := d.Files[0]
	if f.Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected path %q", f.Path)
	}
	if !strings.Contains(f.Patch, "-version: 1.14.0") || !strings.Contains(f.Patch, "+version: 1.15.0") ||
		!strings.Contains(f.Patch, "runMigrations") {
		t.Fatalf("patch missing expected delta:\n%s", f.Patch)
	}
}

func TestLocalUnscoped(t *testing.T) {
	dir, v1, v2 := buildRepo(t)
	d, err := (&Differ{}).Local(dir, v1.String(), v2.String(), "")
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if len(d.Files) != 2 {
		t.Fatalf("want 2 files unscoped, got %d (%v)", len(d.Files), paths(d.Files))
	}
}

func paths(fs []providersFileDiff) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Path
	}
	return out
}
```

Note: `providersFileDiff` is an alias declared in Step 4 so the test reads cleanly; it equals `providers.FileDiff`.

- [ ] **Step 3: Run the test to verify it fails**

Run: `cd /home/smana/Sources/runlore && go test ./internal/whatchanged/ -run TestLocal -v`
Expected: FAIL — package/`Differ` undefined.

- [ ] **Step 4: Implement the core + `Local`**

Create `internal/whatchanged/differ.go`:

```go
// Package whatchanged produces the "what changed" delta between two GitOps
// revisions: a path-scoped unified diff (the actual landed change). It is the
// differentiating core of RunLore's investigation spine.
package whatchanged

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/Smana/runlore/internal/providers"
)

// providersFileDiff aliases the contract type for readable call sites/tests.
type providersFileDiff = providers.FileDiff

// Differ computes path-scoped diffs between Git revisions.
type Differ struct {
	// Token is a GitHub App installation token used for HTTPS clone auth in
	// Remote. Empty disables auth (e.g. public or local repos).
	Token string
}

// Local diffs two revisions in an already-cloned repository at path.
func (d *Differ) Local(path, fromRev, toRev, scope string) (providers.Diff, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("open %s: %w", path, err)
	}
	return diffRevisions(repo, fromRev, toRev, scope)
}

// diffRevisions returns the path-scoped unified diff between two revisions.
// scope is a path prefix; "" includes every changed file.
func diffRevisions(repo *git.Repository, fromRev, toRev, scope string) (providers.Diff, error) {
	from, err := resolveCommit(repo, fromRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", fromRev, err)
	}
	to, err := resolveCommit(repo, toRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", toRev, err)
	}
	patch, err := from.Patch(to)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("patch: %w", err)
	}

	var out providers.Diff
	for _, fp := range patch.FilePatches() {
		path := filePatchPath(fp)
		if scope != "" && !strings.HasPrefix(path, scope) {
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

func resolveCommit(repo *git.Repository, rev string) (*object.Commit, error) {
	h, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, err
	}
	return repo.CommitObject(*h)
}

// filePatchPath returns the post-change path (or pre-change path for deletions).
func filePatchPath(fp diff.FilePatch) string {
	from, to := fp.Files()
	if to != nil {
		return to.Path()
	}
	if from != nil {
		return from.Path()
	}
	return ""
}

// singleFilePatch adapts one FilePatch to the diff.Patch interface so a single
// file can be rendered on its own.
type singleFilePatch struct{ fp diff.FilePatch }

func (p singleFilePatch) FilePatches() []diff.FilePatch { return []diff.FilePatch{p.fp} }
func (p singleFilePatch) Message() string               { return "" }

// auth builds the clone auth method from the installation token.
func (d *Differ) auth() transport.AuthMethod {
	if d.Token == "" {
		return nil
	}
	return &http.BasicAuth{Username: "x-access-token", Password: d.Token}
}

// memStorage is referenced by Remote (Task 2); declared here to keep imports stable.
var _ = memory.NewStorage
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/whatchanged/ -run TestLocal -v`
Expected: PASS (both `TestLocalScoped` and `TestLocalUnscoped`).

- [ ] **Step 6: Run the full quality gate**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: build OK; vet OK; tests pass; `gofmt -l` silent; `golangci-lint` `0 issues`.
(If `golangci-lint` flags the `var _ = memory.NewStorage` placeholder as the only `memory` use, that's expected to disappear in Task 2; if it errors now, instead remove the `memory` import and the placeholder line in this task and re-add the import in Task 2.)

- [ ] **Step 7: Commit**

```bash
cd /home/smana/Sources/runlore
git add internal/whatchanged/ go.mod go.sum
git commit -m "feat(whatchanged): path-scoped git revision diff core + Local"
```

---

## Task 2: `Remote` — in-memory clone with GitHub-App auth

**Files:**
- Modify: `internal/whatchanged/differ.go`
- Test: `internal/whatchanged/differ_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/whatchanged/differ_test.go`:

```go
func TestRemoteFromLocalSource(t *testing.T) {
	// Use the temp repo as the clone source (local transport, no auth/network).
	dir, v1, v2 := buildRepo(t)
	d, err := (&Differ{}).Remote(dir, v1.String(), v2.String(), "apps/harbor")
	if err != nil {
		t.Fatalf("Remote: %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected remote diff: %v", paths(d.Files))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /home/smana/Sources/runlore && go test ./internal/whatchanged/ -run TestRemote -v`
Expected: FAIL — `Remote` undefined.

- [ ] **Step 3: Implement `Remote`**

In `internal/whatchanged/differ.go`, delete the placeholder line `var _ = memory.NewStorage` and add the `Remote` method after `Local`:

```go
// Remote clones url into memory (auth via the installation token when set) and
// diffs two revisions. The source may be a remote HTTPS URL or a local path.
func (d *Differ) Remote(url, fromRev, toRev, scope string) (providers.Diff, error) {
	repo, err := git.Clone(memory.NewStorage(), nil, &git.CloneOptions{
		URL:  url,
		Auth: d.auth(),
	})
	if err != nil {
		return providers.Diff{}, fmt.Errorf("clone %s: %w", url, err)
	}
	return diffRevisions(repo, fromRev, toRev, scope)
}
```

(If go-git's local transport rejects the working-tree path for cloning, point the test at `dir + "/.git"` or prefix `file://`; the production path uses an HTTPS URL where this does not arise.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd /home/smana/Sources/runlore && go test ./internal/whatchanged/ -run TestRemote -v`
Expected: PASS.

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/whatchanged/
git commit -m "feat(whatchanged): Remote — in-memory clone with GitHub App token auth"
```

---

## Task 3: `ForChange` — map a `providers.Change` to its `Diff`

**Files:**
- Modify: `internal/whatchanged/differ.go`
- Test: `internal/whatchanged/differ_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/whatchanged/differ_test.go`:

```go
import "github.com/Smana/runlore/internal/providers" // merge into the existing import block

func TestForChange(t *testing.T) {
	dir, v1, v2 := buildRepo(t)
	c := providers.Change{
		Workload: providers.Workload{Kind: "HelmRelease", Name: "harbor", Namespace: "apps"},
		Engine:   providers.EngineFlux,
		Type:     providers.ChangeChartBump,
		FromRev:  v1.String(),
		ToRev:    v2.String(),
		Source:   providers.SourceRef{RepoURL: dir, Path: "apps/harbor"},
	}
	d, err := (&Differ{}).ForChange(c)
	if err != nil {
		t.Fatalf("ForChange: %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected diff: %v", paths(d.Files))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /home/smana/Sources/runlore && go test ./internal/whatchanged/ -run TestForChange -v`
Expected: FAIL — `ForChange` undefined.

- [ ] **Step 3: Implement `ForChange`**

In `internal/whatchanged/differ.go`, add after `Remote`:

```go
// ForChange resolves the diff for a detected Change, cloning its source repo and
// scoping to the workload's path. This is the integration point a GitOpsProvider
// uses to fill in a Change's diff.
func (d *Differ) ForChange(c providers.Change) (providers.Diff, error) {
	if c.FromRev == "" || c.ToRev == "" {
		return providers.Diff{}, fmt.Errorf("change %s/%s: missing from/to revision", c.Workload.Namespace, c.Workload.Name)
	}
	return d.Remote(c.Source.RepoURL, c.FromRev, c.ToRev, c.Source.Path)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd /home/smana/Sources/runlore && go test ./internal/whatchanged/ -run TestForChange -v`
Expected: PASS.

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l . && golangci-lint run ./...`
Expected: all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/whatchanged/
git commit -m "feat(whatchanged): ForChange — resolve a Change's diff via its source + path"
```

---

## What this plan delivers

`internal/whatchanged.Differ` — given a repo (local path or HTTPS URL + GitHub-App token) and two revisions, the path-scoped unified diff of the actual landed change, returned as `providers.Diff`, and a `ForChange` that maps a `providers.Change` straight to its delta. This is the "what changed" answer nothing else in the OSS ecosystem provides.

## Next plan (not in this plan)

**The `Changes` half of the spine** — a `providers.GitOpsProvider` implementation (`internal/providers/gitops/flux`, then `argocd`) that reads revision history from the cluster via `client-go` (Flux `HelmRelease.status.history` chart bumps; `Kustomization.status.lastAppliedRevision` vs prior; ArgoCD `Application.status.history`) and emits ranked `[]providers.Change` — each then resolved to its diff through this `Differ`. Plus a `what_changed_near(target, time, window)` convenience and the `WatchFailures` React trigger.

---

## Self-Review

- **Spec coverage:** Delivers the `Diff` half of the what-changed spine (`providers.Diff`/`FileDiff` produced from real Git revisions, path-scoped, with the GitHub-App auth seam from the design's §14). The `Changes` (revision history) half is explicitly the named follow-up. ✅
- **Placeholder scan:** Every code step is complete and compilable. The one transitional `var _ = memory.NewStorage` in Task 1 (removed in Task 2) is called out with an alternative if the linter objects — not a silent placeholder. ✅
- **Type consistency:** `Differ`, `diffRevisions`, `Local`/`Remote`/`ForChange`, `singleFilePatch`, and `providersFileDiff` are consistent across tasks; all return `providers.Diff` and consume `providers.Change`/`SourceRef`/`FileDiff` exactly as defined in `internal/providers/providers.go`. ✅
