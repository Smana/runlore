// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "harbor.yaml"), []byte(`
name: harbor-chart-bump
prompt: HarborProbeFailure in apps
tools:
  what_changed: "chart 1.15 enabled DB migrations"
expected:
  must_contain: [chart, migration]
  min_confidence: 0.5
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cases) != 1 || cases[0].Name != "harbor-chart-bump" {
		t.Fatalf("unexpected cases: %+v", cases)
	}
	if cases[0].Tools["what_changed"] == "" || len(cases[0].Expected.MustContain) != 2 || cases[0].Expected.MinConfidence != 0.5 {
		t.Fatalf("case not parsed: %+v", cases[0])
	}
}

func TestScore(t *testing.T) {
	inv := providers.Investigation{
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{Summary: "chart bump enabled a DB migration that stalled harbor-db"}},
	}
	if r := Score("c", inv, Expected{MustContain: []string{"chart", "harbor-db"}, MinConfidence: 0.5}); !r.Pass {
		t.Fatalf("expected pass, got %+v", r)
	}
	if r := Score("c", inv, Expected{MustContain: []string{"network"}}); r.Pass || len(r.Missing) != 1 {
		t.Fatalf("expected fail with 1 missing, got %+v", r)
	}
	if r := Score("c", inv, Expected{MustContain: []string{"chart"}, MinConfidence: 0.95}); r.Pass {
		t.Fatalf("expected fail on confidence floor, got %+v", r)
	}
}

func TestEntityRecallAllNamedPasses(t *testing.T) {
	inv := providers.Investigation{
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{
			Summary:         "apps/web crashed because rds/prod-db hit its connection cap",
			SuggestedAction: "raise max_connections on rds/prod-db",
		}},
	}
	r := Score("c", inv, Expected{
		RootCauseEntities: []string{"apps/web", "rds/prod-db"},
		Distractors:       []string{"apps/worker"},
	})
	if !r.Pass || len(r.Missing) != 0 || len(r.OverClaimed) != 0 {
		t.Fatalf("expected clean pass, got %+v", r)
	}
}

func TestEntityMissingFails(t *testing.T) {
	inv := providers.Investigation{Confidence: 0.8, RootCauses: []providers.Hypothesis{{Summary: "apps/web is unhealthy"}}}
	r := Score("c", inv, Expected{RootCauseEntities: []string{"apps/web", "rds/prod-db"}})
	if r.Pass {
		t.Fatal("expected fail: rds/prod-db was not named as a cause")
	}
	if len(r.Missing) != 1 || r.Missing[0] != "rds/prod-db" {
		t.Fatalf("expected rds/prod-db in Missing, got %+v", r.Missing)
	}
}

func TestOverClaimDistractorBlamedFails(t *testing.T) {
	// All expected entities ARE named, but a distractor is also blamed → over-claim → fail.
	inv := providers.Investigation{
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{Summary: "root cause is apps/web and apps/worker, both talking to rds/prod-db"}},
	}
	r := Score("c", inv, Expected{
		RootCauseEntities: []string{"apps/web", "rds/prod-db"},
		Distractors:       []string{"apps/worker"},
	})
	if r.Pass {
		t.Fatalf("expected fail on over-claim, got pass: %+v", r)
	}
	if len(r.OverClaimed) != 1 || r.OverClaimed[0] != "apps/worker" {
		t.Fatalf("expected apps/worker over-claimed, got %+v", r.OverClaimed)
	}
	// The over-claim is mirrored into Missing so the report renderer shows the reason.
	if !slices.Contains(r.Missing, "over-claimed: apps/worker") {
		t.Fatalf("over-claim should be mirrored into Missing, got %+v", r.Missing)
	}
}

