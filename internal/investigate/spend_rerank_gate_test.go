// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// rerankGateInvestigator builds a loop whose recall fire gate is the reranker, under a
// given cumulative token ceiling. The loop model is a runaway (never concludes), so
// whatever the reranker does, the run ends on the spend ladder rather than by luck.
func rerankGateInvestigator(rr providers.ModelProvider, ceiling int, hyb catalog.HybridSearcher) (*LoopInvestigator, *reportingRunawayModel) {
	model := &reportingRunawayModel{inputTokens: 7_000}
	rec := rerankRecall(rr, []catalog.ScoredEntry{webHit("web.md", 0.6)})
	if hyb != nil {
		rec.Hybrid = hyb
		rec.HybridMinScore, rec.HybridMarginGap = 0.1, 0.1
	}
	return &LoopInvestigator{
		Model:                     model,
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		MaxTokensPerInvestigation: ceiling,
		Recall:                    rec,
		OnComplete:                func(providers.Investigation) {},
	}, model
}

// TestRerankerIsNotCalledOnceTheCeilingIsCrossed is the ordering fix: the reranker is
// a paid completion placed in FRONT of the "free" short-circuit, and the ceiling was
// consulted only after it had already been spent — its usage was accumulated on the
// way back out of rank. So an investigation whose budget could not fund the call made
// it anyway, every time.
//
// A ceiling of 100 tokens cannot fund a ~1k rerank request under any reading, so the
// call must not happen. The run still proceeds exactly as a no-match does — fall
// through to a full investigation — where the EXISTING ladder stops it. That is the
// point: this guard declines to spend, it does not invent a second way to stop a run.
func TestRerankerIsNotCalledOnceTheCeilingIsCrossed(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web.md","confidence":0.9}`)}
	li, _ := rerankGateInvestigator(fake, 100, nil)
	if err := li.Investigate(context.Background(), okReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("the reranker was called %d times against a 100-token ceiling it cannot fit in. "+
			"The check has to run BEFORE the completion — accumulating its usage afterwards means "+
			"the ceiling is consulted for the first time with the money already gone.", fake.calls)
	}
}

// TestRerankerRunsNormallyUnderAnAffordableCeiling is the control that keeps the guard
// from being "always refuse". A guard that never lets the reranker run would pass the
// test above and silently disable instant recall for every deployment.
func TestRerankerRunsNormallyUnderAnAffordableCeiling(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web.md","confidence":0.9}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{webHit("web.md", 0.6)})
	li := &LoopInvestigator{
		Model:                     &reportingRunawayModel{inputTokens: 7_000},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		MaxTokensPerInvestigation: 400_000, // the shipped default
		Recall:                    r,
		OnComplete:                func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), okReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("under the shipped 400000-token ceiling the reranker must run exactly once, got %d — "+
			"the pre-call guard must not be refusing ordinary traffic", fake.calls)
	}
}

// TestRerankerCeilingIsUncheckedWhenUnlimited pins that an operator who opted out with
// -1 (or a caller with no ceiling at all, e.g. the CLI's plain lookup) is not handed a
// bound they never asked for.
func TestRerankerCeilingIsUncheckedWhenUnlimited(t *testing.T) {
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web.md","confidence":0.9}`)}
	r := rerankRecall(fake, []catalog.ScoredEntry{webHit("web.md", 0.6)})
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("a bare lookup carries no spend channel and must behave exactly as before")
	}
	if fake.calls != 1 {
		t.Fatalf("lookup without a spend channel must still rank, got %d calls", fake.calls)
	}
}

