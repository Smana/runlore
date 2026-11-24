// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestCompareReportZeroGroundTruthOmitsVacuousColumns pins #394: when zero cases in a
// comparison suite carry ground_truth, the markdown report must omit the rubric,
// coverage, and confident-wrong columns completely (instead of printing 100% / 0 / —)
// and emit a footnote explaining why. JSON must report judged_cases=0, coverage_cases=0,
// and nil coverage_median so downstream consumers never mistake unmeasured for 0.
func TestCompareReportZeroGroundTruthOmitsVacuousColumns(t *testing.T) {
	entry := ModelEntry{Name: "haiku", Provider: "anthropic", Model: "claude-haiku"}
	cases := []ComparedCase{
		{
			Name: "case-a",
			Runs: []ComparedRun{
				{Result: Result{Name: "case-a", Pass: true}},
				{Result: Result{Name: "case-a", Pass: true}},
			},
		},
		{
			Name: "case-b",
			Runs: []ComparedRun{
				{Result: Result{Name: "case-b", Pass: false}},
				{Result: Result{Name: "case-b", Pass: false}},
			},
		},
	}
	mc := AggregateModel(entry, cases, providers.Usage{InputTokens: 1000, OutputTokens: 200})
	if mc.JudgedCases != 0 || mc.CoverageCases != 0 {
		t.Fatalf("want judged_cases=0 coverage_cases=0, got judged=%d coverage=%d", mc.JudgedCases, mc.CoverageCases)
	}
	if mc.CoverageMedian != nil {
		t.Fatalf("unmeasured suite must have nil CoverageMedian, got %v", *mc.CoverageMedian)
	}

	rep := NewComparisonReport("2026-11-24T00:00:00Z", 2, "anthropic/claude-judge", []ModelComparison{mc})
	md := rep.Markdown()
	summarySection, _, _ := strings.Cut(md, "## Per-case pass rate")
	for _, forbidden := range []string{"| root_cause |", "| coverage |", "| confident-wrong |", "100%"} {
		if strings.Contains(summarySection, forbidden) {
			t.Errorf("zero-ground-truth summary must omit vacuous column/value %q:\n%s", forbidden, summarySection)
		}
	}
	if !strings.Contains(md, "Judge & source-coverage columns omitted (no cases carry ground_truth).") {
		t.Errorf("zero-ground-truth report missing explanatory footnote:\n%s", md)
	}

	raw, err := rep.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	jsonStr := string(raw)
	if !strings.Contains(jsonStr, `"judged_cases": 0`) || !strings.Contains(jsonStr, `"coverage_cases": 0`) {
		t.Errorf("JSON must expose judged_cases=0 and coverage_cases=0:\n%s", jsonStr)
	}
	if strings.Contains(jsonStr, `"coverage_median"`) {
		t.Errorf("JSON must omit nil coverage_median when unmeasured:\n%s", jsonStr)
	}
}

