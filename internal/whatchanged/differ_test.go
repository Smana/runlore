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

// TestRemoteWithMirrorHistoryWalks: with Mirrors set, Remote,
// RemoteLastPathChange and RevisionsInWindow all work off the bare mirror —
// full history is preserved (the reason a shallow clone was rejected).
func TestRemoteWithMirrorHistoryWalks(t *testing.T) {
	src, v1, v2 := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	d := &Differ{Mirrors: mc}
	diff, err := d.Remote(context.Background(), src, v1.String(), v2.String(), "apps/harbor")
	if err != nil {
		t.Fatalf("Remote via mirror: %v", err)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected diff via mirror: %v", paths(diff.Files))
	}
	fb, err := d.RemoteLastPathChange(context.Background(), src, v2.String(), "apps/harbor")
	if err != nil || len(fb.Files) == 0 {
		t.Fatalf("lastPathChange via mirror: files=%d err=%v", len(fb.Files), err)
	}
	revs, err := d.RevisionsInWindow(context.Background(), src, v2.String(), "",
		providers.TimeWindow{Start: time.Unix(0, 0), End: time.Unix(3000, 0)}, 10)
	if err != nil || len(revs) != 2 {
		t.Fatalf("revisionsInWindow via mirror: revs=%d err=%v", len(revs), err)
	}
	// The mirror, not a temp clone, must have been used: exactly one mirror dir.
	entries, _ := os.ReadDir(mc.dir)
	if len(entries) != 1 {
		t.Fatalf("want 1 mirror dir, got %d", len(entries))
	}
}

// TestMirrorFallbackToClone: a broken mirror cache (unwritable dir) must not
// break Remote — it silently falls back to clone-per-call.
func TestMirrorFallbackToClone(t *testing.T) {
	src, v1, v2 := buildRepo(t)
	roDir := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	mc := &MirrorCache{dir: roDir, max: 10, entries: map[string]*mirrorEntry{}}
	d := &Differ{Mirrors: mc}
	diff, err := d.Remote(context.Background(), src, v1.String(), v2.String(), "apps/harbor")
	if err != nil {
		t.Fatalf("Remote should fall back to clone-per-call: %v", err)
	}
	if len(diff.Files) != 1 {
		t.Fatalf("fallback diff wrong: %v", paths(diff.Files))
	}
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

// TestNoCheckoutDiffStillResolves verifies that setting NoCheckout: true on the
// PlainCloneContext call (G1 fix) does not break diffing. Diffing operates
// exclusively on git commit/tree/blob objects via PatchContext — it never reads
// the checked-out working tree — so skipping the checkout is safe and avoids
// materialising large working trees for monorepos.
func TestNoCheckoutDiffStillResolves(t *testing.T) {
	src, v1, v2 := buildRepo(t)
	dst := t.TempDir()

	// Clone with NoCheckout: true — no working tree files are written.
	cloned, err := git.PlainCloneContext(context.Background(), dst, false, &git.CloneOptions{
		URL:        src,
		NoCheckout: true,
	})
	if err != nil {
		t.Fatalf("PlainCloneContext with NoCheckout: %v", err)
	}

	// Confirm no working-tree files exist beyond .git (the worktree is empty).
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			t.Errorf("unexpected file in NoCheckout clone: %s", e.Name())
		}
	}

	// Diffing via git objects must still work correctly on the bare clone.
	d, err := diffRevisions(context.Background(), cloned, v1.String(), v2.String(), "apps/harbor")
	if err != nil {
		t.Fatalf("diffRevisions on NoCheckout clone: %v", err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("want 1 scoped file, got %v", paths(d.Files))
	}
	if !strings.Contains(d.Files[0].Patch, "+version: 1.15.0") || !strings.Contains(d.Files[0].Patch, "runMigrations") {
		t.Fatalf("patch missing expected delta:\n%s", d.Files[0].Patch)
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

// TestRevisionsInWindow proves G3's enumeration: a window spanning multiple commits
// yields a Change-worthy revision per in-window commit, newest-first, honoring the
// path scope, and capped.
func TestRevisionsInWindow(t *testing.T) {
	dir, v1, v2 := buildRepo(t) // apps/harbor touched at t=1000 (v1) and t=2000 (v2)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}

	// A window spanning both apps/harbor commits, scoped to apps/harbor, newest-first.
	w := providers.TimeWindow{Start: time.Unix(500, 0), End: time.Unix(2500, 0)}
	revs, err := revisionsInWindow(repo, v2.String(), "apps/harbor", w, 10)
	if err != nil {
		t.Fatalf("revisionsInWindow: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("want 2 in-window revisions, got %d: %+v", len(revs), revs)
	}
	if revs[0].SHA != v2.String() || revs[1].SHA != v1.String() {
		t.Fatalf("want newest-first [v2,v1], got %s,%s", revs[0].SHA[:7], revs[1].SHA[:7])
	}
	if !revs[0].When.Equal(time.Unix(2000, 0)) {
		t.Fatalf("unexpected When for newest: %v", revs[0].When)
	}

	// A narrow window catching only the newer commit.
	narrow := providers.TimeWindow{Start: time.Unix(1500, 0), End: time.Unix(2500, 0)}
	revs, err = revisionsInWindow(repo, v2.String(), "apps/harbor", narrow, 10)
	if err != nil {
		t.Fatalf("revisionsInWindow (narrow): %v", err)
	}
	if len(revs) != 1 || revs[0].SHA != v2.String() {
		t.Fatalf("narrow window: want [v2], got %d: %+v", len(revs), revs)
	}

	// The cap bounds the result even when more commits are in window.
	capped, err := revisionsInWindow(repo, v2.String(), "apps/harbor", w, 1)
	if err != nil {
		t.Fatalf("revisionsInWindow (capped): %v", err)
	}
	if len(capped) != 1 || capped[0].SHA != v2.String() {
		t.Fatalf("cap=1: want [v2], got %d: %+v", len(capped), capped)
	}
}

// TestRevisionsInWindowZeroWindowAndBounds proves the public RevisionsInWindow
// returns nil (so callers keep single-revision behavior) for a zero window or a
// non-positive cap, cloning the local repo for the non-trivial case.
func TestRevisionsInWindowZeroWindow(t *testing.T) {
	dir, _, v2 := buildRepo(t)
	d := &Differ{}

	// Zero-valued window: nil, no clone/log.
	revs, err := d.RevisionsInWindow(context.Background(), dir, v2.String(), "apps/harbor", providers.TimeWindow{}, 10)
	if err != nil || revs != nil {
		t.Fatalf("zero window: want (nil,nil), got (%v,%v)", revs, err)
	}

	// max<=0: nil.
	revs, err = d.RevisionsInWindow(context.Background(), dir, v2.String(), "apps/harbor",
		providers.TimeWindow{Start: time.Unix(500, 0), End: time.Unix(2500, 0)}, 0)
	if err != nil || revs != nil {
		t.Fatalf("cap<=0: want (nil,nil), got (%v,%v)", revs, err)
	}

	// A real window over a local clone returns the in-window revisions.
	revs, err = d.RevisionsInWindow(context.Background(), dir, v2.String(), "apps/harbor",
		providers.TimeWindow{Start: time.Unix(500, 0), End: time.Unix(2500, 0)}, 10)
	if err != nil {
		t.Fatalf("RevisionsInWindow: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("want 2 in-window revisions via clone, got %d: %+v", len(revs), revs)
	}
}
