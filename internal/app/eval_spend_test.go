// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"github.com/Smana/runlore/internal/config"
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
	// The verify pass runs on its own tier when configured; pricing it at the main
	// model's rate would over-report the cost of every recall-exercising case.
	if spend.VerifyPricing == nil || spend.VerifyPricing.InputUSDPerMTok != 1 {
		t.Errorf("verify pricing: got %+v, want model.verify.pricing's 1", spend.VerifyPricing)
	}
}

// TestEvalSpendIsUnpricedWithoutModelPricing pins the honest half: no model.pricing
// means no Pricing, so the eval reports token totals and no cost, exactly as
// production does. Fabricating a zero rate card here would make an unpriced campaign
// claim a $0.00 spend and make the cost ceiling look armed when it is not.
func TestEvalSpendIsUnpricedWithoutModelPricing(t *testing.T) {
	spend := evalSpend(&config.Config{})
	if spend.Pricing != nil || spend.VerifyPricing != nil {
		t.Fatalf("unpriced config must leave both pricings nil, got %+v / %+v", spend.Pricing, spend.VerifyPricing)
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
