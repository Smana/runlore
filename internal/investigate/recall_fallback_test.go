// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// twoEntryCatalog seeds two structurally-agreeing entries for apps/worker. The
// "stale" entry repeats the query's symptom tokens so it outranks "fixed" on BM25
// and is deterministically the winner; both clear the (low) fire thresholds.
func twoEntryCatalog(t *testing.T) (*catalog.Catalog, string, string) {
	t.Helper()
	dir := t.TempDir()
	stale := `---
type: Incident
title: worker OOMKilled memory limit drop OOMKilled memory
description: apps/worker pods OOMKilled after memory limit drop
resource: apps/worker
---

# Symptom
apps/worker pods OOMKilled after a memory limit drop. OOMKilled memory limit drop.
`
	fixed := `---
type: Incident
title: worker OOMKilled corrected runbook
description: raise the worker memory request and limit together
resource: apps/worker
---

# Symptom
apps/worker OOMKilled.
`
	for name, body := range map[string]string{"stale.md": stale, "fixed.md": fixed} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	var stalePath, fixedPath string
	for _, e := range cat.Entries() {
		if e.Title == "worker OOMKilled corrected runbook" {
			fixedPath = e.Path
		} else {
			stalePath = e.Path
		}
	}
	return cat, stalePath, fixedPath
}

func fallbackReq() Request {
	return Request{
		Title:    "WorkerOOM",
		Message:  "apps/worker pods OOMKilled after memory limit drop",
		Workload: providers.Workload{Namespace: "apps", Name: "worker"},
	}
}

func TestRunnerUpFiresWhenWinnerDecayed(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	r := &Recall{
		Catalog: cat, MinScore: 0.001, SoloFloor: 0.001, MarginGap: 0.001,
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0}, // factor 1/6 < 0.5 → rejected
			fixedPath: {Recalls: 3, Resolved: 3}, // healthy
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	// Premise: the stale entry IS the winner (rejected first).
	e, conf, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e == nil {
		t.Fatalf("runner-up must fire when the winner is outcome-rejected; got nil (rejected=%v)", rejected)
	}
	if e.Path != fixedPath {
		t.Fatalf("fired %q, want the healthy runner-up %q", e.Path, fixedPath)
	}
	if conf <= 0 || conf > 0.90 {
		t.Fatalf("runner-up confidence %v out of (0, 0.90]", conf)
	}
	if len(rejected) != 1 || rejected[0] != stalePath {
		t.Fatalf("rejected paths = %v, want exactly the decayed winner %q", rejected, stalePath)
	}
}

func TestRunnerUpMustClearConservativeSoloBar(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	// Solo floor set between the two BM25 scores: the winner clears it, the
	// runner-up does not → the fallback must NOT fire the runner-up.
	winnerScore, runnerScore := twoScores(t, cat, stalePath, fixedPath)
	r := &Recall{
		Catalog: cat, MinScore: 0.001, MarginGap: 0.001,
		SoloFloor: (winnerScore + runnerScore) / 2,
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0},
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	e, _, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e != nil {
		t.Fatalf("runner-up below the solo bar must not fire, got %q", e.Path)
	}
	if len(rejected) != 1 || rejected[0] != stalePath {
		t.Fatalf("rejected = %v, want [%q]", rejected, stalePath)
	}
}

func TestAllCandidatesDecayedReturnsAllRejected(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	r := &Recall{
		Catalog: cat, MinScore: 0.001, SoloFloor: 0.001, MarginGap: 0.001,
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0},
			fixedPath: {Recalls: 4, Resolved: 0},
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	e, _, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e != nil {
		t.Fatalf("no candidate is healthy; recall must fall through, got %q", e.Path)
	}
	if len(rejected) != 2 {
		t.Fatalf("both decayed candidates must be reported, got %v", rejected)
	}
}

// scriptedRerankModel returns each queued rerank_match verdict in order and
// counts calls — the fallback contract is "at most ONE extra rank call".
type scriptedRerankModel struct {
	calls    int
	verdicts []string // raw rerank_match JSON args, consumed in order
}

