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
	Name     string  `json:"name"`
	Runs     int     `json:"runs"`
	PassRate float64 `json:"pass_rate"`
	Reached  bool    `json:"reached"`
	Flaky    bool    `json:"flaky"`
	Coverage float64 `json:"coverage"` // median coverage ratio over the case's runs
}

// ModelComparison is one model entry's aggregated result across all cases.
type ModelComparison struct {
	Name     string `json:"name"` // the entry's report label
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`

	// Aggregate scores.
	PassRate       float64            `json:"pass_rate"`               // fraction of cases that reached the k-of-n bar
	Reached        int                `json:"reached"`                 // cases whose pass-rate met the k-of-n bar
	Total          int                `json:"total"`                   // cases run
	RubricMedian   map[string]float64 `json:"rubric_median,omitempty"` // per-dimension median over graded runs; nil when ungraded
	GradedRuns     int                `json:"graded_runs"`             // runs the judge graded (0 ⇒ rubric omitted)
	CoverageMedian float64            `json:"coverage_median"`         // median coverage ratio over all runs
	ConfidentWrong int                `json:"confident_wrong"`         // graded runs flagged confident-and-wrong

	// Usage + cost.
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	CostUSD      *float64 `json:"cost_usd,omitempty"` // present only when the entry supplied prices

	Cases []CompareCaseRow `json:"cases"`
}

// AggregateModel folds a model entry's per-case replay runs into one comparison
// row. Per-case pass uses the same k-of-n bar as the single-run campaign; rubric
// medians and confident-wrong are computed over every graded run; coverage is the
// median over every run; cost is computed only when the entry supplied prices.
// Case rows come out sorted by name so two reports diff cleanly.
func AggregateModel(entry ModelEntry, cases []ComparedCase, usage providers.Usage) ModelComparison {
	mc := ModelComparison{
		Name: entry.Name, Provider: entry.Provider, Model: entry.Model, Effort: entry.Effort,
		Total:        len(cases),
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
	}

	var allCoverage []float64
	perDim := map[string][]float64{}
	for _, cc := range cases {
		passes := 0
		var covs []float64
		for _, run := range cc.Runs {
			if run.Result.Pass {
				passes++
			}
			covs = append(covs, run.Coverage.Ratio)
			allCoverage = append(allCoverage, run.Coverage.Ratio)
			if run.Graded {
				mc.GradedRuns++
				if run.Verdict.ConfidentWrong {
					mc.ConfidentWrong++
				}
				for _, d := range Rubric {
					perDim[d.Key] = append(perDim[d.Key], float64(run.Verdict.Scores[d.Key]))
				}
			}
		}
		n := len(cc.Runs)
		rate := 0.0
		if n > 0 {
			rate = float64(passes) / float64(n)
		}
		row := CompareCaseRow{
			Name:     cc.Name,
			Runs:     n,
			PassRate: rate,
			Reached:  rate >= evalMinPassRate,
			Flaky:    rate > 1-evalMinPassRate && rate < evalMinPassRate,
			Coverage: medianFloat(covs),
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
	mc.CoverageMedian = medianFloat(allCoverage)
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

// Markdown renders the human comparison report: a per-model summary table (rubric
// breakdown, pass rate, coverage, confident-wrong, tokens, optional cost) followed
// by a per-case pass-rate matrix. Deterministic: models keep spec order, cases sort
// by name.
func (r ComparisonReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# RunLore model comparison — %s\n\n", r.At)
	fmt.Fprintf(&b, "N=%d runs/case · pass-rate bar ≥%.0f%% · judge: `%s` · blind grading (the judge never sees which model produced a result).\n\n",
		r.N, evalMinPassRate*100, r.Judge)

	showCost := r.anyPriced()
	header := []string{"model", "provider/model", "pass rate", "reached"}
	for _, d := range Rubric {
		header = append(header, d.Key)
	}
	header = append(header, "coverage", "confident-wrong", "in tok", "out tok")
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
		for _, d := range Rubric {
			if m.RubricMedian == nil {
				cells = append(cells, "—")
			} else {
				cells = append(cells, fmt.Sprintf("%.1f/%d", m.RubricMedian[d.Key], d.Max))
			}
		}
		cells = append(cells,
			fmt.Sprintf("%.0f%%", m.CoverageMedian*100),
			fmt.Sprintf("%d", m.ConfidentWrong),
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
