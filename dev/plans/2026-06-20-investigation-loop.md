# Investigation Loop (ReAct core) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the real investigation loop — a ReAct `Investigator` that drives a `ModelProvider` with tools (the what-changed `Changes`/`Diff` first), feeds tool results back, and produces a `providers.Investigation` via a `submit_findings` structured-output tool. It plugs into the workqueue as the `process` function, replacing `LogInvestigator`.

**Architecture:** A `Tool` is a model-callable capability (name + JSON-schema + `Call`). The loop advertises the tools + a reserved `submit_findings` tool to the model, runs whatever tools the model calls, appends results to the conversation, and finishes when the model calls `submit_findings` (parsed into `providers.Investigation`) or hits `MaxSteps`. The completed investigation is handed to an `OnComplete` hook (the future Slack/Matrix delivery point). Everything is tested against a **scripted fake `ModelProvider`** + a **fake `GitOpsProvider`** — no network, CI-safe.

**Tech Stack:** Go 1.26 stdlib. Contracts: `providers.ModelProvider`/`CompletionRequest`/`Message`/`ToolSpec`/`ToolCall`/`CompletionResponse`, `providers.GitOpsProvider`, `providers.Investigation`/`Hypothesis`, `investigate.Request`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/providers/providers.go` *(modify)* | extend `ToolCall` (ID) + `Message` (ToolCalls, ToolCallID) for tool-result feedback |
| `internal/investigate/tools.go` *(create)* | `Tool` interface; `submit_findings` spec + `parseFindings`; tool-spec helpers |
| `internal/investigate/tools_test.go` *(create)* | `parseFindings` test |
| `internal/investigate/whatchanged_tool.go` *(create)* | `WhatChangedTool` (wraps `GitOpsProvider`) |
| `internal/investigate/whatchanged_tool_test.go` *(create)* | tool test vs a fake `GitOpsProvider` |
| `internal/investigate/loop.go` *(create)* | `LoopInvestigator` (the ReAct loop) |
| `internal/investigate/loop_test.go` *(create)* | loop test vs a scripted fake model |

---

## Task 1: Tool abstraction + structured-output parsing

**Files:**
- Modify: `internal/providers/providers.go`
- Create: `internal/investigate/tools.go`, `internal/investigate/tools_test.go`

- [ ] **Step 1: Extend the model-exchange types**

In `internal/providers/providers.go`, replace the `Message` and `ToolCall` types with:

```go
// Message is one turn in an LLM exchange.
type Message struct {
	Role       string     // system | user | assistant | tool
	Content    string
	ToolCalls  []ToolCall // assistant turn requesting tools
	ToolCallID string     // tool turn: the call this answers
}
```

```go
// ToolCall is a model request to invoke a tool.
type ToolCall struct {
	ID   string
	Name string
	Args string // JSON
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/investigate/tools_test.go`:

```go
package investigate

import "testing"

func TestParseFindings(t *testing.T) {
	args := `{"confidence":0.82,"root_causes":[
	  {"summary":"chart 1.15 enabled DB migrations; harbor-db CrashLoopBackOff","confidence":0.82,"suggested_action":"flux rollback hr/harbor","reversible":true,"evidence":["pg_up=0","migration lock timeout"]}
	],"unresolved":["why the migration lock never released"]}`
	inv, err := parseFindings(args)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Confidence != 0.82 || len(inv.RootCauses) != 1 || len(inv.Unresolved) != 1 {
		t.Fatalf("unexpected investigation: %+v", inv)
	}
	rc := inv.RootCauses[0]
	if rc.Confidence != 0.82 || rc.SuggestedAction != "flux rollback hr/harbor" || !rc.Reversible || len(rc.Evidence) != 2 {
		t.Fatalf("unexpected root cause: %+v", rc)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -run TestParseFindings -v`
Expected: FAIL — `parseFindings` undefined.

- [ ] **Step 4: Implement the tool abstraction + parsing**

Create `internal/investigate/tools.go`:

```go
package investigate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Smana/runlore/internal/providers"
)

// Tool is a model-callable capability used during an investigation.
type Tool interface {
	Name() string
	Description() string
	Schema() string // JSON Schema for the arguments
	Call(ctx context.Context, args string) (string, error)
}

// submitFindingsName is the reserved tool the model calls to finish, supplying
// the structured investigation result.
const submitFindingsName = "submit_findings"

// submitFindingsSpec advertises the structured-output tool to the model.
func submitFindingsSpec() providers.ToolSpec {
	return providers.ToolSpec{
		Name:        submitFindingsName,
		Description: "Submit the final investigation: ranked root causes with evidence, plus anything unresolved.",
		Schema: `{"type":"object","properties":{
"confidence":{"type":"number"},
"root_causes":{"type":"array","items":{"type":"object","properties":{
"summary":{"type":"string"},"confidence":{"type":"number"},"change_ref":{"type":"string"},
"evidence":{"type":"array","items":{"type":"string"}},"suggested_action":{"type":"string"},"reversible":{"type":"boolean"}},
"required":["summary"]}},
"unresolved":{"type":"array","items":{"type":"string"}}},"required":["root_causes"]}`,
	}
}

// findings is the JSON shape of submit_findings arguments.
type findings struct {
	Confidence float64 `json:"confidence"`
	RootCauses []struct {
		Summary         string   `json:"summary"`
		Confidence      float64  `json:"confidence"`
		ChangeRef       string   `json:"change_ref"`
		Evidence        []string `json:"evidence"`
		SuggestedAction string   `json:"suggested_action"`
		Reversible      bool     `json:"reversible"`
	} `json:"root_causes"`
	Unresolved []string `json:"unresolved"`
}

// parseFindings turns submit_findings arguments into a providers.Investigation.
func parseFindings(args string) (providers.Investigation, error) {
	var f findings
	if err := json.Unmarshal([]byte(args), &f); err != nil {
		return providers.Investigation{}, fmt.Errorf("parse findings: %w", err)
	}
	inv := providers.Investigation{Confidence: f.Confidence, Unresolved: f.Unresolved}
	for _, rc := range f.RootCauses {
		inv.RootCauses = append(inv.RootCauses, providers.Hypothesis{
			Summary:         rc.Summary,
			Confidence:      rc.Confidence,
			ChangeRef:       rc.ChangeRef,
			Evidence:        rc.Evidence,
			SuggestedAction: rc.SuggestedAction,
			Reversible:      rc.Reversible,
		})
	}
	return inv, nil
}
```

- [ ] **Step 5: Run to verify pass + gate + commit**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -run TestParseFindings -v && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: PASS; all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/providers/providers.go internal/investigate/tools.go internal/investigate/tools_test.go
git commit -m "feat(investigate): Tool abstraction + submit_findings structured output; model tool-call feedback types"
```

---

## Task 2: The what-changed tool

**Files:**
- Create: `internal/investigate/whatchanged_tool.go`, `internal/investigate/whatchanged_tool_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/investigate/whatchanged_tool_test.go`:

```go
package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeGitOps returns canned changes/diffs.
type fakeGitOps struct {
	changes []providers.Change
	diff    providers.Diff
}

func (f fakeGitOps) Changes(context.Context, providers.TimeWindow, providers.Selector) ([]providers.Change, error) {
	return f.changes, nil
}
func (f fakeGitOps) Diff(context.Context, providers.Change) (providers.Diff, error) { return f.diff, nil }
func (f fakeGitOps) WatchFailures(context.Context) (<-chan providers.FailureEvent, error) {
	ch := make(chan providers.FailureEvent)
	close(ch)
	return ch, nil
}

func TestWhatChangedTool(t *testing.T) {
	gp := fakeGitOps{
		changes: []providers.Change{{
			Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
			Engine:   providers.EngineFlux, Type: providers.ChangeSync, FromRev: "aaa", ToRev: "bbb",
		}},
		diff: providers.Diff{Files: []providers.FileDiff{{Path: "apps/harbor/values.yaml", Patch: "+version: 1.15.0"}}},
	}
	tool := WhatChangedTool{GitOps: gp}
	out, err := tool.Call(context.Background(), `{"namespace":"flux-system"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "apps") || !strings.Contains(out, "bbb") || !strings.Contains(out, "version: 1.15.0") {
		t.Fatalf("tool output missing expected content:\n%s", out)
	}
	if tool.Name() != "what_changed" {
		t.Fatalf("unexpected name %q", tool.Name())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -run TestWhatChangedTool -v`
Expected: FAIL — `WhatChangedTool` undefined.

- [ ] **Step 3: Implement the tool**

Create `internal/investigate/whatchanged_tool.go`:

```go
package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// WhatChangedTool exposes the GitOps "what changed" lens to the model: the change
// timeline for a namespace/workload, each with its diff.
type WhatChangedTool struct {
	GitOps providers.GitOpsProvider
}

func (t WhatChangedTool) Name() string { return "what_changed" }

func (t WhatChangedTool) Description() string {
	return "List what changed (GitOps revision history + the actual Git diff) for a namespace, optionally a named workload."
}

func (t WhatChangedTool) Schema() string {
	return `{"type":"object","properties":{"namespace":{"type":"string"},"name":{"type":"string"}},"required":["namespace"]}`
}

// Call lists changes for the selector and renders each with its diff.
func (t WhatChangedTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	changes, err := t.GitOps.Changes(ctx, providers.TimeWindow{}, providers.Selector{Namespace: in.Namespace, Name: in.Name})
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "no changes found for the given selector", nil
	}
	var b strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&b, "%s %s/%s (%s): %s..%s\n", c.Engine, c.Workload.Kind, c.Workload.Name, c.Type, c.FromRev, c.ToRev)
		d, derr := t.GitOps.Diff(ctx, c)
		if derr != nil {
			fmt.Fprintf(&b, "  (diff error: %v)\n", derr)
			continue
		}
		for _, f := range d.Files {
			fmt.Fprintf(&b, "  --- %s\n%s\n", f.Path, f.Patch)
		}
	}
	return b.String(), nil
}
```

- [ ] **Step 4: Run + gate + commit**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -run TestWhatChangedTool -v && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: PASS; all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/investigate/whatchanged_tool.go internal/investigate/whatchanged_tool_test.go
git commit -m "feat(investigate): what_changed tool (GitOps changes + diff) for the loop"
```

