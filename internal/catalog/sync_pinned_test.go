// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// tagUpstream puts an ANNOTATED tag on the current upstream HEAD and returns the
// commit it names. Annotated is what real releases use, and it is the case that
// breaks a naive implementation: the tag ref resolves to a tag OBJECT, not to the
// commit, so anything that checks it out has to peel it first.
func tagUpstream(t *testing.T, src, tag string) plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(src)
	if err != nil {
		t.Fatalf("tagUpstream: open: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("tagUpstream: head: %v", err)
	}
	_, err = repo.CreateTag(tag, head.Hash(), &git.CreateTagOptions{
		Tagger:  &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1_700_000_000, 0)},
		Message: tag,
	})
	if err != nil {
		t.Fatalf("tagUpstream: tag %s: %v", tag, err)
	}
	return head.Hash()
}

// upstreamHead returns the current HEAD commit of the upstream repo at src.
func upstreamHead(t *testing.T, src string) plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(src)
	if err != nil {
		t.Fatalf("upstreamHead: open: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("upstreamHead: head: %v", err)
	}
	return head.Hash()
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// pinnedUpstream builds an upstream with a v1.0.0 tag, then moves on past it:
//
//	init.md, tagged.md   <- refs/tags/v1.0.0
//	later.md             <- main
//
// The pinned mirror must hold the first two and never the third.
func pinnedUpstream(t *testing.T) (src string, tagged plumbing.Hash) {
	t.Helper()
	src = initBareUpstream(t)
	commitToUpstream(t, src, "tagged.md", "# tagged")
	tagged = tagUpstream(t, src, "v1.0.0")
	commitToUpstream(t, src, "later.md", "# later")
	return src, tagged
}

// TestSyncPinnedTagChecksOutThatRevision: the whole point of the option. The
// mirror must hold the tagged tree, not whatever the default branch moved on to.
func TestSyncPinnedTagChecksOutThatRevision(t *testing.T) {
	src, tagged := pinnedUpstream(t)
	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: "refs/tags/v1.0.0", Dir: dir, Log: testLogger()}

	changed, _, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !changed {
		t.Fatal("the first sync must report changed=true so the catalog indexes it")
	}
	if !exists(t, filepath.Join(dir, "tagged.md")) {
		t.Error("the tagged revision's content is missing from the mirror")
	}
	if exists(t, filepath.Join(dir, "later.md")) {
		t.Error("a commit made AFTER the pinned tag reached the mirror — the pin did not hold")
	}
	if es, _, _ := Load(dir); len(es) != 2 {
		t.Errorf("indexed %d entries, want 2 (init.md + tagged.md)", len(es))
	}
	if s.lastRev != tagged {
		t.Errorf("lastRev = %s, want the commit the annotated tag names (%s) — an unpeeled tag object breaks the delta", s.lastRev, tagged)
	}
}

// TestSyncPinnedCommitChecksOutThatRevision: same guarantee via a bare object id,
// which cannot be a clone target at all and has to be checked out after the fetch.
func TestSyncPinnedCommitChecksOutThatRevision(t *testing.T) {
	src := initBareUpstream(t)
	commitToUpstream(t, src, "pinned.md", "# pinned")
	want := upstreamHead(t, src)
	commitToUpstream(t, src, "later.md", "# later")

	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: want.String(), Dir: dir, Log: testLogger()}
	changed, _, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !changed {
		t.Fatal("the first sync must report changed=true")
	}
	if !exists(t, filepath.Join(dir, "pinned.md")) {
		t.Error("the pinned commit's content is missing from the mirror")
	}
	if exists(t, filepath.Join(dir, "later.md")) {
		t.Error("a later commit reached the mirror — the pin did not hold")
	}
	if s.lastRev != want {
		t.Errorf("lastRev = %s, want %s", s.lastRev, want)
	}
}

// TestSyncPinnedRevisionIgnoresUpstreamMovement: upstream keeps committing; the
// pinned mirror reports no change and re-indexes nothing. Without this the option
// would be cosmetic — the operator would still get unreviewed upstream edits.
func TestSyncPinnedRevisionIgnoresUpstreamMovement(t *testing.T) {
	src, _ := pinnedUpstream(t)
	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: "refs/tags/v1.0.0", Dir: dir, Log: testLogger()}
	if _, _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	commitToUpstream(t, src, "even-later.md", "# even later")
	changed, delta, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if changed || delta != nil {
		t.Fatalf("a pinned sync must never move: changed=%v delta=%+v", changed, delta)
	}
	if exists(t, filepath.Join(dir, "even-later.md")) {
		t.Error("an upstream commit made after the pin reached the mirror")
	}
}

// TestSyncPinnedPollTouchesNoRemote: a pin cannot move, so re-checking it must not
// cost a fetch every interval. Proven by making the remote unreachable (the local
// upstream is renamed away) and requiring the poll to still succeed — any network
// call would fail there.
func TestSyncPinnedPollTouchesNoRemote(t *testing.T) {
	src, _ := pinnedUpstream(t)
	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: "refs/tags/v1.0.0", Dir: dir, Log: testLogger()}
	if _, _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	if err := os.Rename(src, src+".gone"); err != nil {
		t.Fatalf("rename upstream: %v", err)
	}
	changed, _, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("a pinned poll must resolve locally, but it reached for the remote: %v", err)
	}
	if changed {
		t.Fatal("a pinned poll must report changed=false")
	}
}

