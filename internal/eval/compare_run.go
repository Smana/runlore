package eval

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// CountingModel wraps a ModelProvider and sums the provider-reported token usage
// across completions. The loop only logs/meters each response's Usage, so this
// wrapper is what turns per-response usage into a per-benchmark total.
type CountingModel struct {
	Inner providers.ModelProvider

	mu    sync.Mutex
	total providers.Usage
}

// Complete delegates to Inner and accumulates the response usage on success.
func (c *CountingModel) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	resp, err := c.Inner.Complete(ctx, req)
	if err == nil {
		c.mu.Lock()
		c.total.InputTokens += resp.Usage.InputTokens
		c.total.OutputTokens += resp.Usage.OutputTokens
		c.total.CachedInputTokens += resp.Usage.CachedInputTokens
		c.total.CacheWriteTokens += resp.Usage.CacheWriteTokens
		c.mu.Unlock()
	}
	return resp, err
}

// Total returns the usage accumulated so far.
func (c *CountingModel) Total() providers.Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

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
}

// RunCases replays every case n times against the entry's model.
func (cr *ComparisonRunner) RunCases(ctx context.Context, cases []Case, n int) []ComparedCase {
	if n < 1 {
		n = 1
	}
	out := make([]ComparedCase, 0, len(cases))
	for _, c := range cases {
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
		OnComplete: func(inv providers.Investigation) {
			got, done = inv, true
		},
	}
	req := investigate.Request{Source: investigate.SourceAlert, Title: c.Name, Message: c.Prompt}
	if err := li.Investigate(ctx, req); err != nil {
		return ComparedRun{Result: Result{Name: c.Name, Missing: []string{"investigation error: " + err.Error()}}}
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