func (m *scriptedRerankModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	i := m.calls
	m.calls++
	if i >= len(m.verdicts) {
		return providers.CompletionResponse{}, nil // no tool call ⇒ no match
	}
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{Name: rerankToolName, Args: m.verdicts[i]}}}, nil
}

func TestRerankFallbackReranksOnceWithoutRejected(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	model := &scriptedRerankModel{verdicts: []string{
		`{"match":true,"entry_id":"` + stalePath + `","confidence":0.9}`,
		`{"match":true,"entry_id":"` + fixedPath + `","confidence":0.85}`,
	}}
	r := &Recall{
		Catalog: cat,
		Rerank:  &Reranker{Model: model, Threshold: 0.7, K: 5, MinScore: 0.001},
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0},
			fixedPath: {Recalls: 3, Resolved: 3},
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	e, conf, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e == nil || e.Path != fixedPath {
		t.Fatalf("re-rank fallback must fire the healthy candidate %q, got %+v (rejected=%v)", fixedPath, e, rejected)
	}
	if model.calls != 2 {
		t.Fatalf("exactly one extra rank call allowed, model saw %d calls", model.calls)
	}
	if conf <= 0 || conf > 0.90 {
		t.Fatalf("confidence %v out of (0, 0.90]", conf)
	}
	if len(rejected) != 1 || rejected[0] != stalePath {
		t.Fatalf("rejected = %v, want [%q]", rejected, stalePath)
	}
}

func TestRerankFallbackStopsAfterSecondRejection(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	model := &scriptedRerankModel{verdicts: []string{
		`{"match":true,"entry_id":"` + stalePath + `","confidence":0.9}`,
		`{"match":true,"entry_id":"` + fixedPath + `","confidence":0.85}`,
	}}
	r := &Recall{
		Catalog: cat,
		Rerank:  &Reranker{Model: model, Threshold: 0.7, K: 5, MinScore: 0.001},
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0},
			fixedPath: {Recalls: 4, Resolved: 0}, // second match also decayed
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	e, _, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e != nil {
		t.Fatalf("second outcome rejection must end the fallback (no third call), got %q", e.Path)
	}
	if model.calls != 2 {
		t.Fatalf("bounded at 2 rank calls total, model saw %d", model.calls)
	}
	if len(rejected) != 2 {
		t.Fatalf("both rejected paths must be reported, got %v", rejected)
	}
}

func TestNearMissExcludesOutcomeRejectedPaths(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	r := &Recall{Catalog: cat, MinScore: 0.001, SoloFloor: 0.001, MarginGap: 0.001}

	// Excluding the top candidate surfaces the next agreeing one…
	if nm := r.nearMissExcluding(context.Background(), fallbackReq(), stalePath); nm == nil || nm.Path != fixedPath {
		t.Fatalf("near-miss with winner excluded = %+v, want %q", nm, fixedPath)
	}
	// …and excluding both surfaces nothing.
	if nm := r.nearMissExcluding(context.Background(), fallbackReq(), stalePath, fixedPath); nm != nil {
		t.Fatalf("near-miss with all candidates excluded must be nil, got %q", nm.Path)
	}
	// Zero exclusions (the nearMiss() path) still returns the top candidate.
	if nm := r.nearMissExcluding(context.Background(), fallbackReq()); nm == nil {
		t.Fatal("near-miss with no exclusions must return the top agreeing candidate")
	}
}

// twoScores returns the BM25 scores of the two seeded entries for the fixture
// query, so thresholds can be pinned between them without hardcoding corpus-
// dependent magnitudes.
func twoScores(t *testing.T, cat *catalog.Catalog, stalePath, fixedPath string) (winner, runner float64) {
	t.Helper()
	hits, err := cat.SearchScored(buildRecallQuery(fallbackReq()), recallCandidateK)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		switch h.Entry.Path {
		case stalePath:
			winner = h.Score
		case fixedPath:
			runner = h.Score
		}
	}
	if winner <= runner {
		t.Fatalf("fixture premise broken: stale (winner) score %v must exceed fixed (runner-up) %v", winner, runner)
	}
	return winner, runner
}
