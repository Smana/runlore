# RunLore Eval Harness — Engine Implementation Plan (Plan 1 of 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the existing `internal/eval` replay harness with a **live-fire** layer — run scenarios against the real cluster, grade data-source coverage deterministically and RCA quality via an LLM-judge, and record each run into the existing `eval.Case` replay format.

**Architecture:** Six new files in `internal/eval` (scenario schema, coverage+recorder, judge, live runner, case recorder, report) plus a `--live` branch in `cmd/lore/main.go`'s `runEval`. The live runner wraps each live tool in a `recordingTool` decorator (no change to `LoopInvestigator`), runs each scenario N times, aggregates median/variance, and writes a markdown+JSON report. Scenario YAMLs + manifests + the baseline campaign are **Plan 2** (`...-scenario-catalog.md`); this plan ships one worked example scenario for integration.

**Tech Stack:** Go 1.26 stdlib + `gopkg.in/yaml.v3` (already used), `log/slog`. Reuses `providers.{Investigation,ModelProvider,CompletionRequest}`, `investigate.{Tool,LoopInvestigator,Request}`, and the existing `eval.{Case,Score,Report}`.

**Confirmed integration points (verbatim from the codebase):**
- `investigate.Tool` = `{ Name() string; Description() string; Schema() string; Call(ctx, args string) (string,error) }` (`internal/investigate/tools.go:12-18`).
- `investigate.LoopInvestigator{Model, Tools, Log, MaxSteps, OnComplete func(providers.Investigation), Actions, Recall, Verify}` (`loop.go:73-85`); `OnComplete` fires once with the final `Investigation`; the tool trace is **not** exposed.
- `providers.ModelProvider` = `{ Complete(ctx, CompletionRequest) (CompletionResponse, error) }`; `CompletionRequest{System, Messages []Message, Tools []ToolSpec}`; `CompletionResponse{Text, ToolCalls}` (`providers.go:255-404`).
- `providers.Investigation{Title, RootCauses []Hypothesis, Changes, Unresolved []string, Confidence float64, Actions, CuratedURL}`; `Hypothesis{Summary, Confidence, ChangeRef, Evidence []string, SuggestedAction, Reversible}`.
- Existing `eval.Case{Name, Prompt, Tools map[string]string, Expected}`; `eval.Expected{MustContain []string, MinConfidence float64}`; `eval.Score(name, inv, exp) Result`; `eval.Runner` replays via `staticTool` (`internal/eval/*.go`).
- Live tools/model built by `buildModelAndTools(ctx, cfg, gp, log) (model, []investigate.Tool, *Recall)` and `gitOpsFromKube(cfg, log)` in `cmd/lore/main.go:624,695`.
- Quality gate (from `CONTRIBUTING.md`): `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`; add `-race` for goroutine tests.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/eval/scenario.go` + `_test.go` *(create)* | `Scenario`/`GroundTruth`/`Trigger` schema + `LoadScenarios(dir)` |
| `internal/eval/coverage.go` + `_test.go` *(create)* | tool→data-source map, `recordingTool` decorator, `Recorder`, `ScoreCoverage` |
| `internal/eval/judge.go` + `_test.go` *(create)* | `Judge` iface, `ModelJudge`, `Rubric`, `Verdict` (per-dimension scores) |
| `internal/eval/live.go` + `_test.go` *(create)* | `LiveRunner`, `StepRunner` iface, `LiveResult`, `RunScenario` (precheck/setup/run×N/judge/teardown), median/variance, pass gate |
| `internal/eval/record.go` + `_test.go` *(create)* | `RecordedCase(scn, calls) eval.Case` + `WriteCase(dir, Case)` → replay corpus |
| `internal/eval/report.go` + `_test.go` *(create)* | `LiveReport` → markdown + JSON, coverage heatmap, regression diff |
| `cmd/lore/main.go` *(modify `runEval`, ~375-417)* | `--live` branch: build live tools + judge, run `LiveRunner`, write report; `shellStepRunner` |

---

## Task 1: Scenario schema + loader

**Files:**
- Create: `internal/eval/scenario.go`, `internal/eval/scenario_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/eval/scenario_test.go`:

```go
package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadScenarios(t *testing.T) {
	dir := t.TempDir()
	yaml := `
id: gitops-bad-image-tag
category: what-changed
description: bad image tag -> ImagePullBackOff
invasive: true
setup:
  - kubectl apply -f manifests/bad-tag.yaml
trigger:
  mode: cli
  symptom: app eval-victim pods not starting in ns runlore-eval
  namespace: runlore-eval
ground_truth:
  root_cause: image tag :v9.9.9 does not exist
  expected_sources: [gitops, kubernetes, logs]
  optional_sources: []
  expected_action: correct the image tag / flux rollback
  must_reach_root: true
teardown:
  - kubectl delete -f manifests/bad-tag.yaml --ignore-not-found
`
	if err := os.WriteFile(filepath.Join(dir, "s1.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	scns, err := LoadScenarios(dir)
	if err != nil {
		t.Fatalf("LoadScenarios: %v", err)
	}
	if len(scns) != 1 {
		t.Fatalf("want 1 scenario, got %d", len(scns))
	}
	s := scns[0]
	if s.ID != "gitops-bad-image-tag" || !s.Invasive || s.Trigger.Mode != "cli" {
		t.Fatalf("parse: %+v", s)
	}
	if len(s.GroundTruth.ExpectedSources) != 3 || !s.GroundTruth.MustReachRoot {
		t.Fatalf("ground_truth: %+v", s.GroundTruth)
	}
	if len(s.Setup) != 1 || len(s.Teardown) != 1 {
		t.Fatalf("steps: setup=%v teardown=%v", s.Setup, s.Teardown)
	}
}

func TestLoadScenariosIDFallsBackToFilename(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "harbor.yaml"), []byte("category: dependency\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scns, err := LoadScenarios(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scns) != 1 || scns[0].ID != "harbor" {
		t.Fatalf("want id=harbor from filename, got %+v", scns)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run TestLoadScenarios -v`
Expected: FAIL — `LoadScenarios`/`Scenario` undefined.

- [ ] **Step 3: Implement the schema + loader**

Create `internal/eval/scenario.go`:

```go
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scenario is one live-fire eval case: an induced-or-natural failure, how to
// trigger an investigation, and the ground truth to grade against. It is a
// superset of Case (which is the recorded/replay form this produces).
type Scenario struct {
	ID          string      `yaml:"id"`
	Category    string      `yaml:"category"`    // what-changed | saturation | network | cloud | dependency | cert | dns | storage | instant-recall
	Description string      `yaml:"description"`
	Invasive    bool        `yaml:"invasive"`    // true => has setup/teardown; false => natural failure
	Precheck    string      `yaml:"precheck"`    // optional shell; non-zero exit => SKIP (natural scenarios)
	Setup       []string    `yaml:"setup"`       // shell steps (kubectl/flux) to induce the fault
	Trigger     Trigger     `yaml:"trigger"`
	GroundTruth GroundTruth `yaml:"ground_truth"`
	Teardown    []string    `yaml:"teardown"`    // shell steps to revert; always run
}

// Trigger describes how the investigation is started.
type Trigger struct {
	Mode      string `yaml:"mode"`      // "cli" (default) | "webhook"
	Symptom   string `yaml:"symptom"`   // free-text incident description
	Namespace string `yaml:"namespace"` // affected namespace (optional)
}

// GroundTruth is the human-authored truth a scenario is graded against.
type GroundTruth struct {
	RootCause       string   `yaml:"root_cause"`
	ExpectedSources []string `yaml:"expected_sources"` // MANDATORY data-source groups -> coverage gate
	OptionalSources []string `yaml:"optional_sources"` // bonus if touched, never gates
	ExpectedAction  string   `yaml:"expected_action"`
	MustReachRoot   bool     `yaml:"must_reach_root"`
}

// LoadScenarios reads every *.yaml / *.yml scenario in dir.
func LoadScenarios(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var scns []Scenario
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := yaml.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if s.ID == "" {
			s.ID = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		}
		if s.Trigger.Mode == "" {
			s.Trigger.Mode = "cli"
		}
		scns = append(scns, s)
	}
	return scns, nil
}
```

(`isYAML` already exists in `case.go` — reuse it; do not redefine.)

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run TestLoadScenarios -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
cd /home/smana/Sources/runlore
git add internal/eval/scenario.go internal/eval/scenario_test.go
git commit -m "feat(eval): live-fire Scenario schema + loader"
```

---

## Task 2: Coverage track — tool→source map, recordingTool, scorer

**Files:**
- Create: `internal/eval/coverage.go`, `internal/eval/coverage_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/eval/coverage_test.go`:

```go
package eval

import (
	"context"
	"errors"
	"testing"
)

// fakeTool is a minimal investigate.Tool for decorator tests.
type fakeTool struct {
	name   string
	out    string
	err    error
}

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string  { return "fake " + f.name }
func (f fakeTool) Schema() string       { return `{"type":"object","properties":{}}` }
func (f fakeTool) Call(context.Context, string) (string, error) {
	return f.out, f.err
}

func TestRecordingToolRecordsCall(t *testing.T) {
	rec := &Recorder{}
	rt := recordingTool{inner: fakeTool{name: "pod_status", out: "phase=Pending"}, rec: rec}
	out, err := rt.Call(context.Background(), `{"namespace":"x"}`)
	if err != nil || out != "phase=Pending" {
		t.Fatalf("delegate broken: out=%q err=%v", out, err)
	}
	if rt.Name() != "pod_status" {
		t.Fatalf("name not delegated: %q", rt.Name())
	}
	calls := rec.Calls()
	if len(calls) != 1 || calls[0].Name != "pod_status" || calls[0].Output != "phase=Pending" {
		t.Fatalf("not recorded: %+v", calls)
	}
}

func TestRecordingToolRecordsError(t *testing.T) {
	rec := &Recorder{}
	rt := recordingTool{inner: fakeTool{name: "cloud_what_changed", err: errors.New("timeout")}, rec: rec}
	if _, err := rt.Call(context.Background(), "{}"); err == nil {
		t.Fatal("want error propagated")
	}
	if c := rec.Calls(); len(c) != 1 || c[0].Err == "" {
		t.Fatalf("error not recorded: %+v", c)
	}
}

func TestScoreCoverage(t *testing.T) {
	calls := []Call{
		{Name: "what_changed", Output: "diff"},
		{Name: "flux_resource_status", Output: "Ready=False"},
		{Name: "query_logs", Output: "boom"},
		{Name: "cloud_what_changed", Err: "timeout"},
	}
	cov := ScoreCoverage([]string{"gitops", "logs"}, []string{"aws"}, calls)
	if cov.Ratio != 1.0 {
		t.Fatalf("want full coverage, got %.2f (touched=%v missing=%v)", cov.Ratio, cov.Touched, cov.Missing)
	}
	if !cov.CrossSignal {
		t.Fatal("want cross-signal true (gitops+logs)")
	}
	if len(cov.Bonus) != 1 || cov.Bonus[0] != "aws" {
		t.Fatalf("want aws bonus, got %v", cov.Bonus)
	}
	if len(cov.ToolErrors) != 1 || cov.ToolErrors[0] != "cloud_what_changed" {
		t.Fatalf("want cloud_what_changed flagged, got %v", cov.ToolErrors)
	}

	miss := ScoreCoverage([]string{"gitops", "metrics"}, nil, calls)
	if miss.Ratio != 0.5 || len(miss.Missing) != 1 || miss.Missing[0] != "metrics" {
		t.Fatalf("want 0.5 + metrics missing, got %.2f %v", miss.Ratio, miss.Missing)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run 'TestRecordingTool|TestScoreCoverage' -v`
Expected: FAIL — `Recorder`/`recordingTool`/`Call`/`ScoreCoverage` undefined.

- [ ] **Step 3: Implement coverage**

Create `internal/eval/coverage.go`:

```go
package eval

import (
	"context"
	"sort"
	"sync"

	"github.com/Smana/runlore/internal/investigate"
)

// toolSource maps each investigation tool to its data-source group. submit_findings
// and any unknown tool map to "" (ignored by coverage).
var toolSource = map[string]string{
	"what_changed":          "gitops",
	"flux_resource_status":  "gitops",
	"flux_tree":             "gitops",
	"pod_status":            "kubernetes",
	"kube_events":           "kubernetes",
	"controller_logs":       "kubernetes",
	"query_metrics":         "metrics",
	"query_logs":            "logs",
	"network_drops":         "network",
	"cloud_what_changed":    "aws",
	"cloud_resource_health": "aws",
	"kb_search":             "kb",
}

// Call is one recorded tool invocation during a live investigation.
type Call struct {
	Name   string
	Args   string
	Output string
	Err    string
}

// Recorder collects tool calls made during one investigation run. Safe for
// concurrent use (the loop is sequential today, but tools may fan out later).
type Recorder struct {
	mu    sync.Mutex
	calls []Call
}

func (r *Recorder) record(c Call) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

// Calls returns a copy of the recorded calls in order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Call, len(r.calls))
	copy(out, r.calls)
	return out
}

// recordingTool wraps an investigate.Tool, recording every call (name, args,
// output, error) before returning the inner result unchanged.
type recordingTool struct {
	inner investigate.Tool
	rec   *Recorder
}

func (t recordingTool) Name() string       { return t.inner.Name() }
func (t recordingTool) Description() string { return t.inner.Description() }
func (t recordingTool) Schema() string      { return t.inner.Schema() }

func (t recordingTool) Call(ctx context.Context, args string) (string, error) {
	out, err := t.inner.Call(ctx, args)
	c := Call{Name: t.inner.Name(), Args: args, Output: out}
	if err != nil {
		c.Err = err.Error()
	}
	t.rec.record(c)
	return out, err
}

// wrap decorates each tool with the recorder.
func wrap(tools []investigate.Tool, rec *Recorder) []investigate.Tool {
	out := make([]investigate.Tool, len(tools))
	for i, tl := range tools {
		out[i] = recordingTool{inner: tl, rec: rec}
	}
	return out
}

// Coverage is the deterministic data-source coverage result for one run.
type Coverage struct {
	Touched    []string // mandatory source groups actually exercised
	Missing    []string // mandatory groups never touched
	Bonus      []string // optional groups touched
	CrossSignal bool    // >=2 distinct source groups exercised
	ToolErrors []string // tool names that returned an error
	Ratio      float64  // |touched| / |expected|  (1.0 when no expected sources)
}

// ScoreCoverage computes coverage of the mandatory expected sources from the
// recorded calls. optional sources count as Bonus and never affect Ratio.
func ScoreCoverage(expected, optional []string, calls []Call) Coverage {
	seen := map[string]bool{}
	var cov Coverage
	for _, c := range calls {
		if c.Err != "" {
			cov.ToolErrors = append(cov.ToolErrors, c.Name)
		}
		if grp := toolSource[c.Name]; grp != "" {
			seen[grp] = true
		}
	}
	cov.CrossSignal = len(seen) >= 2
	for _, e := range expected {
		if seen[e] {
			cov.Touched = append(cov.Touched, e)
		} else {
			cov.Missing = append(cov.Missing, e)
		}
	}
	for _, o := range optional {
		if seen[o] {
			cov.Bonus = append(cov.Bonus, o)
		}
	}
	sort.Strings(cov.Touched)
	sort.Strings(cov.Missing)
	if len(expected) == 0 {
		cov.Ratio = 1.0
	} else {
		cov.Ratio = float64(len(cov.Touched)) / float64(len(expected))
	}
	return cov
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run 'TestRecordingTool|TestScoreCoverage' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/smana/Sources/runlore
git add internal/eval/coverage.go internal/eval/coverage_test.go
git commit -m "feat(eval): deterministic coverage track (recordingTool + tool->source map)"
```

---

## Task 3: LLM-judge (RCA quality rubric)

**Files:**
- Create: `internal/eval/judge.go`, `internal/eval/judge_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/eval/judge_test.go`:

```go
package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// jsonModel returns a fixed Text payload, and records the request it saw.
type jsonModel struct {
	reply string
	gotSystem string
	gotUser   string
}

func (m *jsonModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.gotSystem = req.System
	for _, msg := range req.Messages {
		m.gotUser += msg.Content
	}
	return providers.CompletionResponse{Text: m.reply}, nil
}

func TestModelJudgeParsesVerdict(t *testing.T) {
	m := &jsonModel{reply: "prefix junk\n" + `{"scores":{"root_cause":3,"evidence":2,"solution":2,"description":3,"calibration":2},"confident_wrong":false,"rationale":"correct + deep"}` + "\ntrailing"}
	j := ModelJudge{Model: m}
	scn := Scenario{ID: "x", GroundTruth: GroundTruth{RootCause: "valkey down", ExpectedAction: "restart valkey"}}
	inv := providers.Investigation{Title: "Harbor down", Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "valkey refused", SuggestedAction: "restart valkey"}}}
	v, err := j.Grade(context.Background(), scn, inv)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if v.Scores["root_cause"] != 3 || v.ConfidentWrong {
		t.Fatalf("verdict parse: %+v", v)
	}
	// judge prompt must carry the ground truth and the investigation, and must NOT
	// reveal which model produced it (blind).
	if !strings.Contains(m.gotUser, "valkey down") || !strings.Contains(m.gotUser, "valkey refused") {
		t.Fatalf("prompt missing ground truth/investigation: %q", m.gotUser)
	}
}

func TestRubricMax(t *testing.T) {
	if RubricMax() != 14 { // 3+3+3+3+2
		t.Fatalf("rubric max = %d, want 14", RubricMax())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run 'TestModelJudge|TestRubric' -v`
Expected: FAIL — `ModelJudge`/`Grade`/`RubricMax` undefined.

- [ ] **Step 3: Implement the judge**

Create `internal/eval/judge.go`:

```go
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// Dimension is one rubric axis and its max score.
type Dimension struct {
	Key string
	Max int
}

// Rubric is the RCA-quality grading rubric (matches the design spec §5).
var Rubric = []Dimension{
	{"root_cause", 3},   // 0 wrong / 1 symptom-only / 2 correct-shallow / 3 correct+root
	{"evidence", 3},     // cited facts pertinent & true
	{"solution", 3},     // suggested action vs expected: correct, actionable, reversibility right
	{"description", 3},  // clarity, completeness, honest unresolved
	{"calibration", 2},  // high confidence only when correct
}

// RubricMax is the maximum total score across all dimensions.
func RubricMax() int {
	n := 0
	for _, d := range Rubric {
		n += d.Max
	}
	return n
}

// Verdict is the judge's structured grade for one investigation.
type Verdict struct {
	Scores         map[string]int `json:"scores"`
	ConfidentWrong bool           `json:"confident_wrong"`
	Rationale      string         `json:"rationale"`
}

// Total sums the dimension scores.
func (v Verdict) Total() int {
	n := 0
	for _, d := range Rubric {
		n += v.Scores[d.Key]
	}
	return n
}

// Judge grades an investigation against a scenario's ground truth.
type Judge interface {
	Grade(ctx context.Context, scn Scenario, inv providers.Investigation) (Verdict, error)
}

// ModelJudge grades with an LLM (use a stronger model than the one under test).
type ModelJudge struct {
	Model providers.ModelProvider
}

const judgeSystem = `You are a strict, impartial SRE incident-investigation grader.
You are given the GROUND TRUTH for an incident and an ANONYMOUS investigation result
(you do not know which model produced it — grade only on merit).
Score each rubric dimension as an integer in [0, max]:
- root_cause (max 3): 0 wrong, 1 symptom-only, 2 correct but shallow, 3 correct AND reaches the true root.
- evidence (max 3): cited facts pertinent and true, not hallucinated or correlation-only.
- solution (max 3): suggested action vs expected — correct, actionable, reversibility flagged right.
- description (max 3): clarity, completeness, honest about what is unresolved.
- calibration (max 2): high confidence only when correct; penalise confident-and-wrong hardest.
Set confident_wrong=true if the result states a wrong root cause with confidence >= 0.7.
Reply with ONLY a JSON object: {"scores":{"root_cause":N,"evidence":N,"solution":N,"description":N,"calibration":N},"confident_wrong":bool,"rationale":"..."}.`

// Grade builds a blind grading prompt and parses the JSON verdict.
func (j ModelJudge) Grade(ctx context.Context, scn Scenario, inv providers.Investigation) (Verdict, error) {
	user := fmt.Sprintf(`GROUND TRUTH
root_cause: %s
expected_action: %s
must_reach_root: %t

INVESTIGATION RESULT
%s`, scn.GroundTruth.RootCause, scn.GroundTruth.ExpectedAction, scn.GroundTruth.MustReachRoot, investigationText(inv)+confidenceLine(inv))

	resp, err := j.Model.Complete(ctx, providers.CompletionRequest{
		System:   judgeSystem,
		Messages: []providers.Message{{Role: "user", Content: user}},
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("judge model: %w", err)
	}
	v, err := parseVerdict(resp.Text)
	if err != nil {
		return Verdict{}, fmt.Errorf("parse verdict from %q: %w", resp.Text, err)
	}
	return v, nil
}

func confidenceLine(inv providers.Investigation) string {
	return fmt.Sprintf(" (overall confidence %.2f)", inv.Confidence)
}

// parseVerdict extracts the first JSON object from the model text (models often
// wrap JSON in prose despite instructions).
func parseVerdict(s string) (Verdict, error) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return Verdict{}, fmt.Errorf("no JSON object found")
	}
	var v Verdict
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return Verdict{}, err
	}
	if v.Scores == nil {
		v.Scores = map[string]int{}
	}
	return v, nil
}
```

(`investigationText` already exists in `score.go` — reuse it.)

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run 'TestModelJudge|TestRubric' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/smana/Sources/runlore
git add internal/eval/judge.go internal/eval/judge_test.go
git commit -m "feat(eval): LLM-judge RCA-quality rubric (blind, structured verdict)"
```

---

## Task 4: Case recorder (live run → replay corpus)

**Files:**
- Create: `internal/eval/record.go`, `internal/eval/record_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/eval/record_test.go`:

```go
package eval

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRecordedCase(t *testing.T) {
	scn := Scenario{ID: "harbor-valkey", Trigger: Trigger{Symptom: "harbor core crashlooping"},
		GroundTruth: GroundTruth{RootCause: "valkey down"}}
	calls := []Call{
		{Name: "pod_status", Output: "first"},
		{Name: "pod_status", Output: "second"}, // last wins (v1 single-output replay limit)
		{Name: "query_logs", Output: "connection refused"},
		{Name: "submit_findings", Output: "ignored"}, // reserved, excluded
	}
	c := RecordedCase(scn, calls)
	if c.Name != "harbor-valkey" || c.Prompt != "harbor core crashlooping" {
		t.Fatalf("meta: %+v", c)
	}
	if c.Tools["pod_status"] != "second" || c.Tools["query_logs"] != "connection refused" {
		t.Fatalf("tools: %+v", c.Tools)
	}
	if _, ok := c.Tools["submit_findings"]; ok {
		t.Fatal("submit_findings must be excluded")
	}
}

func TestWriteCase(t *testing.T) {
	dir := t.TempDir()
	c := Case{Name: "x", Prompt: "p", Tools: map[string]string{"pod_status": "Pending"}}
	if err := WriteCase(dir, c); err != nil {
		t.Fatalf("WriteCase: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "x.yaml"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got Case
	if err := yaml.Unmarshal(data, &got); err != nil || got.Name != "x" || got.Tools["pod_status"] != "Pending" {
		t.Fatalf("roundtrip: %+v err=%v", got, err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run 'TestRecordedCase|TestWriteCase' -v`
Expected: FAIL — `RecordedCase`/`WriteCase` undefined.

- [ ] **Step 3: Implement the recorder**

Create `internal/eval/record.go`:

```go
package eval

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// RecordedCase converts a live run's recorded tool calls into a replayable Case
// (the existing examples/eval format). v1 keeps the LAST output per tool: the
// replay staticTool returns one fixed output per tool regardless of args, so
// multi-call tools are flattened. submit_findings is excluded (it is the model's
// own output, not evidence).
func RecordedCase(scn Scenario, calls []Call) Case {
	tools := map[string]string{}
	for _, c := range calls {
		if c.Name == "submit_findings" {
			continue
		}
		tools[c.Name] = c.Output
	}
	return Case{
		Name:   scn.ID,
		Prompt: scn.Trigger.Symptom,
		Tools:  tools,
		Expected: Expected{
			MustContain:   nil, // authored later when promoting a fixture into the regression set
			MinConfidence: 0,
		},
	}
}

// WriteCase writes a Case as <dir>/<name>.yaml.
func WriteCase(dir string, c Case) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, c.Name+".yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write case %s: %w", path, err)
	}
	return nil
}
```

**Note:** add the YAML marshal tags to the existing `Case`/`Expected` structs if missing — they already carry `yaml:"..."` tags (`case.go:17-28`), so `yaml.Marshal` round-trips cleanly. No change needed.

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run 'TestRecordedCase|TestWriteCase' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/smana/Sources/runlore
git add internal/eval/record.go internal/eval/record_test.go
git commit -m "feat(eval): record live runs into the replay Case corpus"
```

---

## Task 5: Live runner (setup → run×N → judge → teardown, aggregation, pass gate)

**Files:**
- Create: `internal/eval/live.go`, `internal/eval/live_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/eval/live_test.go`:

```go
package eval

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// recordingSteps records the shell steps it was asked to run.
type recordingSteps struct {
	ran      []string
	failNext bool
}

func (s *recordingSteps) Run(_ context.Context, step string) error {
	s.ran = append(s.ran, step)
	if s.failNext {
		s.failNext = false
		return io.ErrUnexpectedEOF
	}
	return nil
}

// twoStepModel calls one tool, then submits a fixed finding.
type twoStepModel struct{ calls int }

func (m *twoStepModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	if m.calls%2 == 1 {
		return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: "{}"}}}, nil
	}
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{ID: "2", Name: "submit_findings",
		Args: `{"confidence":0.9,"root_causes":[{"summary":"bad image tag pushed by flux"}]}`}}}, nil
}

// fixedJudge returns the same verdict every time.
type fixedJudge struct{ v Verdict }

func (j fixedJudge) Grade(context.Context, Scenario, providers.Investigation) (Verdict, error) {
	return j.v, nil
}

func liveTestRunner(steps StepRunner, judge Judge) *LiveRunner {
	return &LiveRunner{
		Model:     &twoStepModel{},
		BaseTools: []investigate.Tool{fakeTool{name: "what_changed", out: "diff: tag v9.9.9"}},
		Judge:     judge,
		Steps:     steps,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		N:         3,
	}
}

func passScenario() Scenario {
	return Scenario{
		ID: "gitops-bad-image-tag", Invasive: true,
		Setup: []string{"apply bad tag"}, Teardown: []string{"delete bad tag"},
		Trigger:     Trigger{Mode: "cli", Symptom: "pods not starting"},
		GroundTruth: GroundTruth{ExpectedSources: []string{"gitops"}, MustReachRoot: true},
	}
}

func TestRunScenarioPassesAndTearsDown(t *testing.T) {
	steps := &recordingSteps{}
	judge := fixedJudge{v: Verdict{Scores: map[string]int{"root_cause": 3, "evidence": 2, "solution": 2, "description": 2, "calibration": 2}}}
	res := liveTestRunner(steps, judge).RunScenario(context.Background(), passScenario())

	if res.Skipped {
		t.Fatal("should not skip an invasive scenario")
	}
	if len(res.Runs) != 3 {
		t.Fatalf("want N=3 runs, got %d", len(res.Runs))
	}
	if res.CoverageRatio != 1.0 {
		t.Fatalf("want coverage 1.0, got %.2f", res.CoverageRatio)
	}
	if res.DimMedian["root_cause"] != 3 {
		t.Fatalf("want median root_cause 3, got %d", res.DimMedian["root_cause"])
	}
	if !res.Pass {
		t.Fatalf("want pass, got %+v", res)
	}
	// teardown must run even on success; setup ran once, teardown ran once.
	if steps.ran[0] != "apply bad tag" || steps.ran[len(steps.ran)-1] != "delete bad tag" {
		t.Fatalf("setup/teardown order wrong: %v", steps.ran)
	}
}

func TestRunScenarioFailsGateOnLowRootCause(t *testing.T) {
	judge := fixedJudge{v: Verdict{Scores: map[string]int{"root_cause": 1}}} // symptom-only => fail gate
	res := liveTestRunner(&recordingSteps{}, judge).RunScenario(context.Background(), passScenario())
	if res.Pass {
		t.Fatalf("root_cause=1 must fail the gate: %+v", res)
	}
}

func TestRunScenarioSkipsWhenPrecheckFails(t *testing.T) {
	steps := &recordingSteps{failNext: true} // precheck is the first step -> fails -> SKIP
	scn := Scenario{ID: "harbor-natural", Invasive: false, Precheck: "test harbor broken"}
	res := liveTestRunner(steps, fixedJudge{}).RunScenario(context.Background(), scn)
	if !res.Skipped || res.Pass {
		t.Fatalf("want skipped (precondition absent), got %+v", res)
	}
	if len(res.Runs) != 0 {
		t.Fatal("must not run investigations when skipped")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run TestRunScenario -v`
Expected: FAIL — `LiveRunner`/`StepRunner`/`RunScenario` undefined.

- [ ] **Step 3: Implement the live runner**

Create `internal/eval/live.go`:

```go
package eval

import (
	"context"
	"log/slog"
	"sort"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// StepRunner executes a scenario's shell setup/teardown/precheck steps. The real
// implementation shells out (kubectl/flux); tests use a fake.
type StepRunner interface {
	Run(ctx context.Context, step string) error
}

// LiveRunner runs scenarios against real tools, grading coverage + RCA quality.
// BaseTools and Model are the LIVE tools/model (built by cmd/lore via
// buildModelAndTools); Judge uses a separate, stronger model.
type LiveRunner struct {
	Model     providers.ModelProvider
	BaseTools []investigate.Tool
	Judge     Judge
	Steps     StepRunner
	Log       *slog.Logger
	N         int            // runs per scenario (default 1 if 0)
	OnRecord  func(Scenario, []Call) // optional: persist the run's calls (replay corpus)
}

// RunOutcome is one of the N runs of a scenario.
type RunOutcome struct {
	Investigation providers.Investigation
	Coverage      Coverage
	Verdict       Verdict
}

// LiveResult aggregates the N runs of one scenario.
type LiveResult struct {
	Scenario      string
	Skipped       bool
	SkipReason    string
	Runs          []RunOutcome
	CoverageRatio float64        // median
	DimMedian     map[string]int // median per rubric dimension
	DimVariance   map[string]float64
	ConfidentWrong bool          // any run confident-wrong
	ToolErrors    []string       // union across runs
	Pass          bool
}

// RunScenario runs setup (or precheck), N investigations, judging each, then
// always tears down. Pass gate: median root_cause >= 2 AND median coverage == 1.0
// AND no confident-wrong run.
func (lr *LiveRunner) RunScenario(ctx context.Context, scn Scenario) LiveResult {
	res := LiveResult{Scenario: scn.ID, DimMedian: map[string]int{}, DimVariance: map[string]float64{}}
	n := lr.N
	if n <= 0 {
		n = 1
	}

	// Natural scenarios: precheck the precondition; SKIP (not fail) if absent.
	if !scn.Invasive && scn.Precheck != "" {
		if err := lr.Steps.Run(ctx, scn.Precheck); err != nil {
			res.Skipped = true
			res.SkipReason = "precondition absent: " + err.Error()
			lr.Log.Info("scenario skipped", "id", scn.ID, "reason", res.SkipReason)
			return res
		}
	}

	// Invasive scenarios: induce the fault, and ALWAYS tear down.
	if scn.Invasive {
		for _, step := range scn.Setup {
			if err := lr.Steps.Run(ctx, step); err != nil {
				res.Skipped = true
				res.SkipReason = "setup failed: " + err.Error()
				lr.teardown(ctx, scn)
				return res
			}
		}
		defer lr.teardown(ctx, scn)
	}

	for i := 0; i < n; i++ {
		out := lr.runOnce(ctx, scn)
		res.Runs = append(res.Runs, out)
		if lr.OnRecord != nil {
			// recorded per run; the last run's file wins (stable name per scenario).
		}
	}
	lr.aggregate(&res)
	return res
}

func (lr *LiveRunner) runOnce(ctx context.Context, scn Scenario) RunOutcome {
	rec := &Recorder{}
	var inv providers.Investigation
	li := &investigate.LoopInvestigator{
		Model: lr.Model, Tools: wrap(lr.BaseTools, rec), Log: lr.Log, Verify: true,
		OnComplete: func(got providers.Investigation) { inv = got },
	}
	req := investigate.Request{
		Source: investigate.SourceAlert, Title: scn.ID, Message: scn.Trigger.Symptom,
		Workload: providers.Workload{Namespace: scn.Trigger.Namespace},
	}
	if err := li.Investigate(ctx, req); err != nil {
		lr.Log.Warn("investigation error", "id", scn.ID, "err", err)
	}
	calls := rec.Calls()
	if lr.OnRecord != nil {
		lr.OnRecord(scn, calls)
	}
	cov := ScoreCoverage(scn.GroundTruth.ExpectedSources, scn.GroundTruth.OptionalSources, calls)
	var v Verdict
	if lr.Judge != nil {
		graded, err := lr.Judge.Grade(ctx, scn, inv)
		if err != nil {
			lr.Log.Warn("judge error", "id", scn.ID, "err", err)
		} else {
			v = graded
		}
	}
	return RunOutcome{Investigation: inv, Coverage: cov, Verdict: v}
}

func (lr *LiveRunner) teardown(ctx context.Context, scn Scenario) {
	for _, step := range scn.Teardown {
		if err := lr.Steps.Run(ctx, step); err != nil {
			lr.Log.Warn("teardown step failed", "id", scn.ID, "step", step, "err", err)
		}
	}
}

func (lr *LiveRunner) aggregate(res *LiveResult) {
	if len(res.Runs) == 0 {
		return
	}
	// coverage median
	covs := make([]float64, len(res.Runs))
	errSet := map[string]bool{}
	for i, r := range res.Runs {
		covs[i] = r.Coverage.Ratio
		if r.Verdict.ConfidentWrong {
			res.ConfidentWrong = true
		}
		for _, te := range r.Coverage.ToolErrors {
			errSet[te] = true
		}
	}
	res.CoverageRatio = medianFloat(covs)
	for te := range errSet {
		res.ToolErrors = append(res.ToolErrors, te)
	}
	sort.Strings(res.ToolErrors)
	// per-dimension median + variance
	for _, d := range Rubric {
		vals := make([]float64, len(res.Runs))
		for i, r := range res.Runs {
			vals[i] = float64(r.Verdict.Scores[d.Key])
		}
		res.DimMedian[d.Key] = int(medianFloat(vals) + 0.5)
		res.DimVariance[d.Key] = variance(vals)
	}
	res.Pass = res.DimMedian["root_cause"] >= 2 && res.CoverageRatio == 1.0 && !res.ConfidentWrong
}

func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	m := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[m]
	}
	return (cp[m-1] + cp[m]) / 2
}

func variance(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var v float64
	for _, x := range xs {
		v += (x - mean) * (x - mean)
	}
	return v / float64(len(xs))
}
```

- [ ] **Step 4: Run to verify pass (with -race, goroutine-safe Recorder)**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run TestRunScenario -race -v`
Expected: PASS, no race.

- [ ] **Step 5: Commit**

```bash
cd /home/smana/Sources/runlore
git add internal/eval/live.go internal/eval/live_test.go
git commit -m "feat(eval): live runner — setup/run-N/judge/teardown + median/variance + pass gate"
```

---

## Task 6: Report (markdown + JSON, heatmap, regression diff)

**Files:**
- Create: `internal/eval/report.go`, `internal/eval/report_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/eval/report_test.go`:

```go
package eval

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleResults() []LiveResult {
	return []LiveResult{
		{Scenario: "gitops-bad-image-tag", CoverageRatio: 1.0,
			DimMedian: map[string]int{"root_cause": 3, "evidence": 2, "solution": 2, "description": 2, "calibration": 2},
			DimVariance: map[string]float64{"root_cause": 0}, Pass: true},
		{Scenario: "harbor-natural", Skipped: true, SkipReason: "precondition absent"},
		{Scenario: "saturation-mem", CoverageRatio: 0.5,
			DimMedian: map[string]int{"root_cause": 1}, ToolErrors: []string{"query_metrics"}, Pass: false},
	}
}

func TestReportJSONAndMarkdown(t *testing.T) {
	rep := NewLiveReport("2026-06-21T20:00:00Z", sampleResults())

	js := rep.JSON()
	var back LiveReport
	if err := json.Unmarshal(js, &back); err != nil {
		t.Fatalf("json roundtrip: %v", err)
	}
	if back.Passed != 1 || back.Ran != 2 || back.Skipped != 1 {
		t.Fatalf("counts: passed=%d ran=%d skipped=%d", back.Passed, back.Ran, back.Skipped)
	}

	md := rep.Markdown()
	for _, want := range []string{"gitops-bad-image-tag", "SKIP", "harbor-natural", "query_metrics", "1/2"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestRegressionDiff(t *testing.T) {
	prev := NewLiveReport("t0", []LiveResult{{Scenario: "a", Pass: true}, {Scenario: "b", Pass: true}})
	curr := NewLiveReport("t1", []LiveResult{{Scenario: "a", Pass: true}, {Scenario: "b", Pass: false}})
	regressed := curr.RegressionsVS(prev)
	if len(regressed) != 1 || regressed[0] != "b" {
		t.Fatalf("want [b] regressed, got %v", regressed)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run 'TestReport|TestRegression' -v`
Expected: FAIL — `NewLiveReport`/`LiveReport`/methods undefined.

- [ ] **Step 3: Implement the report**

Create `internal/eval/report.go`:

```go
package eval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// LiveReport is the serializable output of one live-fire campaign run.
type LiveReport struct {
	At      string       `json:"at"`
	Ran     int          `json:"ran"`     // scenarios actually investigated (not skipped)
	Passed  int          `json:"passed"`
	Skipped int          `json:"skipped"`
	Results []LiveResult `json:"results"`
}

// NewLiveReport tallies results into a report.
func NewLiveReport(at string, results []LiveResult) LiveReport {
	rep := LiveReport{At: at, Results: results}
	for _, r := range results {
		if r.Skipped {
			rep.Skipped++
			continue
		}
		rep.Ran++
		if r.Pass {
			rep.Passed++
		}
	}
	return rep
}

// JSON is the machine-readable sibling of the markdown report.
func (rep LiveReport) JSON() []byte {
	b, _ := json.MarshalIndent(rep, "", "  ")
	return b
}

// allSources is the column order for the coverage heatmap.
var allSources = []string{"gitops", "kubernetes", "metrics", "logs", "network", "aws", "kb"}

// Markdown renders the human report: summary, per-scenario table, coverage heatmap.
func (rep LiveReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# RunLore eval report — %s\n\n", rep.At)
	fmt.Fprintf(&b, "**Passed %d/%d** ran (%d skipped).\n\n", rep.Passed, rep.Ran, rep.Skipped)

	b.WriteString("## Scenarios\n\n")
	b.WriteString("| scenario | result | coverage | root_cause | tool errors |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, r := range rep.Results {
		if r.Skipped {
			fmt.Fprintf(&b, "| %s | SKIP | — | — | %s |\n", r.Scenario, r.SkipReason)
			continue
		}
		status := "FAIL"
		if r.Pass {
			status = "PASS"
		}
		te := "—"
		if len(r.ToolErrors) > 0 {
			te = strings.Join(r.ToolErrors, ", ")
		}
		fmt.Fprintf(&b, "| %s | %s | %.0f%% | %d | %s |\n", r.Scenario, status, r.CoverageRatio*100, r.DimMedian["root_cause"], te)
	}

	b.WriteString("\n## Coverage heatmap (median touched per source)\n\n")
	b.WriteString("| scenario | " + strings.Join(allSources, " | ") + " |\n")
	b.WriteString("|---|" + strings.Repeat("---|", len(allSources)) + "\n")
	for _, r := range rep.Results {
		if r.Skipped {
			continue
		}
		row := make([]string, len(allSources))
		touched := map[string]bool{}
		if len(r.Runs) > 0 {
			for _, s := range r.Runs[0].Coverage.Touched {
				touched[s] = true
			}
			for _, s := range r.Runs[0].Coverage.Bonus {
				touched[s] = true
			}
		}
		for i, s := range allSources {
			if touched[s] {
				row[i] = "✓"
			} else {
				row[i] = " "
			}
		}
		fmt.Fprintf(&b, "| %s | %s |\n", r.Scenario, strings.Join(row, " | "))
	}
	return b.String()
}

// RegressionsVS returns scenarios that passed in prev but fail/skip now.
func (rep LiveReport) RegressionsVS(prev LiveReport) []string {
	was := map[string]bool{}
	for _, r := range prev.Results {
		was[r.Scenario] = r.Pass
	}
	var out []string
	for _, r := range rep.Results {
		if was[r.Scenario] && !r.Pass {
			out = append(out, r.Scenario)
		}
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/eval/ -run 'TestReport|TestRegression' -v`
Expected: PASS.

- [ ] **Step 5: Full package gate + commit**

Run:
```bash
cd /home/smana/Sources/runlore
go build ./... && go vet ./... && go test ./internal/eval/ -race && gofmt -l internal/eval && golangci-lint run ./internal/eval/...
```
Expected: clean; `gofmt -l` prints nothing; `0 issues`.

```bash
git add internal/eval/report.go internal/eval/report_test.go
git commit -m "feat(eval): live-fire report (markdown + JSON, heatmap, regression diff)"
```

---

## Task 7: Wire `lore eval --live` into the CLI

**Files:**
- Modify: `cmd/lore/main.go` (`runEval`, ~375-417; add `shellStepRunner`, `runEvalLive`, `buildJudgeModel`; update `usage` ~64-72)

- [ ] **Step 1: Write the failing test (shell step runner)**

Create `cmd/lore/eval_live_test.go`:

```go
package main

import (
	"context"
	"testing"
)

func TestShellStepRunnerEcho(t *testing.T) {
	sr := shellStepRunner{}
	if err := sr.Run(context.Background(), "true"); err != nil {
		t.Fatalf("true should succeed: %v", err)
	}
	if err := sr.Run(context.Background(), "false"); err == nil {
		t.Fatal("false should fail")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./cmd/lore/ -run TestShellStepRunner -v`
Expected: FAIL — `shellStepRunner` undefined.

- [ ] **Step 3: Implement `shellStepRunner` + the `--live` branch**

In `cmd/lore/main.go`, add the shell step runner (near the other helpers):

```go
// shellStepRunner executes a scenario step as a shell command (kubectl/flux/test).
type shellStepRunner struct{}

func (shellStepRunner) Run(ctx context.Context, step string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", step)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr // step output is progress, not findings
	return cmd.Run()
}
```

Add the judge-model builder:

```go
// buildJudgeModel builds the (stronger) grader model from --judge-* flags, falling
// back to the configured investigation model when unset.
func buildJudgeModel(cfg *config.Config, provider, baseURL, model, apiKeyEnv string) providers.ModelProvider {
	if provider == "" && model == "" {
		apiKey := ""
		if cfg.Model.APIKeyEnv != "" {
			apiKey = os.Getenv(cfg.Model.APIKeyEnv)
		}
		return buildModel(cfg, apiKey)
	}
	apiKey := os.Getenv(apiKeyEnv)
	switch provider {
	case "anthropic":
		return anthropic.New(baseURL, model, apiKey)
	case "gemini":
		return gemini.New(baseURL, model, apiKey)
	default:
		return openai.New(baseURL, model, apiKey)
	}
}
```

Replace `runEval` (lines 375-417) so it dispatches to live mode when `--live` is set. Keep the existing replay path intact:

```go
func runEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	casesDir := fs.String("cases", "examples/eval", "directory of replay cases")
	live := fs.Bool("live", false, "live-fire mode: run scenarios against the real cluster")
	scnDir := fs.String("scenarios", "eval/scenarios", "directory of live-fire scenarios")
	recordDir := fs.String("record", "eval/fixtures", "where to write recorded runs (replay corpus)")
	reportDir := fs.String("report-dir", "eval/reports", "where to write the campaign report")
	prevReport := fs.String("baseline", "", "previous report JSON for regression diff")
	n := fs.Int("n", 3, "runs per scenario (live mode)")
	stamp := fs.String("stamp", "", "report timestamp (RFC3339); blank = caller fills in")
	jProvider := fs.String("judge-provider", "", "judge model provider (default: investigation model)")
	jBaseURL := fs.String("judge-base-url", "", "judge model base URL")
	jModel := fs.String("judge-model", "", "judge model name")
	jKeyEnv := fs.String("judge-api-key-env", "", "env var holding the judge API key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !modelConfigured(cfg) {
		return fmt.Errorf("eval requires a configured model (set config.model)")
	}
	if *live {
		return runEvalLive(cfg, *scnDir, *recordDir, *reportDir, *prevReport, *stamp, *n,
			*jProvider, *jBaseURL, *jModel, *jKeyEnv)
	}
	// ---- existing replay path (unchanged) ----
	cases, err := eval.Load(*casesDir)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no eval cases found in %s", *casesDir)
	}
	apiKey := ""
	if cfg.Model.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Model.APIKeyEnv)
	}
	runner := &eval.Runner{Model: buildModel(cfg, apiKey), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rep := runner.Run(context.Background(), cases)
	for _, res := range rep.Results {
		status := "PASS"
		if !res.Pass {
			status = "FAIL"
		}
		fmt.Printf("%-4s  %-32s  confidence=%.2f", status, res.Name, res.Confidence)
		if len(res.Missing) > 0 {
			fmt.Printf("  missing: %s", strings.Join(res.Missing, ", "))
		}
		fmt.Println()
	}
	fmt.Printf("\nRCA identified: %d/%d (%.0f%%)\n", rep.Passed(), len(rep.Results), rep.RCARate()*100)
	return nil
}

// runEvalLive runs the live-fire campaign and writes a dated report.
func runEvalLive(cfg *config.Config, scnDir, recordDir, reportDir, prevReport, stamp string, n int,
	jProvider, jBaseURL, jModel, jKeyEnv string) error {
	scns, err := eval.LoadScenarios(scnDir)
	if err != nil {
		return err
	}
	if len(scns) == 0 {
		return fmt.Errorf("no scenarios found in %s", scnDir)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx := context.Background()
	model, tools, _ := buildModelAndTools(ctx, cfg, gitOpsFromKube(cfg, log), log)
	judge := eval.ModelJudge{Model: buildJudgeModel(cfg, jProvider, jBaseURL, jModel, jKeyEnv)}

	runner := &eval.LiveRunner{
		Model: model, BaseTools: tools, Judge: judge, Steps: shellStepRunner{}, Log: log, N: n,
		OnRecord: func(scn eval.Scenario, calls []eval.Call) {
			if err := eval.WriteCase(recordDir, eval.RecordedCase(scn, calls)); err != nil {
				log.Warn("record case failed", "id", scn.ID, "err", err)
			}
		},
	}
	var results []eval.LiveResult
	for _, scn := range scns {
		log.Info("running scenario", "id", scn.ID)
		results = append(results, runner.RunScenario(ctx, scn))
	}

	if stamp == "" {
		stamp = time.Now().UTC().Format(time.RFC3339)
	}
	rep := eval.NewLiveReport(stamp, results)
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return err
	}
	base := filepath.Join(reportDir, strings.ReplaceAll(stamp, ":", "-"))
	if err := os.WriteFile(base+".json", rep.JSON(), 0o644); err != nil {
		return err
	}
	md := rep.Markdown()
	if prevReport != "" {
		if data, err := os.ReadFile(prevReport); err == nil {
			var prev eval.LiveReport
			if json.Unmarshal(data, &prev) == nil {
				if reg := rep.RegressionsVS(prev); len(reg) > 0 {
					md += "\n## ⚠️ Regressions vs baseline\n\n- " + strings.Join(reg, "\n- ") + "\n"
				}
			}
		}
	}
	if err := os.WriteFile(base+".md", []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Print(md)
	fmt.Printf("\nreport: %s.md / .json\n", base)
	return nil
}
```

Ensure these imports exist in `cmd/lore/main.go` (add any missing): `"os/exec"`, `"path/filepath"`, `"time"`, `"encoding/json"`, and the model packages `anthropic`, `gemini`, `openai` (already imported for `buildModel`). Update the `usage` const's eval line to:

```go
  lore eval [--config <path>] [--cases <dir>]                 replay recorded cases, score RCA
  lore eval --live [--scenarios <dir>] [--n 3] [--judge-model <m>]   live-fire on the cluster, grade coverage + RCA
```

- [ ] **Step 4: Run to verify pass + full gate**

Run:
```bash
cd /home/smana/Sources/runlore
go test ./cmd/lore/ -run TestShellStepRunner -v
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
```
Expected: PASS; build clean; `gofmt -l` prints nothing; `0 issues`.

- [ ] **Step 5: Smoke — `--live` fails cleanly with no scenarios**

Run:
```bash
cd /home/smana/Sources/runlore
go build -o /tmp/lore ./cmd/lore
cat > /tmp/rl-eval.yaml <<'EOF'
model: { provider: openai, base_url: "http://127.0.0.1:1/v1", model: x }
EOF
mkdir -p /tmp/empty-scn
/tmp/lore eval --live --config /tmp/rl-eval.yaml --scenarios /tmp/empty-scn 2>&1 | grep -q "no scenarios found" && echo "OK: clean empty-scenario error"
```
Expected: `OK: clean empty-scenario error`.

- [ ] **Step 6: Commit**

```bash
cd /home/smana/Sources/runlore
git add cmd/lore/main.go cmd/lore/eval_live_test.go
git commit -m "feat(eval): lore eval --live — wire live-fire runner + judge + report into the CLI"
```

---

## Self-Review

- **Spec coverage** (`2026-06-21-runlore-eval-harness-design.md`): §3 architecture → Tasks 1-7 in `internal/eval` + CLI; §4 scenario schema → Task 1; §5 Track A coverage → Task 2, Track B judge → Task 3, N=3 median/variance + pass gate → Task 5; §6 recording → Task 4, report/heatmap/regression → Task 6, safety (precheck-skip, always-teardown) → Task 5; D5 CLI trigger default + D9 stronger blind judge → Tasks 3,7. Webhook trigger mode (D5 opt-in) is parsed in the schema (Task 1) but only the CLI path is exercised by the runner — webhook execution is deferred to Plan 2's scenarios as noted. Sandbox curation (D6) is inherent: `runEvalLive` builds tools but never constructs a curator, so eval never writes to the KB; `--curate` is **not** added here (out of scope for the engine; Plan 2 can add it). Phase-2 replay determinism is out of scope (spec §7).
- **Placeholder scan:** every code step contains complete, compilable code; no TODO/TBD; the `OnRecord` per-run comment in Task 5 is intentional (last file wins) and the wiring lives in Task 7.
- **Type consistency:** `Call`, `Recorder`, `recordingTool`, `wrap`, `Coverage`, `ScoreCoverage` (Task 2) consumed unchanged by `LiveRunner` (Task 5); `Verdict.Scores` keys match `Rubric` dimension keys (`root_cause/evidence/solution/description/calibration`) used in Tasks 3/5/6; `LiveResult`/`RunOutcome` fields produced in Task 5 are read verbatim by `NewLiveReport`/`Markdown` (Task 6); `Scenario`/`GroundTruth`/`Trigger` (Task 1) consumed by Tasks 4/5/7; `eval.Case`/`Expected` reused from existing `case.go` by Task 4. CLI helpers (`buildModelAndTools`, `gitOpsFromKube`, `buildModel`, `config.Load`) match `cmd/lore/main.go` signatures.

---

## What this plan delivers

`lore eval --live` runs scenarios against the real cluster: induces/prechecks the fault, fires `lore investigate` N=3 times, grades **coverage deterministically** (which of gitops/kubernetes/metrics/logs/network/aws/kb were touched, plus tool-error flags) and **RCA quality** via a stronger blind LLM-judge (5-dimension rubric), tears down always, records each run into the existing replay `Case` corpus, and writes a dated markdown+JSON report with a coverage heatmap and regression diff. The existing replay harness is untouched and still works. **Plan 2** authors the 12-scenario catalog + throwaway manifests + `rubric.md` and runs the first baseline campaign.
