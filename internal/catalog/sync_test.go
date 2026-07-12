// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
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

// TestSyncRecoversFromCorruptMirror locks down the wedge fix: a present-but-broken
// .git (e.g. an earlier clone killed mid-write) is discarded and re-cloned, instead
// of erroring on every future Pull forever.
func TestSyncRecoversFromCorruptMirror(t *testing.T) {
	src := initBareUpstream(t)
	dir := t.TempDir()
	// Plant a corrupt mirror: a `.git` that PlainOpen cannot read (Stat still sees it).
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Syncer{URL: src, Branch: "main", Dir: dir, Log: testLogger()}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("Sync should recover by re-cloning a corrupt mirror, got: %v", err)
	}
	if es, _, _ := Load(dir); len(es) != 1 {
		t.Fatalf("after recovery clone: want 1 entry, got %d", len(es))
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

// TestRunReloadsOnlyOnChange locks down the core of #18 Part A: Run calls onSync on
// the first sync, then NOT again while upstream HEAD is unchanged, and again once a
// new commit moves HEAD. The `if changed { onSync() }` gate is what prevents the
// every-poll full index rebuild.
//
// It drives the poll ticks itself rather than sleeping. The previous version slept a
// fixed 120ms and then asserted the initial sync had already fired — silently assuming
// a real `git clone` completes within that window. On a loaded CI runner it does not,
// and the test failed on changes that could not possibly affect it. Nothing here
// depends on wall-clock time now.
func TestRunReloadsOnlyOnChange(t *testing.T) {
	src := initBareUpstream(t)
	dir := t.TempDir()
	tick := make(chan time.Time) // unbuffered — see waitCycle
	s := &Syncer{URL: src, Branch: "main", Dir: dir, Log: testLogger(), tick: tick}

	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, time.Hour, func() error { calls.Add(1); return nil }) // interval unused: tick drives the loop
	}()

	// Run only receives from the (unbuffered) tick channel between poll cycles, so a
	// send that completes proves the PREVIOUS cycle has finished. That is the
	// synchronisation point which replaces every sleep.
	waitCycle := func() {
		t.Helper()
		select {
		case tick <- time.Time{}:
		case <-time.After(30 * time.Second): // a hang, not a timing assumption
			t.Fatal("Run never came back for a tick — the poll loop is stuck")
		}
	}

	// The first send is accepted only once the INITIAL sync has completed.
	waitCycle()
	if n := calls.Load(); n != 1 {
		t.Fatalf("initial sync must fire onSync exactly once, fired %d", n)
	}

	// That tick ran a poll against an unchanged upstream; the next send proves it ended.
	waitCycle()
	if n := calls.Load(); n != 1 {
		t.Fatalf("with no HEAD change onSync must not fire again, fired %d", n)
	}

	// Move HEAD. BOTH sends below are load-bearing, and neither is redundant.
	//
	// Accepting a send starts the next poll immediately, so the poll begun by the send
	// above is in flight RIGHT NOW and races this commit: it may fetch the new HEAD, or
	// it may fetch just before the ref moves and see nothing. Either is fine — every
	// error/no-change path in Sync returns before `s.lastRev = rev`, so a poll that
	// misses the commit changes no state.
	//
	// The first send therefore only DRAINS that racing poll (proving it ended); it does
	// not prove a post-commit poll ran. The second send proves the poll that started
	// after the commit was already visible has completed. onSync fires on exactly one of
	// the two — whichever first sees rev != lastRev — so the count is 2 either way.
	commitToUpstream(t, src, "new.md", "# new")
	waitCycle()
	waitCycle()
	if n := calls.Load(); n != 2 {
		t.Fatalf("a new upstream commit must trigger exactly one more onSync, total %d (want 2)", n)
	}

	cancel()
	<-done
}
