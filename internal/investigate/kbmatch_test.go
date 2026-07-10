// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// fakeScoredCatalog implements BOTH catalog.Searcher and catalog.ScoredSearcher so
// it can back a KBSearchTool (whose field is a Searcher) while still exposing the
// scores the loop captures into Investigation.MatchedKnowledge. SearchScored returns
// the hits in the given order (descending score, as bleve does), so hits[0] is the top.
type fakeScoredCatalog struct{ hits []catalog.ScoredEntry }

func (f fakeScoredCatalog) SearchScored(string, int) ([]catalog.ScoredEntry, error) {
	return f.hits, nil
}

func (f fakeScoredCatalog) Search(_ string, _ int) ([]catalog.Entry, error) {
	out := make([]catalog.Entry, 0, len(f.hits))
	for _, h := range f.hits {
		out = append(out, h.Entry)
	}
	return out, nil
}

// runKBLoop drives a two-turn investigation: turn 1 calls kb_search, turn 2 concludes
// with submit_findings. It returns the delivered investigation so a test can assert on
// the captured MatchedKnowledge.
func runKBLoop(t *testing.T, cat catalog.Searcher) providers.Investigation {
	t.Helper()
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "s1", Name: "kb_search", Args: `{"query":"harbor probe"}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "f1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"probe timeout"}]}`}}},
	}}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{KBSearchTool{Catalog: cat}},
		Log:        discardLog,
		OnComplete: func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "HarborProbeFailure"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	return got
}

// TestMatchedKnowledgeCapturedAboveBar is the core visibility case: a full
// investigation whose kb_search hit clears the clear-match bar lands the strongest
// pre-existing entry on Investigation.MatchedKnowledge, so the notification can show
// RunLore already had a runbook for the incident.
func TestMatchedKnowledgeCapturedAboveBar(t *testing.T) {
	cat := fakeScoredCatalog{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Harbor probe runbook", Path: "runbooks/harbor.md"}, Score: kbClearMatchScore + 2},
		{Entry: catalog.Entry{Title: "runner-up", Path: "x.md"}, Score: 1.0},
	}}
	got := runKBLoop(t, cat)
	mk := got.MatchedKnowledge
	if mk == nil {
		t.Fatalf("expected MatchedKnowledge stamped for an above-bar kb_search hit, got nil")
	}
	if mk.Path != "runbooks/harbor.md" || mk.Title != "Harbor probe runbook" {
		t.Fatalf("captured wrong entry: %+v", mk)
	}
	if mk.Score != kbClearMatchScore+2 {
		t.Fatalf("Score = %v, want %v (recorded so the bar is tunable from live data)", mk.Score, kbClearMatchScore+2)
	}
	// URL is not derivable from the tool without new forge plumbing; the notifier
	// shows Path instead.
	if mk.URL != "" {
		t.Fatalf("URL should be empty (no cheap derivation), got %q", mk.URL)
	}
}

// TestMatchedKnowledgeBelowBarNotCaptured: a hit under the clear-match bar is not a
// confident known-runbook match, so nothing is surfaced (surfacing a weak,
// tangential hit would be noise that erodes trust in the signal).
func TestMatchedKnowledgeBelowBarNotCaptured(t *testing.T) {
	cat := fakeScoredCatalog{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "weak", Path: "weak.md"}, Score: kbClearMatchScore - 0.5},
	}}
	got := runKBLoop(t, cat)
	if got.MatchedKnowledge != nil {
		t.Fatalf("below-bar hit must NOT be captured, got %+v", got.MatchedKnowledge)
	}
}

