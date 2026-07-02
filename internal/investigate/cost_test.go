package investigate

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// TestInvestigationUsageAccumulated proves per-investigation token totals sum
// across every model call — the loop steps AND the adversarial verify pass — and
// that a configured pricing yields a cost on the delivered investigation.
func TestInvestigationUsageAccumulated(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		// step 0: a tool call.
		{Text: "look", ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: `{}`}},
			Usage: providers.Usage{InputTokens: 1000, OutputTokens: 100, CachedInputTokens: 200}},
		// step 1: submit_findings (ends the loop).
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"db down","confidence":0.8}]}`}},
			Usage: providers.Usage{InputTokens: 2000, OutputTokens: 50, CachedInputTokens: 500}},
		// verify pass: keep the root cause.
		{ToolCalls: []providers.ToolCall{{ID: "3", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.8}]}`}},
			Usage: providers.Usage{InputTokens: 300, OutputTokens: 20, CachedInputTokens: 0}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Verify:     true,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:    telemetry.NewMetrics(), // exercise the metric-emission path (no panic)
		Pricing:    &Pricing{InputUSDPerMTok: 3, OutputUSDPerMTok: 15, CachedInputUSDPerMTok: 0.3},
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "DBDown"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("nothing delivered")
	}
	u := got.Usage
	if u.ModelCalls != 3 {
		t.Fatalf("ModelCalls = %d, want 3 (2 loop + 1 verify)", u.ModelCalls)
	}
	if u.InputTokens != 3300 || u.OutputTokens != 170 || u.CachedInputTokens != 700 {
		t.Fatalf("token totals = in %d / out %d / cached %d, want 3300/170/700",
			u.InputTokens, u.OutputTokens, u.CachedInputTokens)
	}
	if !u.Priced {
		t.Fatal("Priced must be true when pricing is configured")
	}
	// loop: uncached 2300@3 + 700@0.3 + 150@15 = 0.0069+0.00021+0.00225
	// verify: uncached 300@3 + 20@15 = 0.0009+0.0003
	want := 0.0069 + 0.00021 + 0.00225 + 0.0009 + 0.0003
	if math.Abs(u.CostUSD-want) > 1e-9 {
		t.Fatalf("CostUSD = %.8f, want %.8f", u.CostUSD, want)
	}
}

// TestInvestigationUsageUnpriced proves token totals are still reported without
// pricing, but no cost is claimed.
func TestInvestigationUsageUnpriced(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.7,"root_causes":[{"summary":"x"}]}`}},
			Usage: providers.Usage{InputTokens: 1200, OutputTokens: 80, CachedInputTokens: 100}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got.Usage.ModelCalls != 1 || got.Usage.InputTokens != 1200 {
		t.Fatalf("usage not accumulated when unpriced: %+v", got.Usage)
	}
	if got.Usage.Priced || got.Usage.CostUSD != 0 {
		t.Fatalf("no pricing configured must leave Priced=false, cost=0: %+v", got.Usage)
	}
}

// TestVerifyPricingInherits proves the verify pass's tokens are priced at the
// verify override rate, and that a nil VerifyPricing inherits the main rate.
func TestVerifyPricingInherits(t *testing.T) {
	loop := providers.UsageTotals{ModelCalls: 1, InputTokens: 1000, OutputTokens: 100}
	verify := providers.UsageTotals{ModelCalls: 1, InputTokens: 500, OutputTokens: 40}
	li := &LoopInvestigator{Pricing: &Pricing{InputUSDPerMTok: 10, OutputUSDPerMTok: 30}}
	// Inherit: verify tokens priced at the main rate.
	inherited := li.aggregateUsage(loop, verify)
	li.VerifyPricing = &Pricing{InputUSDPerMTok: 1, OutputUSDPerMTok: 3} // cheaper verify model
	overridden := li.aggregateUsage(loop, verify)
	if !(overridden.CostUSD < inherited.CostUSD) {
		t.Fatalf("a cheaper verify pricing must lower the cost: inherited=%.8f overridden=%.8f",
			inherited.CostUSD, overridden.CostUSD)
	}
}
