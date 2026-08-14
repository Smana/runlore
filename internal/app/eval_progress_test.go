// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRunEvalReportsProgressToStderr pins the progress output the nightly eval needs
// to be readable while it runs.
//
// The replay campaign is sequential and used to print nothing until every case had
// finished, so the nightly job — ~30 live investigations — emitted 18 minutes of
// silence and then, when CI killed it at its timeout, nothing at all. "The eval is
// broken" was indistinguishable from "the eval is slow", and nothing said which case
// had eaten the budget.
//
// Two properties matter and both are asserted here: the lines appear on STDERR (stdout
// carries the result table, so progress must not contaminate it), and there is one
// line per case carrying the case name and a running count.
func TestRunEvalReportsProgressToStderr(t *testing.T) {
	srv := mockModelServer(t)
	base := srv.URL + "/v1"

	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Two cases so the running count has something to count. Reusing the shared
	// fixture for the first keeps this test about progress, not about scoring.
	writeCompareCase(t, casesDir)
	second := `
name: second-case
prompt: SomethingElse in apps
tools:
  what_changed: "unrelated change"
  query_metrics: "up=1"
  query_logs: "nothing"
expected:
  must_contain: [chart]
  min_confidence: 0.5
`
	if err := os.WriteFile(filepath.Join(casesDir, "second.yaml"), []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "runlore.yaml")
	cfg := fmt.Sprintf("model:\n  provider: openai\n  model: mock\n  base_url: %s\n", base)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr := captureStdoutStderr(t)

	// -fail-under 0 so scoring outcomes can never fail this test: it is about the
	// progress output, not about whether the mock model reaches a root cause.
	err := RunEval([]string{
		"-config", cfgPath,
		"-cases", casesDir,
		"-report-dir", filepath.Join(dir, "reports"),
		"-stamp", "2026-08-14T00:00:00Z",
		"-n", "1",
		"-fail-under", "0",
	})
	outText, errText := stdout(), stderr()
	if err != nil {
		t.Fatalf("RunEval: %v\nstderr:\n%s", err, errText)
	}

	// One progress line per case, in case order, each with a running count.
	progress := regexp.MustCompile(`(?m)^eval: \[(\d)/2\] +(\S+)`)
	matches := progress.FindAllStringSubmatch(errText, -1)
	if len(matches) != 2 {
		t.Fatalf("want 2 progress lines (one per case), got %d:\n%s", len(matches), errText)
	}
	for i, m := range matches {
		if want := fmt.Sprint(i + 1); m[1] != want {
			t.Errorf("progress line %d: want count %s, got %s", i, want, m[1])
		}
	}
	names := []string{matches[0][2], matches[1][2]}
	for _, want := range []string{"harbor-chart-bump", "second-case"} {
		if !contains(names, want) {
			t.Errorf("progress must name each case; %q missing from %v", want, names)
		}
	}
	// Elapsed time is what makes a stalled case identifiable rather than merely late.
	if !strings.Contains(errText, "elapsed=") {
		t.Errorf("progress lines must carry elapsed time:\n%s", errText)
	}

	// Progress must NOT land on stdout: that stream is the result table, and the
	// nightly pipes it around. The per-case verdict lines there are a different,
	// pre-existing format ("REACHED  <name>  pass-rate=…"), with no "eval: [" prefix.
	if strings.Contains(outText, "eval: [") {
		t.Errorf("progress leaked onto stdout:\n%s", outText)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// captureStdoutStderr swaps both streams for pipes and returns readers that drain
// them. Both are read concurrently so a chatty run cannot fill a pipe buffer and
// deadlock the code under test.
func captureStdoutStderr(t *testing.T) (func() string, func() string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW

	outCh, errCh := make(chan string, 1), make(chan string, 1)
	go func() { b, _ := io.ReadAll(outR); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errCh <- string(b) }()

	var outText, errText string
	var done bool
	finish := func() {
		if done {
			return
		}
		done = true
		os.Stdout, os.Stderr = origOut, origErr
		_ = outW.Close()
		_ = errW.Close()
		outText, errText = <-outCh, <-errCh
	}
	t.Cleanup(finish)
	return func() string { finish(); return outText }, func() string { finish(); return errText }
}
