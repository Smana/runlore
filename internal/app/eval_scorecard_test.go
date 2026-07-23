// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const scorecardReportFixture = `{
  "at": "2026-07-23T06:00:00Z",
  "model": "anthropic/claude-haiku-4-5-20251001",
  "n": 5, "pass_rate": 0.5, "reached": 1, "total": 2,
  "input_tokens": 120000, "output_tokens": 9000, "cost_usd": 0.16,
  "cases": [
    {"name": "harbor-chart-bump", "runs": 5, "pass_rate": 1, "reached": true, "flaky": false, "confidence": 0.82},
    {"name": "poisoned-recall-verify", "runs": 5, "pass_rate": 0.4, "reached": false, "flaky": true, "confidence": 0.75,
     "has_recall": true, "expect_recall": "withdrawn", "recall_fired_runs": 5, "recall_short_circuit_runs": 0,
     "missing": ["expect_recall=withdrawn but recall short_circuit"]}
  ]
}`

func TestRunEvalScorecard(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "2026-07-23T06-00-00Z-replay.json")
	if err := os.WriteFile(report, []byte(scorecardReportFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "scorecard")
	if err := RunEvalScorecard([]string{"-report", report, "-dir", out}); err != nil {
		t.Fatalf("RunEvalScorecard: %v", err)
	}
	// Re-running on the same report must be idempotent (same At ⇒ 1 history line).
	if err := RunEvalScorecard([]string{"-report", report, "-dir", out}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	md, err := os.ReadFile(filepath.Join(out, "scorecard.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**1/2 scenarios reached (50%)**", "⚠️ FLAKY", "expect: withdrawn"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("scorecard.md missing %q", want)
		}
	}
	badge, err := os.ReadFile(filepath.Join(out, "badge.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(badge), `"message":"1/2 scenarios · 50%"`) {
		t.Fatalf("badge.json wrong: %s", badge)
	}
	hist, err := os.ReadFile(filepath.Join(out, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(hist)), "\n") + 1; lines != 1 {
		t.Fatalf("want 1 history line after idempotent re-run, got %d:\n%s", lines, hist)
	}
}

func TestRunEvalScorecardRejectsMissingReport(t *testing.T) {
	if err := RunEvalScorecard([]string{"-dir", t.TempDir()}); err == nil {
		t.Fatal("want error when -report is missing")
	}
}
