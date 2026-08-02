// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"strings"
	"testing"
)

func costReport() Report {
	return Report{
		At: "2026-08-02T06:00:00Z", Model: "anthropic/claude-haiku-4-5", N: 5,
		Reached: 2, Total: 2, PassRate: 1,
		Cases: []ReportCase{
			// A full investigation: no recall short-circuit, high token spend.
			{Name: "gitops-bad-image-tag", Runs: 5, Reached: true,
				InputTokens: 96000, OutputTokens: 3200},
			// An instant recall: short-circuited, an order of magnitude cheaper.
			{Name: "known-pattern-recall", Runs: 5, Reached: true, HasRecall: true,
				RecallShortCircuit: 5, InputTokens: 4100, OutputTokens: 260},
		},
	}
}

// TestCostSectionSplitsRecallFromFullLoop is the report's asked-for comparison: a
// full investigation next to an instant recall, priced. It is the number no
// competitor publishes.
func TestCostSectionSplitsRecallFromFullLoop(t *testing.T) {
	got := costSection(costReport(), 1.00, 0.10, 5.00)
	if !strings.Contains(got, "full investigation") {
		t.Errorf("missing the full-investigation row:\n%s", got)
	}
	if !strings.Contains(got, "instant recall") {
		t.Errorf("missing the instant-recall row:\n%s", got)
	}
	// 96000 in @ $1/MTok + 3200 out @ $5/MTok = 0.096 + 0.016 = $0.112
	if !strings.Contains(got, "0.11") {
		t.Errorf("full-investigation cost not rendered:\n%s", got)
	}
	// The model must be named next to the figure — a naked price is unfalsifiable.
	if !strings.Contains(got, "claude-haiku-4-5") {
		t.Errorf("cost figures must name the model:\n%s", got)
	}
}

// TestCostSectionOmittedWithoutPrices: no prices configured means no cost section at
// all. Rendering "$0.00" would be a lie.
func TestCostSectionOmittedWithoutPrices(t *testing.T) {
	if got := costSection(costReport(), 0, 0, 0); got != "" {
		t.Errorf("expected no cost section without prices, got:\n%s", got)
	}
}

// TestCostSectionOmittedWithOnlyOneRateSet: config.Pricing's rates are each
// independently optional, so a config can set input_usd_per_mtok and omit
// output_usd_per_mtok (or vice versa) and still pass validatePricing. If the guard
// only rejected inUSD==0 && outUSD==0, this would slip through and EstimateCostUSD
// would silently price the zero-rate token type at $0 — understating the published
// cost with no signal anything is wrong. Covers both directions of the omission.
func TestCostSectionOmittedWithOnlyOneRateSet(t *testing.T) {
	if got := costSection(costReport(), 1.00, 0, 0); got != "" {
		t.Errorf("expected no cost section with outUSD unset, got:\n%s", got)
	}
	if got := costSection(costReport(), 0, 0, 5.00); got != "" {
		t.Errorf("expected no cost section with inUSD unset, got:\n%s", got)
	}
}

// TestCostSectionOmittedWithoutTokens: an old report carrying no per-case tokens must
// not render an empty or zeroed table.
func TestCostSectionOmittedWithoutTokens(t *testing.T) {
	rep := costReport()
	for i := range rep.Cases {
		rep.Cases[i].InputTokens, rep.Cases[i].OutputTokens = 0, 0
	}
	if got := costSection(rep, 1.00, 0.10, 5.00); got != "" {
		t.Errorf("expected no cost section without token data, got:\n%s", got)
	}
}

// TestCostSectionExcludesMixedRecallCases: RecallShortCircuit is a COUNT of
// repeats that short-circuited, not a boolean. A case whose repeats disagree
// (e.g. 1 of 5 recalled) belongs to neither row — its median token figure would
// blend recall and full-loop paths — so it must be excluded from both the
// "full investigation" and "instant recall" buckets entirely.
func TestCostSectionExcludesMixedRecallCases(t *testing.T) {
	rep := costReport()
	rep.Cases = append(rep.Cases, ReportCase{
		Name: "flaky-recall", Runs: 5, Reached: true, HasRecall: true,
		RecallShortCircuit: 2, InputTokens: 50000, OutputTokens: 1800,
	})
	got := costSection(rep, 1.00, 0.10, 5.00)
	if strings.Contains(got, "flaky-recall") {
		t.Errorf("mixed-recall case name leaked into the table:\n%s", got)
	}
	// The full/recall row counts must still reflect only the two unanimous cases.
	if !strings.Contains(got, "| full investigation | 1 |") {
		t.Errorf("full investigation row should count 1 case, not the mixed one:\n%s", got)
	}
	if !strings.Contains(got, "| instant recall | 1 |") {
		t.Errorf("instant recall row should count 1 case, not the mixed one:\n%s", got)
	}
}

// TestCostSectionDisclosesExcludedMixedCases: dropping a mixed case silently
// would understate full-loop cost in the exact table meant to be trustworthy —
// the disclosure must name the count and the reason.
func TestCostSectionDisclosesExcludedMixedCases(t *testing.T) {
	rep := costReport()
	rep.Cases = append(rep.Cases,
		ReportCase{Name: "flaky-recall-1", Runs: 5, Reached: true, HasRecall: true,
			RecallShortCircuit: 2, InputTokens: 50000, OutputTokens: 1800},
		ReportCase{Name: "flaky-recall-2", Runs: 4, Reached: true, HasRecall: true,
			RecallShortCircuit: 1, InputTokens: 40000, OutputTokens: 1500},
	)
	got := costSection(rep, 1.00, 0.10, 5.00)
	if !strings.Contains(got, "2 case(s)") {
		t.Errorf("disclosure must name the excluded count (2):\n%s", got)
	}
	if !strings.Contains(got, "disagreed about recall") {
		t.Errorf("disclosure must state the reason for exclusion:\n%s", got)
	}
}

// TestCostSectionNoDisclosureWithoutExclusions: the disclosure must not appear
// when every case unanimously landed in one bucket — nothing was excluded.
func TestCostSectionNoDisclosureWithoutExclusions(t *testing.T) {
	got := costSection(costReport(), 1.00, 0.10, 5.00)
	if strings.Contains(got, "excluded from this table") {
		t.Errorf("disclosure should not appear when nothing was excluded:\n%s", got)
	}
}

// TestScorecardIncludesCostSection wires it end to end through the renderer.
//
// ScorecardMarkdown takes the three rates as explicit floats (not read off rep) so
// internal/eval never depends on internal/config; costReport() itself carries no
// InputUSDPerMTok/OutputUSDPerMTok, so the rates below are the only source of price.
func TestScorecardIncludesCostSection(t *testing.T) {
	rep := costReport()
	c := 0.5
	rep.CostUSD = &c
	md := ScorecardMarkdown(rep, nil, 1.00, 0.10, 5.00)
	if !strings.Contains(md, "Cost per investigation") {
		t.Errorf("scorecard missing the cost section:\n%s", md)
	}
}
