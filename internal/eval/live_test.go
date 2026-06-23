package eval

import (
	"context"
	"io"
	"log/slog"
	"testing"

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
