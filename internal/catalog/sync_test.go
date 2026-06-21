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
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("clone sync: %v", err)
	}
	if es, _, _ := Load(mirror); len(es) != 1 || es[0].Title != "First" {
		t.Fatalf("after clone: %v", titles(es))
	}

	// A new commit (e.g. a merged curation PR) appears upstream; Sync fast-forwards.
	commit(t, repo, src, "second.md", "---\ntitle: Second\n---\ny")
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("pull sync: %v", err)
	}
	if es, _, _ := Load(mirror); len(es) != 2 {
		t.Fatalf("after pull: %d entries, want 2", len(es))
	}
}
