package eval

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleResults() []LiveResult {
	return []LiveResult{
		{Scenario: "gitops-bad-image-tag", CoverageRatio: 1.0,
			DimMedian:   map[string]int{"root_cause": 3, "evidence": 2, "solution": 2, "description": 2, "calibration": 2},
			DimVariance: map[string]float64{"root_cause": 0}, Pass: true},
		{Scenario: "harbor-natural", Skipped: true, SkipReason: "precondition absent"},
		{Scenario: "saturation-mem", CoverageRatio: 0.5,
			DimMedian: map[string]int{"root_cause": 1}, ToolErrors: []string{"query_metrics"}, Pass: false},
	}
}

func TestReportJSONAndMarkdown(t *testing.T) {
	rep := NewLiveReport("2026-06-21T20:00:00Z", sampleResults())

	js := rep.JSON()
	var back LiveReport
	if err := json.Unmarshal(js, &back); err != nil {
		t.Fatalf("json roundtrip: %v", err)
	}
	if back.Passed != 1 || back.Ran != 2 || back.Skipped != 1 {
		t.Fatalf("counts: passed=%d ran=%d skipped=%d", back.Passed, back.Ran, back.Skipped)
	}

	md := rep.Markdown()
	for _, want := range []string{"gitops-bad-image-tag", "SKIP", "harbor-natural", "query_metrics", "1/2"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestRegressionDiff(t *testing.T) {
	prev := NewLiveReport("t0", []LiveResult{{Scenario: "a", Pass: true}, {Scenario: "b", Pass: true}})
	curr := NewLiveReport("t1", []LiveResult{{Scenario: "a", Pass: true}, {Scenario: "b", Pass: false}})
	regressed := curr.RegressionsVS(prev)
	if len(regressed) != 1 || regressed[0] != "b" {
		t.Fatalf("want [b] regressed, got %v", regressed)
	}
}
