package eval

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// recordingSteps records the shell steps it was asked to run.
type recordingSteps struct {
	ran      []string
	failNext bool
}

func (s *recordingSteps) Run(_ context.Context, step string) error {
	s.ran = append(s.ran, step)
	if s.failNext {
		s.failNext = false
		return io.ErrUnexpectedEOF
	}
	return nil
}

// twoStepModel is conversation-aware (stateless): it calls what_changed until a
// tool result appears in the messages, then submits findings. This is robust to
// the model being shared across the N runs and to the verify pass consuming extra
// model calls — each run's conversation drives the decision, not a global counter.
type twoStepModel struct{}

func (m *twoStepModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	for _, msg := range req.Messages {
		if msg.Role == "tool" { // a tool already ran in this conversation → conclude
			return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{ID: "2", Name: "submit_findings",
				Args: `{"confidence":0.9,"root_causes":[{"summary":"bad image tag pushed by flux"}]}`}}}, nil
		}
	}
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: "{}"}}}, nil
}

// fixedJudge returns the same verdict every time.
type fixedJudge struct{ v Verdict }

func (j fixedJudge) Grade(context.Context, Scenario, providers.Investigation) (Verdict, error) {
	return j.v, nil
}

func liveTestRunner(steps StepRunner, judge Judge) *LiveRunner {
	return &LiveRunner{
		Model:     &twoStepModel{},
		BaseTools: []investigate.Tool{fakeTool{name: "what_changed", out: "diff: tag v9.9.9"}},
		Judge:     judge,
		Steps:     steps,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		N:         3,
	}
}

func passScenario() Scenario {
	return Scenario{
		ID: "gitops-bad-image-tag", Invasive: true,
		Setup: []string{"apply bad tag"}, Teardown: []string{"delete bad tag"},
		Trigger:     Trigger{Mode: "cli", Symptom: "pods not starting"},
		GroundTruth: GroundTruth{ExpectedSources: []string{"gitops"}, MustReachRoot: true},
	}
}

func TestRunScenarioPassesAndTearsDown(t *testing.T) {
	steps := &recordingSteps{}
	judge := fixedJudge{v: Verdict{Scores: map[string]int{"root_cause": 3, "evidence": 2, "solution": 2, "description": 2, "calibration": 2}}}
	res := liveTestRunner(steps, judge).RunScenario(context.Background(), passScenario())

	if res.Skipped {
		t.Fatal("should not skip an invasive scenario")
	}
	if len(res.Runs) != 3 {
		t.Fatalf("want N=3 runs, got %d", len(res.Runs))
	}
	if res.CoverageRatio != 1.0 {
		t.Fatalf("want coverage 1.0, got %.2f", res.CoverageRatio)
	}
	if res.DimMedian["root_cause"] != 3 {
		t.Fatalf("want median root_cause 3, got %d", res.DimMedian["root_cause"])
	}
	if !res.Pass {
		t.Fatalf("want pass, got %+v", res)
	}
	// teardown must run even on success; setup ran once, teardown ran once.
	if steps.ran[0] != "apply bad tag" || steps.ran[len(steps.ran)-1] != "delete bad tag" {
		t.Fatalf("setup/teardown order wrong: %v", steps.ran)
	}
}

func TestRunScenarioFailsGateOnLowRootCause(t *testing.T) {
	judge := fixedJudge{v: Verdict{Scores: map[string]int{"root_cause": 1}}} // symptom-only => fail gate
	res := liveTestRunner(&recordingSteps{}, judge).RunScenario(context.Background(), passScenario())
	if res.Pass {
		t.Fatalf("root_cause=1 must fail the gate: %+v", res)
	}
}

func TestRunScenarioSkipsWhenPrecheckFails(t *testing.T) {
	steps := &recordingSteps{failNext: true} // precheck is the first step -> fails -> SKIP
	scn := Scenario{ID: "harbor-natural", Invasive: false, Precheck: "test harbor broken"}
	res := liveTestRunner(steps, fixedJudge{}).RunScenario(context.Background(), scn)
	if !res.Skipped || res.Pass {
		t.Fatalf("want skipped (precondition absent), got %+v", res)
	}
	if len(res.Runs) != 0 {
		t.Fatal("must not run investigations when skipped")
	}
}

// rcRuns builds N runs with the given root_cause scores and full coverage.
func rcRuns(scores ...int) []RunOutcome {
	rs := make([]RunOutcome, len(scores))
	for i, s := range scores {
		rs[i] = RunOutcome{
			Coverage: Coverage{Ratio: 1.0},
			Verdict:  Verdict{Scores: map[string]int{"root_cause": s}},
		}
	}
	return rs
}

func aggregated(runs []RunOutcome) LiveResult {
	res := LiveResult{DimMedian: map[string]int{}, DimVariance: map[string]float64{}, Runs: runs}
	(&LiveRunner{}).aggregate(&res)
	return res
}

func TestAggregateKOfNPass(t *testing.T) {
	res := aggregated(rcRuns(2, 2, 2, 2, 2, 2, 2, 1, 1, 1)) // 7/10 at root_cause>=2, variance ~0.21
	if !res.Pass || res.Flaky {
		t.Fatalf("7/10 at root_cause>=2 should pass cleanly: pass=%v flaky=%v var=%v", res.Pass, res.Flaky, res.DimVariance["root_cause"])
	}
}