func TestDistractorSubstringOfEntityNotOverClaim(t *testing.T) {
	// The distractor "apps/worker" is a substring of the required "apps/worker-db".
	// Naming only the required entity must NOT trip a false over-claim.
	inv := providers.Investigation{
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{Summary: "root cause is apps/worker-db connection exhaustion"}},
	}
	r := Score("c", inv, Expected{
		RootCauseEntities: []string{"apps/worker-db"},
		Distractors:       []string{"apps/worker"},
	})
	if !r.Pass || len(r.OverClaimed) != 0 {
		t.Fatalf("distractor that is a substring of a named entity must not be an over-claim, got %+v", r)
	}
}

func TestDistractorInEvidenceNotPenalized(t *testing.T) {
	// The distractor appears only in Evidence/Unresolved, never in the claim → not an over-claim.
	inv := providers.Investigation{
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{
			Summary:  "apps/web crashed due to rds/prod-db saturation",
			Evidence: []string{"ruled out apps/worker: its error rate was flat"},
		}},
		Unresolved: []string{"whether apps/worker retries amplified load"},
	}
	r := Score("c", inv, Expected{
		RootCauseEntities: []string{"apps/web", "rds/prod-db"},
		Distractors:       []string{"apps/worker"},
	})
	if !r.Pass {
		t.Fatalf("a distractor only in evidence/unresolved must not penalize, got %+v", r)
	}
	if len(r.OverClaimed) != 0 {
		t.Fatalf("no over-claim expected, got %+v", r.OverClaimed)
	}
}

func TestNoEntitiesBackwardCompatible(t *testing.T) {
	// A case with only must_contain behaves exactly as before: entities ignored.
	inv := providers.Investigation{Confidence: 0.8, RootCauses: []providers.Hypothesis{{Summary: "chart bump stalled harbor-db"}}}
	r := Score("c", inv, Expected{MustContain: []string{"chart", "harbor-db"}, MinConfidence: 0.5})
	if !r.Pass || len(r.Missing) != 0 || len(r.OverClaimed) != 0 {
		t.Fatalf("a must_contain-only case should pass cleanly, got %+v", r)
	}
}

