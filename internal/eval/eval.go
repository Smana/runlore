package eval

import (
	"context"
	"log/slog"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// Runner replays cases through the investigation loop with a given model.
type Runner struct {
	Model providers.ModelProvider
	Log   *slog.Logger
}

// Report aggregates case results.
type Report struct {
	Results []Result
}

// Passed counts cases whose root cause was identified.
func (r Report) Passed() int {
	n := 0
	for _, res := range r.Results {
		if res.Pass {
			n++
		}
	}
	return n
}

// RCARate is the fraction of cases whose root cause was identified.
func (r Report) RCARate() float64 {
	if len(r.Results) == 0 {
		return 0
	}
	return float64(r.Passed()) / float64(len(r.Results))
}

// Run replays every case and scores it.
func (r *Runner) Run(ctx context.Context, cases []Case) Report {
	var rep Report
	for _, c := range cases {
		rep.Results = append(rep.Results, r.runOne(ctx, c))
	}
	return rep
}

func (r *Runner) runOne(ctx context.Context, c Case) Result {
	tools := make([]investigate.Tool, 0, len(c.Tools))
	for name, output := range c.Tools {
		tools = append(tools, staticTool{name: name, output: output})
	}
	var got providers.Investigation
	done := false
	li := &investigate.LoopInvestigator{
		Model: r.Model,
		Tools: tools,
		Log:   r.Log,
		OnComplete: func(inv providers.Investigation) {
			got, done = inv, true
		},
	}
	req := investigate.Request{Source: investigate.SourceAlert, Title: c.Name, Message: c.Prompt}
	if err := li.Investigate(ctx, req); err != nil {
		return Result{Name: c.Name, Missing: []string{"investigation error: " + err.Error()}}
	}
	if !done {
		return Result{Name: c.Name, Missing: []string{"no findings (loop did not submit)"}}
	}
	return Score(c.Name, got, c.Expected)
}

// staticTool replays a case's recorded evidence to the model, regardless of args.
type staticTool struct {
	name, output string
}

func (t staticTool) Name() string                                 { return t.name }
func (t staticTool) Description() string                          { return "Returns recorded evidence for: " + t.name }
func (t staticTool) Schema() string                               { return `{"type":"object","properties":{}}` }
func (t staticTool) Call(context.Context, string) (string, error) { return t.output, nil }
