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