// TestSyncPinnedFetchesARevisionTheMirrorHasNotSeen: the operator bumps the pin
// after reviewing what upstream changed, and the process restarts onto the mirror
// it already has. The new tag is not in that checkout, so the syncer has to fetch
// before it can honour the pin.
func TestSyncPinnedFetchesARevisionTheMirrorHasNotSeen(t *testing.T) {
	src, _ := pinnedUpstream(t)
	dir := t.TempDir()
	if _, _, err := (&Syncer{URL: src, Branch: "refs/tags/v1.0.0", Dir: dir, Log: testLogger()}).Sync(context.Background()); err != nil {
		t.Fatalf("v1 Sync: %v", err)
	}

	commitToUpstream(t, src, "v2-only.md", "# v2")
	want := tagUpstream(t, src, "v2.0.0")

	bumped := &Syncer{URL: src, Branch: "refs/tags/v2.0.0", Dir: dir, Log: testLogger()}
	changed, _, err := bumped.Sync(context.Background())
	if err != nil {
		t.Fatalf("bumped Sync: %v", err)
	}
	if !changed {
		t.Fatal("moving the pin must report changed=true so the catalog re-indexes")
	}
	if bumped.lastRev != want {
		t.Errorf("lastRev = %s, want %s", bumped.lastRev, want)
	}
	if !exists(t, filepath.Join(dir, "v2-only.md")) {
		t.Error("the bumped pin's content is missing — the syncer never fetched the new tag")
	}
}

// TestSyncPinnedRecoversFromCorruptMirror: the wedge fix has to hold on the pinned
// path too — its clone options differ, so it is a genuinely separate code path.
func TestSyncPinnedRecoversFromCorruptMirror(t *testing.T) {
	src, _ := pinnedUpstream(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Syncer{URL: src, Branch: "refs/tags/v1.0.0", Dir: dir, Log: testLogger()}
	if _, _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("a corrupt mirror must be discarded and re-cloned at the pin, got: %v", err)
	}
	if !exists(t, filepath.Join(dir, "tagged.md")) {
		t.Error("recovery clone did not land on the pinned revision")
	}
}

// TestSyncPinnedRevisionNotFoundFails: a typo'd pin must surface as an error, not
// as a mirror quietly sitting on the default branch. Fail closed — the operator
// asked for a specific revision and must not silently get a different one.
//
// Both rows are needed and they fail in different places: a missing tag is a ref,
// so the clone itself cannot find it; a missing commit id is not a ref, so the
// clone succeeds and only the post-fetch resolve can catch it.
func TestSyncPinnedRevisionNotFoundFails(t *testing.T) {
	for _, tc := range []struct{ name, rev string }{
		{"tag", "refs/tags/v9.9.9"},
		{"commit id", "0123456789abcdef0123456789abcdef01234567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, _ := pinnedUpstream(t)
			dir := t.TempDir()
			s := &Syncer{URL: src, Branch: tc.rev, Dir: dir, Log: testLogger()}
			if _, _, err := s.Sync(context.Background()); err == nil {
				t.Fatal("a pin that does not exist upstream must fail the sync")
			}
			if exists(t, filepath.Join(dir, "tagged.md")) {
				t.Error("a failed pin left a checkout behind; the catalog would index a revision nobody asked for")
			}
		})
	}
}

// TestRunRetriesReindexOnAPinnedRevision: Run rolls lastRev back when the re-index
// fails so the next tick retries. With a pin, HEAD never moves again — if the retry
// leaned on upstream movement the catalog would stay empty forever.
func TestRunRetriesReindexOnAPinnedRevision(t *testing.T) {
	src, _ := pinnedUpstream(t)
	tick := make(chan time.Time)
	s := &Syncer{URL: src, Branch: "refs/tags/v1.0.0", Dir: t.TempDir(), Log: testLogger(), tick: tick}

	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, time.Hour, func(*SyncDelta) error {
			if calls.Add(1) == 1 {
				return context.DeadlineExceeded // first re-index fails
			}
			return nil
		})
	}()
	waitCycle := func() {
		t.Helper()
		select {
		case tick <- time.Time{}:
		case <-time.After(30 * time.Second):
			t.Fatal("Run never came back for a tick — the poll loop is stuck")
		}
	}
	waitCycle() // accepted only once the initial (failing) sync finished
	if n := calls.Load(); n != 1 {
		t.Fatalf("initial sync fired onSync %d times, want 1", n)
	}
	waitCycle() // drains the retry poll started by the send above
	if n := calls.Load(); n != 2 {
		t.Fatalf("a failed re-index must be retried on the next tick even though the pin cannot move, fired %d", n)
	}
	waitCycle() // the retry succeeded; nothing may fire again
	if n := calls.Load(); n != 2 {
		t.Fatalf("after a successful re-index a pinned poll must not re-index again, fired %d", n)
	}
	cancel()
	<-done
}
