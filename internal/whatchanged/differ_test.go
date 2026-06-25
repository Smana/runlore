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
