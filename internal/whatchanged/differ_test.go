// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/Smana/runlore/internal/providers"
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

	// apps/harbor-staging is a sibling of apps/harbor — it guards path-segment
	// scoping (scope "apps/harbor" must NOT match "apps/harbor-staging").
	write("apps/harbor/values.yaml", "version: 1.14.0\n")
	write("apps/harbor-staging/values.yaml", "version: 1.14.0\n")
	write("other/app.yaml", "x: 1\n")
	v1 = commit("v1", 1000)

	write("apps/harbor/values.yaml", "version: 1.15.0\ndatabase:\n  runMigrations: true\n")
	write("apps/harbor-staging/values.yaml", "version: 1.15.0\n")
	write("other/app.yaml", "x: 2\n")
	v2 = commit("v2", 2000)
	return dir, v1, v2
}

func TestLocalScoped(t *testing.T) {
	dir, v1, v2 := buildRepo(t)
	d, err := (&Differ{}).Local(context.Background(), dir, v1.String(), v2.String(), "apps/harbor")
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
	d, err := (&Differ{}).Local(context.Background(), dir, v1.String(), v2.String(), "")
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if len(d.Files) != 3 {
		t.Fatalf("want 3 files unscoped, got %d (%v)", len(d.Files), paths(d.Files))
	}
}

func paths(fs []providersFileDiff) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Path
	}
	return out
}

func TestRemoteFromLocalSource(t *testing.T) {
	// Use the temp repo as the clone source (local transport, no auth/network).
	dir, v1, v2 := buildRepo(t)
	d, err := (&Differ{}).Remote(context.Background(), dir, v1.String(), v2.String(), "apps/harbor")
	if err != nil {
		t.Fatalf("Remote: %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected remote diff: %v", paths(d.Files))
	}
}

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
	d, err := (&Differ{}).ForChange(context.Background(), c)
	if err != nil {
		t.Fatalf("ForChange: %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected diff: %v", paths(d.Files))
	}
}

func TestRemoteFromParent(t *testing.T) {
	dir, _, v2 := buildRepo(t)
	// v2 is the second commit; its parent is v1. Diffing the change introduced by
	// v2, scoped to apps/harbor, must yield exactly that file's delta.
	d, err := (&Differ{}).RemoteFromParent(context.Background(), dir, v2.String(), "apps/harbor")
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
	d, err := (&Differ{}).ForChange(context.Background(), c)
	if err != nil {
		t.Fatalf("ForChange (empty FromRev): %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected diff: %v", paths(d.Files))
	}
}

// TestCommitTime resolves a revision's committer timestamp — the anchor for the
// change↔symptom time correlation (RunLore B1). buildRepo commits v2 at Unix 2000.
func TestCommitTime(t *testing.T) {
	dir, _, v2 := buildRepo(t)
	got, err := (&Differ{}).CommitTime(context.Background(), dir, v2.String())
	if err != nil {
		t.Fatalf("CommitTime: %v", err)
	}
	if !got.Equal(time.Unix(2000, 0)) {
		t.Fatalf("CommitTime = %v, want %v", got, time.Unix(2000, 0))
	}
	// An empty revision is a caller error, not a panic.
	if _, err := (&Differ{}).CommitTime(context.Background(), dir, ""); err == nil {
		t.Fatal("empty revision must error")
	}
}

// TestRemoteCancelledCtx: a ctx cancelled before the clone must abort with a
// wrapped context error (errors.Is) — proving the clone is cancellable.
func TestRemoteCancelledCtx(t *testing.T) {
	dir, v1, v2 := buildRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the clone even starts
	d, err := (&Differ{}).Remote(ctx, dir, v1.String(), v2.String(), "apps/harbor")
	if err == nil {
		t.Fatalf("expected a cancellation error, got diff with %d files", len(d.Files))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error must wrap context.Canceled (errors.Is), got: %v", err)
	}
	if len(d.Files) != 0 {
		t.Fatalf("cancelled clone must yield no diff, got %d files", len(d.Files))
	}
}

