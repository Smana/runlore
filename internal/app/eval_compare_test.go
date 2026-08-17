// SPDX-License-Identifier: Apache-2.0

package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/eval"
)

// mockModelServer is a keyless, offline OpenAI-compatible /chat/completions
// endpoint that streams (SSE) the tool calls the replay loop and the LLM-judge
// need: it scripts what_changed → query_metrics → query_logs → submit_findings by
// counting the tool-role messages already in the request, and answers a judge turn
// (identified by the submit_grade tool) with a fixed rubric grade plus a usage
// block. It lets the whole model-comparison pipeline run in `go test` without any
// API key — the same double the e2e uses, reduced to what this test exercises and
// speaking SSE (which the streaming client requires).
func mockModelServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		for _, tool := range req.Tools {
			if tool.Function.Name == "submit_grade" {
				writeSSE(t, w, "submit_grade",
					`{"scores":{"root_cause":3,"evidence":3,"solution":2,"description":2,"calibration":2},"confident_wrong":false,"rationale":"mock: correct root cause"}`)
				return
			}
		}
		toolResults := 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults++
			}
		}
		var name, args string
		switch toolResults {
		case 0:
			name, args = "what_changed", `{"namespace":"apps"}`
		case 1:
			name, args = "query_metrics", `{"query":"up"}`
		case 2:
			name, args = "query_logs", `{"query":"error","since_minutes":30}`
		default:
			name, args = "submit_findings",
				`{"confidence":0.9,"root_causes":[{"summary":"mock: chart bump broke harbor-db migrations","confidence":0.9,"evidence":["pg_up=0"],"suggested_action":"flux rollback hr/harbor","reversible":true}],"unresolved":[]}`
		}
		writeSSE(t, w, name, args)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeSSE streams one tool call as chat/completions SSE: a tool-call delta chunk,
// a finish_reason chunk, a usage chunk (so token totals + cost are exercised), then
// the [DONE] sentinel.
func writeSSE(t *testing.T, w http.ResponseWriter, name, args string) {
	t.Helper()
	fl, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("test server needs http.Flusher")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	call, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "c1", "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}}},
	})
	for _, event := range []string{
		"data: " + string(call) + "\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":100}}` + "\n\n",
		"data: [DONE]\n\n",
	} {
		_, _ = io.WriteString(w, event)
		fl.Flush()
	}
}

// writeCompareCase writes a replay case carrying ground truth so the comparison
// exercises coverage + rubric grading, not only keyword pass/fail.
func writeCompareCase(t *testing.T, dir string) {
	t.Helper()
	body := `
name: harbor-chart-bump
prompt: HarborProbeFailure in apps
tools:
  what_changed: "chart 1.15 enabled DB migrations"
  query_metrics: "up{job=harbor-core}=0"
  query_logs: "harbor-db FATAL migration lock"
expected:
  must_contain: [chart, harbor-db]
  min_confidence: 0.5
ground_truth:
  root_cause: "the chart bump enabled a DB migration that stalled harbor-db"
  expected_sources: [gitops, metrics, logs]
  expected_action: "roll back the chart"
  must_reach_root: true
