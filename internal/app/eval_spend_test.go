// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
)

// TestEvalSpendCarriesTheConfiguredCeilings pins that `lore eval` runs each case
// under the operator's CONFIGURED per-investigation ceilings — the same
// investigation.max_tokens_per_investigation / max_cost_per_investigation and
// model.pricing that `lore serve` and `lore investigate` use.
//
// They are the same keys on purpose. An eval replays the production loop; a case
// that would be nudged or hard-killed in production must be here too, or the
// campaign's pass-rate describes a loop no deployment ever runs. A second set of
// eval-only keys would let the corpus be scored under limits the cluster does not
// have, which is the failure mode worth designing against.
func TestEvalSpendCarriesTheConfiguredCeilings(t *testing.T) {
	cfg := &config.Config{}
	cfg.Investigation.MaxTokensPerInvestigation = 55_000
	cfg.Investigation.MaxCostPerInvestigation = 0.25
	cfg.Model.Pricing = &config.Pricing{InputUSDPerMTok: 3, OutputUSDPerMTok: 15}
	cfg.Model.Verify = &config.ModelOverride{Pricing: &config.Pricing{InputUSDPerMTok: 1, OutputUSDPerMTok: 5}}

	spend := evalSpend(cfg)
	if spend.MaxTokensPerInvestigation != 55_000 {
		t.Errorf("token ceiling: got %d, want the configured 55000", spend.MaxTokensPerInvestigation)
	}
	if spend.MaxCostPerInvestigation != 0.25 {
		t.Errorf("cost ceiling: got %v, want the configured 0.25", spend.MaxCostPerInvestigation)
	}
	// Pricing is not decoration: without it the cost ceiling has no dollar figure to
	// compare and can never fire (investigate.overCostBudget).
	if spend.Pricing == nil {
		t.Fatal("Pricing is nil though model.pricing is configured — the cost ceiling would be inert")
	}
	if spend.Pricing.InputUSDPerMTok != 3 {
		t.Errorf("loop pricing: got %v, want model.pricing's 3", spend.Pricing.InputUSDPerMTok)
	}
	// An eval runner is handed exactly ONE model and no verify override, so the
	// adversarial pass runs on that model and its tokens must be priced at Pricing.
	// This config has a model.verify.pricing that is 5x cheaper; carrying it into the
	// campaign was how `lore eval --live` reported $0.198 for a run that cost $0.825.
	// eval.Spend.Verifier is one value (model AND its rates) so an empty tier cannot
	// smuggle a rate card in on its own.
	if spend.Verifier != (investigate.VerifyTier{}) {
		t.Errorf("evalSpend built a verify tier (%+v) though no eval runner is given a verify "+
			"model: whatever rates it carries would be applied to tokens the runner's ONE "+
			"model generated", spend.Verifier)
	}
}

// TestEvalSpendCannotPriceVerifyAtAModelItNeverCalls is the end-to-end statement of
// the same thing, through the runner rather than the builder: a `lore eval` case run
// under evalSpend's ceilings must report the cost of the tokens its single model
// actually generated. A 5x-cheaper model.verify.pricing in the config must not move
// that figure, because no call was ever made to the model it describes.
func TestEvalSpendCannotPriceVerifyAtAModelItNeverCalls(t *testing.T) {
	base := &config.Config{}
	base.Model.Pricing = &config.Pricing{InputUSDPerMTok: 15, OutputUSDPerMTok: 75}

	withCheapVerify := &config.Config{}
	withCheapVerify.Model.Pricing = base.Model.Pricing
	withCheapVerify.Model.Verify = &config.ModelOverride{
		Pricing: &config.Pricing{InputUSDPerMTok: 3, OutputUSDPerMTok: 15},
	}

	plain, tiered := evalSpend(base), evalSpend(withCheapVerify)
	if plain.Verifier != tiered.Verifier {
		t.Errorf("model.verify.pricing changed the eval verify tier (%+v vs %+v) without "+
			"changing which model the eval calls — that difference can only show up as a "+
			"wrong dollar figure", plain.Verifier, tiered.Verifier)
	}
	if *plain.Pricing != *tiered.Pricing {
		t.Errorf("loop pricing drifted: %+v vs %+v", plain.Pricing, tiered.Pricing)
	}
}

// TestEvalSpendIsUnpricedWithoutModelPricing pins the honest half: no model.pricing
// means no Pricing, so the eval reports token totals and no cost, exactly as
// production does. Fabricating a zero rate card here would make an unpriced campaign
// claim a $0.00 spend and make the cost ceiling look armed when it is not.
func TestEvalSpendIsUnpricedWithoutModelPricing(t *testing.T) {
	spend := evalSpend(&config.Config{})
	if spend.Pricing != nil || spend.Verifier != (investigate.VerifyTier{}) {
		t.Fatalf("unpriced config must leave the loop rate card nil and the verify tier empty, "+
			"got %+v / %+v", spend.Pricing, spend.Verifier)
	}
}

// TestCompareWithoutConfigStillGetsTheBoundedTokenDefault closes the one eval path
// that can run with no runlore.yaml at all.
//
// `lore eval --compare` is documented as forgiving an absent default config, because
// the comparison spec carries its own per-entry models. It then synthesises an EMPTY
// config — and an empty config's max_tokens_per_investigation is 0, which every
// consumer in internal/investigate reads as "unlimited". So the one eval invocation
// that needs no config was also the one that would benchmark several models, N times
// over the whole corpus, with no token ceiling whatsoever. config.ApplyDefaults is
// what puts the same 100000 default there that a real config would have got from
// config.Load.
func TestCompareWithoutConfigStillGetsTheBoundedTokenDefault(t *testing.T) {
	cfg := configForAbsentFile()
	if cfg.Investigation.MaxTokensPerInvestigation <= 0 {
		t.Fatalf("the synthesised config for an absent runlore.yaml has "+
			"max_tokens_per_investigation=%d, which internal/investigate reads as UNLIMITED — "+
			"a --compare run would benchmark every entry with no token ceiling",
			cfg.Investigation.MaxTokensPerInvestigation)
	}
	if got := evalSpend(cfg).MaxTokensPerInvestigation; got != cfg.Investigation.MaxTokensPerInvestigation {
		t.Errorf("evalSpend dropped the defaulted ceiling: got %d, want %d",
			got, cfg.Investigation.MaxTokensPerInvestigation)
	}
}