func TestLoadParsesEntities(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "db.yaml"), []byte(`
name: db-saturation
prompt: web 5xx spike
tools:
  what_changed: "no change"
expected:
  root_cause_entities: [apps/web, rds/prod-db]
  distractors: [apps/worker]
  min_confidence: 0.5
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cases) != 1 ||
		len(cases[0].Expected.RootCauseEntities) != 2 ||
		len(cases[0].Expected.Distractors) != 1 {
		t.Fatalf("entity fields not parsed: %+v", cases[0].Expected)
	}
}

// rateModel passes (names harbor-db) on its first passN Complete-pairs, then fails.
// Each replay run makes 2 Complete calls (tool, then submit_findings).
type rateModel struct {
	calls, passN int
}

func (m *rateModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	if m.calls%2 == 1 { // first call of a run: invoke the tool
		return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
			{ID: "1", Name: "what_changed", Args: `{}`}}}, nil
	}
	run := m.calls / 2 // 1-based index of the run just completing
	summary := "chart bump broke harbor-db migrations"
	if run > m.passN {
		summary = "unclear, possibly a transient blip"
	}
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "2", Name: "submit_findings",
			Args: `{"confidence":0.9,"root_causes":[{"summary":"` + summary + `"}]}`}}}, nil
}

func newRateRunner(passN int) *Runner {
	return &Runner{Model: &rateModel{passN: passN}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func harborCase() Case {
	return Case{Name: "harbor", Prompt: "probe failing", Tools: map[string]string{"what_changed": "chart 1.15"},
		Expected: Expected{MustContain: []string{"chart", "harbor-db"}, MinConfidence: 0.5}}
}

func TestReplayKOfNRepeats(t *testing.T) {
	// 4 of 5 pass → pass-rate 0.8 ≥ 0.7 → reached, not flaky.
	camp := newRateRunner(4).RunN(context.Background(), []Case{harborCase()}, 5)
	a := camp.Aggregates[0]
	if a.Runs != 5 || a.PassRate < 0.79 || a.PassRate > 0.81 {
		t.Fatalf("want 5 runs at 0.8, got runs=%d rate=%.2f", a.Runs, a.PassRate)
	}
	if !a.Reached || a.Flaky {
		t.Fatalf("4/5 should be reached and not flaky, got %+v", a)
	}

	// 2 of 5 pass → 0.4 < 0.7 → not reached; 0.4 is in (0.3,0.7) → flaky.
	camp = newRateRunner(2).RunN(context.Background(), []Case{harborCase()}, 5)
	a = camp.Aggregates[0]
	if a.Reached {
		t.Fatalf("2/5 must not be reached, got %+v", a)
	}
	if !a.Flaky {
		t.Fatalf("2/5 (rate 0.4) should be flaky, got %+v", a)
	}
}

func TestReplayCampaignPassRate(t *testing.T) {
	// One reachable case (5/5) and one unreachable (0/5) → campaign pass-rate 0.5.
	cases := []Case{harborCase(), {Name: "miss", Prompt: "x", Tools: map[string]string{"what_changed": "y"},
		Expected: Expected{MustContain: []string{"network policy"}}}}
	camp := newRateRunner(5).RunN(context.Background(), cases, 5)
	if camp.ReachedCases() != 1 || camp.PassRate() != 0.5 {
		t.Fatalf("want reached=1 rate=0.5, got reached=%d rate=%.2f", camp.ReachedCases(), camp.PassRate())
	}
}

func TestGateError(t *testing.T) {
	miss := Case{Name: "miss", Prompt: "x", Tools: map[string]string{"what_changed": "y"},
		Expected: Expected{MustContain: []string{"network policy"}}}
	camp := newRateRunner(5).RunN(context.Background(), []Case{harborCase(), miss}, 5) // pass-rate 0.5

	if err := GateError(camp, 0); err != nil {
		t.Fatalf("fail-under 0 must never gate, got %v", err)
	}
	if err := GateError(camp, 0.4); err != nil {
		t.Fatalf("0.5 >= 0.4 should pass, got %v", err)
	}
	if err := GateError(camp, 0.7); err == nil {
		t.Fatal("0.5 < 0.7 should return a gate error")
	}
}

// TestGateErrorSkipsMeasurementCases pins the `gate: false` opt-out end to end: a
// measurement case's failure must not turn the nightly red, and — the part that is easy
// to get wrong — it must leave the denominator too. Excluding it from the numerator
// alone would make the gate STRICTER by adding a case that can never pass.
func TestGateErrorSkipsMeasurementCases(t *testing.T) {
	no := false
	miss := Case{Name: "miss", Prompt: "x", Tools: map[string]string{"what_changed": "y"},
		Expected: Expected{MustContain: []string{"network policy"}}}
	measurement := miss
	measurement.Name, measurement.Gate = "measurement", &no

	// One real case that passes, one measurement case that fails. Ungated, that is a
	// 0.5 pass-rate and a red nightly; the whole point of the opt-out is that it is not.
	camp := newRateRunner(5).RunN(context.Background(), []Case{harborCase(), measurement}, 5)
	if camp.GatedTotal() != 1 || camp.GatedReached() != 1 {
		t.Fatalf("want 1 gate-bearing case, reached: got total=%d reached=%d", camp.GatedTotal(), camp.GatedReached())
	}
	if camp.GatedPassRate() != 1 {
		t.Fatalf("the failing measurement case must not lower the GATED pass-rate, got %.2f", camp.GatedPassRate())
	}
	if err := GateError(camp, 0.7); err != nil {
		t.Fatalf("a failing measurement case must not fail the gate, got %v", err)
	}

	// The published view still counts it: PassRate/ReachedCases stay over every case
	// that ran, because the scorecard's job is to report the number, not to hide it.
	if camp.PassRate() != 0.5 || camp.ReachedCases() != 1 {
		t.Fatalf("the report must still see both cases, got rate=%.2f reached=%d", camp.PassRate(), camp.ReachedCases())
	}

	// And the opt-out is opt-IN only: the same case without `gate:` still gates.
	camp = newRateRunner(5).RunN(context.Background(), []Case{harborCase(), miss}, 5)
	if err := GateError(camp, 0.7); err == nil {
		t.Fatal("a case that does NOT set gate: false must still fail the gate")
	}

	// A campaign of nothing but measurements has nothing to regress and must not fail.
	camp = newRateRunner(5).RunN(context.Background(), []Case{measurement}, 5)
	if err := GateError(camp, 0.7); err != nil {
		t.Fatalf("an all-measurement campaign has no gate to fail, got %v", err)
	}
}

func TestReplayDefaultsPreserveBehavior(t *testing.T) {
	// n=1 (the replay default) runs each case once; GateError with the default
	// fail-under (0) never gates, so local `lore eval` keeps exiting 0.
	camp := newRateRunner(5).RunN(context.Background(), []Case{harborCase()}, 1)
	if len(camp.Aggregates) != 1 || camp.Aggregates[0].Runs != 1 {
		t.Fatalf("n=1 should run each case once, got %+v", camp.Aggregates)
	}
	if err := GateError(camp, 0); err != nil {
		t.Fatalf("default fail-under (0) must never gate, got %v", err)
	}
}

func TestCampaignJSON(t *testing.T) {
	camp := newRateRunner(5).RunN(context.Background(), []Case{harborCase()}, 2)
	b, err := camp.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got struct {
		N        int     `json:"n"`
		PassRate float64 `json:"pass_rate"`
		Reached  int     `json:"reached"`
		Total    int     `json:"total"`
		Cases    []struct {
			Name    string `json:"name"`
			Reached bool   `json:"reached"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.N != 2 || got.Total != 1 || got.Reached != 1 || got.PassRate != 1.0 {
		t.Fatalf("unexpected report header: %+v", got)
	}
	if len(got.Cases) != 1 || got.Cases[0].Name != "harbor" || !got.Cases[0].Reached {
		t.Fatalf("unexpected case rows: %+v", got.Cases)
	}
}

