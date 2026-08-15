// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// The retire/revalidate passes keep no store. They reconstruct an entry's history
// from the forge on every run, and the ONE thing that makes a closed-unmerged
// proposal meaningful is that only a human can produce it: it is read back as
// "a human declined — never ask again". A close by RunLore's own housekeeping is
// byte-for-byte the same signal, so it becomes a permanent veto for a decision
// nobody made.
//
// The tests below pin that distinction at both places curate can close a PR.
// Each arms the hazard first and asserts it IS armed, so the guard cannot pass
// vacuously, and each keeps a control PR that must still be closed — proving the
// exclusion is targeted rather than a disabled pass.

const (
	crashloop = "runbooks/kubernetes/pod-crashloop.md"
	pending   = "runbooks/kubernetes/pod-pending.md"
	oomkilled = "runbooks/kubernetes/pod-oomkilled.md"
)

// entryEditProposals is the fixture both tests share: two revalidate proposals for
// DISTINCT entries filed under one directory, plus a retire proposal.
func entryEditProposals(updated time.Time) []providers.CuratedIssue {
	return []providers.CuratedIssue{
		{Number: 10, Title: "KB revalidate: " + crashloop, UpdatedAt: updated,
			Labels: []string{"runlore", revalidateLabel}, Body: revalidateMarker(crashloop)},
		{Number: 11, Title: "KB revalidate: " + pending, UpdatedAt: updated,
			Labels: []string{"runlore", revalidateLabel}, Body: revalidateMarker(pending)},
		{Number: 12, Title: "KB retire: " + oomkilled, UpdatedAt: updated,
			Labels: []string{"runlore", retireLabel}, Body: retireMarker(oomkilled)},
	}
}