`
	if err := os.WriteFile(filepath.Join(dir, "harbor.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func minimalConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "runlore.yaml")
	if err := os.WriteFile(path, []byte("model:\n  provider: openai\n  model: x\n  base_url: http://unused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunEvalCompareOffline drives the whole comparison pipeline against the
// keyless mock model: two entries + a spec judge, over one ground-truthed replay
// case, and asserts the written report has the aggregated columns populated.
func TestRunEvalCompareOffline(t *testing.T) {
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
    prices: {input_usd: 3, output_usd: 15}
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
	if err := RunEvalCompare(cfg, nil, comparePath, casesDir, reportDir, "2026-07-02T00:00:00Z", 2, "", "", "", ""); err != nil {
		t.Fatalf("RunEvalCompare: %v", err)
	}

	jsonPath := filepath.Join(reportDir, "2026-07-02T00-00-00Z-compare.json")
	raw, err := os.ReadFile(jsonPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	rep, err := eval.ParseComparisonReport(raw)
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if rep.N != 2 || len(rep.Models) != 2 {
		t.Fatalf("header: N=%d models=%d", rep.N, len(rep.Models))
	}
	if rep.Judge != "openai/mock-judge" {
		t.Fatalf("judge disclosure: %q", rep.Judge)
	}
	// Spec order preserved.
	if rep.Models[0].Name != "model-a" || rep.Models[1].Name != "model-b" {
		t.Fatalf("model order: %+v", rep.Models)
	}

	a := rep.Models[0]
	// The scripted loop names harbor-db with confidence 0.9 → case passes both runs.
	if a.PassRate != 1.0 || a.Reached != 1 || a.Total != 1 {
		t.Fatalf("model-a pass aggregation: %+v", a)
	}
	// Coverage: what_changed(gitops) + query_metrics(metrics) + query_logs(logs) = all 3 expected → 1.0.
	if a.CoverageMedian != 1.0 {
		t.Fatalf("model-a coverage median: %v", a.CoverageMedian)
	}
	// Judge graded every run: root_cause median 3, no confident-wrong.
	if a.RubricMedian == nil || a.RubricMedian["root_cause"] != 3 {
		t.Fatalf("model-a rubric: %+v", a.RubricMedian)
	}
	if a.GradedRuns != 2 || a.ConfidentWrong != 0 {
		t.Fatalf("model-a graded=%d confidentWrong=%d", a.GradedRuns, a.ConfidentWrong)
	}
	// Usage accumulates across every completion (loop + judge); cost = prices × MTok.
	if a.InputTokens == 0 || a.OutputTokens == 0 {
		t.Fatalf("model-a tokens not recorded: %+v", a)
	}
	if a.CostUSD == nil || *a.CostUSD <= 0 {
		t.Fatalf("model-a cost: %v", a.CostUSD)
	}
	// model-b supplied no prices → no cost.
	if rep.Models[1].CostUSD != nil {
		t.Fatalf("model-b must have no cost: %v", rep.Models[1].CostUSD)
	}

	// The markdown sibling is written and shows the cost column (some entry priced).
	md, err := os.ReadFile(filepath.Join(reportDir, "2026-07-02T00-00-00Z-compare.md")) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read markdown: %v", err)
	}
	if !strings.Contains(string(md), "est. cost (USD)") || !strings.Contains(string(md), "openai/mock-judge") {
		t.Fatalf("markdown missing expected columns:\n%s", md)
	}
}

// TestRunEvalCompareNoConfigFile drives the documented one-liner
// (`lore eval --compare ... --cases ... -n 3`) through the real CLI entry point
// (RunEval, not RunEvalCompare directly) from a working directory that has NO
// runlore.yaml at all. The spec carries its own judge block, so — per
// benchmarking.md — this must succeed: config.Load's "file absent" error must be
// forgiven for --compare on the untouched default --config path.
func TestRunEvalCompareNoConfigFile(t *testing.T) {
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
`, base, base)
	comparePath := filepath.Join(dir, "compare.yaml")
	if err := os.WriteFile(comparePath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	reportDir := filepath.Join(dir, "reports")

	// A fresh, empty working directory: no runlore.yaml anywhere near it. RunEval's
	// --config default ("runlore.yaml") resolves relative to cwd, so this is what
	// actually exercises the "no config file present" branch — cwd must not resolve
	// to the config.Load call at all.
	t.Chdir(t.TempDir())

	err := RunEval([]string{
		"--compare", comparePath,
		"--cases", casesDir,
		"--report-dir", reportDir,
		"--stamp", "2026-07-02T00:00:00Z",
		"-n", "1",
	})
	if err != nil {
		t.Fatalf("RunEval with no runlore.yaml present: %v", err)
	}

	if _, err := os.Stat(filepath.Join(reportDir, "2026-07-02T00-00-00Z-compare.json")); err != nil {
		t.Fatalf("report not written: %v", err)
	}
}