---

## Task 3: The ReAct loop (`LoopInvestigator`)

**Files:**
- Create: `internal/investigate/loop.go`, `internal/investigate/loop_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/investigate/loop_test.go`:

```go
package investigate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// scriptModel returns a fixed sequence of responses, ignoring its input.
type scriptModel struct {
	responses []providers.CompletionResponse
	i         int
}

func (m *scriptModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	r := m.responses[m.i]
	m.i++
	return r, nil
}

func TestLoopInvestigator(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		// turn 1: ask what changed
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: `{"namespace":"flux-system"}`}}},
		// turn 2: submit findings
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: "submit_findings", Args: `{"confidence":0.8,"root_causes":[{"summary":"chart bump broke db","confidence":0.8}]}`}}},
	}}
	gp := fakeGitOps{changes: []providers.Change{{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}, FromRev: "a", ToRev: "b",
	}}}

	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{WhatChangedTool{GitOps: gp}},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Source: SourceAlert, Title: "HarborProbeFailure", Workload: providers.Workload{Namespace: "flux-system"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("OnComplete was not called")
	}
	if got.Confidence != 0.8 || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "chart bump broke db" {
		t.Fatalf("unexpected investigation: %+v", got)
	}
	if model.i != 2 {
		t.Fatalf("expected exactly 2 model calls, got %d", model.i)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -run TestLoopInvestigator -v`