func TestAggregateKOfNFailsBelowRate(t *testing.T) {
	res := aggregated(rcRuns(2, 2, 2, 2, 2, 2, 1, 1, 1, 1)) // 6/10
	if res.Pass {
		t.Fatal("6/10 at root_cause>=2 should fail the k-of-n gate")
	}
}

func TestAggregateFlakyFails(t *testing.T) {
	res := aggregated(rcRuns(2, 2, 2, 2, 2, 2, 2, 2, 0, 0)) // rate 0.8 but variance ~0.64
	if !res.Flaky || res.Pass {
		t.Fatalf("high-variance run must be flaky and not pass: flaky=%v pass=%v var=%v", res.Flaky, res.Pass, res.DimVariance["root_cause"])
	}
}

func TestAggregateCleanPass(t *testing.T) {
	res := aggregated(rcRuns(3, 3, 3, 3, 3, 3, 3, 3, 3, 2)) // variance ~0.09
	if !res.Pass || res.Flaky {
		t.Fatalf("consistent high scores should pass cleanly: pass=%v flaky=%v", res.Pass, res.Flaky)
	}
}

func TestAggregateCoverageGate(t *testing.T) {
	runs := rcRuns(3, 3, 3, 3, 3, 3, 3, 3, 3, 3) // rate 1.0, not flaky
	for i := range runs {
		runs[i].Coverage.Ratio = 0.5 // median 0.5 != 1.0
	}
	if aggregated(runs).Pass {
		t.Fatal("coverage median < 1.0 must fail regardless of root_cause rate")
	}
}

func TestAggregateConfidentWrongFails(t *testing.T) {
	runs := rcRuns(3, 3, 3, 3, 3, 3, 3, 3, 3, 3)
	runs[0].Verdict.ConfidentWrong = true
	if aggregated(runs).Pass {
		t.Fatal("any confident-wrong run must fail")
	}
}

// fakeCatalog returns a single fixed scored entry regardless of query/k — enough
// to drive the recall gates in a unit test.
type fakeCatalog struct {
	entry catalog.Entry
	score float64
}

func (f fakeCatalog) SearchScored(string, int) ([]catalog.ScoredEntry, error) {
	return []catalog.ScoredEntry{{Entry: f.entry, Score: f.score}}, nil
}

// verifyModel answers only the adversarial verify call (submit_verdicts) with a
// fixed verdict for root cause index 0. On the recall short-circuit path this is
// the sole model call (the ReAct loop is skipped).
type verifyModel struct{ verdict string }

func (m verifyModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{ID: "v", Name: "submit_verdicts",
		Args: fmt.Sprintf(`{"verdicts":[{"index":0,"verdict":%q}]}`, m.verdict)}}}, nil
}

// recallRunner builds a LiveRunner whose catalog has one entry matching the
// recallScenario's namespace (so the structural gate agrees), with low recall
// floors so a single strong hit short-circuits.
func recallRunner(model providers.ModelProvider) *LiveRunner {
	entry := catalog.Entry{Title: "HarborRegistryDown", Description: "IAM quota exceeded", Path: "harbor.md", Resource: "tooling"}
	return &LiveRunner{
		Model:  model,
		Recall: &investigate.Recall{Catalog: fakeCatalog{entry: entry, score: 10}, MinScore: 1, SoloFloor: 1, MarginGap: 1},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func recallScenario() Scenario {
	return Scenario{ID: "known-pattern-recall", Trigger: Trigger{Symptom: "harbor registry down", Namespace: "tooling"}}
}

func TestRunOnceRecallShortCircuits(t *testing.T) {
	out := recallRunner(verifyModel{verdict: "keep"}).runOnce(context.Background(), recallScenario())
	if !out.Investigation.Recalled {
		t.Fatal("recall should have short-circuited the loop (Investigation.Recalled=true)")
	}
	if len(out.Investigation.RootCauses) != 1 {
		t.Fatalf("a kept recall should retain its single root cause, got %d", len(out.Investigation.RootCauses))
	}
	if len(out.Coverage.Touched) != 0 {
		t.Fatalf("recall short-circuit must run no investigation tools, got %v", out.Coverage.Touched)
	}
}

func TestRunOnceRecallPoisonedRejected(t *testing.T) {
	// A poisoned entry: the verify pass rejects it → the wrong root cause is withdrawn,
	// not short-circuited into. (Recalled stays true; the safety is an empty RootCauses.)
	out := recallRunner(verifyModel{verdict: "reject"}).runOnce(context.Background(), recallScenario())
	if len(out.Investigation.RootCauses) != 0 {
		t.Fatalf("a rejected poisoned recall must withdraw its root cause, got %d", len(out.Investigation.RootCauses))
	}
	if !out.Investigation.Recalled {
		t.Fatal("a rejected recall is still a recall (loop skipped): Recalled must stay true")
	}
	if len(out.Investigation.Unresolved) == 0 {
		t.Fatal("a rejected recall hypothesis must be moved to Unresolved, not silently dropped")
	}
}
