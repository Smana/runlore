package curate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeForge records OpenIssue/Comment/Close and serves a fixed PR list. Shared by
// the dedup and lifecycle tests.
type fakeForge struct {
	prs       []providers.CuratedIssue
	commented []int
	closed    []int
}

func (f *fakeForge) ListPRsByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.prs, nil
}
func (f *fakeForge) ListIssuesByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return nil, nil
}
func (f *fakeForge) Comment(_ context.Context, n int, _ string) error {
	f.commented = append(f.commented, n)
	return nil
}
func (f *fakeForge) ReplaceLabel(context.Context, int, string, string) error { return nil }
func (f *fakeForge) Close(_ context.Context, n int) error {
	f.closed = append(f.closed, n)
	return nil
}
func (f *fakeForge) OpenIssue(context.Context, providers.Investigation) (providers.Ref, error) {
	return providers.Ref{}, nil
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestDedupClosesDuplicatesKeepsCanonical(t *testing.T) {
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 12, Title: "KB: Kustomization DependencyNotReady missing GitRepository"},
		{Number: 20, Title: "KB: Kustomization crossplane DependencyNotReady missing GitRepository"},
		{Number: 27, Title: "KB: Kustomization DependencyNotReady due to missing GitRepository"},
		{Number: 99, Title: "KB: Totally unrelated harbor valkey outage"},
	}}
	d := Dedup{Forge: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 2 || !containsInt(f.closed, 20) || !containsInt(f.closed, 27) {
		t.Fatalf("want close [20 27], got %v", f.closed)
	}
	if containsInt(f.closed, 12) || containsInt(f.closed, 99) {
		t.Fatalf("must not close canonical 12 or unrelated 99: %v", f.closed)
	}
	if len(f.commented) != 2 {
		t.Fatalf("want back-ref comments on the 2 closed dups, got %v", f.commented)
	}
}

// TestDedupCollapsesByFingerprintAcrossRewordedTitles proves the fingerprint-first
// path: two PRs with DELIBERATELY DISJOINT titles (Jaccard ≈ 0) but the SAME
// persisted fingerprint marker are collapsed — the deterministic identity beats
// the fragile title comparison that DupFingerprint was built to retire.
func TestDedupCollapsesByFingerprintAcrossRewordedTitles(t *testing.T) {
	const fp = "abc123fingerprint"
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 5, Title: "KB: Kustomization DependencyNotReady missing GitRepository", Body: "drafted\n\n" + providers.FingerprintMarker(fp)},
		{Number: 9, Title: "KB: apps/web pod readiness probe failing after deploy", Body: "drafted\n\n" + providers.FingerprintMarker(fp)},
	}}
	d := Dedup{Forge: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Sanity: these titles would NOT collapse under title-Jaccard alone.
	if jaccard(titleTokens(f.prs[0].Title), titleTokens(f.prs[1].Title)) >= 0.6 {
		t.Fatal("test premise broken: titles are too similar to prove the fingerprint path")
	}
	if len(f.closed) != 1 || f.closed[0] != 9 {
		t.Fatalf("matching fingerprints must collapse #9 onto canonical #5, got closed=%v", f.closed)
	}
}

// TestDedupCollapsesDivergentFingerprintsWhenTitlesSimilar is the regression guard
// for the divergent-fingerprint hole: the SAME incident investigated once via an
// alert (trigger-key fingerprint) and once via a manual `lore investigate`
// (cause-token fingerprint) produces two DIFFERENT markers. Different fingerprints
// must NOT short-circuit dedup — the pass falls through to title-Jaccard, so the
// similar-titled true duplicate is caught and collapsed onto the canonical PR.
func TestDedupCollapsesDivergentFingerprintsWhenTitlesSimilar(t *testing.T) {
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 3, Title: "KB: Kustomization DependencyNotReady missing GitRepository", Body: "x\n\n" + providers.FingerprintMarker("fp-alert")},
		{Number: 7, Title: "KB: Kustomization DependencyNotReady due to missing GitRepository", Body: "x\n\n" + providers.FingerprintMarker("fp-manual")},
	}}
	d := Dedup{Forge: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 1 || f.closed[0] != 7 {
		t.Fatalf("divergent fingerprints with similar titles must collapse #7 onto #3 via Jaccard, got closed=%v", f.closed)
	}
}

// TestDedupKeepsDivergentFingerprintsWhenTitlesDissimilar proves the fall-through is
// still guarded by the title check: two genuinely distinct incidents (different
// fingerprints AND dissimilar titles) must stay open — Jaccard below threshold keeps
// them apart, so the divergent-fingerprint fall-through introduces no false close.
func TestDedupKeepsDivergentFingerprintsWhenTitlesDissimilar(t *testing.T) {
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 3, Title: "KB: Kustomization DependencyNotReady missing GitRepository", Body: "x\n\n" + providers.FingerprintMarker("fp-one")},
		{Number: 7, Title: "KB: apps/web pod readiness probe failing after deploy", Body: "x\n\n" + providers.FingerprintMarker("fp-two")},
	}}
	d := Dedup{Forge: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 0 {
		t.Fatalf("divergent fingerprints with dissimilar titles must NOT collapse, got closed=%v", f.closed)
	}
}

// TestDedupFallsBackToTitleWhenMarkerless proves legacy/hand-filed PRs (no marker)
// still dedup by title-Jaccard — the fingerprint path is additive, not a regression.
func TestDedupFallsBackToTitleWhenMarkerless(t *testing.T) {
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 4, Title: "KB: Kustomization DependencyNotReady missing GitRepository"},
		{Number: 8, Title: "KB: Kustomization DependencyNotReady due to missing GitRepository"},
	}}
	d := Dedup{Forge: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 1 || f.closed[0] != 8 {
		t.Fatalf("markerless near-identical titles must still collapse via Jaccard, got closed=%v", f.closed)
	}
}

func TestDedupSkipsProtectedDuplicates(t *testing.T) {
	// Three title-identical PRs; the middle one is human-`accepted`. Dedup must NOT
	// close the protected one — it closes only the unprotected duplicate (#12),
	// keeping the canonical (#10) and the human-touched (#11) open.
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 10, Title: "KB: Kustomization DependencyNotReady missing GitRepository"},
		{Number: 11, Title: "KB: Kustomization DependencyNotReady missing GitRepository", Labels: []string{"runlore", "accepted"}},
		{Number: 12, Title: "KB: Kustomization DependencyNotReady missing GitRepository"},
	}}
	d := Dedup{Forge: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if containsInt(f.closed, 11) {
		t.Fatalf("must NOT close the protected (accepted) #11, got %v", f.closed)
	}
	if len(f.closed) != 1 || f.closed[0] != 12 {
		t.Fatalf("should close only the unprotected dup #12, got %v", f.closed)
	}
}