// TestQueryEmbedSpendCanRefuseTheReranker is where the first and third gaps meet, and
// the reason the ordering fix is load-bearing rather than theoretical.
//
// The reranker is normally the FIRST paid call of an investigation, so before the
// query embeddings were folded into the totals there was nothing for a pre-call check
// to compare against — spend was always zero. Now the hybrid query embed happens
// inside the same lookup, BEFORE rank, and its tokens are in the running total. So a
// run whose retrieval has already exhausted the budget declines the rerank instead of
// spending past a ceiling it has demonstrably crossed.
func TestQueryEmbedSpendCanRefuseTheReranker(t *testing.T) {
	const (
		perQuery = 25_000
		ceiling  = 20_000 // retrieval alone has already overshot the whole run budget
	)
	hyb := newEmbeddingHybrid(t, perQuery, []catalog.ScoredEntry{webHit("web.md", 0.6)})
	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web.md","confidence":0.9}`)}
	li, _ := rerankGateInvestigator(fake, ceiling, hyb)
	if err := li.Investigate(context.Background(), okReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if n := hyb.calls.Load(); n == 0 {
		t.Fatal("the hybrid query never ran — the test no longer exercises the path it claims to")
	}
	if fake.calls != 0 {
		t.Fatalf("the reranker was called %d times after the query embed had already spent %d of a "+
			"%d-token ceiling. The embed is charged inside the same lookup, before rank, so the "+
			"pre-call check has real spend to compare against.", fake.calls, perQuery, ceiling)
	}
}

// usageRerankModel is scriptedRerankModel that also REPORTS usage, so a first rank
// call can move the running total far enough to price the second one out.
type usageRerankModel struct {
	calls    int
	verdicts []string
	usage    providers.Usage
}

func (m *usageRerankModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	i := m.calls
	m.calls++
	if i >= len(m.verdicts) {
		return providers.CompletionResponse{Usage: m.usage}, nil
	}
	return providers.CompletionResponse{
		ToolCalls: []providers.ToolCall{{Name: rerankToolName, Args: m.verdicts[i]}},
		Usage:     m.usage,
	}, nil
}

// TestOutcomeFallbackRerankIsAlsoGatedOnSpend covers the SECOND rank call.
//
// outcomeFallback exists because a decayed winner must not disable instant recall
// while a healthy corrected entry sits behind it — and it pays for another completion
// to find that entry. It is the one rank call with real prior spend to be measured
// against (the first call's tokens are already in the total by then), so leaving it
// ungated would mean the guard covers only the case where there was nothing to check.
//
// Ceiling 15 000; the first rank is affordable against an empty total and reports
// exactly 15 000 tokens, so the second is refused whatever its estimate turns out to
// be — no dependence on the prompt's current length.
func TestOutcomeFallbackRerankIsAlsoGatedOnSpend(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	model := &usageRerankModel{
		verdicts: []string{
			`{"match":true,"entry_id":"` + stalePath + `","confidence":0.9}`,
			`{"match":true,"entry_id":"` + fixedPath + `","confidence":0.85}`,
		},
		usage: providers.Usage{InputTokens: 14_990, OutputTokens: 10},
	}
	r := &Recall{
		Catalog: cat,
		Rerank:  &Reranker{Model: model, Threshold: 0.7, K: 5, MinScore: 0.001},
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0}, // decayed ⇒ the fallback runs
			fixedPath: {Recalls: 3, Resolved: 3}, // healthy ⇒ it WOULD fire, if affordable
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}

	li := &LoopInvestigator{MaxTokensPerInvestigation: 15_000}
	var verifyTotals providers.UsageTotals
	spend := &recallSpend{
		totals: &verifyTotals,
		afford: func(est int) string {
			return li.budgetTrip(est, li.aggregateUsage(providers.UsageTotals{}, verifyTotals, providers.UsageTotals{}))
		},
	}
	e, _, _ := r.lookupWithUsage(context.Background(), fallbackReq(), spend)

	if model.calls != 1 {
		t.Fatalf("the fallback rank call must be refused once the first has spent the whole "+
			"ceiling: the reranker was called %d times, want 1", model.calls)
	}
	if e != nil {
		t.Fatalf("a fallback that cannot be paid for must fall through, not fire: got %q", e.Path)
	}
	if verifyTotals.InputTokens != 14_990 {
		t.Fatalf("the first rank call's tokens must be in the total the second is measured "+
			"against: got %d, want 14990", verifyTotals.InputTokens)
	}
}

// TestRerankOverBudgetIsVisibleAsARecallRejection pins the instrumentation choice.
//
// A declined rerank is recorded on recall_rejections_total, beside the reranker's
// existing rerank_no_signal / rerank_low_confidence reasons — NOT as a budget trip.
// The run is not stopped here: it falls through to the loop, whose first enforceBudget
// step fires the real ladder with the reason that names the knob to raise. Counting a
// rung here as well would report one stop twice on
// runlore_investigation_budget_trips_total and inflate the nudge rate.
func TestRerankOverBudgetIsVisibleAsARecallRejection(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	fake := &countingReranker{resp: rerankResp(`{"match":true,"entry_id":"web.md","confidence":0.9}`)}
	li, _ := rerankGateInvestigator(fake, 100, nil)
	li.Metrics = telemetry.NewMetrics()
	li.Recall.Metrics = li.Metrics
	if err := li.Investigate(context.Background(), okReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	body := scrapeMetrics(t, h)
	if !strings.Contains(body, `reason="rerank_over_budget"`) {
		t.Fatalf("a rerank declined on spend must be visible on recall_rejections_total, so an "+
			"operator can tell it apart from a reranker that ran and found nothing:\n%s", body)
	}
	// The ladder still owns stopping the run, and must report exactly one nudge rung.
	if strings.Contains(body, `stage="declined"`) {
		t.Fatalf("the pre-call guard must not invent a third rung on the budget ladder:\n%s", body)
	}
	if !strings.Contains(body, `runlore_investigation_budget_trips_total`) {
		t.Fatalf("declining the rerank must not stop the run: the loop's own ladder is what ends "+
			"it, and it has to be the thing that records the trip:\n%s", body)
	}
}