// ---------------------------------------------------------------------------
// Knowledge-commons grounding
//
// The commons has exactly one claimed value: it grounds kb_search mid-loop so a
// fresh deployment has something to reason from before it has curated anything of
// its own (website/content/docs/concepts/knowledge-commons.md). Everything pinned
// until now was the NEGATIVE half of that promise — a commons entry can never fire
// instant recall. These tests pin the replay wiring the positive half needs:
// a case may load a SECOND, commons root, its entries are marked as such, they
// reach the model only through kb_search, and recall still refuses them.
// ---------------------------------------------------------------------------

// recordingModel replays a scripted sequence of responses AND keeps every request it
// was handed, so a test can assert what the loop actually put in front of the model:
// the tool specs it was offered, and the tool OUTPUT it read back. Asserting on the
// finding alone cannot distinguish "the playbook grounded the answer" from "the
// scripted answer happened to say the right words".
type recordingModel struct {
	resp []providers.CompletionResponse
	i    int
	reqs []providers.CompletionRequest
}

func (m *recordingModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.reqs = append(m.reqs, req)
	if m.i >= len(m.resp) {
		return providers.CompletionResponse{}, fmt.Errorf("recordingModel: unscripted call #%d", m.i+1)
	}
	r := m.resp[m.i]
	m.i++
	return r, nil
}

// toolText concatenates every tool result the loop fed back to the model.
func (m *recordingModel) toolText() string {
	var b strings.Builder
	for _, r := range m.reqs {
		for _, msg := range r.Messages {
			if msg.Role == "tool" {
				b.WriteString(msg.Content + "\n")
			}
		}
	}
	return b.String()
}

