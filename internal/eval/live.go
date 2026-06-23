package eval

import (
	"context"
	"log/slog"
	"sort"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// StepRunner executes a scenario's shell setup/teardown/precheck steps. The real
// implementation shells out (kubectl/flux); tests use a fake.
type StepRunner interface {
	Run(ctx context.Context, step string) error
}

// LiveRunner runs scenarios against real tools, grading coverage + RCA quality.
// BaseTools and Model are the LIVE tools/model (built by cmd/lore via
// buildModelAndTools); Judge uses a separate, stronger model.
type LiveRunner struct {
	Model     providers.ModelProvider
	BaseTools []investigate.Tool
	Judge     Judge
	Steps     StepRunner
	Log       *slog.Logger
	N         int                    // runs per scenario (default 1 if 0)
	OnRecord  func(Scenario, []Call) // optional: persist the run's calls (replay corpus)
	Recall    *investigate.Recall    // optional; when set, runOnce takes the instant-recall short-circuit (production path). nil ⇒ no recall.
}

// RunOutcome is one of the N runs of a scenario.
type RunOutcome struct {
	Investigation providers.Investigation
	Coverage      Coverage
	Verdict       Verdict
}

// LiveResult aggregates the N runs of one scenario.
type LiveResult struct {
	Scenario       string
	Skipped        bool
	SkipReason     string
	Runs           []RunOutcome
	CoverageRatio  float64        // median
	DimMedian      map[string]int // median per rubric dimension
	DimVariance    map[string]float64
	ConfidentWrong bool     // any run confident-wrong
	Flaky          bool     // root_cause scores vary too much across runs to trust
	ToolErrors     []string // union across runs
	Pass           bool
}

// RunScenario runs setup (or precheck), N investigations, judging each, then
// always tears down. Pass gate: at least evalMinPassRate of runs reach root_cause >= evalRootCauseBar,
// coverage median == 1.0, no confident-wrong run, and root_cause variance within evalMaxRootCauseVariance (not flaky).
func (lr *LiveRunner) RunScenario(ctx context.Context, scn Scenario) LiveResult {
	res := LiveResult{Scenario: scn.ID, DimMedian: map[string]int{}, DimVariance: map[string]float64{}}
	n := lr.N
	if n <= 0 {
		n = 1
	}

	// Natural scenarios: precheck the precondition; SKIP (not fail) if absent.
	if !scn.Invasive && scn.Precheck != "" {
		if err := lr.Steps.Run(ctx, scn.Precheck); err != nil {
			res.Skipped = true
			res.SkipReason = "precondition absent: " + err.Error()
			lr.Log.Info("scenario skipped", "id", scn.ID, "reason", res.SkipReason)
			return res
		}
	}

	// Invasive scenarios: induce the fault, and ALWAYS tear down.
	if scn.Invasive {
		for _, step := range scn.Setup {
			if err := lr.Steps.Run(ctx, step); err != nil {
				res.Skipped = true
				res.SkipReason = "setup failed: " + err.Error()
				lr.teardown(ctx, scn)
				return res
			}
		}
		defer lr.teardown(ctx, scn)
	}

	for i := 0; i < n; i++ {
		res.Runs = append(res.Runs, lr.runOnce(ctx, scn))
	}
	lr.aggregate(&res)
	return res
}

func (lr *LiveRunner) runOnce(ctx context.Context, scn Scenario) RunOutcome {
	rec := &Recorder{}
	var inv providers.Investigation
	li := &investigate.LoopInvestigator{
		Model: lr.Model, Tools: wrap(lr.BaseTools, rec), Log: lr.Log, Verify: true, Recall: lr.Recall,
		OnComplete: func(got providers.Investigation) { inv = got },
	}
	req := investigate.Request{
		Source: investigate.SourceAlert, Title: scn.ID, Message: scn.Trigger.Symptom,
		Workload: providers.Workload{Namespace: scn.Trigger.Namespace},
	}
	if err := li.Investigate(ctx, req); err != nil {
		lr.Log.Warn("investigation error", "id", scn.ID, "err", err)
	}
	calls := rec.Calls()
	if lr.OnRecord != nil {
		lr.OnRecord(scn, calls)
	}
	cov := ScoreCoverage(scn.GroundTruth.ExpectedSources, scn.GroundTruth.OptionalSources, calls)
	var v Verdict
	if lr.Judge != nil {
		graded, err := lr.Judge.Grade(ctx, scn, inv)
		if err != nil {
			lr.Log.Warn("judge error", "id", scn.ID, "err", err)
		} else {
			v = graded
		}
	}
	return RunOutcome{Investigation: inv, Coverage: cov, Verdict: v}
}

func (lr *LiveRunner) teardown(ctx context.Context, scn Scenario) {
	for _, step := range scn.Teardown {
		if err := lr.Steps.Run(ctx, step); err != nil {
			lr.Log.Warn("teardown step failed", "id", scn.ID, "step", step, "err", err)
		}
	}
}

func (lr *LiveRunner) aggregate(res *LiveResult) {
	if len(res.Runs) == 0 {
		return
	}
	covs := make([]float64, len(res.Runs))
	errSet := map[string]bool{}
	for i, r := range res.Runs {
		covs[i] = r.Coverage.Ratio
		if r.Verdict.ConfidentWrong {
			res.ConfidentWrong = true
		}
		for _, te := range r.Coverage.ToolErrors {
			errSet[te] = true
		}
	}
	res.CoverageRatio = medianFloat(covs)
	for te := range errSet {
		res.ToolErrors = append(res.ToolErrors, te)
	}
	sort.Strings(res.ToolErrors)
	for _, d := range Rubric {
		vals := make([]float64, len(res.Runs))
		for i, r := range res.Runs {
			vals[i] = float64(r.Verdict.Scores[d.Key])
		}
		res.DimMedian[d.Key] = int(medianFloat(vals) + 0.5)
		res.DimVariance[d.Key] = variance(vals)
	}
	// k-of-n: a clear majority of runs must reach the root cause. A median over an
	// integer 0–3 dimension is too coarse to trust at small N.
	rootCausePasses := 0
	for _, r := range res.Runs {
		if r.Verdict.Scores["root_cause"] >= evalRootCauseBar {
			rootCausePasses++
		}
	}
	rootCausePassRate := float64(rootCausePasses) / float64(len(res.Runs))
	// Flaky: runs disagree too much on root_cause to call this a reliable pass.
	res.Flaky = res.DimVariance["root_cause"] > evalMaxRootCauseVariance
	res.Pass = rootCausePassRate >= evalMinPassRate &&
		res.CoverageRatio == 1.0 &&
		!res.ConfidentWrong &&
		!res.Flaky
}

