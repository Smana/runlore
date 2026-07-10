// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// countingReranker is a fake reranker ModelProvider: it records how many times it was
// asked to rank (so the COST-GUARD tests can prove it was NOT called) and returns a
// fixed, scripted rerank_match verdict.
type countingReranker struct {
	calls int
	resp  providers.CompletionResponse
}

func (m *countingReranker) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	return m.resp, nil
}

// errReranker always errors — the fail-safe path (a reranker outage must fall through
// to a full investigation, never a wrong recall).
type errReranker struct{ calls int }

func (m *errReranker) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	return providers.CompletionResponse{}, errors.New("reranker unavailable")
}

// verdict builds a scripted rerank_match tool response.
func rerankResp(args string) providers.CompletionResponse {
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{ID: "rr", Name: rerankToolName, Args: args}}}
}

// rerankRecall builds a Recall whose fire gate is the reranker (Rerank set). The BM25
// thresholds are the PRODUCTION defaults (SoloFloor 4.0) precisely so the tests prove
// the reranker fires where the magnitude gate cannot: the candidate scores below are
// sub-1.0, an order of magnitude under SoloFloor.
func rerankRecall(model providers.ModelProvider, hits []catalog.ScoredEntry) *Recall {
	return &Recall{
		Catalog:  fakeScored{hits: hits},
		MinScore: 1.0, MarginGap: 1.0, SoloFloor: 4.0,
		Rerank: &Reranker{Model: model, Threshold: 0.7, K: 5, MinScore: 0.1},
	}
}

// webHit is a structurally-agreeing candidate for okReq() (apps/web) at a realistic,
// enriched sub-1.0 BM25 score.
func webHit(path string, score float64) catalog.ScoredEntry {
	return catalog.ScoredEntry{Entry: catalog.Entry{Title: "Web OOM", Path: path, Resource: "apps/web", Description: "d"}, Score: score}
}

// TestRerankFiresBelowSoloFloor is the whole point: a calibrated match at a BM25 score
// (0.6) an order of magnitude BELOW SoloFloor (4.0) — which the magnitude gate rejects
// today — now SHORT-CIRCUITS because the reranker's confidence clears the (corpus-
// independent) threshold. The delivered confidence is the calibrated match confidence,
// capped below 1.0.
func TestRerankFiresBelowSoloFloor(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web.md","confidence":0.85}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{webHit("web.md", 0.6)})
	e, conf := r.lookup(context.Background(), okReq())
	if e == nil || e.Path != "web.md" {
		t.Fatalf("a calibrated match must fire even at score 0.6 << SoloFloor 4.0, got %+v", e)
	}
	if fake.calls != 1 {
		t.Fatalf("expected exactly one rerank call, got %d", fake.calls)
	}
	if conf != 0.85 {
		t.Fatalf("recall confidence must be the calibrated match confidence (capped 0.90), got %v", conf)
	}
}

// TestRerankConfidenceCappedBelowOne proves a reranker that over-asserts (confidence
// 1.0) is still capped — a cache hit never claims certainty.
func TestRerankConfidenceCappedBelowOne(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web.md","confidence":1.0}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{webHit("web.md", 0.6)})
	e, conf := r.lookup(context.Background(), okReq())
	if e == nil || conf > 0.90 {
		t.Fatalf("recall confidence must be capped at 0.90, got %v (entry %+v)", conf, e)
	}
}

// TestRerankBelowThresholdDoesNotFire: a plausible-but-not-confident match (0.6 < 0.7
// threshold) must NOT short-circuit — it falls through to a full investigation.
func TestRerankBelowThresholdDoesNotFire(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web.md","confidence":0.6}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{webHit("web.md", 0.6)})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("a match below the confidence threshold must not fire")
	}
	if fake.calls != 1 {
		t.Fatalf("the reranker must be consulted once (candidate was plausible), got %d", fake.calls)
	}
}

// TestRerankNoMatchDoesNotFire is the NEGATIVE-case guard: the reranker says none of
// the candidates is correct → no recall, however strong the BM25 signal.
func TestRerankNoMatchDoesNotFire(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":false}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{webHit("web.md", 0.9)})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("match=false must not fire a recall (a hallucinated match is worse than none)")
	}
}

// TestRerankRejectsUnknownEntryID is the HALLUCINATION guard: the model returns high
// confidence for an id it was never offered — that must NEVER fire a recall.
func TestRerankRejectsUnknownEntryID(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"ghost.md","confidence":0.99}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{webHit("web.md", 0.9)})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatalf("an entry_id outside the candidate set must not fire (hallucination guard), got %+v", e)
	}
}

// TestRerankModelErrorFallsThrough: a reranker outage must fail safe — no recall, no
// panic, fall through to a full investigation.
func TestRerankModelErrorFallsThrough(t *testing.T) {
	fake := &errReranker{}
	r := rerankRecall(fake, []catalog.ScoredEntry{webHit("web.md", 0.9)})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("a reranker error must fall through to a full investigation, not fire")
	}
	if fake.calls != 1 {
		t.Fatalf("expected one attempted call, got %d", fake.calls)
	}
}