// TestCompareReportMixedGroundTruthShowsDenominators pins #394 for mixed suites:
// when only k of N cases carry ground_truth, cells show the explicit (k/N) denominator,
// and a model with 0 judged/coverage cases renders "—" rather than "0.0%" or "0".
func TestCompareReportMixedGroundTruthShowsDenominators(t *testing.T) {
	entryA := ModelEntry{Name: "model-a", Provider: "openai", Model: "gpt-a"}
	casesA := []ComparedCase{
		{
			Name: "with-gt-1",
			Runs: []ComparedRun{{
				Result:      Result{Name: "with-gt-1", Pass: true},
				HasCoverage: true,
				Coverage:    Coverage{Ratio: 1.0},
				Graded:      true,
				Verdict: Verdict{
					Scores: map[string]int{
						"root_cause": 3, "evidence": 2, "solution": 2, "description": 2, "calibration": 2,
					},
				},
			}},
		},
		{
			Name: "with-gt-2",
			Runs: []ComparedRun{{
				Result:      Result{Name: "with-gt-2", Pass: true},
				HasCoverage: true,
				Coverage:    Coverage{Ratio: 0.5},
				Graded:      true,
				Verdict: Verdict{
					Scores: map[string]int{
						"root_cause": 2, "evidence": 2, "solution": 2, "description": 2, "calibration": 1,
					},
				},
			}},
		},
		{Name: "no-gt-1", Runs: []ComparedRun{{Result: Result{Name: "no-gt-1", Pass: true}}}},
		{Name: "no-gt-2", Runs: []ComparedRun{{Result: Result{Name: "no-gt-2", Pass: true}}}},
		{Name: "no-gt-3", Runs: []ComparedRun{{Result: Result{Name: "no-gt-3", Pass: false}}}},
	}
	mcA := AggregateModel(entryA, casesA, providers.Usage{InputTokens: 500, OutputTokens: 100})
	if mcA.JudgedCases != 2 || mcA.CoverageCases != 2 || mcA.Total != 5 {
		t.Fatalf("model-a denominators: judged=%d coverage=%d total=%d", mcA.JudgedCases, mcA.CoverageCases, mcA.Total)
	}
	if mcA.CoverageMedian == nil || *mcA.CoverageMedian != 0.75 {
		t.Fatalf("model-a coverage median must average only the 2 measured cases (0.75), got %v", mcA.CoverageMedian)
	}

	entryB := ModelEntry{Name: "model-b", Provider: "openai", Model: "gpt-b"}
	casesB := []ComparedCase{
		{Name: "no-gt-1", Runs: []ComparedRun{{Result: Result{Name: "no-gt-1", Pass: true}}}},
	}
	mcB := AggregateModel(entryB, casesB, providers.Usage{InputTokens: 200, OutputTokens: 50})

	rep := NewComparisonReport("2026-11-24T00:00:00Z", 1, "anthropic/claude-judge", []ModelComparison{mcA, mcB})
	md := rep.Markdown()

	var rowA, rowB string
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| model-a |") {
			rowA = line
		}
		if strings.HasPrefix(line, "| model-b |") {
			rowB = line
		}
	}
	if !strings.Contains(rowA, "75% (2/5)") {
		t.Errorf("mixed suite model-a coverage cell must show denominator 75%% (2/5): %q", rowA)
	}
	if !strings.Contains(rowA, "2.5/3 (2/5)") {
		t.Errorf("mixed suite model-a rubric cell must show denominator 2.5/3 (2/5): %q", rowA)
	}
	// model-b has 0 judged runs, 0 coverage cases, and nil grounding: rubric, coverage, grounding, confident-wrong must all be "—".
	dashCells := 0
	for _, cell := range strings.Split(rowB, "|") {
		if strings.TrimSpace(cell) == "—" {
			dashCells++
		}
	}
	if dashCells != len(Rubric)+3 {
		t.Errorf("model-b unmeasured cells (rubric + coverage + grounding + confident-wrong) must be %d em-dashes, got %d: %q", len(Rubric)+3, dashCells, rowB)
	}
}

// TestComparisonRunnerLeavesHasCoverageFalseWithoutGroundTruth verifies that runOnce
// only marks HasCoverage=true when the case declares ground_truth.expected_sources.
func TestComparisonRunnerLeavesHasCoverageFalseWithoutGroundTruth(t *testing.T) {
	model := &seqModel{resp: []providers.CompletionResponse{
		findings("harbor-db migration lock"),
	}}
	cr := &ComparisonRunner{Model: model, Log: discardLog()}
	c := Case{
		Name:     "no-gt",
		Prompt:   "harbor-core 503",
		Tools:    map[string]string{"pod_status": "harbor-db-0 Running"},
		Expected: Expected{MustContain: []string{"migration"}},
	}
	got := cr.RunCases(context.Background(), []Case{c}, 1)
	if len(got) != 1 || len(got[0].Runs) != 1 {
		t.Fatalf("unexpected runs: %+v", got)
	}
	if got[0].Runs[0].HasCoverage {
		t.Fatalf("case without ground_truth must have HasCoverage=false, got true")
	}
}
