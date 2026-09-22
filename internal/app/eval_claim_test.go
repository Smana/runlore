// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunEvalPrintsWhatAMissedCaseClaimed pins the diagnostic a red nightly must
// carry in its own log.
//
// Every gated case in the nightly failed on one absent root_cause_entities substring
// and the summary printed only that term ("missing: harbor-db"). A reader could not
// tell whether the agent had named the right cause in looser words or blamed
// something else entirely, and the answer existed nowhere — not in the log, not in
// the uploaded report. Six weeks of red nightlies stayed undiagnosed because of it.
// The summary now prints what the failing repeats actually claimed.
func TestRunEvalPrintsWhatAMissedCaseClaimed(t *testing.T) {
	srv := mockModelServer(t)

	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// The mock always blames "chart bump broke harbor-db migrations", so a case that
	// requires an unrelated term misses — exactly the nightly's shape.
	body := `
name: claim-miss
prompt: HarborProbeFailure in apps
tools:
  what_changed: "chart 1.15 enabled DB migrations"
  query_metrics: "up{job=harbor-core}=0"
  query_logs: "harbor-db FATAL migration lock"
expected:
  must_contain: [network]
  min_confidence: 0.5
`
	if err := os.WriteFile(filepath.Join(casesDir, "claim-miss.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "runlore.yaml")
	cfg := fmt.Sprintf("model:\n  provider: openai\n  model: mock\n  base_url: %s/v1\n", srv.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _ := captureStdoutStderr(t)
	_ = RunEval([]string{
		"-config", cfgPath, "-cases", casesDir,
		"-n", "1", "-report-dir", filepath.Join(dir, "reports"),
	})
	out := stdout()

	if !strings.Contains(out, "missing: network") {
		t.Fatalf("expected the case to miss on network:\n%s", out)
	}
	if !strings.Contains(out, "claimed:") {
		t.Fatalf("summary must label what the failing run claimed:\n%s", out)
	}
	if !strings.Contains(out, "harbor-db migrations") {
		t.Fatalf("summary must print the claim text itself:\n%s", out)
	}
}

// TestRunEvalOmitsClaimLineWhenEverythingPasses keeps a green run's summary terse —
// the claim line is a failure diagnostic, not a transcript of every answer.
func TestRunEvalOmitsClaimLineWhenEverythingPasses(t *testing.T) {
	srv := mockModelServer(t)

	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeCompareCase(t, casesDir) // must_contain: [chart, harbor-db] — the mock passes it
	cfgPath := filepath.Join(dir, "runlore.yaml")
	cfg := fmt.Sprintf("model:\n  provider: openai\n  model: mock\n  base_url: %s/v1\n", srv.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _ := captureStdoutStderr(t)
	_ = RunEval([]string{
		"-config", cfgPath, "-cases", casesDir,
		"-n", "1", "-report-dir", filepath.Join(dir, "reports"),
	})
	out := stdout()

	if !strings.Contains(out, "REACHED") {
		t.Fatalf("expected the shared fixture to pass:\n%s", out)
	}
	if strings.Contains(out, "claimed:") {
		t.Fatalf("a green run must not print claim lines:\n%s", out)
	}
}
