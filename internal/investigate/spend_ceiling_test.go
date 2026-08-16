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

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// TestTokenCeilingIsARunningTotal pins the property the audit found missing:
// max_tokens_per_investigation must bound the investigation's CUMULATIVE model
// spend, not merely the size of the next request.
//
// The shape that made the old guard vacuous is reproduced exactly: every request
// is modest (30k reported input tokens, well under the 100k ceiling), the message
// history stays tiny, and the model never winds down. Under a next-request-only
// check nothing ever exceeds the ceiling, so the loop runs the full step budget
// and bills ~10 x 30k against a "100k budget". With a running total the ladder
// must engage after the fourth call, which is the first moment accumulated spend
// (4 x 30_010 = 120_040) crosses 100_000.
//
// The exact call count is the assertion, not a range: it pins WHERE on the ladder
// the stop happens. Four free calls, one nudged call (the model is told to conclude
// and forced to submit_findings), then the hard-kill — the same two-rung ladder the
// per-request ceiling has always used, so an operator sees one behaviour.
func TestTokenCeilingIsARunningTotal(t *testing.T) {
	const (
		perCall  = 30_010 // reportingRunawayModel: 30_000 input + 10 output
		ceiling  = 100_000
		maxSteps = 10
	)
	model := &reportingRunawayModel{inputTokens: 30_000}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:                     model,
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  maxSteps,
		MaxTokensPerInvestigation: ceiling,
		OnComplete:                func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "runaway-total", Fingerprint: "fp-total"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	// freeCalls = the smallest k whose accumulated spend k*perCall crosses the
	// ceiling; the nudge fires at the top of the step after those. One nudged call
	// follows (the model is forced to conclude and refuses), then the next check
	// hard-kills — so the model is called freeCalls+1 times in total.
	freeCalls := 0
	for spent := 0; spent <= ceiling; spent += perCall {
		freeCalls++
	}
	wantCalls := freeCalls + 1
	if model.calls != wantCalls {
		t.Fatalf("model was called %d times against a %d-token ceiling at %d tokens per call "+
			"(cumulative spend %d); want %d. A per-REQUEST check alone never trips here — "+
			"max_tokens_per_investigation must bound accumulated spend.",
			model.calls, ceiling, perCall, model.calls*perCall, wantCalls)
	}
	if got == nil {
		t.Fatal("OnComplete never called: the hard-kill must still deliver an unresolved result")
	}
	if !mentions(got.Unresolved, "budget") {
		t.Fatalf("the hard-kill result must name the budget it hit; got: %v", got.Unresolved)
	}
}

// costPerCallUSD is what one reportingRunawayModel{inputTokens: 30_000} completion
// costs under costPricing: 30_000 input @ $10/Mtok = $0.3000, plus its 10 output
// tokens @ $50/Mtok = $0.0005.
const costPerCallUSD = 0.3005

// costPricing is the rate card costPerCallUSD is derived from.
var costPricing = &Pricing{InputUSDPerMTok: 10, OutputUSDPerMTok: 50}

// costCeilingInvestigator builds a runaway loop whose every completion reports the
// same usage. Pricing is passed in so the caller can also exercise the unpriced
// case, where the ceiling is inert by construction.
func costCeilingInvestigator(pricing *Pricing, ceilingUSD float64) (*LoopInvestigator, *reportingRunawayModel) {
	model := &reportingRunawayModel{inputTokens: 30_000}
	return &LoopInvestigator{
		Model:   model,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pricing: pricing,
		// The token ceiling is OFF so nothing but the currency comparison can stop
		// this loop — otherwise a passing test would not tell the two apart.
		MaxTokensPerInvestigation: 0,
		MaxCostPerInvestigation:   ceilingUSD,
		MaxSteps:                  10,
		OnComplete:                func(providers.Investigation) {},
	}, model
}

