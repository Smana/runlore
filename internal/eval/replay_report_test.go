// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestCampaignReportCarriesProvenance(t *testing.T) {
	camp := Campaign{N: 5, Aggregates: []CaseAggregate{
		{Name: "harbor-chart-bump", Runs: 5, PassRate: 1, Reached: true, Confidence: 0.82},
	}}
	cost := 0.42
	rep := camp.Report("2026-07-23T06:00:00Z", "anthropic/claude-haiku-4-5-20251001",
		providers.Usage{InputTokens: 120000, OutputTokens: 9000}, &cost)
	b, err := rep.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.At != "2026-07-23T06:00:00Z" || got.Model != "anthropic/claude-haiku-4-5-20251001" {
		t.Fatalf("provenance not carried: %+v", got)
	}
	if got.InputTokens != 120000 || got.OutputTokens != 9000 || got.CostUSD == nil || *got.CostUSD != 0.42 {
		t.Fatalf("usage/cost not carried: %+v", got)
	}
	if got.N != 5 || got.Total != 1 || got.Reached != 1 || got.PassRate != 1.0 {
		t.Fatalf("campaign header wrong: %+v", got)
	}
}

func TestCaseAggregateCountsRecall(t *testing.T) {
	c := Case{Name: "recall-case", CatalogDir: "kb", ExpectRecall: "short_circuit"}
	results := []Result{
		{Pass: true, Confidence: 0.9, RecallFired: true, RecallShortCircuit: true},
		{Pass: true, Confidence: 0.8, RecallFired: true},
		{Pass: false, Confidence: 0.4},
	}
	a := aggregateResults(c, results)
	if !a.HasRecall || a.ExpectRecall != "short_circuit" {
		t.Fatalf("recall case identity not carried: %+v", a)
	}
	if a.RecallFired != 2 || a.RecallShortCircuit != 1 {
		t.Fatalf("want fired=2 short-circuit=1, got %+v", a)
	}
	// Existing fold semantics must survive the refactor: 2/3 ≈ 0.67 < 0.7 ⇒ not reached, flaky.
	if a.Reached || !a.Flaky || a.Runs != 3 {
		t.Fatalf("k-of-n fold broken by refactor: %+v", a)
	}
}

func TestEstimateCostUSD(t *testing.T) {
	u := providers.Usage{InputTokens: 2_000_000, CachedInputTokens: 1_000_000, OutputTokens: 200_000}
	// 1M uncached × $1 + 1M cached × $0.10 + 0.2M out × $5 = $2.10
	got := EstimateCostUSD(u, 1.0, 0.10, 5.0)
	if got < 2.099 || got > 2.101 {
		t.Fatalf("want $2.10, got %v", got)
	}
}
