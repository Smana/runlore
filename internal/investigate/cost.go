// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/runlore/internal/providers"
)

// Pricing is per-model token pricing in USD per MILLION tokens, used to estimate a
// per-investigation cost. A nil *Pricing means unpriced (cost is not surfaced);
// the zero value prices everything at $0.
type Pricing struct {
	InputUSDPerMTok       float64
	OutputUSDPerMTok      float64
	CachedInputUSDPerMTok float64
}

// cost estimates the USD cost of u under this pricing. Cached input tokens bill at
// the cached rate and the rest of the input at the standard rate — InputTokens
// INCLUDES cached (see providers.Usage), so the non-cached remainder is split out.
func (p *Pricing) cost(u providers.UsageTotals) float64 {
	if p == nil {
		return 0
	}
	uncachedInput := u.InputTokens - u.CachedInputTokens
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	return float64(uncachedInput)/1e6*p.InputUSDPerMTok +
		float64(u.CachedInputTokens)/1e6*p.CachedInputUSDPerMTok +
		float64(u.OutputTokens)/1e6*p.OutputUSDPerMTok
}

// addUsage accumulates one completion's provider-reported usage into a running
// per-investigation total. Zero usage (provider didn't report) still counts as a
// model call but adds no tokens.
func addUsage(t *providers.UsageTotals, u providers.Usage) {
	t.ModelCalls++
	t.InputTokens += u.InputTokens
	t.OutputTokens += u.OutputTokens
	t.CachedInputTokens += u.CachedInputTokens
}

// aggregateUsage combines the loop's model usage with the verify pass's into one
// per-investigation total and, when pricing is configured, estimates cost. The
// loop tokens are priced at the main model's rate and the verify tokens at the
// verify override's rate (which inherits the main rate when unset) — so a cheaper
// verify model is costed correctly even though the token totals are reported
// combined.
func (li *LoopInvestigator) aggregateUsage(loop, verify providers.UsageTotals) providers.UsageTotals {
	total := providers.UsageTotals{
		ModelCalls:        loop.ModelCalls + verify.ModelCalls,
		InputTokens:       loop.InputTokens + verify.InputTokens,
		OutputTokens:      loop.OutputTokens + verify.OutputTokens,
		CachedInputTokens: loop.CachedInputTokens + verify.CachedInputTokens,
	}
	if li.Pricing != nil {
		total.Priced = true
		verifyPricing := li.Pricing
		if li.VerifyPricing != nil {
			verifyPricing = li.VerifyPricing
		}
		total.CostUSD = li.Pricing.cost(loop) + verifyPricing.cost(verify)
	}
	return total
}

// recordUsageMetrics emits the per-investigation token totals (and estimated cost
// when priced) to telemetry. Nil-safe: a no-op when metrics are disabled.
//
// result carries the SAME completion label investigations_completed_total uses (the
// caller sets it before every finish), so `{result="recall"}` selects exactly the
// delivered recall short-circuits and `{result!="recall"}` exactly the investigations
// that ran the full loop. Without it these histograms mixed both populations with no
// way to tell them apart, and the "recall saving" was a subtraction whose subtrahend
// silently contained its own minuend: the more work recall short-circuited, the more
// recall runs dragged the per-investigation quantiles down, so the measured saving
// shrank precisely as recall got better. One label turns that subtraction into a
// filter.
func (li *LoopInvestigator) recordUsageMetrics(ctx context.Context, u providers.UsageTotals, result string) {
	if li.Metrics == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("result", result))
	li.Metrics.InvestigationModelCalls.Record(ctx, int64(u.ModelCalls), attrs)
	li.Metrics.InvestigationInputTokens.Record(ctx, int64(u.InputTokens), attrs)
	li.Metrics.InvestigationOutputTokens.Record(ctx, int64(u.OutputTokens), attrs)
	li.Metrics.InvestigationCachedInputTokens.Record(ctx, int64(u.CachedInputTokens), attrs)
	if u.Priced {
		li.Metrics.InvestigationCostUSD.Record(ctx, u.CostUSD, attrs)
	}
}
