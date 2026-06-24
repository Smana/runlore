package curate

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// gapForge records the issue titles it was asked to open. It embeds recordingForge
// (which supplies ListIssuesByLabel from its `issues` field) and overrides OpenIssue.
type gapForge struct {
	*recordingForge
	openedTitles []string
}

func (g *gapForge) OpenIssue(_ context.Context, inv providers.Investigation) (providers.Ref, error) {
	g.openedTitles = append(g.openedTitles, inv.Title)
	return providers.Ref{URL: "https://github.com/x/y/issues/1"}, nil
}

func unresolved(pattern string, n int) []outcome.Episode {
	eps := make([]outcome.Episode, n)
	for i := range eps {
		eps[i] = outcome.Episode{Resource: pattern, Resolved: false}
	}
	return eps
}

func TestRecurrenceOpensGapIssueAtThreshold(t *testing.T) {
	eps := append(unresolved("apps/web", 3), outcome.Episode{Resource: "apps/worker", Resolved: false}) // worker: only 1
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 1 || gf.openedTitles[0] != "knowledge-gap: apps/web" {
		t.Fatalf("want one gap issue for apps/web, got %v", gf.openedTitles)
	}
}

func TestRecurrenceBelowThresholdNoIssue(t *testing.T) {
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: unresolved("apps/web", 2)}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("below threshold must open nothing, got %v", gf.openedTitles)
	}
}

func TestRecurrenceIdempotentWhenIssueExists(t *testing.T) {
	gf := &gapForge{recordingForge: &recordingForge{
		issues: []providers.CuratedIssue{{Title: "knowledge-gap: apps/web"}}, // already open
	}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: unresolved("apps/web", 5)}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("an existing gap issue must prevent a duplicate, got %v", gf.openedTitles)
	}
}

func TestRecurrenceResolvedEpisodesDoNotCount(t *testing.T) {
	eps := []outcome.Episode{
		{Resource: "apps/web", Resolved: true}, {Resource: "apps/web", Resolved: true},
		{Resource: "apps/web", Resolved: false}, // only 1 unresolved
	}
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("resolved episodes must not count, got %v", gf.openedTitles)
	}
}

func TestRecurrencePatternFallsBackToTitle(t *testing.T) {
	eps := []outcome.Episode{
		{Title: "DNSFailure", Resolved: false}, {Title: "DNSFailure", Resolved: false}, {Title: "DNSFailure", Resolved: false},
	}
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 1 || gf.openedTitles[0] != "knowledge-gap: DNSFailure" {
		t.Fatalf("a resource-less episode should group by title, got %v", gf.openedTitles)
	}
}
