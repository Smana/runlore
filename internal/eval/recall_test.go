// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// seqModel replays a fixed sequence of completion responses, so a test can script a
// full recall→verify→fall-through interaction (verify reject, fresh submit, verify keep).
type seqModel struct {
	resp []providers.CompletionResponse
	i    int
}

func (m *seqModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	r := m.resp[m.i]
	m.i++
	return r, nil
}

func verdict(v string) providers.CompletionResponse {
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "v", Name: "submit_verdicts", Args: `{"verdicts":[{"index":0,"verdict":"` + v + `","confidence":0.8,"reason":"r"}]}`}}}
}

func findings(summary string) providers.CompletionResponse {
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "f", Name: "submit_findings", Args: `{"confidence":0.8,"root_causes":[{"summary":"` + summary + `","confidence":0.8}]}`}}}
}

// writeKBFixture writes a single-entry KB catalog under dir/kb and returns "kb".
func writeKBFixture(t *testing.T, dir, resource string) string {
	t.Helper()
	kb := filepath.Join(dir, "kb")
	if err := os.MkdirAll(kb, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := `---
type: Incident
title: eval-victim pods not starting stale configmap key
description: eval-victim pods fail to start; recreate the drifted configmap
resource: ` + resource + `
tags: [configmap, eval-victim]
---

# Symptom
eval-victim pods in runlore-eval are not starting; container start failures.
`
	if err := os.WriteFile(filepath.Join(kb, "poisoned.md"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	return "kb"
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRunOneRecallWithdrawn drives the real Catalog + Recall + verify stack through a
// case with a seeded fixture: recall FIRES on the poisoned entry, the (scripted)
// verify pass rejects it, and the loop falls through to a fresh investigation. The
// case asserts this mechanically via expect_recall: withdrawn.
func TestRunOneRecallWithdrawn(t *testing.T) {
	dir := t.TempDir()
	c := Case{
		Name:       "poison",
		Prompt:     "eval-victim pods not starting in runlore-eval image pull errors",
		Workload:   &CaseWorkload{Namespace: "runlore-eval", Name: "eval-victim"},
		CatalogDir: writeKBFixture(t, dir, "runlore-eval/eval-victim"),
		Recall:     &CaseRecall{MinScore: 0.01, SoloFloor: 0.01, MarginGap: 0.01},
		Tools: map[string]string{
			"pod_status":   "eval-victim-abc  0/1  ImagePullBackOff  registry.k8s.io/pause:v9.9.9-does-not-exist",
			"kube_events":  "Warning  Failed  Failed to pull image ...: not found",
			"what_changed": "image tag set to pause:v9.9.9-does-not-exist",
		},
		Expected:     Expected{MustContain: []string{"image"}},
		ExpectRecall: "withdrawn",
		dir:          dir,
	}
	// verify recall → reject; fresh loop submits; verify fresh → keep.
	model := &seqModel{resp: []providers.CompletionResponse{
		verdict("reject"),
		findings("bad image tag causes ImagePullBackOff for eval-victim"),
		verdict("keep"),
	}}
	r := &Runner{Model: model, Log: discardLog()}
	res := r.runOne(context.Background(), c)
	if !res.RecallFired {
		t.Fatalf("recall must FIRE on the seeded poisoned entry: %+v", res)
	}
	if res.RecallShortCircuit {
		t.Fatalf("a verify-rejected recall must NOT short-circuit: %+v", res)
	}
	if !res.Pass {
		t.Fatalf("case should pass (fresh finding names image + expect_recall withdrawn): %+v", res)
	}
}

// TestRunOneRecallShortCircuit: recall fires and the verify pass keeps it, so the
// recalled answer is delivered and the loop never runs. expect_recall: short_circuit.
func TestRunOneRecallShortCircuit(t *testing.T) {
	dir := t.TempDir()
	c := Case{
		Name:         "known",
		Prompt:       "eval-victim pods not starting in runlore-eval",
		Workload:     &CaseWorkload{Namespace: "runlore-eval", Name: "eval-victim"},
		CatalogDir:   writeKBFixture(t, dir, "runlore-eval/eval-victim"),
		Recall:       &CaseRecall{MinScore: 0.01, SoloFloor: 0.01, MarginGap: 0.01},
		Tools:        map[string]string{"pod_status": "eval-victim-abc  0/1  Pending"},
		ExpectRecall: "short_circuit",
		dir:          dir,
	}
	model := &seqModel{resp: []providers.CompletionResponse{verdict("keep")}}
	r := &Runner{Model: model, Log: discardLog()}
	res := r.runOne(context.Background(), c)
	if !res.RecallFired || !res.RecallShortCircuit {
		t.Fatalf("expected fired+short-circuit, got %+v", res)
	}
	if !res.Pass {
		t.Fatalf("short_circuit case should pass: %+v", res)
	}
}

// TestRunOneRecallGateRejects: the fixture's resource does not agree with the
// incident workload, so the structural gate rejects recall (it never fires) and the
// loop runs fully. expect_recall: rejected.
func TestRunOneRecallGateRejects(t *testing.T) {
	dir := t.TempDir()
	c := Case{
		Name:         "mismatch",
		Prompt:       "eval-victim pods not starting",
		Workload:     &CaseWorkload{Namespace: "runlore-eval", Name: "eval-victim"},
		CatalogDir:   writeKBFixture(t, dir, "other-ns/other-app"), // wrong workload → Gate 1 fails
		Recall:       &CaseRecall{MinScore: 0.01, SoloFloor: 0.01, MarginGap: 0.01},
		Tools:        map[string]string{"what_changed": "nothing relevant"},
		Expected:     Expected{MustContain: []string{"fresh"}},
		ExpectRecall: "rejected",
		dir:          dir,
	}
	// No recall fires: loop submits fresh, then verify keeps it.
	model := &seqModel{resp: []providers.CompletionResponse{findings("fresh investigation result"), verdict("keep")}}
	r := &Runner{Model: model, Log: discardLog()}
	res := r.runOne(context.Background(), c)
	if res.RecallFired {
		t.Fatalf("recall must NOT fire on a structural mismatch: %+v", res)
	}
	if !res.Pass {
		t.Fatalf("rejected case should pass: %+v", res)
	}
}

// TestRunOneExpectRecallMismatchFails: when the observed recall outcome contradicts
// expect_recall, the case fails with an explanatory Missing entry.
func TestRunOneExpectRecallMismatchFails(t *testing.T) {
	dir := t.TempDir()
	c := Case{
		Name:         "expect-short-but-withdrawn",
		Prompt:       "eval-victim pods not starting in runlore-eval",
		Workload:     &CaseWorkload{Namespace: "runlore-eval", Name: "eval-victim"},
		CatalogDir:   writeKBFixture(t, dir, "runlore-eval/eval-victim"),
		Recall:       &CaseRecall{MinScore: 0.01, SoloFloor: 0.01, MarginGap: 0.01},
		Tools:        map[string]string{"pod_status": "eval-victim-abc Pending"},
		ExpectRecall: "short_circuit", // but verify will reject → actually withdrawn
		dir:          dir,
	}
	model := &seqModel{resp: []providers.CompletionResponse{verdict("reject"), findings("fresh"), verdict("keep")}}
	r := &Runner{Model: model, Log: discardLog()}
	res := r.runOne(context.Background(), c)
	if res.Pass {
		t.Fatalf("case must FAIL when expect_recall does not match the observed outcome: %+v", res)
	}
	found := false
	for _, m := range res.Missing {
		if m == "expect_recall=short_circuit but recall withdrawn" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing the recall mismatch explanation: %+v", res.Missing)
	}
}

// TestShippedPoisonedRecallCaseFires guards the shipped examples/eval fixture: the
// poisoned-recall-verify case must actually FIRE recall and be WITHDRAWN by verify
// (with a scripted model), so nightly CI is exercising the recall→verify machinery
// rather than silently degrading to "recall never fired" if the fixture drifts. It
// does NOT need a live model — only the recall lookup + verify wiring, scripted here.
func TestShippedPoisonedRecallCaseFires(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "examples", "eval"))
	if err != nil {
		t.Fatalf("Load examples/eval: %v", err)
	}
	var pc *Case
	for i := range cases {
		if cases[i].Name == "poisoned-recall-verify" {
			pc = &cases[i]
		}
	}
	if pc == nil {
		t.Fatal("examples/eval/poisoned-recall-verify.yaml not loaded")
	}
	if pc.ExpectRecall != "withdrawn" {
		t.Fatalf("shipped case must assert expect_recall: withdrawn, got %q", pc.ExpectRecall)
	}
	model := &seqModel{resp: []providers.CompletionResponse{
		verdict("reject"), // verify rejects the poisoned recall
		findings("the image tag registry.k8s.io/pause:v9.9.9-does-not-exist cannot be pulled (ImagePullBackOff)"),
		verdict("keep"), // verify keeps the fresh finding
	}}
	r := &Runner{Model: model, Log: discardLog()}
	res := r.runOne(context.Background(), *pc)
	if !res.RecallFired || res.RecallShortCircuit {
		t.Fatalf("shipped fixture must FIRE recall and be WITHDRAWN by verify: %+v", res)
	}
	if !res.Pass {
		t.Fatalf("shipped case should pass with the scripted true-cause finding: %+v", res)
	}
}

func TestCheckRecall(t *testing.T) {
	fired := investigate.RecallDecision{Fired: true, ShortCircuited: true}
	withdrawn := investigate.RecallDecision{Fired: true}
	none := investigate.RecallDecision{}
	tests := []struct {
		want string
		d    investigate.RecallDecision
		ok   bool
	}{
		{"", none, true},
		{"short_circuit", fired, true},
		{"short_circuit", withdrawn, false},
		{"withdrawn", withdrawn, true},
		{"withdrawn", fired, false},
		{"rejected", none, true},
		{"rejected", fired, false},
		{"fired", fired, true},
		{"fired", withdrawn, true},
		{"fired", none, false},
		{"bogus", fired, false},
	}
	for _, tt := range tests {
		got := checkRecall(tt.want, tt.d) == ""
		if got != tt.ok {
			t.Errorf("checkRecall(%q, %+v) ok=%v, want %v", tt.want, tt.d, got, tt.ok)
		}
	}
}

func TestLoadRecallCaseSchema(t *testing.T) {
	dir := t.TempDir()
	yaml := `
name: poisoned-recall
prompt: eval-victim pods not starting
workload:
  namespace: runlore-eval
  name: eval-victim
catalog_dir: fixtures/poisoned
recall:
  solo_floor: 0.2
  min_score: 0.1
expect_recall: withdrawn
tools:
  pod_status: "ImagePullBackOff"
expected:
  must_contain: [image]
`
	if err := os.WriteFile(filepath.Join(dir, "c.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cases[0]
	if c.CatalogDir != "fixtures/poisoned" || c.ExpectRecall != "withdrawn" {
		t.Fatalf("recall fields not parsed: %+v", c)
	}
	if c.Workload == nil || c.Workload.Namespace != "runlore-eval" || c.Workload.Name != "eval-victim" {
		t.Fatalf("workload not parsed: %+v", c.Workload)
	}
	if c.dir != dir {
		t.Fatalf("case dir not recorded for catalog resolution: got %q want %q", c.dir, dir)
	}
	// Defaults fill zero fields; explicit ones survive.
	rc := c.recallConfig()
	if rc.SoloFloor != 0.2 || rc.MinScore != 0.1 || rc.MarginGap != 1.0 || rc.OutcomeFloor != 0.5 {
		t.Fatalf("recallConfig defaults wrong: %+v", rc)
	}
}
