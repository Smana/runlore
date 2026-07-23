// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

func TestEvalCostUSDFromPricing(t *testing.T) {
	cfg := &config.Config{}
	if got := evalCostUSD(cfg, providers.Usage{InputTokens: 1_000_000}); got != nil {
		t.Fatalf("unpriced config must yield nil (omit cost, do not claim $0), got %v", *got)
	}
	cfg.Model.Pricing = &config.Pricing{
		InputUSDPerMTok: 1.0, OutputUSDPerMTok: 5.0, CachedInputUSDPerMTok: 0.10,
	}
	got := evalCostUSD(cfg, providers.Usage{
		InputTokens: 2_000_000, CachedInputTokens: 1_000_000, OutputTokens: 200_000,
	})
	// 1M uncached × $1 + 1M cached × $0.10 + 0.2M out × $5 = $2.10
	if got == nil || *got < 2.099 || *got > 2.101 {
		t.Fatalf("want ≈$2.10, got %v", got)
	}
}