func TestDedupNeverClosesAnEntryEditProposal(t *testing.T) {
	// Arm the hazard: these two titles differ only in the entry FILENAME, so the
	// markerless title-Jaccard fallback scores them as near-identical even though
	// they concern unrelated entries.
	score := jaccard(titleTokens("KB revalidate: "+crashloop), titleTokens("KB revalidate: "+pending))
	if score < 0.6 {
		t.Fatalf("fixture titles score %.2f, below Dedup's default threshold — this test "+
			"no longer exercises the hazard it exists to pin", score)
	}

	prs := entryEditProposals(time.Time{})
	// Control: two genuine duplicate KB drafts, which Dedup must still collapse.
	prs = append(prs,
		providers.CuratedIssue{Number: 20, Title: "KB: Kustomization DependencyNotReady missing GitRepository"},
		providers.CuratedIssue{Number: 21, Title: "KB: Kustomization DependencyNotReady due to missing GitRepository"},
	)
	f := &fakeForge{prs: prs}
	if err := (Dedup{Forge: f, Log: discardLog()}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, n := range []int{10, 11, 12} {
		if containsInt(f.closed, n) {
			t.Errorf("Dedup closed entry-edit proposal #%d — that close is indistinguishable "+
				"from a human veto and permanently silences the entry (closed=%v)", n, f.closed)
		}
	}
	if len(f.closed) != 1 || f.closed[0] != 21 {
		t.Fatalf("the duplicate KB draft #21 must still close, got %v", f.closed)
	}
}

func TestLifecycleNeverClosesAnEntryEditProposal(t *testing.T) {
	now := lifecycleNow()
	const staleAfter = 30 * 24 * time.Hour
	updated := now.Add(-40 * 24 * time.Hour)
	// Arm the hazard: the proposals are past the staleness horizon, so only the
	// exclusion keeps them open. The shipped chart makes this the DEFAULT collision
	// — curate.stale_after and curate.revalidation.min_interval are both 720h.
	if now.Sub(updated) <= staleAfter {
		t.Fatalf("fixture PRs are not stale, so this test no longer exercises the hazard")
	}

	prs := entryEditProposals(updated)
	// Control: an ordinary stale KB draft, which the sweep must still close.
	prs = append(prs, providers.CuratedIssue{Number: 20, Labels: []string{"runlore"}, UpdatedAt: updated})
	f := &fakeForge{prs: prs}
	l := Lifecycle{Forge: f, StaleAfter: staleAfter, Now: func() time.Time { return now }, Log: discardLog()}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, n := range []int{10, 11, 12} {
		if containsInt(f.closed, n) {
			t.Errorf("the stale sweep closed entry-edit proposal #%d — a slow reviewer would "+
				"become a permanent veto (closed=%v)", n, f.closed)
		}
	}
	if len(f.closed) != 1 || f.closed[0] != 20 {
		t.Fatalf("the ordinary stale KB draft #20 must still close, got %v", f.closed)
	}
}

// TestEntryEditProposalDetectionIsLabelExact is the mutation guard for the
// predicate itself: it must fire on each pass label and stay quiet on the shared
// "runlore" namespace label, which every KB draft also carries — keying on that
// would disable Dedup and the stale sweep wholesale.
func TestEntryEditProposalDetectionIsLabelExact(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"retire proposal", []string{"runlore", retireLabel}, true},
		{"revalidate proposal", []string{"runlore", revalidateLabel}, true},
		{"ordinary KB draft", []string{"runlore"}, false},
		{"protected KB draft", []string{"runlore", "ready-to-merge"}, false},
		{"no labels", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEntryEditProposal(tc.labels); got != tc.want {
				t.Errorf("isEntryEditProposal(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// An operator note (internal/thread.ConceptEntry) is a human's own contribution
// filed via thread capture, not a curator-drafted finding. It carries the same
// self-veto hazard as an entry-edit proposal — a close by RunLore's own
// housekeeping is indistinguishable from a human veto — for a stronger reason:
// ConceptEntry deliberately leaves Fingerprint unset, so every note PR falls
// through dedup's title-Jaccard fallback, and every note PR shares the title
// prefix "KB: Operator note: <finding title>". Two notes on the SAME recurring
// incident therefore score a Jaccard of 1.0 — the strongest possible match —
// with no label protection at all.

// operatorNoteTitle is the shared, colliding title two notes on the same
// recurring incident would carry — arming the hazard these tests pin.
const operatorNoteTitle = "KB: Operator note: Kustomization DependencyNotReady"

func TestDedupNeverClosesAnOperatorNote(t *testing.T) {
	// Arm the hazard: identical titles score the maximum possible Jaccard.
	if score := jaccard(titleTokens(operatorNoteTitle), titleTokens(operatorNoteTitle)); score != 1 {
		t.Fatalf("fixture titles score %.2f, not the maximal match this test needs to exercise the hazard", score)
	}

	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 30, Title: operatorNoteTitle, Labels: []string{"runlore", operatorNoteLabel}},
		{Number: 31, Title: operatorNoteTitle, Labels: []string{"runlore", operatorNoteLabel}},
		// Control: two genuine curated findings (no note label) must still dedup —
		// the exclusion must not weaken dedup for ordinary drafts.
		{Number: 20, Title: "KB: Kustomization DependencyNotReady missing GitRepository"},
		{Number: 21, Title: "KB: Kustomization DependencyNotReady due to missing GitRepository"},
	}}
	if err := (Dedup{Forge: f, Log: discardLog()}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, n := range []int{30, 31} {
		if containsInt(f.closed, n) {
			t.Errorf("Dedup closed operator note #%d as a duplicate of the other — RunLore closing "+
				"one human's correction is indistinguishable from a human veto of it (closed=%v)", n, f.closed)
		}
	}
	if len(f.closed) != 1 || f.closed[0] != 21 {
		t.Fatalf("the genuine duplicate curated finding #21 must still close, got %v", f.closed)
	}
}

func TestLifecycleNeverClosesAnOperatorNote(t *testing.T) {
	now := lifecycleNow()
	const staleAfter = 30 * 24 * time.Hour
	updated := now.Add(-40 * 24 * time.Hour)
	if now.Sub(updated) <= staleAfter {
		t.Fatalf("fixture PRs are not stale, so this test no longer exercises the hazard")
	}

	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 30, Labels: []string{"runlore", operatorNoteLabel}, UpdatedAt: updated},
		// Control: an ordinary stale KB draft, which the sweep must still close.
		{Number: 20, Labels: []string{"runlore"}, UpdatedAt: updated},
	}}
	l := Lifecycle{Forge: f, StaleAfter: staleAfter, Now: func() time.Time { return now }, Log: discardLog()}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if containsInt(f.closed, 30) {
		t.Errorf("the stale sweep closed operator note #30 — an unreviewed human correction "+
			"would be silently discarded (closed=%v)", f.closed)
	}
	if len(f.closed) != 1 || f.closed[0] != 20 {
		t.Fatalf("the ordinary stale KB draft #20 must still close, got %v", f.closed)
	}
}

// TestOperatorNoteDetectionIsLabelExact is the mutation guard for the predicate
// itself, mirroring TestEntryEditProposalDetectionIsLabelExact.
func TestOperatorNoteDetectionIsLabelExact(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"operator note", []string{"runlore", operatorNoteLabel}, true},
		{"ordinary KB draft", []string{"runlore"}, false},
		{"protected KB draft", []string{"runlore", "ready-to-merge"}, false},
		{"entry-edit proposal", []string{"runlore", retireLabel}, false},
		{"no labels", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOperatorNote(tc.labels); got != tc.want {
				t.Errorf("isOperatorNote(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}