// offeredTools lists the tool names advertised on the first completion.
func (m *recordingModel) offeredTools() []string {
	if len(m.reqs) == 0 {
		return nil
	}
	names := make([]string, 0, len(m.reqs[0].Tools))
	for _, s := range m.reqs[0].Tools {
		names = append(names, s.Name)
	}
	return names
}

// kbSearch scripts one kb_search tool call.
func kbSearch(query string) providers.CompletionResponse {
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "kb", Name: "kb_search", Args: `{"query":"` + query + `"}`}}}
}

// commonsMarker is a sentence that exists only in the commons fixture, so finding it
// in a tool result proves the shared corpus (not the case's recorded evidence) is
// what reached the model.
const commonsMarker = "The evicted pod is usually not the culprit"

// writeCommonsFixture writes a resource-less generic playbook under dir/commons —
// the shape every real commons entry has — and returns "commons".
func writeCommonsFixture(t *testing.T, dir string) string {
	t.Helper()
	cd := filepath.Join(dir, "commons")
	if err := os.MkdirAll(cd, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := `---
type: Playbook
title: Node under memory pressure evicting pods
description: the kubelet evicts pods with reason Evicted when a node reports MemoryPressure
tags: [node, memory, pressure, eviction, evicted, overcommit, requests]
---

# Symptom

Pods on one node show Status: Evicted with "The node was low on resource: memory".
` + commonsMarker + `.

# Not covered

- Container-level OOMKills at a container's own limit.
`
	if err := os.WriteFile(filepath.Join(cd, "node-memory-pressure-eviction.md"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	return "commons"
}

// TestLoadParsesCommonsDir pins the schema addition: commons_dir is a DISTINCT root
// from catalog_dir. Pointing catalog_dir at a commons snapshot would load it as the
// operator's own catalog — entries unmarked, free to fire recall — which is the exact
// opposite of what a commons case must exercise.
func TestLoadParsesCommonsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.yaml"), []byte(`
name: grounded
prompt: node is evicting pods
commons_dir: fixtures/commons-memory
expect_recall: rejected
tools:
  pod_status: "Evicted"
expected:
  must_contain: [memory]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cases) != 1 || cases[0].CommonsDir != "fixtures/commons-memory" {
		t.Fatalf("commons_dir not parsed: %+v", cases[0])
	}
	if cases[0].CatalogDir != "" {
		t.Fatalf("commons_dir must not populate catalog_dir: %+v", cases[0])
	}
	if !cases[0].hasCatalog() {
		t.Fatal("a commons-only case must still count as catalog-bearing (it seeds recall + kb_search)")
	}
}

// TestRunOneCommonsGroundsKBSearchButNeverFiresRecall drives the real Catalog +
// Recall + kb_search stack through a commons-only case and asserts BOTH halves of
// the commons contract on the replay path:
//
//	positive — the playbook text reaches the model mid-loop, through kb_search;
//	negative — instant recall refuses it, so it can never answer the incident.
//
// The request deliberately carries NO workload. That is the matchScopeless tier, the
// one place a resource-less entry structurally agrees with an incident — so the
// rejection here is caused by the provenance skip in recall.go and nothing else.
func TestRunOneCommonsGroundsKBSearchButNeverFiresRecall(t *testing.T) {
	dir := t.TempDir()
	commons := writeCommonsFixture(t, dir)
	base := Case{
		Name:     "node-eviction",
		Prompt:   "node ip-10-0-22-115 is at 97% memory utilisation and the kubelet is evicting pods",
		Recall:   &CaseRecall{MinScore: 0.01, SoloFloor: 0.01, MarginGap: 0.01},
		Tools:    map[string]string{"pod_status": "checkout-api-abc  Failed  Evicted  The node was low on resource: memory"},
		Expected: Expected{MustContain: []string{"memory"}},
		dir:      dir,
	}

	c := base
	c.CommonsDir = commons
	c.ExpectRecall = "rejected"
	model := &recordingModel{resp: []providers.CompletionResponse{
		kbSearch("pods evicted node low on memory"),
		findings("the node ran out of memory because a neighbour overcommitted it"),
		verdict("keep"),
	}}
	res := (&Runner{Model: model, Log: discardLog()}).runOne(context.Background(), c)
	if res.RecallFired {
		t.Fatalf("a COMMONS entry fired instant recall through the replay runner: %+v", res)
	}
	if !slices.Contains(model.offeredTools(), "kb_search") {
		t.Fatalf("a commons case must offer kb_search — it is the ONLY path the commons reaches the model, got %v", model.offeredTools())
	}
	if !strings.Contains(model.toolText(), commonsMarker) {
		t.Fatalf("the commons playbook never reached the model mid-loop; tool results were:\n%s", model.toolText())
	}
	if !res.Pass {
		t.Fatalf("commons case should pass: %+v", res)
	}

	// THE CONTROL. Same corpus, same request, same gates — loaded as the operator's
	// OWN catalog instead. Recall fires and short-circuits, so the rejection above is
	// attributable to provenance alone and not to a corpus recall could never match.
	// Without this, expect_recall: rejected would pass just as happily on an empty
	// index and assert nothing.
	own := base
	own.CatalogDir = commons
	own.ExpectRecall = "short_circuit"
	ownModel := &recordingModel{resp: []providers.CompletionResponse{verdict("keep")}}
	ownRes := (&Runner{Model: ownModel, Log: discardLog()}).runOne(context.Background(), own)
	if !ownRes.RecallFired || !ownRes.RecallShortCircuit {
		t.Fatalf("control: the identical corpus loaded as the operator's OWN catalog must fire recall, else the commons rejection proves nothing: %+v", ownRes)
	}
}

// TestRunOneCatalogOnlyCaseOffersKBSearchLikeProduction pins the replay's tool surface
// to production's rule: kb_search is offered whenever a catalog EXISTS, not only when a
// commons is configured.
//
// This assertion is the inverse of the one this test shipped with, deliberately.
// Production (app.BuildModelAndTools) appends KBSearchTool on `cat != nil`, above and
// independent of instant recall and of the commons, and internal/eval's own live arm
// takes its BaseTools from that same function. A commons-only gate left the replay arm
// as the odd one out, so a catalog_dir case replayed a tool surface no deployment ever
// ships: after verify withdraws a recalled entry, production's model can still look the
// entry up and the replay's could not. Matching production is worth more than holding
// the previous tool surface fixed.
func TestRunOneCatalogOnlyCaseOffersKBSearchLikeProduction(t *testing.T) {
	dir := t.TempDir()
	c := Case{
		Name:       "catalog-only",
		Prompt:     "eval-victim pods not starting",
		Workload:   &CaseWorkload{Namespace: "runlore-eval", Name: "eval-victim"},
		CatalogDir: writeKBFixture(t, dir, "other-ns/other-app"),
		Recall:     &CaseRecall{MinScore: 0.01, SoloFloor: 0.01, MarginGap: 0.01},
		Tools:      map[string]string{"what_changed": "nothing relevant"},
		Expected:   Expected{MustContain: []string{"fresh"}},
		dir:        dir,
	}
	model := &recordingModel{resp: []providers.CompletionResponse{
		findings("fresh investigation result"), verdict("keep"),
	}}
	res := (&Runner{Model: model, Log: discardLog()}).runOne(context.Background(), c)
	if !slices.Contains(model.offeredTools(), "kb_search") {
		t.Fatalf("a catalog-bearing case must offer kb_search exactly as production does, got %v", model.offeredTools())
	}
	if !res.Pass {
		t.Fatalf("catalog-only case should still pass: %+v", res)
	}
}

// shippedCase returns the named case from the REAL examples/eval corpus, failing the
// test when it is absent. Fatal-on-miss rather than nil-returning is the point: every
// shipped-case guard below asserts nothing at all if a rename quietly turns it into a
// no-op, so the lookup itself has to be the thing that fails.
func shippedCase(t *testing.T, name string) Case {
	t.Helper()
	cases, err := Load(filepath.Join("..", "..", "examples", "eval"))
	if err != nil {
		t.Fatalf("Load examples/eval: %v", err)
	}
	for _, c := range cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("examples/eval has no case named %q", name)
	return Case{}
}

// TestShippedCommonsGroundingPairIsControlled guards the shipped A/B: the two cases
// must differ in the commons corpus and NOTHING ELSE. A paired eval whose halves
// drift apart stops measuring the commons and starts measuring the drift, and the
// published delta would keep looking like a number the whole time.
func TestShippedCommonsGroundingPairIsControlled(t *testing.T) {
	with := shippedCase(t, "node-eviction-with-commons")
	without := shippedCase(t, "node-eviction-no-commons")
	if with.CommonsDir == without.CommonsDir {
		t.Fatal("the pair must point at DIFFERENT commons roots — that is the only variable")
	}
	// Zero the only two fields allowed to differ, then require EVERYTHING else to be
	// equal. A field-by-field comparison can only ever guard the fields someone
	// remembered to list, and the fields most able to skew the measurement are the ones
	// least likely to be listed: alert_title feeds kbSearchEnrichment, so a divergence
	// there hands the two arms different kb_search queries; ground_truth is the LLM
	// judge's rubric; catalog_dir would give one arm real, non-commons recall. Whole-
	// struct equality also covers Case fields that do not exist yet.
	a, b := with, without
	a.Name, b.Name = "", ""
	a.CommonsDir, b.CommonsDir = "", ""
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("the paired cases must differ ONLY in name and commons_dir\nwith:    %+v\nwithout: %+v", a, b)
	}
	if with.Workload != nil {
		t.Fatal("the pair must stay workload-LESS: the scopeless tier is what makes expect_recall: rejected test the provenance skip")
	}
	if len(with.Expected.RootCauseEntities) == 0 || len(with.Expected.Distractors) == 0 {
		t.Fatal("the pair must be scored on entity precision (root_cause_entities + distractors), not keyword overlap")
	}
	if with.ExpectRecall != "rejected" {
		t.Fatalf("the commons case must assert expect_recall: rejected, got %q", with.ExpectRecall)
	}
	if with.gates() {
		t.Fatal("the pair is a MEASUREMENT — its failure is the finding, not a regression — so it must not vote on the nightly gate (gate: false)")
	}
}

