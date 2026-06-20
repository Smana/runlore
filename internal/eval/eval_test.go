package eval

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
