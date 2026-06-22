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