// TestShippedCommonsCorpusIsGenericAndInert pins the fixture's load-bearing
// properties: the "with" corpus is real generic playbooks (resource-less, scoped by a
// "# Not covered" section) that load as COMMONS entries, and the "without" corpus is
// genuinely empty so the baseline models a fresh deployment.
func TestShippedCommonsCorpusIsGenericAndInert(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "examples", "eval"))
	if err != nil {
		t.Fatalf("Load examples/eval: %v", err)
	}
	checked := 0
	for _, c := range cases {
		if c.Name != "node-eviction-with-commons" && c.Name != "node-eviction-no-commons" {
			continue
		}
		checked++
		cat, err := c.buildCatalog(context.Background())
		if err != nil {
			t.Fatalf("%s: build catalog: %v", c.Name, err)
		}
		entries := cat.Entries()
		if c.Name == "node-eviction-no-commons" {
			if len(entries) != 0 {
				t.Fatalf("the baseline must index NOTHING (a fresh deployment), got %d entries", len(entries))
			}
			continue
		}
		if len(entries) < 2 {
			t.Fatalf("the commons corpus must hold several playbooks, got %d", len(entries))
		}
		for _, e := range entries {
			if !e.Commons {
				t.Fatalf("%s loaded UNMARKED — a commons_dir entry that is not Commons is free to fire recall", e.Path)
			}
			if e.Type != "Playbook" {
				t.Fatalf("%s: commons entries are generic playbooks, got type %q", e.Path, e.Type)
			}
			if e.Resource != "" {
				t.Fatalf("%s: a commons entry must be resource-less, got %q", e.Path, e.Resource)
			}
			if !strings.Contains(e.Body, "# Not covered") {
				t.Fatalf("%s: scope discipline is load-bearing — every commons entry states its own boundary", e.Path)
			}
		}
	}
	// Without this the loop is vacuous: rename or delete either arm and every
	// assertion above is simply skipped, leaving a green test guarding nothing.
	if checked != 2 {
		t.Fatalf("expected to check both arms of the commons pair, checked %d", checked)
	}
}

