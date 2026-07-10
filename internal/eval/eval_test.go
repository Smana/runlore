// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
