package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compare.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadCompareSpec(t *testing.T) {
	path := writeSpec(t, `
judge:
  provider: anthropic
  model: claude-judge
  api_key_env: JUDGE_KEY
models:
  - name: haiku
    provider: anthropic
    model: claude-haiku
    api_key_env: KEY_A
    prices: {input_usd: 1, output_usd: 5}
  - name: local-qwen
    provider: openai
    base_url: http://localhost:8000/v1
    model: qwen3
    effort: low
`)
	spec, err := LoadCompareSpec(path)
	if err != nil {
		t.Fatalf("LoadCompareSpec: %v", err)
	}
	if spec.Judge == nil || spec.Judge.Model != "claude-judge" || spec.Judge.Provider != "anthropic" {
		t.Fatalf("judge not parsed: %+v", spec.Judge)
	}
	if len(spec.Models) != 2 {
		t.Fatalf("want 2 models, got %+v", spec.Models)
	}
	if spec.Models[0].Name != "haiku" || spec.Models[0].Prices == nil || spec.Models[0].Prices.OutputUSD != 5 {
		t.Fatalf("first entry not parsed: %+v", spec.Models[0])
	}
	if spec.Models[1].Effort != "low" || spec.Models[1].Prices != nil {
		t.Fatalf("second entry not parsed: %+v", spec.Models[1])
	}
}