// TestShippedCommonsCaseGroundsTheLoop replays the shipped commons case with a
// scripted model — no API key, no network — so nightly CI cannot silently degrade to
// "the commons was configured but never reached the model". It asserts the playbook
// body is what kb_search handed back, and that recall still refused it.
func TestShippedCommonsCaseGroundsTheLoop(t *testing.T) {
	c := shippedCase(t, "node-eviction-with-commons")
	model := &recordingModel{resp: []providers.CompletionResponse{
		kbSearch("pods evicted node low on memory"),
		findings("analytics/report-worker consumed the node: its memory request is 256Mi while it uses 7.42Gi, so the scheduler overcommitted ip-10-0-22-115 and the kubelet evicted its neighbours; raise its memory request"),
		verdict("keep"),
	}}
	res := (&Runner{Model: model, Log: discardLog()}).runOne(context.Background(), c)
	if res.RecallFired {
		t.Fatalf("the shipped commons case must never fire instant recall: %+v", res)
	}
	if !strings.Contains(model.toolText(), "The evicted pod is usually not the culprit") {
		t.Fatalf("the shipped commons corpus never reached the model through kb_search:\n%s", model.toolText())
	}
	if !res.Pass {
		t.Fatalf("the shipped commons case should pass on a finding that names the true consumer: %+v", res)
	}
}