// TestForChangeCancelledCtx exercises the empty-FromRev (RemoteFromParent) path
// under a cancelled ctx.
func TestForChangeCancelledCtx(t *testing.T) {
	dir, _, v2 := buildRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := providers.Change{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Engine:   providers.EngineFlux,
		Type:     providers.ChangeSync,
		ToRev:    v2.String(), // FromRev empty → RemoteFromParent
		Source:   providers.SourceRef{RepoURL: dir, Path: "apps/harbor"},
	}
	_, err := (&Differ{}).ForChange(ctx, c)
	if err == nil {
		t.Fatal("expected a cancellation error from ForChange")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error must wrap context.Canceled (errors.Is), got: %v", err)
	}
}

// addUnrelatedCommit adds a commit touching only other/app.yaml (not apps/harbor)
// on top of the repo, advancing HEAD past the last apps/harbor change.
func addUnrelatedCommit(t *testing.T, dir string) plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other/app.yaml"), []byte("x: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("other/app.yaml"); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit("unrelated", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@x", When: time.Unix(3000, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRemoteLastPathChange(t *testing.T) {
	dir, _, v2 := buildRepo(t)
	addUnrelatedCommit(t, dir) // advance HEAD past the last apps/harbor change

	// Finds v2 (the newest commit touching apps/harbor) and diffs it vs its parent.
	d, err := (&Differ{}).RemoteLastPathChange(context.Background(), dir, "HEAD", "apps/harbor")
	if err != nil {
		t.Fatalf("RemoteLastPathChange: %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" ||
		!strings.Contains(d.Files[0].Patch, "+version: 1.15.0") {
		t.Fatalf("want the v2=%s apps/harbor delta, got %v", v2.String()[:7], paths(d.Files))
	}

	// A path never touched in history degrades to an empty diff, not an error.
	empty, err := (&Differ{}).RemoteLastPathChange(context.Background(), dir, "HEAD", "apps/does-not-exist")
	if err != nil {
		t.Fatalf("RemoteLastPathChange (untouched path): %v", err)
	}
	if len(empty.Files) != 0 {
		t.Fatalf("untouched path must yield an empty diff, got %v", paths(empty.Files))
	}
}

// TestForChangeFallsBackToLastPathChange reproduces RunLore #239: on a health-check
// failure Flux advances lastAppliedRevision to (or past) the breaking commit, so the
// forward range diff for the resource's path is EMPTY. ForChange must then fall back
// to the newest commit that actually touched the path — the real "what changed" —
// rather than returning nothing (which leaves the model with only a bare SHA).
func TestForChangeFallsBackToLastPathChange(t *testing.T) {
	dir, _, v2 := buildRepo(t) // v2 is the last commit touching apps/harbor (the break)
	head := addUnrelatedCommit(t, dir)

	// The Change the flux provider builds for a failing Kustomization whose
	// lastAppliedRevision advanced to HEAD: ToRev=HEAD, and HEAD did not touch
	// apps/harbor, so the primary parent-diff of HEAD scoped to apps/harbor is empty.
	c := providers.Change{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Engine:   providers.EngineFlux,
		Type:     providers.ChangeSync,
		ToRev:    head.String(),
		Source:   providers.SourceRef{RepoURL: dir, Path: "apps/harbor"},
	}
	d, err := (&Differ{}).ForChange(context.Background(), c)
	if err != nil {
		t.Fatalf("ForChange: %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("want the last apps/harbor change (v2=%s), got %v", v2.String()[:7], paths(d.Files))
	}
	if !strings.Contains(d.Files[0].Patch, "+version: 1.15.0") || !strings.Contains(d.Files[0].Patch, "runMigrations") {
		t.Fatalf("fallback diff missing the actual change:\n%s", d.Files[0].Patch)
	}
}
