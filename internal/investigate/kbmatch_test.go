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
// the captured MatchedKnowledge. KBMatchScore is left 0, so the tracker falls back to
// the 4.0 default bar — the unchanged high-bar behaviour.
func runKBLoop(t *testing.T, cat catalog.Searcher) providers.Investigation {
	t.Helper()
	return runKBLoopWithMatchScore(t, cat, 0)
}

// runKBLoopWithMatchScore is runKBLoop with an explicit KBMatchScore, so a test can
// exercise the config-tracked visibility bar: a low configured floor (the live sub-1.0
// regime), the 4.0 default, or 0 (unconfigured ⇒ the tracker's 4.0 fallback).
func runKBLoopWithMatchScore(t *testing.T, cat catalog.Searcher, kbMatchScore float64) providers.Investigation {
	t.Helper()
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "s1", Name: "kb_search", Args: `{"query":"harbor probe"}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "f1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"probe timeout"}]}`}}},
	}}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:        model,
		Tools:        []Tool{KBSearchTool{Catalog: cat}},
		Log:          discardLog,
		KBMatchScore: kbMatchScore,
		OnComplete:   func(inv providers.Investigation) { got = inv },
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
		{Entry: catalog.Entry{Title: "Harbor probe runbook", Path: "runbooks/harbor.md"}, Score: kbClearMatchScoreDefault + 2},
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
	if mk.Score != kbClearMatchScoreDefault+2 {
		t.Fatalf("Score = %v, want %v (recorded so the bar is tunable from live data)", mk.Score, kbClearMatchScoreDefault+2)
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
		{Entry: catalog.Entry{Title: "weak", Path: "weak.md"}, Score: kbClearMatchScoreDefault - 0.5},
	}}
	got := runKBLoop(t, cat)
	if got.MatchedKnowledge != nil {
		t.Fatalf("below-bar hit must NOT be captured, got %+v", got.MatchedKnowledge)
	}
}

// TestMatchedKnowledgeTracksLowConfiguredFloor is the live regression: on a real
// Alertmanager-driven cluster whose label-derived alert queries score sub-1.0, the
// operator tunes solo_floor DOWN (observed live: 0.2). A kb_search hit at 0.3 is a
// genuine known-runbook match there and MUST be surfaced. With the old hardcoded 4.0 bar
// it never could be — the visibility feature silently no-opped on exactly the clusters
// that need it (a Harbor investigation matched its runbook yet showed no prior-knowledge
// block). Wiring the tracker's bar to the configured 0.2 floor fixes it.
func TestMatchedKnowledgeTracksLowConfiguredFloor(t *testing.T) {
	cat := fakeScoredCatalog{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Harbor probe runbook", Path: "runbooks/harbor.md"}, Score: 0.3},
	}}
	got := runKBLoopWithMatchScore(t, cat, 0.2) // configured recall SoloFloor on this cluster
	mk := got.MatchedKnowledge
	if mk == nil {
		t.Fatalf("expected a sub-1.0 kb_search hit stamped under a 0.2 configured floor, got nil")
	}
	if mk.Path != "runbooks/harbor.md" || mk.Score != 0.3 {
		t.Fatalf("captured wrong entry: %+v", mk)
	}
}

// TestMatchedKnowledgeDefaultFloorRejectsSub1: with the default 4.0 bar (a cluster
// running instant recall at its default solo_floor), a 0.3 hit is far too weak to claim
// prior knowledge — the high-bar behaviour is unchanged. This is the guard that the fix
// LOWERS the bar only where the operator lowered solo_floor, never globally.
func TestMatchedKnowledgeDefaultFloorRejectsSub1(t *testing.T) {
	cat := fakeScoredCatalog{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "weak", Path: "weak.md"}, Score: 0.3},
	}}
	got := runKBLoopWithMatchScore(t, cat, kbClearMatchScoreDefault) // default 4.0 floor
	if got.MatchedKnowledge != nil {
		t.Fatalf("a 0.3 hit under the 4.0 default bar must NOT be captured, got %+v", got.MatchedKnowledge)
	}
}

// TestMatchedKnowledgeUnconfiguredFallsBackToDefault: instant recall disabled/unconfigured
// ⇒ no SoloFloor to borrow ⇒ KBMatchScore 0 ⇒ the tracker falls back to the historical 4.0
// bar. A 0.3 hit is not surfaced; a hit above 4.0 still is — behaviour is unchanged when
// nothing is configured.
func TestMatchedKnowledgeUnconfiguredFallsBackToDefault(t *testing.T) {
	weak := fakeScoredCatalog{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "weak", Path: "weak.md"}, Score: 0.3},
	}}
	if got := runKBLoopWithMatchScore(t, weak, 0); got.MatchedKnowledge != nil {
		t.Fatalf("unconfigured (0) must fall back to 4.0; a 0.3 hit must not be captured, got %+v", got.MatchedKnowledge)
	}
	strong := fakeScoredCatalog{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Harbor probe runbook", Path: "runbooks/harbor.md"}, Score: kbClearMatchScoreDefault + 1},
	}}
	if got := runKBLoopWithMatchScore(t, strong, 0); got.MatchedKnowledge == nil {
		t.Fatalf("unconfigured (0) fallback should still stamp an above-4.0 hit, got nil")
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
	stateful := &decliningCatalog{scores: []float64{kbClearMatchScoreDefault + 3, kbClearMatchScoreDefault + 1}}
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
	if got.MatchedKnowledge.Score != kbClearMatchScoreDefault+3 {
		t.Fatalf("tracker kept Score %v, want the strongest %v", got.MatchedKnowledge.Score, kbClearMatchScoreDefault+3)
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
