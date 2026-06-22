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
