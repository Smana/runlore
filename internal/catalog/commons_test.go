// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"testing"
)

// writeOKF drops a minimal, valid OKF entry into dir, on top of the package's
// existing raw writeEntry helper.
func writeOKF(t *testing.T, dir, name, title, body string) {
	t.Helper()
	writeEntry(t, dir, name,
		"---\ntype: Playbook\ntitle: "+title+"\ndescription: "+title+"\ntags: [test]\n---\n\n"+body+"\n")
}

// TestCommonsRootIsIndexedAlongsideTheUsersOwn pins the point of a commons
// catalog: an adopter with an empty KB still gets something back from kb_search.
func TestCommonsRootIsIndexedAlongsideTheUsersOwn(t *testing.T) {
	own, commons := t.TempDir(), t.TempDir()
	writeOKF(t, own, "mine.md", "payments checkout latency", "our own runbook for checkout latency")
	writeOKF(t, commons, "shared.md", "crashloopbackoff after deploy", "generic guidance for a crash-looping pod")

	c := NewEmpty()
	c.SetCommonsDir(commons)
	if _, err := c.ReloadContext(context.Background(), own); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("both roots must be indexed, got %d entries", got)
	}

	hits, err := c.Search("crashloopbackoff", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("a commons entry must be findable by search — that is the whole point")
	}
	if !hits[0].Commons {
		t.Errorf("the commons entry must be MARKED as commons, or an on-call cannot tell "+
			"generic advice from their platform's recorded truth: %+v", hits[0])
	}

	// And the user's own entry must not be marked.
	mine, err := c.Search("checkout latency", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) == 0 || mine[0].Commons {
		t.Errorf("the user's own entry must not be flagged commons: %+v", mine)
	}
}

// TestUsersOwnEntryWinsATie is the important one: your platform's recorded truth
// outranks generic advice whenever the two score equally.
//
// THE FILENAMES ARE LOAD-BEARING. bleve breaks equal-score ties by document ID, and
// the document ID is Entry.Path. This test's original fixture used "a.md" (own) and
// "b.md" (commons, indexed as "commons/b.md") — so bleve ALREADY returned the
// operator's entry first and SearchScored's provenance sort was a no-op. Deleting the
// tie-break entirely left the test green. It was a guard over nothing.
//
// So the commons entry is deliberately named to sort BEFORE the operator's, making
// bleve hand back the WRONG order and leaving the provenance sort as the only thing
// that can fix it. The invariant is asserted below rather than left to a comment,
// because a rename would otherwise silently return this test to vacuity.
func TestUsersOwnEntryWinsATie(t *testing.T) {
	own, commons := t.TempDir(), t.TempDir()
	// Byte-identical bodies and titles => identical BM25 scores. The ONLY thing
	// that can order them is provenance.
	const same = "readiness probe failing after a deploy"
	const ownName, commonsName = "z-our-incident.md", "a-generic-playbook.md"
	commonsDocID := commonsPathPrefix + commonsName
	if commonsDocID >= ownName {
		t.Fatalf("fixture is no longer adversarial: bleve breaks equal-score ties by doc ID, "+
			"so the commons doc ID (%q) must sort BEFORE the operator's (%q) — otherwise "+
			"bleve's own order already satisfies the assertion and this test passes with "+
			"the provenance tie-break deleted", commonsDocID, ownName)
	}
	writeOKF(t, own, ownName, same, same)
	writeOKF(t, commons, commonsName, same, same)

	c := NewEmpty()
	c.SetCommonsDir(commons)
	if _, err := c.ReloadContext(context.Background(), own); err != nil {
		t.Fatalf("reload: %v", err)
	}
	hits, err := c.SearchScored(same, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("want both entries, got %d", len(hits))
	}
	// Not a Skip. Identical corpus text must produce identical BM25 scores; if it
	// stops doing so the premise of this test is gone, and skipping would retire the
	// tie-break guard in silence rather than telling anyone.
	if hits[0].Score != hits[1].Score {
		t.Fatalf("byte-identical entries scored differently (%v vs %v) — the tie-break can no "+
			"longer be exercised by this fixture; re-derive it", hits[0].Score, hits[1].Score)
	}
	if hits[0].Entry.Commons {
		t.Error("at equal score the user's OWN entry must rank first; a shipped generic " +
			"entry outranking the operator's own runbook is the wrong default")
	}
}

