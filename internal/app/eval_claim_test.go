// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runEvalOnCase writes one replay case, points the eval at a mock model, and returns
// what the run printed. Shared so a test that is about OUTPUT is not also a copy of
// RunEval's flag list — the scaffolding otherwise duplicates eval_progress_test.go.
func runEvalOnCase(t *testing.T, caseYAML string) (stdoutText, stderrText string) {
	t.Helper()
	srv := mockModelServer(t)
	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(casesDir, "case.yaml"), []byte(caseYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "runlore.yaml")
	cfg := fmt.Sprintf("model:\n  provider: openai\n  model: mock\n  base_url: %s/v1\n", srv.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := captureStdoutStderr(t)
	_ = RunEval([]string{
		"-config", cfgPath, "-cases", casesDir,
		"-n", "1", "-report-dir", filepath.Join(dir, "reports"),
	})
	return stdout(), stderr()
}

// claimCaseYAML is one replay case whose required term is the knob: the mock always
// blames "chart bump broke harbor-db migrations", so requiring harbor-db passes and
// requiring anything else misses — the nightly's shape.
const claimCaseYAML = `
name: claim-case
prompt: HarborProbeFailure in apps
tools:
  what_changed: "chart 1.15 enabled DB migrations"
  query_metrics: "up{job=harbor-core}=0"
  query_logs: "harbor-db FATAL migration lock"
expected:
  must_contain: [%s]
  min_confidence: 0.5
`

// TestRunEvalReportsWhatAMissedCaseClaimed pins the diagnostic a red nightly must carry
// in its own log: the missing TERM alone cannot distinguish a right cause phrased
// loosely from a wrong one (see eval.Result.Claim).
//
// The claim goes to STDERR, on the per-case progress line, because stdout carries the
// result table and because the nightly is often killed by its own timeout before the
// summary prints.
func TestRunEvalReportsWhatAMissedCaseClaimed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		term      string
		wantClaim bool
	}{
		{"a miss reports what was blamed", "network", true},
		{"every repeat passing reports nothing", "harbor-db", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := runEvalOnCase(t, fmt.Sprintf(claimCaseYAML, tc.term))

			if !tc.wantClaim {
				if !strings.Contains(stdout, "REACHED") {
					t.Fatalf("expected the case to pass:\n%s", stdout)
				}
				if strings.Contains(stdout+stderr, "claimed:") {
					t.Fatalf("an all-passing run must print no claim lines:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
				}
				return
			}
			if !strings.Contains(stdout, "missing: "+tc.term) {
				t.Fatalf("expected the case to miss on %s:\n%s", tc.term, stdout)
			}
			for _, want := range []string{"claimed:", "harbor-db migrations"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr must carry %q:\n%s", want, stderr)
				}
			}
			if strings.Contains(stdout, "claimed:") {
				t.Fatalf("the claim belongs on stderr, not in the stdout result table:\n%s", stdout)
			}
		})
	}
}