// TestRerankNotCalledWithoutAgreeingCandidate is the COST GUARD: when no candidate
// structurally agrees with the workload, the reranker (a paid LLM call) is never made.
func TestRerankNotCalledWithoutAgreeingCandidate(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"a.md","confidence":0.99}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{
		{Entry: catalog.Entry{Path: "a.md", Resource: "other/web"}, Score: 9.0}, // wrong namespace ⇒ no structural agreement
	})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("no structurally-agreeing candidate must not fire")
	}
	if fake.calls != 0 {
		t.Fatalf("cost guard: the reranker must NOT be called when nothing structurally agrees, calls=%d", fake.calls)
	}
}

// TestRerankNotCalledBelowScoreFloor is the second COST GUARD: an agreeing candidate
// whose retrieval score is below the trivial floor means retrieval found nothing
// plausible — don't pay for a rerank.
func TestRerankNotCalledBelowScoreFloor(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web.md","confidence":0.99}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{webHit("web.md", 0.05)}) // below Rerank.MinScore 0.1
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("a below-floor candidate must not fire")
	}
	if fake.calls != 0 {
		t.Fatalf("cost guard: the reranker must NOT be called below the score floor, calls=%d", fake.calls)
	}
}

// TestRerankPicksAmongTopK verifies the reranker is handed the top-K structurally-
// agreeing candidates (bounded) and may pick any of them — here the 2nd-ranked one.
func TestRerankPicksAmongTopK(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web2.md","confidence":0.8}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{
		webHit("web1.md", 0.9), // higher lexical
		webHit("web2.md", 0.4), // the reranker judges THIS one correct for the incident
	})
	e, _ := r.lookup(context.Background(), okReq())
	if e == nil || e.Path != "web2.md" {
		t.Fatalf("the reranker must be able to pick a lower-lexical candidate, got %+v", e)
	}
}

// TestRerankOffIsUnchanged pins the "rerank OFF is byte-for-byte the old behavior"
// contract: with Rerank nil the BM25-magnitude gate still governs, so the SAME
// sub-SoloFloor candidate that the reranker fired on above is REJECTED here, and no
// model is ever consulted.
func TestRerankOffIsUnchanged(t *testing.T) {
	// Identical Recall config to rerankRecall but with Rerank nil.
	r := &Recall{
		Catalog:  fakeScored{hits: []catalog.ScoredEntry{webHit("web.md", 0.6)}},
		MinScore: 1.0, MarginGap: 1.0, SoloFloor: 4.0,
	}
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("rerank OFF: a lone hit below SoloFloor must fall through, exactly as before")
	}
	// And a strong hit still fires via the magnitude gate — the classic behavior.
	r.Catalog = fakeScored{hits: []catalog.ScoredEntry{webHit("web.md", 6.0)}}
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("rerank OFF: a strong lone hit above SoloFloor must still fire via the magnitude gate")
	}
}

// TestInstantRecallReranked is the end-to-end wiring test: through LoopInvestigator, a
// reranker-gated recall at a sub-SoloFloor BM25 score short-circuits the ReAct loop (the
// investigation model is NEVER called) and delivers the recalled entry. This is exactly
// the case the magnitude gate cannot serve at the default SoloFloor.
func TestInstantRecallReranked(t *testing.T) {
	invModel := &scriptModel{} // no responses scripted: any loop call would panic, proving the loop is skipped
	reranker := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"known.md","confidence":0.82}`)}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model: invModel,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall: &Recall{
			Catalog:  fakeScored{hits: []catalog.ScoredEntry{{Entry: catalog.Entry{Title: "Known incident", Description: "chart bump", Path: "known.md", Resource: "tooling/harbor"}, Score: 0.6}}},
			MinScore: 1.0, MarginGap: 1.0, SoloFloor: 4.0, // 0.6 is far below SoloFloor — only the reranker can fire this
			Rerank: &Reranker{Model: reranker, Threshold: 0.7, K: 5, MinScore: 0.1},
		},
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	req := Request{Title: "HarborProbeFailure", Workload: providers.Workload{Namespace: "tooling", Name: "harbor"}}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if invModel.i != 0 {
		t.Fatalf("investigation model called %d times; a reranked recall must skip the loop", invModel.i)
	}
	if reranker.calls != 1 {
		t.Fatalf("the reranker must be consulted exactly once, got %d", reranker.calls)
	}
	if got == nil || len(got.RootCauses) != 1 || !strings.Contains(got.RootCauses[0].Summary, "Known incident") {
		t.Fatalf("unexpected reranked recall investigation: %+v", got)
	}
	if !got.Recalled || got.RecalledEntry != "known.md" {
		t.Fatalf("must be flagged as a recall of known.md, got recalled=%v entry=%q", got.Recalled, got.RecalledEntry)
	}
}
