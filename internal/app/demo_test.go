// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/eval"
	"github.com/Smana/runlore/internal/model/replay"
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

// TestDemoOfflineThroughSeam is the funnel guarantee: with NO api key and NO network,
// `demo investigate --offline` drives the real loop over the real fixture tools and
// renders the real verdict card. This is what hack/demo.sh shows a first-time
// visitor. It asserts through the same writer seam the existing end-to-end test uses.
func TestDemoOfflineThroughSeam(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runDemoInvestigateWithModel([]string{
		"--scenarios", "../../examples/scenarios",
		"--scenario", "harbor-chart-bump",
		"--offline", "testdata/demo-transcript.json",
	}, &out, &errOut, nil)
	if err != nil {
		t.Fatalf("demo --offline: %v\nstderr:\n%s", err, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"→ what_changed", "submit_findings", "migration"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q, got:\n%s", want, got)
		}
	}
	// Provenance: a replayed card must say it is replayed, and name the model.
	if !strings.Contains(got, "recorded") {
		t.Errorf("output must disclose that the model turns are recorded, got:\n%s", got)
	}
}

// TestDemoOfflineDefaultsScenarioToTranscript is the regression guard for the bug
// where `--offline` with no `--scenario` fell back to pickScenario's default: the
// FIRST scenario in the directory (alphabetically, "crashloop-after-deploy"), not the
// scenario the shipped transcript was actually recorded against ("harbor-chart-bump").
// That mismatch made the demo announce one incident while replaying tool calls and a
// verdict for a completely different one — exactly what a first-time visitor sees.
// This replays the real shipped transcript with NO --scenario and asserts the
// rendered incident is the transcript's own scenario, not the directory's first entry.
func TestDemoOfflineDefaultsScenarioToTranscript(t *testing.T) {
	tr, err := replay.Load("../../" + demoDefaultTranscript)
	if err != nil {
		t.Fatalf("load shipped transcript: %v", err)
	}
	if tr.Scenario != "harbor-chart-bump" {
		t.Fatalf("test assumes the shipped transcript is scenario %q, got %q — update the assertions below", "harbor-chart-bump", tr.Scenario)
	}

	var out, errOut bytes.Buffer
	err = runDemoInvestigateWithModel([]string{
		"--scenarios", "../../examples/scenarios",
		"--offline", "../../" + demoDefaultTranscript,
	}, &out, &errOut, nil)
	if err != nil {
		t.Fatalf("demo --offline with no --scenario: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if !strings.Contains(got, "HarborProbeFailure") {
		t.Errorf("expected the transcript's own scenario (harbor-chart-bump) announced, got:\n%s", got)
	}
	if strings.Contains(got, "CheckoutPodCrashLoop") {
		t.Errorf("demo announced the directory's first scenario (crashloop-after-deploy) instead of the transcript's own, got:\n%s", got)
	}
}

// TestDemoOfflineNeedsNoAPIKey proves the key check is skipped on the offline path —
// the whole point of --offline.
func TestDemoOfflineNeedsNoAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	var out, errOut bytes.Buffer
	if err := runDemoInvestigateWithModel([]string{
		"--scenarios", "../../examples/scenarios",
		"--scenario", "harbor-chart-bump",
		"--offline", "testdata/demo-transcript.json",
	}, &out, &errOut, nil); err != nil {
		t.Fatalf("offline demo must not require a key, got: %v", err)
	}
}

// TestShippedTranscriptToolsStillExist is the DRIFT GUARD over the fixture the demo
// actually ships. Every tool the transcript calls must still be registered by the
// demo's fixture tools; a renamed or removed tool fails CI here instead of breaking
// the demo silently for every new visitor.
func TestShippedTranscriptToolsStillExist(t *testing.T) {
	tr, err := replay.Load("../../" + demoDefaultTranscript)
	if err != nil {
		t.Fatalf("load shipped transcript: %v", err)
	}
	cases, err := eval.Load("../../examples/scenarios")
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}
	c, err := pickScenario(cases, tr.Scenario)
	if err != nil {
		t.Fatalf("the transcript's scenario %q is gone: %v", tr.Scenario, err)
	}
	have := map[string]bool{
		// Loop-control tools are answered by the loop itself, not the fixture set.
		"submit_findings": true, "submit_verdicts": true,
	}
	for _, tool := range c.FakeTools() {
		have[tool.Name()] = true
	}
	for _, name := range tr.ToolNames() {
		if !have[name] {
			t.Errorf("shipped transcript calls tool %q, which no longer exists — re-record with `lore demo investigate --record %s`", name, demoDefaultTranscript)
		}
	}
}

// TestDemoNotifyRequiresAConfiguredNotifier pins --notify's failure mode.
//
// The demo's promise is that it touches nothing: no cluster, no API key, no
// network. --notify is the single exception, so it must be impossible to ask for
// delivery and silently get none — a demo that prints "done" while posting
// nowhere is worse than one that refuses.
func TestDemoNotifyRequiresAConfiguredNotifier(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "runlore.yaml")
	// A model is configured but NO notify block.
	if err := os.WriteFile(cfgPath, []byte(`
model:
  provider: openai
  base_url: http://unused.invalid/v1
  model: replay
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	err := runDemoInvestigateWithModel([]string{
		"--scenarios", "../../examples/scenarios",
		"--scenario", "harbor-chart-bump",
		"--offline", "testdata/demo-transcript.json",
		"--config", cfgPath,
		"--notify",
	}, &out, &errOut, nil)
	if err == nil {
		t.Fatal("--notify with no notifier configured must fail, not silently deliver nothing")
	}
	if !strings.Contains(err.Error(), "notify") {
		t.Errorf("the error must name what is missing so the user can fix it: %v", err)
	}
}

// TestDemoCatalogWiresRecall pins that --catalog actually consults the knowledge
// base through the real Recall gate.
//
// The shipped entry names the harbor-chart-bump scenario's workload, so recall
// reaches its decision — which the demo prints. What it must NOT do is quietly run
// a full investigation while claiming a catalog was loaded.
func TestDemoCatalogWiresRecall(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runDemoInvestigateWithModel([]string{
		"--scenarios", "../../examples/scenarios",
		"--scenario", "harbor-chart-bump",
		"--offline", "testdata/demo-transcript.json",
		"--catalog", "../../examples/demo/catalog",
	}, &out, &errOut, nil)
	if err != nil {
		t.Fatalf("demo with --catalog: %v\nstderr:\n%s", err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "knowledge catalog: 1 entr") {
		t.Errorf("the shipped demo catalog must load exactly one entry:\n%s", got)
	}
	// The demo's recall gate is NOT production's, and saying so is the point — a
	// reader must not mistake a permissive BM25 floor for the real bar.
	if !strings.Contains(got, "DEMO values, not production's") {
		t.Errorf("the demo must disclose that its recall gate is not production's:\n%s", got)
	}
	// The gate itself must have been consulted, not skipped.
	if !strings.Contains(errOut.String(), "instant recall") {
		t.Errorf("recall was never consulted despite --catalog:\n%s", errOut.String())
	}
}
