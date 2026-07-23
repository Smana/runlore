// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"strings"
	"testing"
)

func scorecardFixtureReport() Report {
	cost := 0.16
	return Report{
		At: "2026-07-23T06:00:00Z", Model: "anthropic/claude-haiku-4-5-20251001",
		N: 5, PassRate: 0.5, Reached: 1, Total: 2,
		InputTokens: 120000, OutputTokens: 9000, CostUSD: &cost,
		Cases: []ReportCase{
			{Name: "harbor-chart-bump", Runs: 5, PassRate: 1, Reached: true, Confidence: 0.82},
			{Name: "poisoned-recall-verify", Runs: 5, PassRate: 0.4, Flaky: true, Confidence: 0.75,
				HasRecall: true, ExpectRecall: "withdrawn", RecallFired: 5,
				Missing: []string{"expect_recall=withdrawn but recall short_circuit"}},
		},
	}
}

func TestBadgeJSON(t *testing.T) {
	b := string(BadgeJSON(scorecardFixtureReport()))
	if !strings.Contains(b, `"schemaVersion":1`) {
		t.Fatalf("not a shields endpoint doc: %s", b)
	}
	if !strings.Contains(b, `"message":"1/2 scenarios · 50%"`) {
		t.Fatalf("badge message wrong: %s", b)
	}
	if !strings.Contains(b, `"color":"yellow"`) { // 0.5 is in [0.5, 0.7) ⇒ yellow
		t.Fatalf("badge color wrong: %s", b)
	}
	green := scorecardFixtureReport()
	green.PassRate, green.Reached = 1.0, 2
	if g := string(BadgeJSON(green)); !strings.Contains(g, `"color":"brightgreen"`) {
		t.Fatalf("1.0 should be brightgreen: %s", g)
	}
}

func TestAppendHistoryDedupesAndCaps(t *testing.T) {
	e := HistoryFromReport(scorecardFixtureReport())
	out, entries, err := AppendHistory(nil, e)
	if err != nil || len(entries) != 1 {
		t.Fatalf("first append: %v / %d entries", err, len(entries))
	}
	// Re-appending the same run (same At) must be idempotent.
	out2, entries2, err := AppendHistory(out, e)
	if err != nil || len(entries2) != 1 || string(out2) != string(out) {
		t.Fatalf("dedupe on At failed: %v / %d entries", err, len(entries2))
	}
	// Cap: appending beyond maxHistory drops the oldest.
	long := out
	for i := 0; i < maxHistory+10; i++ {
		e.At = "2026-07-23T06:00:00Z" + strings.Repeat("x", i+1) // unique At per line
		long, entries, err = AppendHistory(long, e)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(entries) != maxHistory {
		t.Fatalf("want cap %d, got %d", maxHistory, len(entries))
	}
}

func TestScorecardMarkdown(t *testing.T) {
	rep := scorecardFixtureReport()
	_, entries, err := AppendHistory(nil, HistoryFromReport(rep))
	if err != nil {
		t.Fatal(err)
	}
	md := ScorecardMarkdown(rep, entries)
	for _, want := range []string{
		"# RunLore nightly eval scorecard",
		"lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7", // reproduce command
		"anthropic/claude-haiku-4-5-20251001",                                              // model disclosure
		"**1/2 scenarios reached (50%)**",
		"est. cost $0.16",
		"| harbor-chart-bump | ✅ PASS |",
		"| poisoned-recall-verify | ⚠️ FLAKY |",
		"fired 5/5 · short-circuit 0/5 (expect: withdrawn)", // recall outcome column
		"## Confidence calibration",
		"poisoned-recall-verify", // 0.75 ≥ 0.70 and not reached ⇒ confidently wrong
		"## History",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("scorecard missing %q in:\n%s", want, md)
		}
	}
}
