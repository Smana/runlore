// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"

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
func (li *LoopInvestigator) recordUsageMetrics(ctx context.Context, u providers.UsageTotals) {
	if li.Metrics == nil {
		return
	}
	li.Metrics.InvestigationModelCalls.Record(ctx, int64(u.ModelCalls))
	li.Metrics.InvestigationInputTokens.Record(ctx, int64(u.InputTokens))
	li.Metrics.InvestigationOutputTokens.Record(ctx, int64(u.OutputTokens))
	li.Metrics.InvestigationCachedInputTokens.Record(ctx, int64(u.CachedInputTokens))
	if u.Priced {
		li.Metrics.InvestigationCostUSD.Record(ctx, u.CostUSD)
	}
}