// TestMatchedKnowledgeKeepsStrongestAcrossCalls: across an investigation's kb_search
// calls the tracker keeps the single strongest clear-match hit, not the last one.
func TestMatchedKnowledgeKeepsStrongestAcrossCalls(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "s1", Name: "kb_search", Args: `{"query":"first"}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "s2", Name: "kb_search", Args: `{"query":"second"}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "f1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"x"}]}`}}},
	}}
	// A stateful fake that scores its hit lower on the second call, so "strongest wins"
	// is distinguishable from "most recent wins": the tracker must keep the first,
	// higher score.
	stateful := &decliningCatalog{scores: []float64{kbClearMatchScore + 3, kbClearMatchScore + 1}}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{KBSearchTool{Catalog: stateful}},
		Log:        discardLog,
		OnComplete: func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "t"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got.MatchedKnowledge == nil {
		t.Fatalf("expected a captured hit")
	}
	if got.MatchedKnowledge.Score != kbClearMatchScore+3 {
		t.Fatalf("tracker kept Score %v, want the strongest %v", got.MatchedKnowledge.Score, kbClearMatchScore+3)
	}
}

// decliningCatalog scores its single hit lower on each successive call, proving the
// tracker keeps the strongest across calls (not the most recent).
type decliningCatalog struct {
	scores []float64
	i      int
}

func (c *decliningCatalog) SearchScored(string, int) ([]catalog.ScoredEntry, error) {
	s := c.scores[c.i%len(c.scores)]
	c.i++
	return []catalog.ScoredEntry{{Entry: catalog.Entry{Title: "e", Path: "e.md"}, Score: s}}, nil
}

func (c *decliningCatalog) Search(string, int) ([]catalog.Entry, error) {
	return []catalog.Entry{{Title: "e", Path: "e.md"}}, nil
}

// TestStampMatchedKnowledgeSelfReferenceGuard covers the capture-side guard directly:
// the strongest hit is never stamped when it IS the entry the answer was recalled from
// (self-reference), a distinct entry IS stamped, and a nil best is a no-op.
func TestStampMatchedKnowledgeSelfReferenceGuard(t *testing.T) {
	// Self-reference: the matched entry equals the delivered recalled entry → skip.
	inv := providers.Investigation{RecalledEntry: "runbooks/harbor.md"}
	stampMatchedKnowledge(&inv, &providers.MatchedEntry{Path: "runbooks/harbor.md", Title: "self", Score: 9})
	if inv.MatchedKnowledge != nil {
		t.Fatalf("must not stamp the entry being delivered (self-reference): %+v", inv.MatchedKnowledge)
	}
	// A distinct pre-existing entry IS surfaced.
	stampMatchedKnowledge(&inv, &providers.MatchedEntry{Path: "runbooks/other.md", Title: "other", Score: 9})
	if inv.MatchedKnowledge == nil || inv.MatchedKnowledge.Path != "runbooks/other.md" {
		t.Fatalf("distinct entry must be stamped: %+v", inv.MatchedKnowledge)
	}
	// nil best is a no-op.
	var blank providers.Investigation
	stampMatchedKnowledge(&blank, nil)
	if blank.MatchedKnowledge != nil {
		t.Fatalf("nil best must be a no-op, got %+v", blank.MatchedKnowledge)
	}
}

// TestRecallShortCircuitLeavesMatchedKnowledgeNil confirms the recall path is left
// alone: a delivered instant-recall answer carries no MatchedKnowledge (the loop never
// ran, so kb_search never fired — and Prior/"Seen before" is the recall surface).
func TestRecallShortCircuitLeavesMatchedKnowledgeNil(t *testing.T) {
	model := &scriptModel{} // a model call would panic — proves the loop is skipped
	var got providers.Investigation
	li := &LoopInvestigator{
		Model: model,
		Log:   discardLog,
		Recall: &Recall{MinScore: 2.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "Known", Description: "d", Path: "known.md", Resource: "tooling/harbor"}, Score: 5.0}}}},
		OnComplete: func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "HarborProbeFailure", Workload: providers.Workload{Namespace: "tooling", Name: "harbor"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if !got.Recalled {
		t.Fatalf("expected a recalled delivery")
	}
	if got.MatchedKnowledge != nil {
		t.Fatalf("recall path must leave MatchedKnowledge nil (Prior covers it): %+v", got.MatchedKnowledge)
	}
}
