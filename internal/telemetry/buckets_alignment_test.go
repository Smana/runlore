// SPDX-License-Identifier: Apache-2.0

// Package telemetry_test holds the telemetry tests that must reach outside this
// package. See export_test.go for why the alignment guard cannot live in
// `package telemetry`.
package telemetry_test

import (
	"slices"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/telemetry"
)

// TestScoreBucketsAlignWithTheDecisionThresholds pins the score-histogram
// boundaries against the
// thresholds it claims to serve, read from the config package rather than
// restated here.
//
// scoreBuckets' doc comment maps four boundaries to four gates — rerank_min_score,
// min_score/margin_gap, solo_floor, dup_score — and that mapping is the whole
// reason the ladder is shaped the way it is: a bucket count is only an answer to
// "how much of the corpus clears this gate?" while the boundary and the gate are
// the same number. Nothing enforced that. Lower solo_floor's default to 3.5 and
// the panel keeps rendering, keeps looking authoritative, and silently stops
// answering the question the comment promises.
//
// Every gate here comes from ApplyDefaults, so this reads what an operator
// actually runs under — dup_score included. It used to be asserted as a bare
// 5.0 literal, because its default was applied in app.BuildCurator instead and
// there was no resolved value to read without building a forge client; a guard
// checking the ladder against its own copy of a number is a guard that cannot
// notice the number moving.
func TestScoreBucketsAlignWithTheDecisionThresholds(t *testing.T) {
	// instant_recall is off by default and ApplyDefaults only fills its knobs once
	// it is enabled, so the fixture enables it. The reranker is on by default,
	// so RerankMinScore resolves without further help.
	var cfg config.Config
	cfg.Catalog.InstantRecall.Enabled = true
	config.ApplyDefaults(&cfg)
	ir := cfg.Catalog.InstantRecall

	for _, tc := range []struct {
		gate  string
		value float64
	}{
		{"catalog.instant_recall.rerank_min_score", ir.RerankMinScore},
		{"catalog.instant_recall.min_score", ir.MinScore},
		{"catalog.instant_recall.margin_gap", ir.MarginGap},
		{"catalog.instant_recall.solo_floor", ir.SoloFloor},
		{"forge.dup_score", cfg.Forge.DupScore},
	} {
		if tc.value == 0 {
			t.Errorf("%s resolved to 0 — either the default moved out of ApplyDefaults or the field was renamed; "+
				"this guard cannot check a gate it cannot read", tc.gate)
			continue
		}
		if !slices.Contains(telemetry.ScoreBuckets, tc.value) {
			t.Errorf("%s is %g, which is not a scoreBuckets boundary %v — a bucket count no longer answers "+
				"\"how much of the corpus clears this gate?\" for it", tc.gate, tc.value, telemetry.ScoreBuckets)
		}
	}
}
