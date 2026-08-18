// SPDX-License-Identifier: Apache-2.0

package eval

import "github.com/Smana/runlore/internal/investigate"

// Spend is the per-investigation ceiling set an eval runner hands to every
// LoopInvestigator it builds. It carries the operator's CONFIGURED
// investigation.max_tokens_per_investigation / max_cost_per_investigation and
// model.pricing unchanged — an eval is a replay of the production loop, so a case
// that would be cut short in production must be cut short here too, or the campaign
// measures a loop no deployment ever runs.
//
// It is a struct rather than four loose fields so the three runners (replay,
// compare, live) share ONE definition, and so a construction site that forgets it is
// visibly missing a named field — see the module-wide guard in
// internal/investigate/spend_controls_wired_test.go.
//
// WHAT IT DOES NOT COVER: this bounds ONE investigation. A campaign is
// cases x n investigations plus a judge call each, so the ceilings here multiply by
// the corpus size and bound nothing about the run as a whole. CampaignBudget is the
// bound on the run; the two are complementary, not alternatives.
type Spend struct {
	// MaxTokensPerInvestigation is investigate.LoopInvestigator's cumulative token
	// ceiling. 0 ⇒ unlimited (the consumer sentinel config.ApplyDefaults maps the
	// user-visible -1 opt-out onto).
	MaxTokensPerInvestigation int
	// MaxCostPerInvestigation is the USD ceiling on one investigation. 0 ⇒ none. It
	// is inert without Pricing, exactly as in production.
	MaxCostPerInvestigation float64
	// Pricing prices the loop's tokens. nil ⇒ unpriced: token totals are still
	// reported, no cost is, and the cost ceiling cannot fire.
	Pricing *investigate.Pricing
	// Verifier is the adversarial verify pass's model AND the rate card for that
	// model's tokens, as one value (investigate.VerifyOn). The zero value — which is
	// what every runner here gets today, because an eval hands the loop exactly one
	// model — means verify runs on that model and is priced at Pricing.
	//
	// It is one field rather than two because this is precisely where two went wrong:
	// all three runners forwarded a verify RATE CARD built from model.verify.pricing
	// while forwarding no verify MODEL, so `lore eval --live` billed the main model's
	// verify tokens at a cheaper model's rates and reported $0.198 for a run that cost
	// $0.825. VerifyOn cannot carry rates without a model, so the halves cannot drift
	// apart again.
	Verifier investigate.VerifyTier
}
