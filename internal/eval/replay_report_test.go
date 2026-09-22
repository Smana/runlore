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

// TestAggregateCollectsFailingRunClaims pins the fold: the FAILING repeats' claims
// only, first-seen order, exact-text dedup. Rationale: see eval.Result.Claim.
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

// TestAggregateCapsAClaimAndMarksTheCut keeps the report bounded while telling a reader
// the difference between "the model stopped here" and "we cut it here".
func TestAggregateCapsAClaimAndMarksTheCut(t *testing.T) {
	long := strings.Repeat("x", maxClaimBytes*2)
	a := aggregateResults(Case{Name: "c"}, []Result{{Pass: false, Claim: long}})
	if len(a.FailedClaims) != 1 {
		t.Fatalf("want one claim, got %d", len(a.FailedClaims))
	}
	got := a.FailedClaims[0]
	if len(got) > maxClaimBytes+len(" […]") {
		t.Fatalf("claim not capped: %d bytes (cap %d)", len(got), maxClaimBytes)
	}
	if !strings.HasSuffix(got, "[…]") {
		t.Fatalf("a cut claim must say so, got the tail %q", got[max(0, len(got)-16):])
	}
}

// TestAggregateDedupsOnTheFullClaimNotItsCappedForm pins why `seen` keys on the
// untruncated text: two answers that agree for the length of the cap and then diverge
// are different answers, and comparing their capped forms would collapse them into one,
// losing exactly the part that differed.
func TestAggregateDedupsOnTheFullClaimNotItsCappedForm(t *testing.T) {
	shared := strings.Repeat("y", maxClaimBytes+50)
	a := aggregateResults(Case{Name: "c"}, []Result{
		{Pass: false, Claim: shared + " and it is the payments DB"},
		{Pass: false, Claim: shared + " and it is the search index"},
	})
	if len(a.FailedClaims) != 2 {
		t.Fatalf("two claims sharing a long prefix are two claims, got %d: %q", len(a.FailedClaims), a.FailedClaims)
	}
}

// TestAggregateKeepsTheLoserClaimOnAReachedCase pins the contract a 1-repeat test can
// never see: "reached the k-of-n bar" is not "every repeat passed".
//
// At n=5 a 4/5 case is Reached, and the one failing repeat's claim is exactly the
// flakiness a reader wants explained — so the claim is recorded, and only an
// all-passing case carries none.
func TestAggregateKeepsTheLoserClaimOnAReachedCase(t *testing.T) {
	results := []Result{
		{Pass: true}, {Pass: true}, {Pass: true}, {Pass: true},
		{Pass: false, Claim: "blamed the search index"},
	}
	a := aggregateResults(Case{Name: "flaky-but-reached"}, results)
	if !a.Reached {
		t.Fatalf("4/5 must clear the %.0f%% bar, got %+v", evalMinPassRate*100, a)
	}
	if len(a.FailedClaims) != 1 || a.FailedClaims[0] != "blamed the search index" {
		t.Fatalf("a reached case must still report its losing repeat's claim, got %q", a.FailedClaims)
	}
	allPass := aggregateResults(Case{Name: "clean"}, []Result{{Pass: true}, {Pass: true}})
	if len(allPass.FailedClaims) != 0 {
		t.Fatalf("an all-passing case must carry no claims, got %q", allPass.FailedClaims)
	}
}