// TestCostCeilingStopsTheInvestigation pins the second half of the audit finding:
// there was no ceiling denominated in CURRENCY at any scope. model.pricing computed
// a cost at the END of an investigation and nothing compared it to anything.
//
// The token ceiling is explicitly disabled here, so the only thing that can stop this
// runaway loop before max_steps is max_cost_per_investigation. One call costs
// $0.3005; against a $1.00 ceiling the fourth call is the first to cross it, so the
// ladder nudges on the step after that and kills on the one after.
func TestCostCeilingStopsTheInvestigation(t *testing.T) {
	const (
		perCallUSD = costPerCallUSD
		ceilingUSD = 1.00
	)
	li, model := costCeilingInvestigator(costPricing, ceilingUSD)
	var got *providers.Investigation
	li.OnComplete = func(inv providers.Investigation) { got = &inv }
	if err := li.Investigate(context.Background(), Request{Title: "runaway-cost", Fingerprint: "fp-cost"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	freeCalls := 0
	for spent := 0.0; spent <= ceilingUSD; spent += perCallUSD {
		freeCalls++
	}
	if want := freeCalls + 1; model.calls != want {
		t.Fatalf("model was called %d times at $%.2f per call against a $%.2f ceiling "+
			"(estimated spend $%.2f); want %d. model.pricing must gate the run, not merely report on it.",
			model.calls, perCallUSD, ceilingUSD, float64(model.calls)*perCallUSD, want)
	}
	if got == nil {
		t.Fatal("OnComplete never called: the cost hard-kill must still deliver an unresolved result")
	}
	if !mentions(got.Unresolved, "cost") {
		t.Fatalf("the hard-kill result must name the cost ceiling it hit; got: %v", got.Unresolved)
	}
	// Premise: the delivered finding really is priced, so the comparison had a
	// dollar figure to work with rather than passing on a zero.
	if !got.Usage.Priced || got.Usage.CostUSD <= ceilingUSD {
		t.Fatalf("premise failed — delivered usage must be priced and over the ceiling: %+v", got.Usage)
	}
}

// TestOverCostBudgetRequiresAPricedTotal pins the guard that separates "$0 spent so
// far" from "no dollar figure exists". Both leave UsageTotals.CostUSD at 0, so only
// the Priced flag can tell them apart, and only the first may be compared.
//
// Today aggregateUsage never produces a CostUSD without setting Priced, so the loop
// test above cannot reach this case — which is exactly why it is pinned here
// directly. Reading CostUSD without the flag is the bug that would let an unpriced
// deployment believe a ceiling was in force.
func TestOverCostBudgetRequiresAPricedTotal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spent   providers.UsageTotals
		ceiling float64
		want    bool
	}{
		{"unpriced totals never trip, whatever CostUSD holds",
			providers.UsageTotals{CostUSD: 5, Priced: false}, 1, false},
		{"priced and over", providers.UsageTotals{CostUSD: 5, Priced: true}, 1, true},
		{"priced and under", providers.UsageTotals{CostUSD: 0.5, Priced: true}, 1, false},
		{"priced exactly at the ceiling is not over (strict >)",
			providers.UsageTotals{CostUSD: 1, Priced: true}, 1, false},
		{"no ceiling configured", providers.UsageTotals{CostUSD: 999, Priced: true}, 0, false},
	} {
		if got := overCostBudget(tc.spent, tc.ceiling); got != tc.want {
			t.Errorf("%s: overCostBudget(%+v, %v) = %v, want %v", tc.name, tc.spent, tc.ceiling, got, tc.want)
		}
	}
}

