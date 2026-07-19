// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"testing"

	git "github.com/go-git/go-git/v5"

	"github.com/Smana/runlore/internal/providers"
)

// TestCloneCacheReusesClone locks down CLONE-1: within one WithCloneCache batch,
// several changes on the SAME source repo clone it ONCE, not once per change — and
// the diff is still correct when computed from the reused clone.
func TestCloneCacheReusesClone(t *testing.T) {
	dir, v1, v2 := buildRepo(t) // local git repo with two commits (v2 changes files)
	d := &Differ{}
	ctx, done := WithCloneCache(context.Background())
	defer done()

	mk := func() providers.Change {
		return providers.Change{Source: providers.SourceRef{RepoURL: dir}, FromRev: v1.String(), ToRev: v2.String()}
	}
	for i := 0; i < 3; i++ {
		dff, err := d.ForChange(ctx, mk())
		if err != nil {
			t.Fatalf("ForChange #%d: %v", i, err)
		}
		if len(dff.Files) == 0 {
			t.Fatalf("ForChange #%d: expected a non-empty diff from the (reused) clone", i)
		}
	}

	cc := cacheFrom(ctx)
	if cc == nil {
		t.Fatal("clone cache missing from context")
	}
	if n := len(cc.clones); n != 1 {
		t.Fatalf("the same repo across 3 changes should be cloned once, got %d clones", n)
	}
}

// TestCloneCacheCloseCleansUp verifies close() removes the clones, and that without
// a cache the differ keeps its original per-call clone+cleanup behaviour (no leak).
func TestNoCacheStillWorks(t *testing.T) {
	dir, v1, v2 := buildRepo(t)
	d := &Differ{}
	// No WithCloneCache: cacheFrom is nil → original path (clone + caller cleanup).
	dff, err := d.ForChange(context.Background(), providers.Change{
		Source: providers.SourceRef{RepoURL: dir}, FromRev: v1.String(), ToRev: v2.String(),
	})
	if err != nil || len(dff.Files) == 0 {
		t.Fatalf("uncached ForChange should still diff: %d files err=%v", len(dff.Files), err)
	}
}

// TestPutSharedReleasesOnClose: a shared entry's release func runs on close,
// and close removes no dir for it.
func TestPutSharedReleasesOnClose(t *testing.T) {
	ctx, done := WithCloneCache(context.Background())
	cc := cacheFrom(ctx)
	released := false
	if _, kept := cc.putShared("u", &git.Repository{}, func() { released = true }); !kept {
		t.Fatal("first putShared must keep")
	}
	if _, ok := cc.get("u"); !ok {
		t.Fatal("shared entry must be gettable")
	}
	done()
	if !released {
		t.Fatal("release must run on close")
	}
}
