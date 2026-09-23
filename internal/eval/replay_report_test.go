// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"strings"
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

// TestReportCarriesFailedClaims pins the claims surviving the trip through the
// serialized report — the artifact a reader actually opens the morning after a red
// nightly. CaseAggregate converts to ReportCase by struct conversion, so this also
// guards that the two stay field-compatible.
func TestReportCarriesFailedClaims(t *testing.T) {
	camp := Campaign{N: 5, Aggregates: []CaseAggregate{
		{Name: "harbor-chart-bump", Runs: 5, PassRate: 0.4, Missing: []string{"harbor-db"},
			FailedClaims: []string{"a DB migration stalled the database"}},
	}}
	b, err := camp.Report("2026-09-22T11:06:41Z", "openai/glm-4.5-air", providers.Usage{}, nil).JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Cases) != 1 || len(got.Cases[0].FailedClaims) != 1 ||
		got.Cases[0].FailedClaims[0] != "a DB migration stalled the database" {
		t.Fatalf("failed claims not carried into the report: %+v", got.Cases)
	}
	// A passing case must not carry the key at all, so a green report stays terse.
	clean := Campaign{N: 1, Aggregates: []CaseAggregate{{Name: "ok", Runs: 1, PassRate: 1, Reached: true}}}
	cb, err := clean.Report("t", "m", providers.Usage{}, nil).JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if strings.Contains(string(cb), "failed_claims") {
		t.Fatalf("green report must omit failed_claims:\n%s", cb)
	}
}

func TestReportCarriesShadowAgreement(t *testing.T) {
	a := aggregateResults(Case{Name: "c"}, []Result{
		{Pass: true, ShadowRan: true, ShadowAgreed: true},
		{Pass: true, ShadowRan: true, ShadowAgreed: true},
		{Pass: true, ShadowRan: true},
	})
	if a.ShadowAgree != 2 || a.ShadowTotal != 3 {
		t.Fatalf("want 2/3, got %d/%d", a.ShadowAgree, a.ShadowTotal)
	}
	b, err := (Campaign{N: 3, Aggregates: []CaseAggregate{a}}).Report("t", "m", providers.Usage{}, nil).JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Cases[0].ShadowAgree != 2 || got.Cases[0].ShadowTotal != 3 {
		t.Fatalf("shadow agreement not carried: %+v", got.Cases[0])
	}

	// A run with no shadow arm must not ship the keys at all.
	clean := aggregateResults(Case{Name: "c"}, []Result{{Pass: true}})
	cb, err := (Campaign{N: 1, Aggregates: []CaseAggregate{clean}}).Report("t", "m", providers.Usage{}, nil).JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if strings.Contains(string(cb), "shadow_") {
		t.Fatalf("a non-shadow run must omit the shadow keys:\n%s", cb)
	}
}
