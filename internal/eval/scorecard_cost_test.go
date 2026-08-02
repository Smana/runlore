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