// TestCostCeilingIsInertWithoutPricing states the limitation plainly rather than
// leaving it implied: with no model.pricing there is no cost to compare, so
// max_cost_per_investigation cannot fire and the run goes the full distance. This is
// exactly the silent-no-op that CostCeilingWithoutPricingWarning exists to announce
// at startup — the guard for that lives in internal/app.
func TestCostCeilingIsInertWithoutPricing(t *testing.T) {
	li, model := costCeilingInvestigator(nil /* unpriced */, 0.01)
	if err := li.Investigate(context.Background(), Request{Title: "unpriced"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if model.calls != li.MaxSteps {
		t.Fatalf("model was called %d times, want %d (the full step budget): an unpriced "+
			"investigation has no cost to compare, so the ceiling must be inert — not "+
			"accidentally firing on a zero cost", model.calls, li.MaxSteps)
	}
}

// TestCompactionDigestCountsTowardTheRunningTotal closes the last model call that was
// spending outside the per-investigation accounting.
//
// Under `compaction: summarize` the loop pays for an extra completion on every
// compaction event: a digest of the batch it just elided. Its tokens reached
// model_input_tokens_total but never loopTotals/verifyTotals, so they were absent from
// the delivered cost footer, from investigation_cost_usd, and — once the ceilings
// became running totals — from both ceilings. A ceiling with a hole in it is worse
// than none, because it reports a number the operator believes.
//
// It is folded into the VERIFY totals, not the loop's: the digest call routes to
// VerifyModel when one is configured, so aggregateUsage's existing split prices it at
// the verify rate, which is the rate that was actually billed.
func TestCompactionDigestCountsTowardTheRunningTotal(t *testing.T) {
	// Small enough that the digests fit summarizeLoop's 6000-token ceiling: this test
	// is about the tokens being COUNTED, and a fixture that also tripped the ceiling
	// would stop the loop early and confuse the two properties.
	const digestTokens = 400
	sm := &fakeSummarizer{resp: providers.CompletionResponse{
		Text:  digestSentinel,
		Usage: providers.Usage{InputTokens: digestTokens, OutputTokens: 7},
	}}
	li, got := summarizeLoop(t, sm, nil)
	// The main scriptModel reports zero usage, so every token in the delivered totals
	// is the summarizer's — and the digest count is exactly its call count.
	if err := li.Investigate(context.Background(), Request{Title: "digest accounting"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	digests := sm.count()
	if digests == 0 {
		t.Fatal("premise failed — no compaction digest was produced, so there is nothing to account for")
	}
	wantTokens := digests * (digestTokens + 7)
	if spent := got.Usage.InputTokens + got.Usage.OutputTokens; spent != wantTokens {
		t.Fatalf("delivered usage counts %d tokens after %d compaction digests; want %d — "+
			"the digest call is billed like any other, so a running-total ceiling that cannot "+
			"see it under-counts every summarize-mode investigation",
			spent, digests, wantTokens)
	}
	if got.Usage.ModelCalls < digests {
		t.Fatalf("delivered usage counts %d model calls but the summarizer alone made %d",
			got.Usage.ModelCalls, digests)
	}
}

// TestBudgetTripTelemetryNamesTheCeilingAndRung pins what an operator can actually
// see when a ceiling fires in production.
//
// investigations_dropped_total already counts the kills, but it cannot answer either
// question that matters: WHICH ceiling to raise, and how often runs are being cut
// short WITHOUT dying — the nudge rung still delivers findings, so it leaves no other
// trace at all. Both labels are therefore asserted on both rungs.
func TestBudgetTripTelemetryNamesTheCeilingAndRung(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	li := &LoopInvestigator{
		Model:                     &reportingRunawayModel{inputTokens: 30_000},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		MaxTokensPerInvestigation: 100_000,
		Metrics:                   telemetry.NewMetrics(),
		OnComplete:                func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "trip-telemetry"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	body := scrapeMetrics(t, h)
	for _, want := range []string{
		`runlore_investigation_budget_trips_total`,
		`reason="tokens_total"`,
		`stage="nudge"`,
		`stage="kill"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("budget-trip telemetry missing %s — an operator cannot see which ceiling fired "+
				"or which rung of the ladder it reached:\n%s", want, body)
		}
	}
	// The reason must be the CUMULATIVE one: every individual request here is small,
	// so a series labelled tokens_request would be pointing at the wrong knob.
	if strings.Contains(body, `reason="tokens_request"`) {
		t.Fatalf("modest requests must not be reported as a per-request trip:\n%s", body)
	}
}

// mentions reports whether any line contains sub (case-insensitively).
func mentions(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), sub) {
			return true
		}
	}
	return false
}
