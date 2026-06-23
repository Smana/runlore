package eval

import (
	"context"
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

// scriptModel returns a fixed response sequence: call a tool, then submit_findings.
type scriptModel struct {
	calls int
}

func (m *scriptModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	if m.calls == 1 {
		return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
			{ID: "1", Name: "what_changed", Args: `{}`}}}, nil
	}
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "2", Name: "submit_findings", Args: `{"confidence":0.9,"root_causes":[{"summary":"chart bump broke harbor-db migrations"}]}`}}}, nil
}

func TestRun(t *testing.T) {
	cases := []Case{
		{Name: "hit", Prompt: "probe failing", Tools: map[string]string{"what_changed": "chart 1.15"},
			Expected: Expected{MustContain: []string{"chart", "harbor-db"}, MinConfidence: 0.5}},
		{Name: "miss", Prompt: "probe failing", Tools: map[string]string{"what_changed": "chart 1.15"},
			Expected: Expected{MustContain: []string{"network policy"}}},
	}
	r := &Runner{Model: &scriptModel{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rep := r.Run(context.Background(), cases)
	if len(rep.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(rep.Results))
	}
	if rep.Passed() != 1 || rep.RCARate() != 0.5 {
		t.Fatalf("want 1 passed / 0.5 rate, got passed=%d rate=%.2f", rep.Passed(), rep.RCARate())
	}
	if !rep.Results[0].Pass || rep.Results[1].Pass {
		t.Fatalf("unexpected per-case: %+v", rep.Results)
	}
}