// TestRunEvalCompareExplicitConfigMissingErrors asserts that a typo'd,
// explicitly-passed --config still hard-fails even on the --compare path: only the
// untouched DEFAULT path forgives an absent config file, never an explicit one.
func TestRunEvalCompareExplicitConfigMissingErrors(t *testing.T) {
	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeCompareCase(t, casesDir)

	spec := `
judge:
  provider: openai
  base_url: http://unused
  model: mock-judge
models:
  - name: model-a
    provider: openai
    base_url: http://unused
    model: mock-a
`
	comparePath := filepath.Join(dir, "compare.yaml")
	if err := os.WriteFile(comparePath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	err := RunEval([]string{
		"--config", filepath.Join(dir, "does-not-exist.yaml"),
		"--compare", comparePath,
		"--cases", casesDir,
		"--report-dir", filepath.Join(dir, "reports"),
	})
	if err == nil {
		t.Fatal("explicit --config pointing at a missing file must still error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want a not-exist error, got: %v", err)
	}
}

// TestRunEvalCompareDefaultConfigMalformedErrors asserts that a runlore.yaml that
// EXISTS but fails to parse is never silently treated as "absent": only a missing
// file is forgiven on the default --config path, never a broken one.
func TestRunEvalCompareDefaultConfigMalformedErrors(t *testing.T) {
	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeCompareCase(t, casesDir)

	spec := `
judge:
  provider: openai
  base_url: http://unused
  model: mock-judge
models:
  - name: model-a
    provider: openai
    base_url: http://unused
    model: mock-a
`
	comparePath := filepath.Join(dir, "compare.yaml")
	if err := os.WriteFile(comparePath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	// Unknown field: KnownFields(true) rejects it, so this is a parse error, not a
	// missing-file error — errors.Is(err, fs.ErrNotExist) must be false for it.
	if err := os.WriteFile(filepath.Join(workDir, "runlore.yaml"), []byte("model:\n  bogus_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workDir)

	err := RunEval([]string{
		"--compare", comparePath,
		"--cases", casesDir,
		"--report-dir", filepath.Join(dir, "reports"),
	})
	if err == nil {
		t.Fatal("malformed default runlore.yaml must still error, not be silently skipped")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a malformed file must not look like a missing one: %v", err)
	}
}

// TestRunEvalCompareNoJudgeSourceErrors asserts the third precedence rule from
// benchmarking.md end to end: with no --judge-* flags, no spec judge: block, and a
// zero-value config (no config.model), --compare must fail with a clear,
// actionable error instead of silently disabling rubric grading.
func TestRunEvalCompareNoJudgeSourceErrors(t *testing.T) {
	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeCompareCase(t, casesDir)

	// No judge: block at all.
	spec := `
models:
  - name: model-a
    provider: openai
    base_url: http://127.0.0.1:0
    model: mock-a
`
	comparePath := filepath.Join(dir, "compare.yaml")
	if err := os.WriteFile(comparePath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	// Judge resolution happens before any per-entry model call, so this must fail
	// fast without ever dialing the (deliberately unreachable) entry endpoint.
	err := RunEvalCompare(&config.Config{}, nil, comparePath, casesDir, filepath.Join(dir, "reports"), "2026-07-02T00:00:00Z", 1, "", "", "", "")
	if err == nil {
		t.Fatal("compare with no judge source (no flags, no spec judge, no config.model) must error")
	}
	if !strings.Contains(err.Error(), "judge") {
		t.Fatalf("error should explain the missing judge, got: %v", err)
	}
}

// TestRunEvalNonCompareStillRequiresConfig locks down that the --compare carve-out
// does not leak into the plain `lore eval` path: with no runlore.yaml present and
// no --compare flag, RunEval must fail exactly as it always has.
func TestRunEvalNonCompareStillRequiresConfig(t *testing.T) {
	dir := t.TempDir()
	casesDir := filepath.Join(dir, "cases")
	if err := os.MkdirAll(casesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeCompareCase(t, casesDir)

	t.Chdir(t.TempDir())

	err := RunEval([]string{"--cases", casesDir})
	if err == nil {
		t.Fatal("plain eval with no runlore.yaml present must still error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want a not-exist error from config.Load, got: %v", err)
	}
}
