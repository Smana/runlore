// SPDX-License-Identifier: Apache-2.0

package eval

import (
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
