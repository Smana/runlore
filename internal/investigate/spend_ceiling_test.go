// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"strconv"
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
// is modest — 60k reported input tokens, comfortably under the 100_000 bound this
// ceiling derives for one request — the message history stays tiny, and the model
// never winds down. Under a next-request-only check nothing ever exceeds anything,
// so the loop runs the full step budget and bills ~10 x 60k against a "400k budget".
// With a running total the ladder must engage on the step whose request would carry
// the run past 400_000: the seventh, since 6 x 60_010 already spent plus a seventh
// ~60k request projects to 420_060.
//
// The exact call count is the assertion, not a range: it pins WHERE on the ladder
// the stop happens. Six free calls, one nudged call (the model is told to conclude
// and forced to submit_findings), then the hard-kill — the same two-rung ladder the
// per-request ceiling has always used, so an operator sees one behaviour.
func TestTokenCeilingIsARunningTotal(t *testing.T) {
	const (
		perCall  = 60_010 // reportingRunawayModel: 60_000 input + 10 output
		perEst   = 60_000 // the anchored estimate of the NEXT request: its input half
		ceiling  = 400_000
		maxSteps = 10
	)
	model := &reportingRunawayModel{inputTokens: perEst}
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

// TestCompactionDigestCountsTowardTheRunningTotal closes the last COMPLETION inside
// the investigation loop that was spending outside the per-investigation accounting.
// Not the last spend path overall: the /embeddings call on the hybrid-recall query
// path also runs inside an investigation and is still uncounted (instrumented, not
// bounded — see internal/embed and the inventory in docs/configuration).
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
		// 60k per request against a 400_000 ceiling: modest next to the 100_000 bound
		// the ceiling derives for one request, so only the CUMULATIVE arm can fire.
		Model:                     &reportingRunawayModel{inputTokens: 60_000},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		MaxTokensPerInvestigation: 400_000,
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

	// Per call: 60_010 tokens, $0.6005. Each step projects one more request on top:
	// +60_000 tokens, +$0.60 (input only — see projectSpend).
	//   after 4 calls: 240_040 tokens / $2.4020 → projects to 300_040 / $3.0020
	//   after 5 calls: 300_050 tokens / $3.0025 → projects to 360_050 / $3.6025
	// $3.40 is crossed a step BEFORE 400_000 is, so the COST arm engages the ladder;
	// by the kill the token projection is over too, and the ordered switch — which
	// puts tokens ahead of cost — would answer tokens_total if it were re-evaluated.
	// 60_000 stays under the 100_000 bound the ceiling derives for one request, so the
	// per-request arm is not a third candidate here.
	const (
		perEst     = 60_000 // the anchored estimate of the next request
		tokCeiling = 400_000
	)
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:                     &reportingRunawayModel{inputTokens: perEst},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		Pricing:                   costPricing,
		MaxTokensPerInvestigation: tokCeiling,
		MaxCostPerInvestigation:   3.40,
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
//
// tool names the tool it calls each turn, because that decides whether mid-loop
// compaction may touch the transcript at all: `what_changed` is on compactHistory's
// keep list (never elided), `big_tool` is ordinary and therefore elidable. A fixture
// that only ever calls a keep-listed tool cannot observe compaction, so the field is
// explicit rather than defaulted.
type growingUsageModel struct {
	density int
	tool    string
	calls   int
	billed  []int // reported input+output tokens per call, in order

	// elidedAtCall is the 1-based index of the first request that arrived carrying an
	// elision marker — i.e. the first request compaction had shrunk. 0 means compaction
	// never fired. spentBeforeElision is what the run had already billed at that point,
	// so a test can assert compaction happened while the ceiling still had headroom
	// rather than after the ladder had already stopped the run.
	elidedAtCall       int
	spentBeforeElision int
}

func (m *growingUsageModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	const out = 300
	if m.elidedAtCall == 0 {
		for _, msg := range req.Messages {
			if isElidedMarker(msg.Content) {
				m.elidedAtCall = m.calls
				m.spentBeforeElision = m.spent()
				break
			}
		}
	}
	in := m.density * estimateTokens(req.System, req.Messages, req.Tools)
	m.billed = append(m.billed, in+out)
	resp := providers.CompletionResponse{Usage: providers.Usage{InputTokens: in, OutputTokens: out}}
	if req.ToolChoice == submitFindingsName {
		resp.ToolCalls = []providers.ToolCall{{ID: "s", Name: submitFindingsName,
			Args: `{"confidence":0.5,"root_causes":[{"summary":"wrapped up at the ceiling"}]}`}}
		return resp, nil
	}
	// A distinct id per call: compactHistory attributes a tool RESULT to its tool
	// through the call id, and a fixture that reused one id would make every result
	// look like a repeat of the same call.
	resp.ToolCalls = []providers.ToolCall{{ID: "t" + strconv.Itoa(m.calls), Name: m.tool, Args: `{}`}}
	return resp, nil
}

// spent is everything this model has reported so far.
func (m *growingUsageModel) spent() int {
	n := 0
	for _, b := range m.billed {
		n += b
	}
	return n
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
	model := &growingUsageModel{density: 2, tool: "what_changed"}
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

// TestCompactionFiresBeforeTheCumulativeCeiling pins the property that made
// `compaction: summarize` dead code the moment the token ceiling became a running
// total: mid-loop compaction has to be REACHABLE.
//
// Compaction is the loop's answer to a transcript that outgrows one request. It has
// to trigger off a bound on ONE request, because that is the quantity it can do
// something about — it shrinks the next request; it cannot un-spend what is already
// billed. Keyed off the CUMULATIVE ceiling instead, its trigger sits at 0.7x a number
// the run's accumulated spend reaches first: on a monotonically growing transcript
// sum(est_i) passes the ceiling long before any single est_i reaches 0.7 of it, so the
// ladder always stops the run first and compaction never runs. Measured at the shipped
// tool-output cap before this was split: the largest single request of a run was 51 323
// tokens against a 70 000 trigger — compaction never fired at any ceiling.
//
// The whole existing compaction suite passed throughout, because its main model
// reports ZERO usage: with loopTotals pinned at 0 the cumulative arm can never engage,
// so those tests cannot see this at all. That is why this one drives the loop with a
// model that reports usage proportional to the request it was actually sent, and why
// it asserts the run had really started spending before compaction fired.
func TestCompactionFiresBeforeTheCumulativeCeiling(t *testing.T) {
	const ceiling = 400_000
	// big_tool is NOT on compactHistory's keep list, so its outputs are elidable —
	// the case an operator's log/diff-heavy investigation actually hits.
	model := &growingUsageModel{density: 2, tool: "big_tool"}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:                     model,
		Tools:                     []Tool{bigTool{size: 32768}},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  20,
		MaxToolOutputBytes:        32768,
		MaxTokensPerInvestigation: ceiling,
		OnComplete:                func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "compaction-reachable", Fingerprint: "fp-compact"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	// Premise: this fixture really is billing tokens. A model reporting zero usage
	// keeps the cumulative arm at 0 forever, which is exactly how the regression hid.
	if delivered := got.Usage.InputTokens + got.Usage.OutputTokens; delivered == 0 {
		t.Fatal("premise failed — the model reported no usage, so the cumulative ceiling was " +
			"never engaged and this test could not observe the interaction it exists to pin")
	}
	if model.elidedAtCall == 0 {
		t.Fatalf("mid-loop compaction never fired in %d requests against a %d-token ceiling "+
			"(billed per call %v). The compaction trigger must key off the bound on ONE request, "+
			"not off the cumulative run budget: keyed off the cumulative number its trigger sits "+
			"above what a single request ever reaches, so the ladder stops the run first and "+
			"`compaction: summarize` cannot run at any default-shaped config.",
			model.calls, ceiling, model.billed)
	}
	// …and it fired while the ceiling still had headroom. Compaction that only ever
	// ran on the nudged turn would technically "fire" while still being useless: by
	// then the spend it exists to avoid has already happened.
	if model.spentBeforeElision >= ceiling {
		t.Fatalf("compaction first fired on call %d, by which point the run had already billed "+
			"%d of its %d-token ceiling — compaction exists to keep a run under the ceiling, so "+
			"firing at or past it buys nothing", model.elidedAtCall, model.spentBeforeElision, ceiling)
	}
}

// TestPerRequestCeilingIsAFractionOfTheCumulativeOne pins the second half of the same
// split: the per-request arm needs its OWN threshold.
//
// One number cannot mean both "what the whole run may spend" and "what one request may
// spend". Read as a run budget it is right; reused as the per-request bound it says a
// single request may consume the entire investigation's budget — which is no bound at
// all, and leaves compaction (0.7x of it) unreachable. So the per-request arm compares
// against requestBudget: a quarter of the run's budget, i.e. "the ceiling must fund at
// least four full-size requests".
//
// The fixture is the shape only the per-request arm can catch: one request four times
// the size of a normal one, arriving while the run's cumulative spend is still well
// under the ceiling. Against a per-request arm set to the whole ceiling, nothing stops
// it — the run drifts on and is eventually stopped by the cumulative arm instead, which
// names a different knob to the operator and lets the oversized request be billed twice
// over before anything reacts.
func TestPerRequestCeilingIsAFractionOfTheCumulativeOne(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// perCall is over the per-request bound but small enough that three of them still
	// project under the run budget — so the cumulative arm demonstrably has room left
	// when the per-request arm fires, and only the per-request arm can explain the stop.
	const (
		ceiling  = 400_000 // ⇒ per-request bound 100_000
		perCall  = 120_000 // one request, a fifth over that bound
		wantCall = 2       // one free call, then the nudged one; the next check kills
	)
	model := &reportingRunawayModel{inputTokens: perCall}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:                     model,
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		MaxTokensPerInvestigation: ceiling,
		Metrics:                   telemetry.NewMetrics(),
		OnComplete:                func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "oversized-request", Fingerprint: "fp-req"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	if model.calls != wantCall {
		t.Fatalf("the model was called %d times, want %d: a request of %d tokens is over a %d-token "+
			"per-request bound (a quarter of the %d ceiling) and must be caught before it is sent "+
			"a second time, not left to the cumulative arm several requests later",
			model.calls, wantCall, perCall, ceiling/4, ceiling)
	}
	// Premise: the CUMULATIVE arm cannot be what stopped this. Even projecting one more
	// request on top of everything billed, the run is under the ceiling — so a stop here
	// can only have come from the per-request arm.
	spent := got.Usage.InputTokens + got.Usage.OutputTokens
	if spent+perCall > ceiling {
		t.Fatalf("premise failed — the run billed %d and projects to %d against a %d ceiling, so "+
			"the cumulative arm could have stopped it and this test would pass vacuously",
			spent, spent+perCall, ceiling)
	}
	body := scrapeMetrics(t, h)
	if !strings.Contains(body, `reason="tokens_request"`) {
		t.Fatalf("a single oversized request must be reported as a per-request trip — the operator's "+
			"fix is to shrink one request (max_tool_output_bytes, compaction), not to raise the run "+
			"budget:\n%s", body)
	}
	if strings.Contains(body, `reason="tokens_total"`) {
		t.Fatalf("this run never approached its cumulative budget, so a tokens_total series points "+
			"the operator at the wrong knob:\n%s", body)
	}
	if !mentions(got.Unresolved, "per-request") {
		t.Fatalf("the delivered stub must name the per-request bound it hit; got: %v", got.Unresolved)
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
