package curate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// recordingForge records the label added per PR number. Shared by the resolution
// and recurrence tests.
type recordingForge struct {
	prs     []providers.CuratedIssue
	relabel map[int]string // number -> added label
}

func (f *recordingForge) ListPRsByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.prs, nil
}
func (f *recordingForge) ListIssuesByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return nil, nil
}
func (f *recordingForge) Comment(context.Context, int, string) error { return nil }
func (f *recordingForge) ReplaceLabel(_ context.Context, n int, _, add string) error {
	if f.relabel == nil {
		f.relabel = map[int]string{}
	}
	f.relabel[n] = add
	return nil
}
func (f *recordingForge) Close(context.Context, int) error { return nil }
func (f *recordingForge) OpenIssue(context.Context, providers.Investigation) (providers.Ref, error) {
	return providers.Ref{}, nil
}

// fakeChecker reports a fixed resolution per PR number.
type fakeChecker struct{ resolved map[int]bool }

func (c fakeChecker) IsResolved(_ context.Context, pr providers.CuratedIssue) (bool, error) {
	return c.resolved[pr.Number], nil
}

func TestResolutionQueuesResolvedOnly(t *testing.T) {
	f := &recordingForge{prs: []providers.CuratedIssue{
		{Number: 48, Title: "KB: HarborRegistryDown", Labels: []string{"runlore", "solved"}},
		{Number: 50, Title: "KB: StillBroken", Labels: []string{"runlore", "solved"}},
	}}
	q := Queue{Forge: f, Checker: fakeChecker{resolved: map[int]bool{48: true, 50: false}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := q.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.relabel[48] != "ready-to-merge" {
		t.Fatalf("resolved PR 48 should be ready-to-merge, got %q", f.relabel[48])
	}
	if _, queued := f.relabel[50]; queued {
		t.Fatalf("unresolved PR 50 must NOT be auto-queued (waits for human accepted)")
	}
}

func TestResolutionRespectsHumanAccepted(t *testing.T) {
	f := &recordingForge{prs: []providers.CuratedIssue{
		{Number: 48, Title: "KB: AcceptedButUnresolved", Labels: []string{"runlore", "solved", "accepted"}},
	}}
	q := Queue{Forge: f, Checker: fakeChecker{resolved: map[int]bool{48: false}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := q.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.relabel[48] != "ready-to-merge" {
		t.Fatalf("human-accepted PR should be ready-to-merge even if unresolved, got %q", f.relabel[48])
	}
}
