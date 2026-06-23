package catalog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initBareUpstream creates a local (non-bare) git repo with one initial commit
// and returns its path. The Syncer can clone from it via a local file path.
func initBareUpstream(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	repo, err := git.PlainInitWithOptions(src, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("initBareUpstream: init: %v", err)
	}
	commit(t, repo, src, "init.md", "# init")
	return src
}

// commitToUpstream opens the repo at src, writes a file (creating parent dirs
// as needed), and commits it.
func commitToUpstream(t *testing.T, src, name, content string) {
	t.Helper()
	repo, err := git.PlainOpen(src)
	if err != nil {
		t.Fatalf("commitToUpstream: open: %v", err)
	}
	if dir := filepath.Dir(filepath.Join(src, name)); dir != src {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("commitToUpstream: mkdir: %v", err)
		}
	}
	commit(t, repo, src, name, content)
}

// testLogger returns a logger that discards all output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSyncReportsChangeOnClone(t *testing.T) {
	src := initBareUpstream(t)
	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: "main", Dir: dir, Log: testLogger()}
	changed, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !changed {
		t.Fatal("the first sync (clone) must report changed=true")
	}
}

func TestSyncNoChangeOnRepeatedPull(t *testing.T) {
	src := initBareUpstream(t)
	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: "main", Dir: dir, Log: testLogger()}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	changed, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if changed {
		t.Fatal("a second sync with no upstream commit must report changed=false")
	}
}

func TestSyncReportsChangeAfterNewCommit(t *testing.T) {
	src := initBareUpstream(t)
	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: "main", Dir: dir, Log: testLogger()}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	commitToUpstream(t, src, "runbooks/new.md", "# new")
	changed, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("third Sync: %v", err)
	}
	if !changed {
		t.Fatal("a sync after a new upstream commit must report changed=true")
	}
}

func commit(t *testing.T, repo *git.Repository, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(name); err != nil {
		t.Fatal(err)
	}
	_, err = wt.Commit("add "+name, &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1_700_000_000, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSyncerCloneAndPull(t *testing.T) {
	src := t.TempDir()
	repo, err := git.PlainInitWithOptions(src, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	commit(t, repo, src, "first.md", "---\ntitle: First\n---\nx")

	mirror := filepath.Join(t.TempDir(), "mirror")
	s := &Syncer{URL: src, Branch: "main", Dir: mirror, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// First Sync clones.
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("clone sync: %v", err)
	}
	if es, _, _ := Load(mirror); len(es) != 1 || es[0].Title != "First" {
		t.Fatalf("after clone: %v", titles(es))
	}

	// A new commit (e.g. a merged curation PR) appears upstream; Sync fast-forwards.
	commit(t, repo, src, "second.md", "---\ntitle: Second\n---\ny")
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("pull sync: %v", err)
	}
	if es, _, _ := Load(mirror); len(es) != 2 {
		t.Fatalf("after pull: %d entries, want 2", len(es))
	}
}