// TestCommonsEntriesAreNotWritable proves the curator cannot write into the
// commons root. Entries are read-only by construction: the commons is a shared,
// upstream corpus, and a local curation writing into it would both corrupt the
// shared view and silently diverge from upstream on the next sync.
func TestCommonsEntriesAreNotWritable(t *testing.T) {
	own, commons := t.TempDir(), t.TempDir()
	writeOKF(t, commons, "shared.md", "dns resolution failure", "generic dns guidance")

	c := NewEmpty()
	c.SetCommonsDir(commons)
	if _, err := c.ReloadContext(context.Background(), own); err != nil {
		t.Fatalf("reload: %v", err)
	}
	hits, err := c.Search("dns resolution", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("setup: commons entry not found (%v)", err)
	}
	if got := c.WriteRootFor(hits[0]); got != "" {
		t.Errorf("a commons entry must have NO writable root, got %q — the curator would "+
			"otherwise write into a corpus it does not own", got)
	}
	// A user-owned entry still resolves to the writable root.
	writeOKF(t, own, "mine.md", "payments latency", "our runbook")
	if _, err := c.ReloadContext(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	mine, _ := c.Search("payments latency", 5)
	if len(mine) == 0 || c.WriteRootFor(mine[0]) != own {
		t.Errorf("a user entry must resolve to the writable root %q, got %q", own, c.WriteRootFor(mine[0]))
	}
}

// TestSameFilenameInBothRootsDoesNotCollide pins the path namespacing.
//
// Entry.Path is relative to its OWN root, so "crashloop.md" in the user's KB and
// "crashloop.md" in the commons produce the same Path. pathIdx is keyed by Path and
// bleve keys its documents by Path, so without a prefix the second entry overwrites
// the first in both structures — one entry silently disappears, and every search hit
// for the survivor resolves to the wrong entry.
//
// The earlier tests all used distinct filenames, so they passed with the prefix
// removed. This is the one that fails.
func TestSameFilenameInBothRootsDoesNotCollide(t *testing.T) {
	own, commons := t.TempDir(), t.TempDir()
	const name = "crashloop.md"
	writeOKF(t, own, name, "our checkout service crashloops on rollout", "team-specific rollout guidance")
	writeOKF(t, commons, name, "generic crashloopbackoff after deploy", "generic guidance")

	c := NewEmpty()
	c.SetCommonsDir(commons)
	if _, err := c.ReloadContext(context.Background(), own); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("same filename in both roots must yield TWO entries, got %d — one was "+
			"overwritten by the other", got)
	}

	// Both must be independently retrievable, and each must resolve to itself.
	ours, err := c.Search("checkout service rollout", 5)
	if err != nil || len(ours) == 0 {
		t.Fatalf("the user's entry is unreachable (%v)", err)
	}
	if ours[0].Commons {
		t.Errorf("search for the user's entry resolved to the COMMONS entry — the doc IDs collided: %+v", ours[0])
	}
	theirs, err := c.Search("generic crashloopbackoff", 5)
	if err != nil || len(theirs) == 0 {
		t.Fatalf("the commons entry is unreachable (%v)", err)
	}
	if !theirs[0].Commons {
		t.Errorf("search for the commons entry resolved to the USER's entry — the doc IDs collided: %+v", theirs[0])
	}
}

// TestIncrementalSyncKeepsTheCommonsIndexed: ReloadDelta replaced entries and
// pathIdx wholesale with what Load(operatorRoot) returned, while the delta only
// ever describes the operator's root. The commons documents stayed in the live
// bleve index but vanished from pathIdx, so SearchScored dropped them — and they
// still consumed top-k slots, so searches also returned fewer results than asked
// for. Every operator sync after the first silently un-indexed the whole shared
// corpus until the commons syncer's own much slower tick healed it.
func TestIncrementalSyncKeepsTheCommonsIndexed(t *testing.T) {
	own := t.TempDir()
	commons := t.TempDir()
	writeEntry(t, own, "mine.md", "---\ntype: Incident\ntitle: My own outage\ndescription: mine\nresource: apps/web\n---\nbody\n")
	writeEntry(t, commons, "generic.md", "---\ntype: Playbook\ntitle: Generic crashloop playbook\ndescription: generic\n---\nbody\n")

	cat := NewEmpty()
	cat.SetCommonsDir(commons)
	if _, err := cat.ReloadContext(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	if cat.Len() != 2 {
		t.Fatalf("fixture: want both roots indexed, got %d", cat.Len())
	}

	// An incremental sync of the OPERATOR's root only — the normal steady state.
	writeEntry(t, own, "second.md", "---\ntype: Incident\ntitle: Another outage\ndescription: second\nresource: apps/api\n---\nbody\n")
	if _, err := cat.ReloadDelta(context.Background(), own, &SyncDelta{Changed: []string{"second.md"}}); err != nil {
		t.Fatal(err)
	}
	if cat.Len() != 3 {
		t.Fatalf("after an incremental sync the catalog has %d entries, want 3 — the commons was dropped", cat.Len())
	}
	hits, _ := cat.SearchScored("generic crashloop playbook", 5)
	found := false
	for _, h := range hits {
		if h.Entry.Commons {
			found = true
		}
	}
	if !found {
		t.Fatalf("the commons entry is no longer searchable after an incremental sync of the operator's own root; hits=%d", len(hits))
	}
}