Expected: FAIL — `LoopInvestigator` undefined.

- [ ] **Step 3: Implement the loop**

Create `internal/investigate/loop.go`:

```go
package investigate

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Smana/runlore/internal/providers"
)

const systemPrompt = `You are an SRE incident investigator. The cause is unknown — investigate by
calling the available tools to gather evidence (start with what_changed), reason about both
change-caused and no-change causes, then call submit_findings exactly once with ranked root causes,
evidence, and anything you could not determine. Be honest about uncertainty.`

// LoopInvestigator is the ReAct investigation loop: it drives a ModelProvider with
// tools, feeds tool results back, and finishes when the model calls submit_findings
// (or MaxSteps is reached). The completed investigation is handed to OnComplete.
type LoopInvestigator struct {
	Model      providers.ModelProvider
	Tools      []Tool
	Log        *slog.Logger
	MaxSteps   int
	OnComplete func(providers.Investigation) // delivery hook (Slack/Matrix later)
}

// Investigate runs the loop for a request. It implements Investigator.
func (li *LoopInvestigator) Investigate(ctx context.Context, req Request) error {
	byName := map[string]Tool{}
	specs := make([]providers.ToolSpec, 0, len(li.Tools)+1)
	for _, t := range li.Tools {
		byName[t.Name()] = t
		specs = append(specs, providers.ToolSpec{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
	}
	specs = append(specs, submitFindingsSpec())

	messages := []providers.Message{{Role: "user", Content: seedPrompt(req)}}
	maxSteps := li.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 8
	}

	for step := 0; step < maxSteps; step++ {
		resp, err := li.Model.Complete(ctx, providers.CompletionRequest{System: systemPrompt, Messages: messages, Tools: specs})
		if err != nil {
			return fmt.Errorf("model: %w", err)
		}
		if len(resp.ToolCalls) == 0 {
			li.Log.Warn("investigation inconclusive (no submit_findings)", "title", req.Title)
			return nil
		}
		messages = append(messages, providers.Message{Role: "assistant", Content: resp.Text, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			if tc.Name == submitFindingsName {
				inv, perr := parseFindings(tc.Args)
				if perr != nil {
					messages = append(messages, providers.Message{Role: "tool", ToolCallID: tc.ID, Content: "error: " + perr.Error()})
					continue
				}
				li.deliver(req, inv)
				return nil
			}
			messages = append(messages, providers.Message{Role: "tool", ToolCallID: tc.ID, Content: li.runTool(ctx, byName, tc)})
		}
	}
	li.Log.Warn("investigation hit max steps", "title", req.Title, "max", maxSteps)
	return nil
}

func (li *LoopInvestigator) runTool(ctx context.Context, byName map[string]Tool, tc providers.ToolCall) string {
	tool, ok := byName[tc.Name]
	if !ok {
		return "unknown tool: " + tc.Name
	}
	out, err := tool.Call(ctx, tc.Args)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

func (li *LoopInvestigator) deliver(req Request, inv providers.Investigation) {
	li.Log.Info("investigation complete",
		"title", req.Title, "confidence", inv.Confidence,
		"root_causes", len(inv.RootCauses), "unresolved", len(inv.Unresolved))
	if li.OnComplete != nil {
		li.OnComplete(inv)
	}
}

func seedPrompt(req Request) string {
	return fmt.Sprintf("Incident: %s (source=%s). Workload: %s/%s. Reason: %s. Message: %s.\nInvestigate the likely cause.",
		req.Title, req.Source, req.Workload.Namespace, req.Workload.Name, req.Reason, req.Message)
}
```

