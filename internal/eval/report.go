package eval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// LiveReport is the serializable output of one live-fire campaign run.
type LiveReport struct {
	At      string       `json:"at"`
	N       int          `json:"n"` // runs per scenario (live mode); 0 in replay mode
	Ran     int          `json:"ran"` // scenarios actually investigated (not skipped)
	Passed  int          `json:"passed"`
	Skipped int          `json:"skipped"`
	Results []LiveResult `json:"results"`
}

// NewLiveReport tallies results into a report.
func NewLiveReport(at string, n int, results []LiveResult) LiveReport {
	rep := LiveReport{At: at, N: n, Results: results}
	for _, r := range results {
		if r.Skipped {
			rep.Skipped++
			continue
		}
		rep.Ran++
		if r.Pass {
			rep.Passed++
		}
	}
	return rep
}

// JSON is the machine-readable sibling of the markdown report.
func (rep LiveReport) JSON() []byte {
	b, _ := json.MarshalIndent(rep, "", "  ")
	return b
}

// allSources is the column order for the coverage heatmap.
var allSources = []string{"gitops", "kubernetes", "metrics", "logs", "network", "aws", "kb"}

// Markdown renders the human report: summary, per-scenario table, coverage heatmap.
func (rep LiveReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# RunLore eval report — %s\n\n", rep.At)
	fmt.Fprintf(&b, "**Passed %d/%d** ran (%d skipped) · N=%d runs/scenario, pass-rate ≥%.0f%%.\n\n",
		rep.Passed, rep.Ran, rep.Skipped, rep.N, evalMinPassRate*100)

	b.WriteString("## Scenarios\n\n")
	b.WriteString("| scenario | result | coverage | root_cause | tool errors |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, r := range rep.Results {
		if r.Skipped {
			fmt.Fprintf(&b, "| %s | SKIP | — | — | %s |\n", r.Scenario, r.SkipReason)
			continue
		}
		status := "FAIL"
		if r.Pass {
			status = "PASS"
		} else if r.Flaky {
			status = "FLAKY"
		}
		te := "—"
		if len(r.ToolErrors) > 0 {
			te = strings.Join(r.ToolErrors, ", ")
		}
		fmt.Fprintf(&b, "| %s | %s | %.0f%% | %d | %s |\n", r.Scenario, status, r.CoverageRatio*100, r.DimMedian["root_cause"], te)
	}

	b.WriteString("\n## Coverage heatmap (median touched per source)\n\n")
	b.WriteString("| scenario | " + strings.Join(allSources, " | ") + " |\n")
	b.WriteString("|---|" + strings.Repeat("---|", len(allSources)) + "\n")
	for _, r := range rep.Results {
		if r.Skipped {
			continue
		}
		row := make([]string, len(allSources))
		touched := map[string]bool{}
		if len(r.Runs) > 0 {
			for _, s := range r.Runs[0].Coverage.Touched {
				touched[s] = true
			}
			for _, s := range r.Runs[0].Coverage.Bonus {
				touched[s] = true
			}
		}
		for i, s := range allSources {
			if touched[s] {
				row[i] = "✓"
			} else {
				row[i] = " "
			}
		}
		fmt.Fprintf(&b, "| %s | %s |\n", r.Scenario, strings.Join(row, " | "))
	}
	return b.String()
}

// RegressionsVS returns scenarios that passed in prev but fail/skip now.
func (rep LiveReport) RegressionsVS(prev LiveReport) []string {
	was := map[string]bool{}
	for _, r := range prev.Results {
		was[r.Scenario] = r.Pass
	}
	var out []string
	for _, r := range rep.Results {
		if was[r.Scenario] && !r.Pass {
			out = append(out, r.Scenario)
		}
	}
	sort.Strings(out)
	return out
}
