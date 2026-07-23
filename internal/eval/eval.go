// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// Runner replays cases through the investigation loop with a given model.
type Runner struct {
	Model providers.ModelProvider
	Log   *slog.Logger
}

func (r *Runner) runOne(ctx context.Context, c Case) Result {
	tools := make([]investigate.Tool, 0, len(c.Tools))
	for name, output := range c.Tools {
		tools = append(tools, staticTool{name: name, output: output})
	}
	// When the case ships a catalog fixture, seed instant recall + the verify pass so
	// the replay exercises the closed recall→verify loop exactly as production does
	// (BuildModelAndTools). Cases without a fixture replay with no recall, unchanged.
	var recall *investigate.Recall
	if c.CatalogDir != "" {
		cat, err := catalog.New(filepath.Join(c.dir, c.CatalogDir))
		if err != nil {
			return Result{Name: c.Name, Missing: []string{"catalog fixture load error: " + err.Error()}}
		}
		rc := c.recallConfig()
		recall = &investigate.Recall{
			Catalog:              cat,
			MinScore:             rc.MinScore,
			MarginGap:            rc.MarginGap,
			SoloFloor:            rc.SoloFloor,
			RequireWorkloadMatch: rc.RequireWorkloadMatch,
			OutcomePrior:         rc.OutcomePrior,
			OutcomeFloor:         rc.OutcomeFloor,
		}
	}
	var got providers.Investigation
	var decision investigate.RecallDecision
	done := false
	li := &investigate.LoopInvestigator{
		Model:    r.Model,
		Tools:    tools,
		Log:      r.Log,
		Recall:   recall,
		Verify:   recall != nil, // recall is untrusted; verify guards it (the property under test)
		OnRecall: func(d investigate.RecallDecision) { decision = d },
		OnComplete: func(inv providers.Investigation) {
			got, done = inv, true
		},
	}
	req := investigate.Request{Source: investigate.SourceAlert, Title: c.Name, Message: c.Prompt, Workload: c.workload()}
	if err := li.Investigate(ctx, req); err != nil {
		return Result{Name: c.Name, Missing: []string{"investigation error: " + err.Error()}}
	}
	if !done {
		return Result{Name: c.Name, Missing: []string{"no findings (loop did not submit)"}}
	}
	res := Score(c.Name, got, c.Expected)
	res.RecallFired = decision.Fired
	res.RecallShortCircuit = decision.ShortCircuited
	if miss := checkRecall(c.ExpectRecall, decision); miss != "" {
		res.Pass = false
		res.Missing = append(res.Missing, miss)
	}
	return res
}

// checkRecall verifies the case's expect_recall assertion against the observed
// decision. It returns "" when the expectation holds (or is unset), else a
// human-readable mismatch string appended to Missing so the failure explains itself.
func checkRecall(want string, d investigate.RecallDecision) string {
	got := "rejected"
	switch {
	case d.Fired && d.ShortCircuited:
		got = "short_circuit"
	case d.Fired:
		got = "withdrawn"
	}
	switch want {
	case "":
		return ""
	case "short_circuit", "withdrawn", "rejected":
		if got != want {
			return fmt.Sprintf("expect_recall=%s but recall %s", want, got)
		}
	case "fired":
		if !d.Fired {
			return "expect_recall=fired but recall did not fire (rejected)"
		}
	default:
		return fmt.Sprintf("unknown expect_recall value %q", want)
	}
	return ""
}

// staticTool replays a case's recorded evidence to the model, regardless of args.
type staticTool struct {
	name, output string
}

func (t staticTool) Name() string                                 { return t.name }
func (t staticTool) Description() string                          { return "Returns recorded evidence for: " + t.name }
func (t staticTool) Schema() string                               { return `{"type":"object","properties":{}}` }
func (t staticTool) Call(context.Context, string) (string, error) { return t.output, nil }

// NewStaticTool exposes the replay tool (fixed recorded output regardless of args)
// for reuse OUTSIDE the eval package — notably the `lore demo investigate` command,
// which drives the real loop against these fakes with no cluster. Additive: it does
// not change how runOne constructs its own staticTools.
func NewStaticTool(name, output string) investigate.Tool {
	return staticTool{name: name, output: output}
}

// FakeTools returns the case's recorded evidence as replay tools (one per entry in
// c.Tools), the same fakes runOne wires into the loop. Exposed additively so the demo
// command can build the identical zero-cluster tool set from a fixture.
func (c Case) FakeTools() []investigate.Tool {
	tools := make([]investigate.Tool, 0, len(c.Tools))
	for name, output := range c.Tools {
		tools = append(tools, staticTool{name: name, output: output})
	}
	return tools
}

// Symptom returns the case's incident description (the loop's seed prompt). Exposed so
// the demo can build the investigation Request from a fixture without reaching into
// unexported fields.
func (c Case) Symptom() string { return c.Prompt }

// DisplayName returns the case's name for demo/report labeling.
func (c Case) DisplayName() string { return c.Name }

// AffectedWorkload returns the case's affected workload (zero when unset), so the demo
// can seed the Request.Workload exactly as runOne does.
func (c Case) AffectedWorkload() providers.Workload { return c.workload() }

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

	// Recall telemetry aggregated over the repeats (cases with a catalog fixture only):
	// HasRecall marks the case as recall-exercising, ExpectRecall echoes its assertion,
	// and the counters say in how many of the N repeats recall fired / short-circuited.
	HasRecall          bool
	ExpectRecall       string
	RecallFired        int
	RecallShortCircuit int
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
	results := make([]Result, 0, n)
	for i := 0; i < n; i++ {
		results = append(results, r.runOne(ctx, c))
	}
	return aggregateResults(c, results)
}

// aggregateResults folds the repeats of one case into its k-of-n aggregate. Pure —
// separated from the runner so the fold (including recall counting) is unit-testable.
func aggregateResults(c Case, results []Result) CaseAggregate {
	confs := make([]float64, 0, len(results))
	missSet := map[string]struct{}{}
	ocSet := map[string]struct{}{}
	passes, fired, shortCircuits := 0, 0, 0
	for _, res := range results {
		if res.Pass {
			passes++
		}
		if res.RecallFired {
			fired++
		}
		if res.RecallShortCircuit {
			shortCircuits++
		}
		confs = append(confs, res.Confidence)
		for _, m := range res.Missing {
			missSet[m] = struct{}{}
		}
		for _, o := range res.OverClaimed {
			ocSet[o] = struct{}{}
		}
	}
	rate := float64(passes) / float64(len(results))
	return CaseAggregate{
		Name:               c.Name,
		Runs:               len(results),
		PassRate:           rate,
		Reached:            rate >= evalMinPassRate,
		Flaky:              rate > 1-evalMinPassRate && rate < evalMinPassRate,
		Confidence:         medianFloat(confs),
		Missing:            sortedSet(missSet),
		OverClaimed:        sortedSet(ocSet),
		HasRecall:          c.CatalogDir != "",
		ExpectRecall:       c.ExpectRecall,
		RecallFired:        fired,
		RecallShortCircuit: shortCircuits,
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

// JSON renders the campaign as an indented report without provenance. Kept for
// callers that have no model/usage context; the nightly eval uses
// Campaign.Report(...).JSON() so the published report carries provenance.
func (c Campaign) JSON() ([]byte, error) {
	return c.Report("", "", providers.Usage{}, nil).JSON()
}
