package curate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// recordingForge records the label added per PR number. Shared by the resolution
// and recurrence tests.
type recordingForge struct {
	prs     []providers.CuratedIssue
	issues  []providers.CuratedIssue // returned by ListIssuesByLabel
	relabel map[int]string           // number -> added label
}

func (f *recordingForge) ListPRsByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.prs, nil
}
func (f *recordingForge) ListIssuesByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.issues, nil
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

// fakeLedger is a fixed Episodes() source for resolution/recurrence tests.
type fakeLedger struct{ eps []outcome.Episode }

func (f fakeLedger) Episodes() ([]outcome.Episode, error) { return f.eps, nil }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

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

func TestLedgerResolutionCheckerResolvedTitle(t *testing.T) {
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "HarborRegistryDown", Resolved: true},
		{Title: "Other", Resolved: false},
	}}}
	got, err := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: HarborRegistryDown"})
	if err != nil || !got {
		t.Fatalf("a resolved episode with the PR's title must be resolved=true; got %v err=%v", got, err)
	}
}

func TestLedgerResolutionCheckerUnresolved(t *testing.T) {
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "HarborRegistryDown", Resolved: false}, // opened, never resolved
	}}}
	got, _ := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: HarborRegistryDown"})
	if got {
		t.Fatal("an unresolved episode must yield resolved=false")
	}
	// absent entirely → false
	if got, _ := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: Unknown"}); got {
		t.Fatal("a title with no episode must yield resolved=false")
	}
}

func TestLedgerResolutionCheckerEmptyTitle(t *testing.T) {
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{{Title: "", Resolved: true}}}}
	if got, _ := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: "}); got {
		t.Fatal("an empty title must never match (avoids resolving on a blank episode)")
	}
}

func TestQueuePromotesResolvedViaLedger(t *testing.T) {
	f := &recordingForge{prs: []providers.CuratedIssue{
		{Number: 48, Title: "KB: HarborRegistryDown", Labels: []string{"runlore", "solved"}},
		{Number: 50, Title: "KB: StillBroken", Labels: []string{"runlore", "solved"}},
	}}
	led := fakeLedger{eps: []outcome.Episode{{Title: "HarborRegistryDown", Resolved: true}}}
	q := Queue{Forge: f, Checker: LedgerResolutionChecker{Ledger: led}, Log: discardLog()}
	if err := q.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.relabel[48] != "ready-to-merge" {
		t.Fatalf("the resolved PR #48 should be queued ready-to-merge, got %v", f.relabel)
	}
	if _, ok := f.relabel[50]; ok {
		t.Fatalf("the unresolved PR #50 must not be queued, got %v", f.relabel)
	}
}
