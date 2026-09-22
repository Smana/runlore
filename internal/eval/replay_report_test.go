// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"fmt"
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

// TestAggregateCollectsFailingRunClaims pins the diagnostic the nightly eval lacked.
//
// Every gated case failed on a single absent root_cause_entities substring, and the
// report recorded only the missing TERM — never what the agent actually claimed. Six
// weeks of red nightlies therefore could not distinguish "named the right cause in
// looser words" from "blamed the wrong thing", and nobody could tell without
// re-running the campaign by hand. The fold keeps the FAILING runs' claims only:
// a passing run's claim is not a finding, and carrying every repeat would bloat the
// report with near-duplicates.
func TestAggregateCollectsFailingRunClaims(t *testing.T) {
	c := Case{Name: "harbor-chart-bump"}
	results := []Result{
		{Pass: false, Claim: "a DB migration stalled the database"},
		{Pass: true, Claim: "the harbor-db migration lock is held"},
		{Pass: false, Claim: "a DB migration stalled the database"}, // same wording twice
		{Pass: false, Claim: "the network dropped connections"},
	}
	a := aggregateResults(c, results)
	want := []string{"a DB migration stalled the database", "the network dropped connections"}
	if len(a.FailedClaims) != len(want) {
		t.Fatalf("want %d distinct failing claims, got %d: %q", len(want), len(a.FailedClaims), a.FailedClaims)
	}
	for i, w := range want {
		if a.FailedClaims[i] != w {
			t.Fatalf("claim %d: want %q, got %q", i, w, a.FailedClaims[i])
		}
	}
}

// TestAggregateCapsFailingClaims keeps the report bounded: a 10-repeat case whose runs
// all disagree must not paste ten paragraphs into the published JSON.
func TestAggregateCapsFailingClaims(t *testing.T) {
	results := make([]Result, 0, 10)
	for i := range 10 {
		results = append(results, Result{Pass: false, Claim: fmt.Sprintf("distinct claim %d", i)})
	}
	a := aggregateResults(Case{Name: "noisy"}, results)
	if len(a.FailedClaims) != maxFailedClaims {
		t.Fatalf("want the cap of %d claims, got %d", maxFailedClaims, len(a.FailedClaims))
	}
}

// TestAggregateSkipsEmptyClaims covers the runs that never produced a claim at all —
// an investigation error, or a loop that never called submit_findings. Those Results
// carry a Missing note and no Claim, and an empty string is not a diagnostic.
func TestAggregateSkipsEmptyClaims(t *testing.T) {
	results := []Result{
		{Pass: false, Missing: []string{"no findings (loop did not submit)"}},
		{Pass: false, Claim: "blamed the wrong workload"},
	}
	a := aggregateResults(Case{Name: "c"}, results)
	if len(a.FailedClaims) != 1 || a.FailedClaims[0] != "blamed the wrong workload" {
		t.Fatalf("empty claims must be skipped, got %q", a.FailedClaims)
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
