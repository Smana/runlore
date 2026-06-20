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
