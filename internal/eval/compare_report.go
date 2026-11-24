// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// CompareCaseRow is one case's k-of-n verdict for one model, in the comparison.
type CompareCaseRow struct {
	Name        string   `json:"name"`
	Runs        int      `json:"runs"`
	PassRate    float64  `json:"pass_rate"`
	Reached     bool     `json:"reached"`
	Flaky       bool     `json:"flaky"`
	Coverage    float64  `json:"coverage"` // median coverage ratio over measured runs (0 when unmeasured)
	HasCoverage bool     `json:"has_coverage,omitempty"`
	Grounding   *float64 `json:"grounding,omitempty"` // median grounding ratio over measured runs; nil when unmeasured
}

// ModelComparison is one model entry's aggregated result across all cases.
type ModelComparison struct {
	Name     string `json:"name"` // the entry's report label
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`

	// Aggregate scores.
	PassRate        float64            `json:"pass_rate"`                  // fraction of cases that reached the k-of-n bar
	Reached         int                `json:"reached"`                    // cases whose pass-rate met the k-of-n bar
	Total           int                `json:"total"`                      // cases run
	RubricMedian    map[string]float64 `json:"rubric_median,omitempty"`    // per-dimension median over graded runs; nil when ungraded
	GradedRuns      int                `json:"graded_runs"`                // runs the judge graded (0 ⇒ rubric omitted)
	JudgedCases     int                `json:"judged_cases"`               // cases with at least one graded run
	CoverageCases   int                `json:"coverage_cases"`             // cases with ground_truth.expected_sources measured
	CoverageMedian  *float64           `json:"coverage_median,omitempty"`  // median coverage ratio over measured runs; nil when CoverageCases == 0
	GroundingMedian *float64           `json:"grounding_median,omitempty"` // median grounding ratio over measured runs; nil when unmeasured
	ConfidentWrong  int                `json:"confident_wrong"`            // graded runs flagged confident-and-wrong

	// Usage + cost.
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	CostUSD      *float64 `json:"cost_usd,omitempty"` // present only when the entry supplied prices

	Cases []CompareCaseRow `json:"cases"`
}

// AggregateModel folds a model entry's per-case replay runs into one comparison
// row. Per-case pass uses the same k-of-n bar as the single-run campaign; rubric
// medians and confident-wrong are computed over every graded run; coverage is the
// median over measured runs only (nil when no case declared expected_sources); cost
// is computed only when the entry supplied prices. Case rows come out sorted by name
// so two reports diff cleanly.
func AggregateModel(entry ModelEntry, cases []ComparedCase, usage providers.Usage) ModelComparison {
	mc := ModelComparison{
		Name: entry.Name, Provider: entry.Provider, Model: entry.Model, Effort: entry.Effort,
		Total:        len(cases),
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
	}

	var allCoverage []float64
	var allGrounding []float64
	perDim := map[string][]float64{}
	for _, cc := range cases {
		passes := 0
		var covs []float64
		var groundings []float64
		caseGraded := false
		caseCoverage := false
		for _, run := range cc.Runs {
			if run.Result.Pass {
				passes++
			}
			if run.HasCoverage {
				caseCoverage = true
				covs = append(covs, run.Coverage.Ratio)
				allCoverage = append(allCoverage, run.Coverage.Ratio)
			}
			if r, ok := run.Result.GroundingRatio(); ok {
				groundings = append(groundings, r)
				allGrounding = append(allGrounding, r)
			}
			if run.Graded {
				caseGraded = true
				mc.GradedRuns++
				if run.Verdict.ConfidentWrong {
					mc.ConfidentWrong++
				}
				for _, d := range Rubric {
					perDim[d.Key] = append(perDim[d.Key], float64(run.Verdict.Scores[d.Key]))
				}
			}
		}
		if caseGraded {
			mc.JudgedCases++
		}
		if caseCoverage {
			mc.CoverageCases++
		}
		n := len(cc.Runs)
		rate := 0.0
		if n > 0 {
			rate = float64(passes) / float64(n)
		}
		var rowGround *float64
		if len(groundings) > 0 {
			m := medianFloat(groundings)
			rowGround = &m
		}
		row := CompareCaseRow{
			Name:        cc.Name,
			Runs:        n,
			PassRate:    rate,
			Reached:     rate >= evalMinPassRate,
			Flaky:       rate > 1-evalMinPassRate && rate < evalMinPassRate,
			Coverage:    medianFloat(covs),
			HasCoverage: caseCoverage,
			Grounding:   rowGround,
		}
		if row.Reached {
			mc.Reached++
		}
		mc.Cases = append(mc.Cases, row)
	}
	sort.Slice(mc.Cases, func(i, j int) bool { return mc.Cases[i].Name < mc.Cases[j].Name })

	if mc.Total > 0 {
		mc.PassRate = float64(mc.Reached) / float64(mc.Total)
	}
	if len(allCoverage) > 0 {
		med := medianFloat(allCoverage)
		mc.CoverageMedian = &med
	}
	if len(allGrounding) > 0 {
		m := medianFloat(allGrounding)
		mc.GroundingMedian = &m
	}
	if mc.GradedRuns > 0 {
		mc.RubricMedian = map[string]float64{}
		for _, d := range Rubric {
			mc.RubricMedian[d.Key] = medianFloat(perDim[d.Key])
		}
	}
	if entry.Prices != nil {
		cost := float64(usage.InputTokens)/1e6*entry.Prices.InputUSD +
			float64(usage.OutputTokens)/1e6*entry.Prices.OutputUSD
		mc.CostUSD = &cost
	}
	return mc
}

// ComparisonReport is the aggregate of a multi-model benchmark run: one row per
// model entry, in spec order (comparisons should not reorder by score), plus the
// disclosure a published benchmark needs — N runs and the judge model identity.
type ComparisonReport struct {
	At     string            `json:"at"`
	N      int               `json:"n"`     // runs per case
	Judge  string            `json:"judge"` // judge model disclosure, e.g. "anthropic/claude-…"
	Models []ModelComparison `json:"models"`
}

// NewComparisonReport builds a report from already-aggregated model rows,
// preserving the given (spec) order.
func NewComparisonReport(at string, n int, judge string, models []ModelComparison) ComparisonReport {
	return ComparisonReport{At: at, N: n, Judge: judge, Models: models}
}

// JSON renders the deterministic machine-readable comparison report.
func (r ComparisonReport) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// ParseComparisonReport reads a report back from its JSON form (baseline diffs, tests).
func ParseComparisonReport(b []byte) (ComparisonReport, error) {
	var rep ComparisonReport
	if err := json.Unmarshal(b, &rep); err != nil {
		return ComparisonReport{}, fmt.Errorf("parse comparison report: %w", err)
	}
	return rep, nil
}

// anyPriced reports whether at least one model row carries an estimated cost, so
// the cost column is shown only when some entry supplied prices.
func (r ComparisonReport) anyPriced() bool {
	for _, m := range r.Models {
		if m.CostUSD != nil {
			return true
		}
	}
	return false
}

// anyJudged reports whether at least one model row has graded runs, so the rubric
// and confident-wrong columns are omitted when zero cases were graded (#394).
func (r ComparisonReport) anyJudged() bool {
	for _, m := range r.Models {
		if m.GradedRuns > 0 || m.JudgedCases > 0 || m.RubricMedian != nil {
			return true
		}
	}
	return false
}

// anyCoverage reports whether at least one model row has measured source coverage,
// so the coverage column is omitted when zero cases declared expected_sources (#394).
func (r ComparisonReport) anyCoverage() bool {
	for _, m := range r.Models {
		if m.CoverageCases > 0 || m.CoverageMedian != nil {
			return true
		}
	}
	return false
}

// Markdown renders the human comparison report: a per-model summary table (pass rate,
// optional rubric breakdown, optional coverage, grounding, optional confident-wrong,
// tokens, optional cost) followed by a per-case pass-rate matrix. Vacuous columns
// (judge or coverage when zero cases carry ground_truth) are omitted with an
// explanatory footnote (#394). Deterministic: models keep spec order, cases sort by name.
func (r ComparisonReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# RunLore model comparison — %s\n\n", r.At)
	fmt.Fprintf(&b, "N=%d runs/case · pass-rate bar ≥%.0f%% · judge: `%s` · blind grading (the judge never sees which model produced a result).\n\n",
		r.N, evalMinPassRate*100, r.Judge)

	showCost := r.anyPriced()
	showJudge := r.anyJudged()
	showCoverage := r.anyCoverage()

	header := []string{"model", "provider/model", "pass rate", "reached"}
	if showJudge {
		for _, d := range Rubric {
			header = append(header, d.Key)
		}
	}
	if showCoverage {
		header = append(header, "coverage")
	}
	header = append(header, "grounding")
	if showJudge {
		header = append(header, "confident-wrong")
	}
	header = append(header, "in tok", "out tok")
	if showCost {
		header = append(header, "est. cost (USD)")
	}
	b.WriteString("## Summary\n\n")
	b.WriteString("| " + strings.Join(header, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat("---|", len(header)) + "\n")

	for _, m := range r.Models {
		provModel := m.Model
		if m.Provider != "" {
			provModel = m.Provider + "/" + m.Model
		}
		if m.Effort != "" {
			provModel += " (effort=" + m.Effort + ")"
		}
		cells := []string{
			m.Name,
			provModel,
			fmt.Sprintf("%.0f%% (%d/%d)", m.PassRate*100, m.Reached, m.Total),
			fmt.Sprintf("%d/%d", m.Reached, m.Total),
		}
		if showJudge {
			for _, d := range Rubric {
				switch {
				case m.RubricMedian == nil || m.GradedRuns == 0:
					cells = append(cells, "—")
				case m.Total > 0 && m.JudgedCases > 0 && m.JudgedCases < m.Total:
					cells = append(cells, fmt.Sprintf("%.1f/%d (%d/%d)", m.RubricMedian[d.Key], d.Max, m.JudgedCases, m.Total))
				default:
					cells = append(cells, fmt.Sprintf("%.1f/%d", m.RubricMedian[d.Key], d.Max))
				}
			}
		}
		if showCoverage {
			switch {
			case m.CoverageMedian == nil:
				cells = append(cells, "—")
			case m.Total > 0 && m.CoverageCases > 0 && m.CoverageCases < m.Total:
				cells = append(cells, fmt.Sprintf("%.0f%% (%d/%d)", *m.CoverageMedian*100, m.CoverageCases, m.Total))
			default:
				cells = append(cells, fmt.Sprintf("%.0f%%", *m.CoverageMedian*100))
			}
		}
		groundingCell := "—"
		if m.GroundingMedian != nil {
			groundingCell = fmt.Sprintf("%.0f%%", *m.GroundingMedian*100)
		}
		cells = append(cells, groundingCell)
		if showJudge {
			if m.GradedRuns == 0 {
				cells = append(cells, "—")
			} else {
				cells = append(cells, fmt.Sprintf("%d", m.ConfidentWrong))
			}
		}
		cells = append(cells,
			fmt.Sprintf("%d", m.InputTokens),
			fmt.Sprintf("%d", m.OutputTokens),
		)
		if showCost {
			if m.CostUSD != nil {
				cells = append(cells, fmt.Sprintf("$%.2f", *m.CostUSD))
			} else {
				cells = append(cells, "—")
			}
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}

	switch {
	case !showJudge && !showCoverage:
		b.WriteString("\n_Judge & source-coverage columns omitted (no cases carry ground_truth)._\n")
	case !showJudge:
		b.WriteString("\n_Judge columns omitted (no cases were graded)._\n")
	case !showCoverage:
		b.WriteString("\n_Source-coverage column omitted (no cases declare ground_truth.expected_sources)._\n")
	}

	b.WriteString("\n## Per-case pass rate (k-of-n)\n\n")
	names := caseNames(r.Models)
	b.WriteString("| case | " + strings.Join(modelLabels(r.Models), " | ") + " |\n")
	b.WriteString("|---|" + strings.Repeat("---|", len(r.Models)) + "\n")
	for _, name := range names {
		row := []string{name}
		for _, m := range r.Models {
			row = append(row, caseCell(m, name))
		}
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	return b.String()
}

// caseNames returns the union of case names across all models, sorted.
func caseNames(models []ModelComparison) []string {
	set := map[string]bool{}
	for _, m := range models {
		for _, c := range m.Cases {
			set[c.Name] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func modelLabels(models []ModelComparison) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = m.Name
	}
	return out
}

// caseCell renders one model's verdict for one case: pass-rate, with a "flaky"
// or "—" (case not run for this model) annotation.
func caseCell(m ModelComparison, name string) string {
	for _, c := range m.Cases {
		if c.Name != name {
			continue
		}
		s := fmt.Sprintf("%.0f%%", c.PassRate*100)
		switch {
		case c.Reached:
			s += " ✓"
		case c.Flaky:
			s += " flaky"
		}
		return s
	}
	return "—"
}