- [ ] **Step 4: Run to verify pass + gate + commit**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -v && go test -race ./internal/investigate/ && go build ./... && go vet ./... && gofmt -l . && golangci-lint run ./...`
Expected: PASS, race-clean; `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/investigate/loop.go internal/investigate/loop_test.go
git commit -m "feat(investigate): ReAct LoopInvestigator (model + tools -> Investigation)"
```

---

## What this plan delivers

The real investigation loop: a model-driven ReAct loop that calls tools (the what-changed lens first), feeds results back, and produces a structured `providers.Investigation` via `submit_findings` — fully tested against a scripted model + fake provider, plugging into the workqueue as the `process` function (it satisfies `Investigator`).

## Next plans (not in this plan — need an API key, so not CI)

1. **A real `ModelProvider`** — `internal/model/openai` (OpenAI-compatible: in-cluster vLLM / Ollama / OpenAI) and/or Anthropic, mapping `CompletionRequest`↔the SDK incl. tool calls. Behind config; manual/integration test.
2. **Wire `serve`** — when a model is configured, build `LoopInvestigator{Model, Tools: [WhatChangedTool{flux provider}], OnComplete: deliverToSlack}` and pass it to `NewQueue` instead of `LogInvestigator`.
3. **More tools** — metrics (PromQL), logs, network (Hubble); **catalog `kb_search`** for instant recall + grounding.

---

## Self-Review

- **Spec coverage:** The ReAct loop (model ↔ tools ↔ submit_findings → Investigation) with the what-changed tool; `LoopInvestigator` satisfies `Investigator` so it drops into the workqueue. Real model client + serve wiring + more tools are named follow-ups (API-key-dependent). ✅
- **Placeholder scan:** Complete code per step; `OnComplete` is a real delivery seam (not a stub gap); the model-type extension is concrete. ✅
- **Type consistency:** `providers.Message`/`ToolCall` extended (ID, ToolCalls, ToolCallID) and consumed by the loop; `Tool`/`submitFindingsSpec`/`parseFindings` (Task 1) used by `WhatChangedTool` (Task 2) and `LoopInvestigator` (Task 3); `providers.Investigation`/`Hypothesis` produced exactly per the contract; `Investigator` interface satisfied. ✅
