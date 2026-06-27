# Real Model Provider + `serve` Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `lore serve` actually investigate with an LLM. Implement an **OpenAI-compatible** `providers.ModelProvider` (covers in-cluster vLLM, Ollama, OpenAI, OpenRouter) and wire it into `serve`: when a model is configured, the queue dispatches to a `LoopInvestigator` (model + the what-changed tool) instead of `LogInvestigator`.

**Architecture:** A small hand-rolled `/chat/completions` client (net/http + JSON) maps `providers.CompletionRequest`↔the OpenAI chat shape, including the tool-call protocol (assistant `tool_calls`, `role:"tool"` results with `tool_call_id`). It's tested against an `httptest` server — **CI-safe, no API key**. `serve` builds the model client + (best-effort) the Flux `GitOpsProvider` + `WhatChangedTool` + `LoopInvestigator`, falling back to `LogInvestigator` when no model is configured.

**Tech Stack:** Go 1.26 stdlib (`net/http`, `encoding/json`, `net/http/httptest`). Contracts: `providers.ModelProvider`/`CompletionRequest`/`CompletionResponse`/`Message`/`ToolSpec`/`ToolCall`; `investigate.LoopInvestigator`/`WhatChangedTool`; `config`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/model/openai/openai.go` *(create)* | OpenAI-compatible `ModelProvider` client |
| `internal/model/openai/openai_test.go` *(create)* | `httptest` round-trip + tool-call mapping test |
| `internal/config/config.go` *(modify)* | `Model` config + top-level field |
| `cmd/lore/main.go` *(modify)* | build the investigator (LoopInvestigator when a model is set; else LogInvestigator) |

---

## Task 1: OpenAI-compatible `ModelProvider`

**Files:**
- Create: `internal/model/openai/openai.go`, `internal/model/openai/openai_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/model/openai/openai_test.go`:

```go
package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestComplete(t *testing.T) {
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth header = %q, want Bearer k", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(chatResponse{Choices: []chatChoice{{Message: chatMessage{
			Role: "assistant",
			ToolCalls: []chatToolCall{{ID: "tc1", Type: "function", Function: chatFunctionCall{
				Name: "what_changed", Arguments: `{"namespace":"apps"}`}}},
		}}}})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", "k")
	resp, err := c.Complete(context.Background(), providers.CompletionRequest{
		System:   "sys",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Tools:    []providers.ToolSpec{{Name: "what_changed", Description: "d", Schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// request mapping
	if gotReq.Model != "test-model" {
		t.Fatalf("model = %q", gotReq.Model)
	}
	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" || gotReq.Messages[1].Content != "hi" {
		t.Fatalf("messages mapped wrong: %+v", gotReq.Messages)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "what_changed" {
		t.Fatalf("tools mapped wrong: %+v", gotReq.Tools)
	}
	// response mapping
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "tc1" ||
		resp.ToolCalls[0].Name != "what_changed" || resp.ToolCalls[0].Args != `{"namespace":"apps"}` {
		t.Fatalf("response tool calls mapped wrong: %+v", resp.ToolCalls)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/model/openai/ -v`
Expected: FAIL — package/types undefined.

- [ ] **Step 3: Implement the client**

Create `internal/model/openai/openai.go`:

```go
// Package openai implements providers.ModelProvider against an OpenAI-compatible
// /chat/completions endpoint (OpenAI, in-cluster vLLM, Ollama, OpenRouter).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Client is an OpenAI-compatible model provider.
type Client struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client
}

// New builds a client. apiKey may be empty (keyless vLLM/Ollama).
func New(baseURL, model, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 2 * time.Minute},
	}
}

var _ providers.ModelProvider = (*Client)(nil)

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatChoice struct {
	Message chatMessage `json:"message"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

// Complete sends a chat completion with tools and maps the result back.
func (c *Client) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		cm := chatMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID: tc.ID, Type: "function",
				Function: chatFunctionCall{Name: tc.Name, Arguments: tc.Args},
			})
		}
		msgs = append(msgs, cm)
	}
	tools := make([]chatTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, chatTool{Type: "function", Function: chatFunction{
			Name: t.Name, Description: t.Description, Parameters: json.RawMessage(t.Schema),
		}})
	}

	body, err := json.Marshal(chatRequest{Model: c.model, Messages: msgs, Tools: tools})
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return providers.CompletionResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("chat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return providers.CompletionResponse{}, fmt.Errorf("chat status %d: %s", resp.StatusCode, string(data))
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("parse response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return providers.CompletionResponse{}, fmt.Errorf("no choices in response")
	}
	msg := cr.Choices[0].Message
	out := providers.CompletionResponse{Text: msg.Content}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, providers.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	return out, nil
}
```

- [ ] **Step 4: Run + gate + commit**

Run: `cd /home/smana/Sources/runlore && go test ./internal/model/openai/ -v && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: PASS; all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/model/
git commit -m "feat(model): OpenAI-compatible ModelProvider (chat/completions + tool calls)"
```

---

## Task 2: Config + wire the investigator in `serve`

**Files:**
- Modify: `internal/config/config.go`, `cmd/lore/main.go`

- [ ] **Step 1: Add the model config**

In `internal/config/config.go`, add the `Model` type and a top-level field on `Config`:

```go
	Model Model `yaml:"model"` // optional; when BaseURL is set, serve uses the LLM investigator
```

```go
// Model configures the OpenAI-compatible LLM endpoint used for investigation.
// When BaseURL is empty, serve falls back to the log-only investigator.
type Model struct {
	BaseURL   string `yaml:"base_url"`   // e.g. https://vllm.svc/v1
	Model     string `yaml:"model"`      // model name
	APIKeyEnv string `yaml:"api_key_env"` // env var holding the API key (empty = keyless)
}
```

- [ ] **Step 2: Build the investigator in `runServe`**

In `cmd/lore/main.go`, refactor `runServe` to build the investigator and to build the kube client once (shared by the failure-watch and the what-changed tool). Replace the queue/watch construction with:

```go
	// Build the (best-effort) dynamic client once: used by both the GitOps-failure
	// watch and the what-changed tool.
	var fluxProvider *flux.Provider
	if client, err := dynamicClient(); err != nil {
		log.Warn("no kube client; GitOps features disabled", "err", err)
	} else {
		fluxProvider = flux.New(flux.NewDynamicReader(client), &whatchanged.Differ{})
	}

	inv := buildInvestigator(cfg, fluxProvider, log)
	queue := investigate.NewQueue(inv, log)
	go queue.Run(ctx)

	if cfg.Triggers.GitOpsFailures.Enabled && fluxProvider != nil {
		startGitOpsFailureWatch(ctx, cfg, queue, fluxProvider, log)
	}
```

Add `buildInvestigator` and update `startGitOpsFailureWatch` to take the provider:

```go
// buildInvestigator returns the LLM ReAct investigator when a model is configured,
// otherwise the read-only LogInvestigator.
func buildInvestigator(cfg *config.Config, fp *flux.Provider, log *slog.Logger) investigate.Investigator {
	if cfg.Model.BaseURL == "" {
		log.Info("no model configured; using log-only investigator")
		return investigate.LogInvestigator{Log: log}
	}
	apiKey := ""
	if cfg.Model.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Model.APIKeyEnv)
	}
	model := openai.New(cfg.Model.BaseURL, cfg.Model.Model, apiKey)
	var tools []investigate.Tool
	if fp != nil {
		tools = append(tools, investigate.WhatChangedTool{GitOps: fp})
	}
	log.Info("using LLM investigator", "model", cfg.Model.Model, "tools", len(tools))
	return &investigate.LoopInvestigator{
		Model: model,
		Tools: tools,
		Log:   log,
		OnComplete: func(found providers.Investigation) {
			log.Info("findings",
				"confidence", found.Confidence, "root_causes", len(found.RootCauses), "unresolved", len(found.Unresolved))
		},
	}
}

// startGitOpsFailureWatch drains Flux WatchFailures into the queue.
func startGitOpsFailureWatch(ctx context.Context, cfg *config.Config, q investigate.Enqueuer, fp *flux.Provider, log *slog.Logger) {
	events, err := fp.WatchFailures(ctx)
	if err != nil {
		log.Warn("gitops-failure watch disabled", "err", err)
		return
	}
	log.Info("watching gitops failures (Flux Kustomizations)")
	go investigate.DrainFailures(ctx, events, q, trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std()))
}
```

Update the import block to add `"github.com/Smana/runlore/internal/model/openai"` and `"github.com/Smana/runlore/internal/providers"`, and remove the old `startGitOpsFailureWatch` that built its own client (it now receives `fp`). Keep `dynamicClient()`.

- [ ] **Step 3: Build + gate + smoke test**

Run:
```bash
cd /home/smana/Sources/runlore
go mod tidy
go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l . && golangci-lint run ./...
```
Expected: clean, `0 issues`.

Smoke (no model → log-only investigator path still works, no cluster needed):
```bash
go build -o /tmp/lore ./cmd/lore
cat > /tmp/rl.yaml <<'EOF'
triggers:
  incidents:
    enabled: true
    match: { severity: [critical], environment: [prod] }
    dedup: { window: 30m }
  gitops_failures: { enabled: false }
EOF
/tmp/lore serve --config /tmp/rl.yaml --addr :18084 >/tmp/rl.log 2>&1 &
SRV=$!
curl -s --retry-connrefused --retry 10 --retry-delay 1 -o /dev/null localhost:18084/healthz
curl -s -o /dev/null -XPOST localhost:18084/webhook/alertmanager --data @examples/alertmanager-webhook.json
curl -s -o /dev/null --retry 3 --retry-delay 1 localhost:18084/healthz
kill $SRV 2>/dev/null
grep -E 'no model configured|msg=investigate' /tmp/rl.log
```
Expected: `no model configured; using log-only investigator`, then `msg=investigate …` for the matched incident. (With a model `base_url` set + a reachable endpoint, the investigator would instead run the ReAct loop and log `findings …` — manual/integration verification, not part of this smoke.)

- [ ] **Step 4: Commit**

```bash
cd /home/smana/Sources/runlore
git add internal/config/config.go cmd/lore/main.go go.mod go.sum
git commit -m "feat(serve): use the LLM ReAct investigator when a model is configured"
```

---

## What this plan delivers

`lore serve` now runs a real LLM investigation when a model is configured (`model.base_url`): incidents/GitOps-failures → queue → `LoopInvestigator` (OpenAI-compatible model + the what-changed tool) → structured findings logged. No model configured → the read-only log-only path, unchanged. The OpenAI-compatible client works with in-cluster vLLM, Ollama, OpenAI, or OpenRouter.

## Next plans (not in this plan)

- **Delivery**: replace the `OnComplete` log with Slack/Matrix posting of the investigation.
- **Native Anthropic** `ModelProvider` (different wire format).
- **More tools**: metrics (PromQL), logs, network (Hubble); **catalog `kb_search`**.
- Surface the API key via a Secret/External Secrets rather than only an env var.

---

## Self-Review

- **Spec coverage:** OpenAI-compatible `ModelProvider` (tested via `httptest`, CI-safe); `serve` builds `LoopInvestigator` (model + what-changed tool) when configured, else `LogInvestigator`; kube client built once and shared. Delivery/Anthropic/more-tools are named follow-ups. ✅
- **Placeholder scan:** Complete code per step; the `OnComplete` log is an interim delivery (next plan), explicitly noted. ✅
- **Type consistency:** the client satisfies `providers.ModelProvider` (compile-time check); request/response mapping covers system/user/assistant(tool_calls)/tool(tool_call_id) and tool specs; `config.Model` consumed by `buildInvestigator`; `LoopInvestigator`/`WhatChangedTool`/`Investigator` used per their contracts; `startGitOpsFailureWatch` updated to receive the shared provider. ✅
