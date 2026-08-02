// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestAggregateMediansUsage: per-case token counts are the MEDIAN across repeats,
// matching how confidence is already aggregated. The median is what a published
// cost figure should quote — one expensive outlier must not set the headline number.
func TestAggregateMediansUsage(t *testing.T) {
	results := []Result{
		{Name: "c", Pass: true, Confidence: 0.8, Usage: providers.Usage{InputTokens: 100, OutputTokens: 10}},
		{Name: "c", Pass: true, Confidence: 0.8, Usage: providers.Usage{InputTokens: 300, OutputTokens: 30}},
		{Name: "c", Pass: true, Confidence: 0.8, Usage: providers.Usage{InputTokens: 200, OutputTokens: 20}},
	}
	agg := aggregateResults(Case{Name: "c"}, results)
	if agg.InputTokens != 200 {
		t.Errorf("median input tokens = %d, want 200", agg.InputTokens)
	}
	if agg.OutputTokens != 20 {
		t.Errorf("median output tokens = %d, want 20", agg.OutputTokens)
	}
}

// TestAggregateUsageZeroWhenUnreported: a provider that reports no usage must yield
// zero, never a fabricated estimate. Zero renders as "unknown" downstream.
func TestAggregateUsageZeroWhenUnreported(t *testing.T) {
	agg := aggregateResults(Case{Name: "c"}, []Result{{Name: "c", Pass: true}})
	if agg.InputTokens != 0 || agg.OutputTokens != 0 {
		t.Errorf("usage = %d/%d, want 0/0 when the provider reports none", agg.InputTokens, agg.OutputTokens)
	}
}

// TestUsageDeltaIsPerRun: the runner attributes each run only the tokens that run
// spent, by differencing the cumulative counter before and after.
func TestUsageDeltaIsPerRun(t *testing.T) {
	before := providers.Usage{InputTokens: 1000, OutputTokens: 100}
	after := providers.Usage{InputTokens: 1350, OutputTokens: 140}
	got := usageDelta(before, after)
	if got.InputTokens != 350 || got.OutputTokens != 40 {
		t.Errorf("usageDelta = %+v, want 350 in / 40 out", got)
	}
}

// TestReportCarriesPerCaseTokens: the tokens must survive projection into the report,
// because that JSON is what the published scorecard renders from.
func TestReportCarriesPerCaseTokens(t *testing.T) {
	camp := Campaign{N: 1, Aggregates: []CaseAggregate{
		{Name: "c", Runs: 1, Reached: true, InputTokens: 4200, OutputTokens: 380},
	}}
	rep := camp.Report("2026-08-02T00:00:00Z", "anthropic/claude-haiku-4-5", providers.Usage{}, nil)
	if len(rep.Cases) != 1 {
		t.Fatalf("cases = %d, want 1", len(rep.Cases))
	}
	if rep.Cases[0].InputTokens != 4200 || rep.Cases[0].OutputTokens != 380 {
		t.Errorf("report case tokens = %d/%d, want 4200/380",
			rep.Cases[0].InputTokens, rep.Cases[0].OutputTokens)
	}
}

// countingFakeModel replays the same two-step Complete sequence as rateModel
// (eval_test.go: tool call, then submit_findings — one replay run makes exactly
// 2 Complete calls), but each response also carries a fixed token Usage. Every
// run therefore spends the identical 250 input / 50 output tokens, which is
// what makes cumulative-vs-per-run attribution distinguishable below.
type countingFakeModel struct {
	calls int
}

func (m *countingFakeModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	if m.calls%2 == 1 { // odd call: tool step
		return providers.CompletionResponse{
			ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: `{}`}},
			Usage:     providers.Usage{InputTokens: 100, OutputTokens: 20},
		}, nil
	}
	// even call: submit_findings step, always names harbor-db so the case passes
	return providers.CompletionResponse{
		ToolCalls: []providers.ToolCall{{ID: "2", Name: "submit_findings",
			Args: `{"confidence":0.9,"root_causes":[{"summary":"chart bump broke harbor-db migrations"}]}`}},
		Usage: providers.Usage{InputTokens: 150, OutputTokens: 30},
	}, nil
}

// TestUsageAttributedPerRunNotCumulative drives Runner.RunN end to end with
// Runner.Model set to the REAL CountingModel (compare_run.go) — the exact
// wrapper internal/app/eval.go uses in production — around countingFakeModel.
// CountingModel accumulates a running total across Complete calls just like a
// real provider meter would, so this is the one test in the package that can
// tell cumulative spend apart from per-run spend.
//
// Each of the 3 repeats spends 250 input / 50 output tokens. If aggregateCase
// ever regressed to snapshotting `before` once per CASE instead of once per
// RUN (e.g. hoisting it out of the repeat loop), the per-run deltas would
// become a running series [250, 500, 750] instead of [250, 250, 250] — run 2
// would report double its actual spend, run 3 triple. The bug is silent
// against every other test in this package because rateModel (eval_test.go)
// has no Total() method, so counted==false there and Usage is never computed.
func TestUsageAttributedPerRunNotCumulative(t *testing.T) {
	counting := &CountingModel{Inner: &countingFakeModel{}}
	r := &Runner{Model: counting, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	camp := r.RunN(context.Background(), []Case{harborCase()}, 3)
	if len(camp.Aggregates) != 1 || camp.Aggregates[0].Runs != 3 {
		t.Fatalf("want 1 case with 3 runs, got %+v", camp.Aggregates)
	}

	agg := camp.Aggregates[0]
	// A per-case (cumulative) snapshot would median the series [250,500,750]
	// to 500 — exactly double. Catching that is the point of this test.
	if agg.InputTokens != 250 {
		t.Errorf("InputTokens = %d, want 250 (one run's spend, not the cumulative total)", agg.InputTokens)
	}
	if agg.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50 (one run's spend, not the cumulative total)", agg.OutputTokens)
	}
}
