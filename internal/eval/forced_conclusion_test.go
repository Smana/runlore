package eval

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// forcedOnlyModel is a non-converging model (issue #234's claude-sonnet-5 shape): it
// keeps calling a benign investigation tool on every free turn and emits
// submit_findings ONLY when the request compels it via ToolChoice. It never winds
// down on its own, so without the loop's step-budget forced-conclusion turn it would
// exhaust maxSteps and deliver nothing.
type forcedOnlyModel struct{}

func (forcedOnlyModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	if req.ToolChoice == "submit_findings" {
		return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
			{ID: "f", Name: "submit_findings",
				Args: `{"confidence":0.3,"root_causes":[{"summary":"best-effort: a bad image tag is the likely cause for eval-victim","confidence":0.3}]}`}}}, nil
	}
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "t", Name: "what_changed", Args: `{}`}}}, nil
}

// TestRunOneForcedFinalStepDelivers is the replay-eval scenario for the step-budget
// forced-conclusion path: a non-converging model that only answers when tool choice is
// forced must still deliver a scored verdict through the harness (forced on the loop's
// final step) rather than exhausting maxSteps with "no findings (loop did not submit)".
func TestRunOneForcedFinalStepDelivers(t *testing.T) {
	c := Case{
		Name:     "non-converging-forced-conclusion",
		Prompt:   "eval-victim pods not starting in runlore-eval; image pull errors",
		Tools:    map[string]string{"what_changed": "image tag set to a nonexistent digest"},
		Expected: Expected{MustContain: []string{"image"}},
	}
	r := &Runner{Model: forcedOnlyModel{}, Log: discardLog()}
	res := r.runOne(context.Background(), c)

	// The forced final step must deliver a verdict — not the silent-exhaustion sentinel.
	for _, m := range res.Missing {
		if m == "no findings (loop did not submit)" {
			t.Fatalf("forced final step must deliver a verdict, not exhaust silently: %+v", res)
		}
	}
	if !res.Pass {
		t.Fatalf("the forced degraded finding names the true cause and should score a pass: %+v", res)
	}
}
