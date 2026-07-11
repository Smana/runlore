// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// demoScriptModel is a scripted, no-network model for the demo end-to-end test: it
// replays a fixed sequence of completions so the real LoopInvestigator drives one
// tool call, then submit_findings, then a keep verdict for the verify pass. A call
// past the script returns an empty completion (the loop then force-nudges to a final),
// so the test never hangs on the network.
type demoScriptModel struct {
	responses []providers.CompletionResponse
	i         int
}

func (m *demoScriptModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	if m.i >= len(m.responses) {
		return providers.CompletionResponse{}, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// TestRunDemoInvestigateEndToEnd runs `demo investigate` against a curated fixture with
// a scripted (no-network) model wired through the real loop, and asserts the ReAct
// steps and final findings stream to stdout. This proves the command constructs and
// runs end to end with zero cluster and zero live model. It swaps BuildModel's client
// via a scripted model injected through the exported runDemoInvestigateWithModel seam.
func TestRunDemoInvestigateEndToEnd(t *testing.T) {
	model := &demoScriptModel{responses: []providers.CompletionResponse{
		// 1) the agent inspects what changed (a real fixture tool).
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: `{"namespace":"apps"}`}}},
		// 2) the agent submits findings tying the failure to the chart bump migration.
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: "submit_findings",
			Args: `{"confidence":0.8,"root_causes":[{"summary":"chart 1.15 bump enabled a DB migration on harbor-db that blocks harbor-core","confidence":0.8}]}`}}},
		// 3) the verify pass keeps the finding (Verify is on in the demo).
		{ToolCalls: []providers.ToolCall{{ID: "3", Name: "submit_verdicts",
			Args: `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.8}]}`}}},
	}}

	var out, errOut bytes.Buffer
	err := runDemoInvestigateWithModel(
		[]string{"--scenarios", "../../examples/scenarios", "--scenario", "harbor-chart-bump"},
		&out, &errOut, model)
	if err != nil {
		t.Fatalf("runDemoInvestigate: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	// The ReAct step trace must show the tool call the model made.
	if !strings.Contains(got, "→ what_changed") {
		t.Errorf("expected a what_changed ReAct step in the trace, got:\n%s", got)
	}
	// The final findings block must render and carry the root cause.
	if !strings.Contains(got, "submit_findings") {
		t.Errorf("expected a submit_findings section, got:\n%s", got)
	}
	if !strings.Contains(got, "migration") {
		t.Errorf("expected the delivered root cause in the output, got:\n%s", got)
	}
	if model.i == 0 {
		t.Fatal("scripted model was never called; the loop did not run")
	}
}

// TestRunDemoInvestigateUnknownScenario returns a helpful error (listing the available
// ids) when --scenario names a fixture that does not exist.
func TestRunDemoInvestigateUnknownScenario(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runDemoInvestigateWithModel(
		[]string{"--scenarios", "../../examples/scenarios", "--scenario", "does-not-exist"},
		&out, &errOut, &demoScriptModel{})
	if err == nil {
		t.Fatal("expected an error for an unknown scenario id")
	}
	if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "available") {
		t.Errorf("error should name the missing scenario and list the available ones, got: %v", err)
	}
}
