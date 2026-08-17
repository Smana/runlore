// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/eval"
)

// TestCompareHaltsOnTheCampaignTokenCeiling drives the real `lore eval --compare`
// pipeline against the offline mock model with a campaign ceiling small enough that
// the very first completion crosses it, and asserts the run STOPS.
//
// The unit tests in internal/eval prove the budget refuses and the loops honour a
// cancelled context. This one proves the wiring: that RunEvalCompare actually hands
// every model it builds to the budget and walks its entries under the budget's
// context. Wiring is exactly what was missing here in the first place — every eval
// runner had a correct loop and no ceiling reaching it — so the mechanism being
// right is not evidence the command is bounded.
func TestCompareHaltsOnTheCampaignTokenCeiling(t *testing.T) {
	srv := mockModelServer(t)
	base := srv.URL + "/v1"

	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeCompareCase(t, casesDir)

	spec := fmt.Sprintf(`
judge:
  provider: openai
  base_url: %s
  model: mock-judge
models:
  - name: model-a
    provider: openai
    base_url: %s
    model: mock-a
  - name: model-b
    provider: openai
    base_url: %s
    model: mock-b
`, base, base, base)
	comparePath := filepath.Join(dir, "compare.yaml")
	if err := os.WriteFile(comparePath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	reportDir := filepath.Join(dir, "reports")
	cfg, err := config.Load(minimalConfig(t, dir))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// The mock reports 1100 tokens per completion, so a 1000-token campaign ceiling
	// is crossed by the first one.
	budget := &eval.CampaignBudget{MaxTokens: 1000}
	if err := RunEvalCompare(cfg, budget, comparePath, casesDir, reportDir, "2026-07-02T00:00:00Z", 2, "", "", "", ""); err != nil {
		t.Fatalf("a halted campaign still writes its (partial) report: %v", err)
	}

	if !budget.Exceeded() {
		t.Fatalf("the budget never tripped after a campaign that spent %d tokens against a %d ceiling — "+
			"RunEvalCompare is not routing its models through the budget",
			budget.SpentTokens(), budget.MaxTokens)
	}

	raw, err := os.ReadFile(filepath.Join(reportDir, "2026-07-02T00-00-00Z-compare.json")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	rep, err := eval.ParseComparisonReport(raw)
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if len(rep.Models) != 1 {
		t.Errorf("the campaign benchmarked %d of 2 entries after its token ceiling tripped, want 1 — "+
			"a halted campaign must stop starting entries, not merely stop paying for them", len(rep.Models))
	}
}

// TestCompareWithoutACampaignCeilingRunsEveryEntry is the control for the test
// above: the same pipeline with no ceiling must benchmark BOTH entries. Without it
// the assertion "only one entry ran" could be satisfied by a pipeline that was
// broken for unrelated reasons.
func TestCompareWithoutACampaignCeilingRunsEveryEntry(t *testing.T) {
	srv := mockModelServer(t)
	base := srv.URL + "/v1"

	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeCompareCase(t, casesDir)

	spec := fmt.Sprintf(`
judge:
  provider: openai
  base_url: %s
  model: mock-judge
models:
  - name: model-a
    provider: openai
    base_url: %s
    model: mock-a
  - name: model-b
    provider: openai
    base_url: %s
    model: mock-b
`, base, base, base)
	comparePath := filepath.Join(dir, "compare.yaml")
	if err := os.WriteFile(comparePath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	reportDir := filepath.Join(dir, "reports")
	cfg, err := config.Load(minimalConfig(t, dir))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	budget := &eval.CampaignBudget{} // accounting only, no ceiling
	if err := RunEvalCompare(cfg, budget, comparePath, casesDir, reportDir, "2026-07-02T00:00:00Z", 1, "", "", "", ""); err != nil {
		t.Fatalf("RunEvalCompare: %v", err)
	}
	if budget.Exceeded() {
		t.Fatal("a budget with no ceiling must never trip")
	}
	// The accounting half is what makes the judge's spend visible at all: it is a
	// separate model, called once per graded run, that no per-entry counter wraps.
	if budget.SpentTokens() == 0 {
		t.Error("the campaign budget recorded no spend, so `lore eval` can report none")
	}

	raw, err := os.ReadFile(filepath.Join(reportDir, "2026-07-02T00-00-00Z-compare.json")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	rep, err := eval.ParseComparisonReport(raw)
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if len(rep.Models) != 2 {
		t.Fatalf("with no ceiling both entries must be benchmarked, got %d", len(rep.Models))
	}
}
