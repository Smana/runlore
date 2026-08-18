// SPDX-License-Identifier: Apache-2.0

package investigate

import "github.com/Smana/runlore/internal/providers"

// VerifyTier is the adversarial verify pass's model together with the rate card that
// prices ITS tokens — ONE value, because the two are only ever right together.
//
// They used to be two loose fields on LoopInvestigator, VerifyModel and
// VerifyPricing, which every production site set as a pair and the whole eval harness
// set only half of. All three eval runners forwarded a VerifyPricing built from
// model.verify.pricing while handing the loop no verify model at all, so the verify
// pass ran on the main model and was billed at a cheaper model's rates: a `lore eval
// --live` run that truly cost $0.825 reported $0.198, 76% under, and
// max_cost_per_investigation was 76% too loose with it. Two fields a caller must
// remember to set together is a defect waiting for the next call site; the repo made
// this argument once already when recallSpend replaced a split check-and-account,
// because splitting them is how that bug happened.
//
// So the fields are unexported and VerifyOn is the only way in. A rate card cannot
// outlive the model it prices, and the fourth runner — whenever someone writes it —
// cannot reintroduce the bug by naming one field and forgetting the other, because
// there is only one field to name.
//
// The zero value is the common case and is always honest: no separate model, so the
// verify pass reuses LoopInvestigator.Model and its tokens are priced at
// LoopInvestigator.Pricing, the rates of the model that actually generated them.
type VerifyTier struct {
	model   providers.ModelProvider
	pricing *Pricing
}

// VerifyOn routes the adversarial verify pass to m and prices its tokens at p.
//
// A nil m returns the ZERO tier and DROPS p. That is the enforcement, not a
// convenience: with no separate model there is no separate model to price, and
// applying p to tokens the main model generated would put a figure on a notification
// and on runlore_investigation_cost_usd that no call actually incurred. Verify itself
// always runs either way — this only says where, and at what rates.
//
// A nil p with a non-nil m is the ordinary inherit case: the tier has its own model
// but no rates of its own, so aggregateUsage prices its tokens at the main Pricing.
func VerifyOn(m providers.ModelProvider, p *Pricing) VerifyTier {
	if m == nil {
		return VerifyTier{}
	}
	return VerifyTier{model: m, pricing: p}
}

// modelOr returns the model the verify pass actually runs on: this tier's own when
// one was configured, else the investigation model passed as fallback.
func (t VerifyTier) modelOr(fallback providers.ModelProvider) providers.ModelProvider {
	if t.model != nil {
		return t.model
	}
	return fallback
}

// pricingOr returns the rate card the verify pass's tokens are billed at: this tier's
// own when it has one, else the fallback (the main model's). By construction the tier
// only has one when it also has a model, so this can never price a call that was
// never made.
func (t VerifyTier) pricingOr(fallback *Pricing) *Pricing {
	if t.pricing != nil {
		return t.pricing
	}
	return fallback
}