func TestLoadCompareSpecRejectsBadSpecs(t *testing.T) {
	tests := []struct {
		name, body, wantErr string
	}{
		{"no models", `models: []`, "at least one"},
		{"missing name", `models: [{model: m}]`, "name"},
		{"missing model", `models: [{name: a}]`, "model"},
		{"duplicate names", `models: [{name: a, model: m1}, {name: a, model: m2}]`, "duplicate"},
		{"effort on gemini", `models: [{name: a, provider: gemini, model: m, effort: high}]`, "effort is not supported for provider gemini"},
		{"bad effort level", `models: [{name: a, provider: openai, model: m, effort: turbo}]`, "not a valid effort"},
		{"unknown key", `models: [{name: a, model: m, pricess: {}}]`, "field pricess not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadCompareSpec(writeSpec(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadCompareSpecEffortAccepted(t *testing.T) {
	// Effort is valid on the OpenAI-compatible default and on anthropic (different
	// vocabularies), matching config's per-provider effort validation.
	for _, body := range []string{
		`models: [{name: a, model: m, effort: high}]`,                      // empty provider ⇒ openai-compatible
		`models: [{name: a, provider: anthropic, model: m, effort: max}]`,  // anthropic-only level
		`models: [{name: a, provider: openai, model: m, effort: minimal}]`, // openai-only level
	} {
		if _, err := LoadCompareSpec(writeSpec(t, body)); err != nil {
			t.Fatalf("effort should be accepted for %q: %v", body, err)
		}
	}
}

// comparedCase builds a ComparedCase from compact per-run tuples.
type runTuple struct {
	pass           bool
	coverage       float64
	rootCause      int
	graded         bool
	confidentWrong bool
}

func comparedCase(name string, runs ...runTuple) ComparedCase {
	cc := ComparedCase{Name: name}
	for _, r := range runs {
		cr := ComparedRun{
			Result:   Result{Name: name, Pass: r.pass},
			Coverage: Coverage{Ratio: r.coverage},
			Graded:   r.graded,
		}
		if r.graded {
			cr.Verdict = Verdict{
				Scores: map[string]int{
					"root_cause": r.rootCause, "evidence": 2, "solution": 2,
					"description": 2, "calibration": 1,
				},
				ConfidentWrong: r.confidentWrong,
			}
		}
		cc.Runs = append(cc.Runs, cr)
	}
	return cc
}

func TestAggregateModel(t *testing.T) {
	entry := ModelEntry{Name: "m", Provider: "openai", Model: "gpt-x",
		Prices: &Prices{InputUSD: 2, OutputUSD: 10}}
	cases := []ComparedCase{
		// zeta before alpha: case rows must come out sorted by name.
		comparedCase("zeta",
			runTuple{pass: true, coverage: 1.0, rootCause: 3, graded: true},
			runTuple{pass: true, coverage: 0.5, rootCause: 2, graded: true},
			runTuple{pass: false, coverage: 1.0, rootCause: 1, graded: true, confidentWrong: true},
		),
		comparedCase("alpha",
			runTuple{pass: true, coverage: 1.0, rootCause: 3, graded: true},
			runTuple{pass: true, coverage: 1.0, rootCause: 3, graded: true},
			runTuple{pass: true, coverage: 1.0, rootCause: 3, graded: true},
		),
	}
	mc := AggregateModel(entry, cases, providers.Usage{InputTokens: 1_000_000, OutputTokens: 100_000})

	if mc.Name != "m" || mc.Model != "gpt-x" {
		t.Fatalf("identity: %+v", mc)
	}
	// alpha 3/3 reached; zeta 2/3 (0.67 < 0.7) not reached → pass rate 0.5.
	if mc.Reached != 1 || mc.PassRate != 0.5 {
		t.Fatalf("want reached=1 pass-rate=0.5, got reached=%d rate=%.2f", mc.Reached, mc.PassRate)
	}
	if len(mc.Cases) != 2 || mc.Cases[0].Name != "alpha" || mc.Cases[1].Name != "zeta" {
		t.Fatalf("case rows must be sorted by name: %+v", mc.Cases)
	}
	if !mc.Cases[0].Reached || mc.Cases[1].Reached {
		t.Fatalf("per-case reached wrong: %+v", mc.Cases)
	}
	// root_cause over all graded runs: 3,2,1,3,3,3 → median 3.
	if mc.RubricMedian["root_cause"] != 3 {
		t.Fatalf("root_cause median: %+v", mc.RubricMedian)
	}
	// calibration is 1 in every graded run.
	if mc.RubricMedian["calibration"] != 1 {
		t.Fatalf("calibration median: %+v", mc.RubricMedian)
	}
	if mc.GradedRuns != 6 {
		t.Fatalf("graded runs: %d", mc.GradedRuns)
	}
	// coverage over all runs: 1,0.5,1,1,1,1 → median 1.
	if mc.CoverageMedian != 1.0 {
		t.Fatalf("coverage median: %v", mc.CoverageMedian)
	}
	if mc.ConfidentWrong != 1 {
		t.Fatalf("confident-wrong count: %d", mc.ConfidentWrong)
	}
	if mc.InputTokens != 1_000_000 || mc.OutputTokens != 100_000 {
		t.Fatalf("tokens: %+v", mc)
	}
	// cost: 1 MTok in × $2 + 0.1 MTok out × $10 = $3.
	if mc.CostUSD == nil || *mc.CostUSD != 3.0 {
		t.Fatalf("cost: %v", mc.CostUSD)
	}
}

func TestAggregateModelNoPricesNoJudge(t *testing.T) {
	cases := []ComparedCase{comparedCase("only",
		runTuple{pass: true, coverage: 1.0},
		runTuple{pass: true, coverage: 1.0},
	)}
	mc := AggregateModel(ModelEntry{Name: "m", Model: "x"}, cases, providers.Usage{})
	if mc.CostUSD != nil {
		t.Fatalf("no prices ⇒ no cost, got %v", mc.CostUSD)
	}
	if mc.RubricMedian != nil || mc.GradedRuns != 0 {
		t.Fatalf("no graded runs ⇒ no rubric medians, got %+v", mc.RubricMedian)
	}
	if mc.PassRate != 1.0 || !mc.Cases[0].Reached {
		t.Fatalf("pass aggregation: %+v", mc)
	}
}

func TestAggregateModelFlakyBand(t *testing.T) {
	// 1/2 pass → rate 0.5, inside (0.3, 0.7) → flaky, not reached.
	cases := []ComparedCase{comparedCase("f",
		runTuple{pass: true, coverage: 1.0},
		runTuple{pass: false, coverage: 1.0},
	)}
	mc := AggregateModel(ModelEntry{Name: "m", Model: "x"}, cases, providers.Usage{})
	if !mc.Cases[0].Flaky || mc.Cases[0].Reached {
		t.Fatalf("0.5 should be flaky and not reached: %+v", mc.Cases[0])
	}
}

func costPtr(v float64) *float64 { return &v }

func comparisonFixture() ComparisonReport {
	return NewComparisonReport("2026-07-02T00:00:00Z", 3, "anthropic/claude-judge", []ModelComparison{
		{
			Name: "haiku", Provider: "anthropic", Model: "claude-haiku",
			PassRate: 1.0, Reached: 2,
			Cases: []CompareCaseRow{
				{Name: "alpha", Runs: 3, PassRate: 1, Reached: true, Coverage: 1},
				{Name: "zeta", Runs: 3, PassRate: 1, Reached: true, Coverage: 1},
			},
			RubricMedian: map[string]float64{
				"root_cause": 3, "evidence": 2.5, "solution": 2, "description": 2, "calibration": 2,
			},
			GradedRuns: 6, CoverageMedian: 1.0, ConfidentWrong: 0,
			InputTokens: 500_000, OutputTokens: 40_000, CostUSD: costPtr(0.9),
		},
		{
			Name: "local-qwen", Provider: "openai", Model: "qwen3", Effort: "low",
			PassRate: 0.5, Reached: 1,
			Cases: []CompareCaseRow{
				{Name: "alpha", Runs: 3, PassRate: 1, Reached: true, Coverage: 1},
				{Name: "zeta", Runs: 3, PassRate: 0.33, Reached: false, Flaky: true, Coverage: 0.5},
			},
			GradedRuns: 0, CoverageMedian: 0.75, ConfidentWrong: 2,
			InputTokens: 800_000, OutputTokens: 90_000,
		},
	})
}

func TestComparisonMarkdown(t *testing.T) {
	md := comparisonFixture().Markdown()

	for _, want := range []string{
		"# RunLore model comparison — 2026-07-02T00:00:00Z",
		"anthropic/claude-judge", // judge disclosure
		"N=3",
		"| haiku |",
		"| local-qwen |",
		"est. cost (USD)",
		"| alpha |",
		"| zeta |",
		"flaky",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n%s", want, md)
		}
	}
	// Spec order preserved: haiku row before local-qwen row.
	if strings.Index(md, "| haiku |") > strings.Index(md, "| local-qwen |") {
		t.Fatalf("models must render in spec order:\n%s", md)
	}
	// Ungraded model renders rubric cells as em-dashes, not zeros.
	qwenRow := ""
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| local-qwen |") {
			qwenRow = line
			break
		}
	}
	if !strings.Contains(qwenRow, "—") {
		t.Fatalf("ungraded rubric cells should be —: %q", qwenRow)
	}
}

func TestComparisonMarkdownOmitsCostColumnWithoutPrices(t *testing.T) {
	rep := comparisonFixture()
	rep.Models[0].CostUSD = nil
	md := rep.Markdown()
	if strings.Contains(md, "est. cost") {
		t.Fatalf("cost column must be omitted when no entry has prices:\n%s", md)
	}
}

func TestComparisonJSONRoundTrip(t *testing.T) {
	rep := comparisonFixture()
	b, err := rep.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	got, err := ParseComparisonReport(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.At != rep.At || got.N != 3 || got.Judge != "anthropic/claude-judge" || len(got.Models) != 2 {
		t.Fatalf("round trip header: %+v", got)
	}
	if got.Models[0].CostUSD == nil || *got.Models[0].CostUSD != 0.9 {
		t.Fatalf("cost lost in round trip: %+v", got.Models[0])
	}
	if got.Models[1].CostUSD != nil {
		t.Fatalf("absent cost must stay absent: %+v", got.Models[1])
	}
	if got.Models[0].RubricMedian["evidence"] != 2.5 {
		t.Fatalf("rubric lost in round trip: %+v", got.Models[0].RubricMedian)
	}
}
