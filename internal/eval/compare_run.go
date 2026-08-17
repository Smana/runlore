// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"log/slog"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// CountingModel is providers.CountingModel. The wrapper started here — the loop only
// logs/meters each response's Usage, so something had to turn per-response usage into
// a per-benchmark total — but that need turned out to belong to every one-shot
// command (`lore kb import`, `lore validate-kb`), none of which can reasonably reach
// into the eval harness for it. The alias keeps this package's call sites and its
// exported name intact while the implementation lives beside the interface it
// decorates.
type CountingModel = providers.CountingModel

// UsageCounter is the "how many tokens so far" capability the runner needs to
// attribute spend per case. CountingModel implements it; a plain model does not, and
// the runner simply records zero usage in that case.
type UsageCounter interface {
	Total() providers.Usage
}

// compile-time assertion: the counting wrapper satisfies the capability.
var _ UsageCounter = (*CountingModel)(nil)

// ComparedRun is one replay run of one case for one model entry: the
// deterministic keyword score, the tool-call coverage, and (when the case
// carries ground truth and a judge is set) the judge's rubric verdict.
type ComparedRun struct {
	Result   Result
	Coverage Coverage
	Verdict  Verdict
	Graded   bool
}

// ComparedCase is all N runs of one case for one model entry.
type ComparedCase struct {
	Name string
	Runs []ComparedRun
}

// ComparisonRunner benchmarks one model entry over the replay cases. It mirrors
// the replay Runner (static tools, same loop) but additionally records tool
// calls for coverage and grades every run with a fixed judge, so entries can be
// compared on the full rubric — not only keyword pass/fail.
type ComparisonRunner struct {
	Model providers.ModelProvider // the entry under test (wrap with CountingModel for token totals)
	Judge Judge                   // fixed across entries; nil skips rubric grading
	Log   *slog.Logger
	// Spend is the per-investigation ceiling set handed to every benchmarked loop.
	// One set across all entries on purpose: an entry that is allowed to spend more
	// than its rivals is not being compared with them.
	Spend Spend
}

// RunCases replays every case n times against the entry's model. It stops early on
// a cancelled context — a campaign-wide halt (Ctrl-C, or CampaignBudget tripping)
// must not keep grinding every remaining case into the same failure.
func (cr *ComparisonRunner) RunCases(ctx context.Context, cases []Case, n int) []ComparedCase {
	if n < 1 {
		n = 1
	}
	out := make([]ComparedCase, 0, len(cases))
	for _, c := range cases {
		if ctx.Err() != nil {
			break
		}
		cc := ComparedCase{Name: c.Name}
		for i := 0; i < n; i++ {
			cc.Runs = append(cc.Runs, cr.runOnce(ctx, c))
		}
		out = append(out, cc)
	}
	return out
}

func (cr *ComparisonRunner) runOnce(ctx context.Context, c Case) ComparedRun {
	rec := &Recorder{}
	tools := make([]investigate.Tool, 0, len(c.Tools))
	for name, output := range c.Tools {
		tools = append(tools, staticTool{name: name, output: output})
	}
	var got providers.Investigation
	done := false
	li := &investigate.LoopInvestigator{
		Model: cr.Model,
		Tools: wrap(tools, rec),
		Log:   cr.Log,
		// Identical ceilings for every entry — see ComparisonRunner.Spend.
		MaxTokensPerInvestigation: cr.Spend.MaxTokensPerInvestigation,
		MaxCostPerInvestigation:   cr.Spend.MaxCostPerInvestigation,
		Pricing:                   cr.Spend.Pricing,
		VerifyPricing:             cr.Spend.VerifyPricing,
		OnComplete: func(inv providers.Investigation) {
			got, done = inv, true
		},
	}
	req := investigate.Request{Source: investigate.SourceAlert, Title: c.Name, Message: c.Prompt}
	if err := li.Investigate(ctx, req); err != nil {
		return ComparedRun{Result: Result{Name: c.Name, Missing: []string{noteInvestigationError + err.Error()}}}
	}
	if !done {
		return ComparedRun{Result: Result{Name: c.Name, Missing: []string{"no findings (loop did not submit)"}}}
	}

	run := ComparedRun{Result: Score(c.Name, got, c.Expected)}
	var expected, optional []string
	if c.GroundTruth != nil {
		expected, optional = c.GroundTruth.ExpectedSources, c.GroundTruth.OptionalSources
	}
	run.Coverage = ScoreCoverage(expected, optional, rec.Calls())
	if cr.Judge != nil && c.GroundTruth != nil {
		scn := Scenario{ID: c.Name, GroundTruth: *c.GroundTruth}
		v, err := cr.Judge.Grade(ctx, scn, got)
		if err != nil {
			cr.Log.Warn("judge error", "case", c.Name, "err", err)
		} else {
			run.Verdict, run.Graded = v, true
		}
	}
	return run
}