// TestShippedCommonsControlArmReplaysAgainstAnEmptyIndex replays the OTHER half of the
// pair end to end. Without it the control was only ever checked statically (its corpus
// indexes nothing), leaving the arm that actually produces the baseline number
// unexercised: kb_search over an empty index has its own return path
// ("no matching catalog entries"), and a baseline that errored or hung mid-loop would
// surface as a mysterious nightly failure rather than as a number.
func TestShippedCommonsControlArmReplaysAgainstAnEmptyIndex(t *testing.T) {
	c := shippedCase(t, "node-eviction-no-commons")
	model := &recordingModel{resp: []providers.CompletionResponse{
		kbSearch("pods evicted node low on memory"),
		findings("analytics/report-worker consumed the node: its memory request is 256Mi while it uses 7.42Gi, so the scheduler overcommitted ip-10-0-22-115 and the kubelet evicted its neighbours; raise its memory request"),
		verdict("keep"),
	}}
	res := (&Runner{Model: model, Log: discardLog()}).runOne(context.Background(), c)
	if !slices.Contains(model.offeredTools(), "kb_search") {
		t.Fatalf("both arms must offer the same tools, or the pair measures the tool surface too, got %v", model.offeredTools())
	}
	if !strings.Contains(model.toolText(), "no matching catalog entries") {
		t.Fatalf("the control's kb_search must answer from an EMPTY index; tool results were:\n%s", model.toolText())
	}
	if res.RecallFired {
		t.Fatalf("an empty catalog cannot fire recall: %+v", res)
	}
	if !res.Pass {
		t.Fatalf("the control arm must still complete and score on the same finding: %+v", res)
	}
}
