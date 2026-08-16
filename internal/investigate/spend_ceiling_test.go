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
// must engage on the step whose request would carry the run past 100_000 — the
// fourth, since 3 x 30_010 already spent plus a fourth ~30k request projects to
// 120_030.
//
// The exact call count is the assertion, not a range: it pins WHERE on the ladder
// the stop happens. Three free calls, one nudged call (the model is told to conclude
// and forced to submit_findings), then the hard-kill — the same two-rung ladder the
// per-request ceiling has always used, so an operator sees one behaviour.
func TestTokenCeilingIsARunningTotal(t *testing.T) {
	const (
		perCall  = 30_010 // reportingRunawayModel: 30_000 input + 10 output
		perEst   = 30_000 // the anchored estimate of the NEXT request: its input half
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

	// freeCalls = how many requests fit before the PROJECTED total (what is already
	// spent plus the request about to be sent) crosses the ceiling. The nudge fires on
	// the step after those; one nudged call follows (the model is forced to conclude
	// and refuses), then the next check hard-kills — so the model is called
	// freeCalls+1 times in total.
	freeCalls := 0
	for spent := 0; spent+perEst <= ceiling; spent += perCall {
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
// $0.3005, and the pending request projects at $0.30 (its input half, priced as
// uncached); against a $1.00 ceiling the fourth request is the first whose projection
// crosses, so the ladder nudges on that step and kills on the one after.
func TestCostCeilingStopsTheInvestigation(t *testing.T) {
	const (
		perCallUSD = costPerCallUSD
		// estCostUSD is what the NEXT request projects at: 30_000 input tokens at
		// $10/Mtok. Output length is unknown before the request is sent, so the
		// projection deliberately omits it and errs low.
		estCostUSD = 0.30
		ceilingUSD = 1.00
	)
	li, model := costCeilingInvestigator(costPricing, ceilingUSD)
	var got *providers.Investigation
	li.OnComplete = func(inv providers.Investigation) { got = &inv }
	if err := li.Investigate(context.Background(), Request{Title: "runaway-cost", Fingerprint: "fp-cost"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	freeCalls := 0
	for spent := 0.0; spent+estCostUSD <= ceilingUSD; spent += perCallUSD {
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

// TestBudgetTripReportsOneCeilingForTheWholeRun pins that a run names ONE ceiling.
//
// budgetTrip is an ordered switch (tokens_request → tokens_total → cost) re-evaluated
// on every rung, so the two rungs can disagree: the ceilings below are set so the COST
// arm is the one that stops the run, while the nudged turn it concedes pushes the token
// total past its own ceiling. Re-evaluated at the kill, the ordered switch answers
// "tokens_total" — a different knob from the one the operator was told about at the
// nudge, and the wrong one to raise. The delivered stub then names the cumulative token
// budget for a run that only ever exceeded its dollar ceiling, and
// `sum by (reason) (rate(...))` splits one stop across two series.
//
// The reason is therefore latched when the ladder first engages and carried to the
// kill. Recomputing at the kill with the original ordering would not fix this: the
// spend it reads has grown since, so the ordering can still land on a different arm.
// Only the ceiling that FIRST stopped the run answers "which knob do I raise".
func TestBudgetTripReportsOneCeilingForTheWholeRun(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// Per call: 30_010 tokens, $0.3005. Each step projects one more request on top:
	// +30_000 tokens, +$0.30 (input only — see projectSpend).
	//   after 2 calls: 60_020 tokens / $0.6010 → projects to  90_020 / $0.9010
	//   after 3 calls: 90_030 tokens / $0.9015 → projects to 120_030 / $1.2015
	// $0.85 is crossed a step BEFORE 100_000 is, so the COST arm engages the ladder;
	// by the kill the token projection is over too, and the ordered switch — which
	// puts tokens ahead of cost — would answer tokens_total if it were re-evaluated.
	const (
		perEst     = 30_000 // the anchored estimate of the next request
		tokCeiling = 100_000
	)
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:                     &reportingRunawayModel{inputTokens: perEst},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		Pricing:                   costPricing,
		MaxTokensPerInvestigation: tokCeiling,
		MaxCostPerInvestigation:   0.85,
		Metrics:                   telemetry.NewMetrics(),
		OnComplete:                func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "one-ceiling", Fingerprint: "fp-one"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	body := scrapeMetrics(t, h)
	for _, want := range []string{
		`reason="cost",stage="nudge"`,
		`reason="cost",stage="kill"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("budget-trip telemetry missing %s — the ceiling that stopped the run must be "+
				"the one reported on BOTH rungs:\n%s", want, body)
		}
	}
	// Premise: at the kill the TOKEN arm is genuinely crossed too — otherwise the
	// ordered switch would have nothing else to name and the test would pass vacuously.
	if spent := got.Usage.InputTokens + got.Usage.OutputTokens; spent+perEst <= tokCeiling {
		t.Fatalf("premise failed — by the kill the projected token total must be over %d so the "+
			"ordered switch has a second arm to land on; spent %d, projected %d",
			tokCeiling, spent, spent+perEst)
	}
	if strings.Contains(body, `reason="tokens_total"`) {
		t.Fatalf("the kill renamed the ceiling: this run was stopped by max_cost_per_investigation, "+
			"so a tokens_total series tells the operator to raise the wrong knob:\n%s", body)
	}
	if !mentions(got.Unresolved, "cost ceiling") {
		t.Fatalf("the delivered stub must name the ceiling that stopped the run (the cost one); got: %v",
			got.Unresolved)
	}
}

// bulkTool is a tool whose result is a fixed, large blob, so the message history —
// and therefore every subsequent request — grows the way a real tool-heavy
// investigation's does. size is the reply length in bytes.
type bulkTool struct{ size int }

func (bulkTool) Name() string        { return "what_changed" }
func (bulkTool) Description() string { return "returns a bulky tool result" }
func (bulkTool) Schema() string      { return `{"type":"object"}` }
func (b bulkTool) Call(context.Context, string) (string, error) {
	return strings.Repeat("evidence line for the investigation transcript\n", b.size/45), nil
}

// growingUsageModel reports provider usage proportional to the request it was
// actually sent, so the reported total grows monotonically with the transcript —
// which is what makes the requests AFTER a ceiling trips the largest of the run.
// density is the tokens-per-heuristic-token ratio a real tokenizer shows once JSON
// envelope and role overhead are counted; reporting off the request rather than off
// a fixed script means the fixture cannot drift away from what the loop really sent.
//
// It concludes the moment the loop forces submit_findings, i.e. on the nudged turn,
// so a run takes the ladder's BEST case: nudge, one more request, done. Every worse
// path (model keeps rambling, hard-kill) bills at least as much.
type growingUsageModel struct {
	density int
	calls   int
	billed  []int // reported input+output tokens per call, in order
}

func (m *growingUsageModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	const out = 300
	in := m.density * estimateTokens(req.System, req.Messages, req.Tools)
	m.billed = append(m.billed, in+out)
	resp := providers.CompletionResponse{Usage: providers.Usage{InputTokens: in, OutputTokens: out}}
	if req.ToolChoice == submitFindingsName {
		resp.ToolCalls = []providers.ToolCall{{ID: "s", Name: submitFindingsName,
			Args: `{"confidence":0.5,"root_causes":[{"summary":"wrapped up at the ceiling"}]}`}}
		return resp, nil
	}
	resp.ToolCalls = []providers.ToolCall{{ID: "t", Name: "what_changed", Args: `{}`}}
	return resp, nil
}

// largestBilled returns the biggest single completion this model was billed for.
func (m *growingUsageModel) largestBilled() int {
	n := 0
	for _, b := range m.billed {
		if b > n {
			n = b
		}
	}
	return n
}

// TestTokenCeilingBoundsTheTokensActuallyDelivered measures what the ceiling costs
// rather than asserting what the guard compares.
//
// The ladder is allowed exactly ONE request past the trip by design: the nudge has to
// give the model a turn to conclude. So the honest bound is `ceiling + the largest
// single completion the run made` — one request of residual overshoot, no more.
//
// Comparing what is ALREADY spent against the ceiling cannot hold that bound: the
// cumulative arm only fires once spend has crossed, and THEN spends a further request
// on the nudged turn — and because the transcript grows monotonically, that request is
// the largest of the run. Two of the run's biggest requests land past the ceiling
// instead of one, which is how a 100k ceiling delivers ~1.9x its number. Tripping on
// the PROJECTED total (spent + the request about to be sent) moves the trip one turn
// earlier, so the nudged turn is the request that crosses rather than the one after it.
func TestTokenCeilingBoundsTheTokensActuallyDelivered(t *testing.T) {
	const ceiling = 100_000
	model := &growingUsageModel{density: 2}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:                     model,
		Tools:                     []Tool{bulkTool{size: 32768}},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  20,
		MaxToolOutputBytes:        32768,
		MaxTokensPerInvestigation: ceiling,
		OnComplete:                func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "delivered-tokens", Fingerprint: "fp-delivered"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	delivered := got.Usage.InputTokens + got.Usage.OutputTokens
	// Premise: the run really did have to be stopped by the ceiling, and it really did
	// grow — otherwise the bound below would hold for uninteresting reasons.
	if model.calls < 3 || model.calls >= li.MaxSteps {
		t.Fatalf("premise failed — the ceiling must stop a growing run before max_steps; %d calls of %d, billed %v",
			model.calls, li.MaxSteps, model.billed)
	}
	if largest := model.largestBilled(); delivered > ceiling+largest {
		t.Fatalf("a %d-token ceiling delivered %d tokens (%.2fx), billed per call %v.\n"+
			"The ladder concedes ONE request past the trip (the nudged turn), so the most an "+
			"operator may be billed is ceiling+largest request = %d. Anything beyond that is a "+
			"second oversized request charged after the ceiling was already known to be crossed: "+
			"the cumulative arm must trip on spent+est — what the run will have cost once this "+
			"request is sent — not on what it has cost already.",
			ceiling, delivered, float64(delivered)/ceiling, model.billed, ceiling+largest)
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
