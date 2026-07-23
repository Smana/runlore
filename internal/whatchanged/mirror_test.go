// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestMirrorAcquireClonesOnce: first Acquire clones a bare mirror; a second
// Acquire reuses the same on-disk dir (no re-clone) and still resolves commits.
func TestMirrorAcquireClonesOnce(t *testing.T) {
	src, v1, _ := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	repo, release, err := mc.Acquire(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, err := resolveCommit(repo, v1.String()); err != nil {
		t.Fatalf("resolve v1 in mirror: %v", err)
	}
	release()
	entries, _ := os.ReadDir(mc.dir)
	if len(entries) != 1 {
		t.Fatalf("want 1 mirror dir, got %d", len(entries))
	}
	repo2, release2, err := mc.Acquire(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	defer release2()
	if _, err := resolveCommit(repo2, v1.String()); err != nil {
		t.Fatalf("resolve v1 on reuse: %v", err)
	}
}

// TestMirrorAcquireFetchesNewCommits: a commit pushed to the source AFTER the
// mirror was created is visible on the next Acquire (incremental fetch).
func TestMirrorAcquireFetchesNewCommits(t *testing.T) {
	src, _, _ := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := mc.Acquire(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	v3 := addCommit(t, src, "apps/harbor/values.yaml", "version: 1.16.0\n", "v3", 3000)
	repo, release2, err := mc.Acquire(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if _, err := resolveCommit(repo, v3.String()); err != nil {
		t.Fatalf("v3 not fetched into mirror: %v", err)
	}
}

// TestMirrorAcquireConcurrent: N concurrent Acquire/release cycles on the same
// URL race-cleanly (run with -race). Each goroutine must resolve a known SHA.
func TestMirrorAcquireConcurrent(t *testing.T) {
	src, v1, _ := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo, release, err := mc.Acquire(context.Background(), src, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			if _, err := resolveCommit(repo, v1.String()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// TestMirrorAcquireBadURL: an unclonable URL returns an error and leaves no
// half-created mirror dir behind.
func TestMirrorAcquireBadURL(t *testing.T) {
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mc.Acquire(context.Background(), filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Fatal("want error for unclonable URL")
	}
	entries, _ := os.ReadDir(mc.dir)
	if len(entries) != 0 {
		t.Fatalf("want no leftover dirs, got %d", len(entries))
	}
}

// TestMirrorEviction: with max=2, acquiring a 3rd distinct repo evicts the
// oldest-mtime mirror; the two newest survive.
func TestMirrorEviction(t *testing.T) {
	srcA, _, _ := buildRepo(t)
	srcB, _, _ := buildRepo(t)
	srcC, _, _ := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{srcA, srcB} {
		_, release, err := mc.Acquire(context.Background(), src, nil)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	// Age A so it is the eviction victim regardless of clone timing.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(mc.dir, mirrorKey(srcA)), old, old); err != nil {
		t.Fatal(err)
	}
	_, release, err := mc.Acquire(context.Background(), srcC, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(filepath.Join(mc.dir, mirrorKey(srcA))); !os.IsNotExist(err) {
		t.Fatal("oldest mirror (A) should have been evicted")
	}
	for _, src := range []string{srcB, srcC} {
		if _, err := os.Stat(filepath.Join(mc.dir, mirrorKey(src))); err != nil {
			t.Fatalf("mirror for %s should survive: %v", src, err)
		}
	}
}

// TestMirrorEvictionSparesNonMirrorDirs: eviction must not touch directories
// that were not created by MirrorCache itself (e.g. the "source" subdir used by
// the source_diff cache, or stray files on an operator PV). With max=1 and two
// distinct repos acquired sequentially, the foreign "source" subdir must survive.
func TestMirrorEvictionSparesNonMirrorDirs(t *testing.T) {
	srcA, _, _ := buildRepo(t)
	srcB, _, _ := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the cache with A.
	_, release, err := mc.Acquire(context.Background(), srcA, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()

	// Plant a foreign "source" subdir (the source_diff mirror cache location)
	// and a stray file — neither is a mirror key.
	foreignDir := filepath.Join(mc.dir, "source")
	if err := os.MkdirAll(foreignDir, 0o700); err != nil {
		t.Fatal(err)
	}
	markerFile := filepath.Join(foreignDir, "marker.txt")
	if err := os.WriteFile(markerFile, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	strayFile := filepath.Join(mc.dir, "stray.txt")
	if err := os.WriteFile(strayFile, []byte("also keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Age A so it is the eviction victim.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(mc.dir, mirrorKey(srcA)), old, old); err != nil {
		t.Fatal(err)
	}

	// Acquiring B forces eviction of A (max=1, so we need to free a slot).
	_, release2, err := mc.Acquire(context.Background(), srcB, nil)
	if err != nil {
		t.Fatal(err)
	}
	release2()

	// A should be gone.
	if _, err := os.Stat(filepath.Join(mc.dir, mirrorKey(srcA))); !os.IsNotExist(err) {
		t.Fatal("oldest mirror (A) should have been evicted")
	}

	// Foreign dirs and stray files must survive eviction.
	if _, err := os.Stat(markerFile); err != nil {
		t.Fatalf("foreign source/marker.txt was deleted by eviction: %v", err)
	}
	if _, err := os.Stat(strayFile); err != nil {
		t.Fatalf("stray file was deleted by eviction: %v", err)
	}
}

// addCommit writes one file into the fixture repo at dir and commits it with a
// fixed timestamp, returning the new commit hash.
func addCommit(t testing.TB, dir, rel, content, msg string, sec int64) plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
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
	h, err := wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@x", When: time.Unix(sec, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
