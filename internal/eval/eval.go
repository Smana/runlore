package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

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

// CaseAggregate is the k-of-n verdict for one case over N replay repeats.
type CaseAggregate struct {
	Name        string
	Runs        int
	PassRate    float64  // fraction of repeats whose Result.Pass is true
	Reached     bool     // PassRate >= evalMinPassRate
	Flaky       bool     // PassRate in (1-evalMinPassRate, evalMinPassRate): runs disagree
	Confidence  float64  // median confidence over repeats
	Missing     []string // union of missing keywords/entities across repeats
	OverClaimed []string // union of over-claimed distractors across repeats
}

// Campaign is the aggregate of a multi-repeat replay run.
type Campaign struct {
	N          int
	Aggregates []CaseAggregate
}

// ReachedCases counts cases whose pass-rate met the k-of-n bar.
func (c Campaign) ReachedCases() int {
	n := 0
	for _, a := range c.Aggregates {
		if a.Reached {
			n++
		}
	}
	return n
}

// PassRate is the fraction of cases that reached RCA (0 for empty).
func (c Campaign) PassRate() float64 {
	if len(c.Aggregates) == 0 {
		return 0
	}
	return float64(c.ReachedCases()) / float64(len(c.Aggregates))
}

// FlakyNames lists cases whose repeats disagreed too much to trust.
func (c Campaign) FlakyNames() []string {
	var names []string
	for _, a := range c.Aggregates {
		if a.Flaky {
			names = append(names, a.Name)
		}
	}
	return names
}

// RunN replays every case n times and returns the aggregated campaign.
func (r *Runner) RunN(ctx context.Context, cases []Case, n int) Campaign {
	if n < 1 {
		n = 1
	}
	camp := Campaign{N: n}
	for _, c := range cases {
		camp.Aggregates = append(camp.Aggregates, r.aggregateCase(ctx, c, n))
	}
	return camp
}

func (r *Runner) aggregateCase(ctx context.Context, c Case, n int) CaseAggregate {
	confs := make([]float64, n)
	missSet := map[string]struct{}{}
	ocSet := map[string]struct{}{}
	passes := 0
	for i := 0; i < n; i++ {
		res := r.runOne(ctx, c)
		if res.Pass {
			passes++
		}
		confs[i] = res.Confidence
		for _, m := range res.Missing {
			missSet[m] = struct{}{}
		}
		for _, o := range res.OverClaimed {
			ocSet[o] = struct{}{}
		}
	}
	rate := float64(passes) / float64(n)
	return CaseAggregate{
		Name:        c.Name,
		Runs:        n,
		PassRate:    rate,
		Reached:     rate >= evalMinPassRate,
		Flaky:       rate > 1-evalMinPassRate && rate < evalMinPassRate,
		Confidence:  medianFloat(confs),
		Missing:     sortedSet(missSet),
		OverClaimed: sortedSet(ocSet),
	}
}

func sortedSet(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// GateError returns a non-nil error when the campaign pass-rate is below failUnder
// (which is only enforced when failUnder > 0). The message names the cases that did
// not reach RCA and any flaky cases, so CI logs explain the failure.
func GateError(c Campaign, failUnder float64) error {
	if failUnder <= 0 || c.PassRate() >= failUnder {
		return nil
	}
	var missed []string
	for _, a := range c.Aggregates {
		if !a.Reached {
			missed = append(missed, a.Name)
		}
	}
	msg := fmt.Sprintf("eval gate failed: pass-rate %.0f%% < threshold %.0f%% (reached %d/%d)",
		c.PassRate()*100, failUnder*100, c.ReachedCases(), len(c.Aggregates))
	if len(missed) > 0 {
		msg += "; missed: " + strings.Join(missed, ", ")
	}
	if fl := c.FlakyNames(); len(fl) > 0 {
		msg += "; flaky: " + strings.Join(fl, ", ")
	}
	return errors.New(msg)
}

// JSON renders the campaign as an indented report for CI artifacts.
func (c Campaign) JSON() ([]byte, error) {
	type row struct {
		Name        string   `json:"name"`
		Runs        int      `json:"runs"`
		PassRate    float64  `json:"pass_rate"`
		Reached     bool     `json:"reached"`
		Flaky       bool     `json:"flaky"`
		Confidence  float64  `json:"confidence"`
		Missing     []string `json:"missing,omitempty"`
		OverClaimed []string `json:"over_claimed,omitempty"`
	}
	rows := make([]row, len(c.Aggregates))
	for i, a := range c.Aggregates {
		rows[i] = row(a)
	}
	return json.MarshalIndent(struct {
		N        int     `json:"n"`
		PassRate float64 `json:"pass_rate"`
		Reached  int     `json:"reached"`
		Total    int     `json:"total"`
		Cases    []row   `json:"cases"`
	}{
		N:        c.N,
		PassRate: c.PassRate(),
		Reached:  c.ReachedCases(),
		Total:    len(c.Aggregates),
		Cases:    rows,
	}, "", "  ")
}
