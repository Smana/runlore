# Cross-step Prompt Caching + Cache Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut the investigation loop's dominant token cost by caching the growing conversation prefix on every provider, and make the cache win measurable.

**Architecture:** Three independent provider changes behind the shared `providers.Usage` contract. Anthropic gets a rolling `cache_control` breakpoint on conversation history (active win). Gemini relies on automatic implicit caching, guarded by a prefix-stability test. All three providers parse the cache-token fields they already return, normalized so a hit-ratio is comparable, and the loop records two new metrics.

**Tech Stack:** Go 1.26, standard library + OpenTelemetry metrics (already a dependency). No new module deps.

## Global Constraints

- Go 1.26.0; standard library + existing deps only — no new module dependencies.
- No co-authored commits; no AI attribution in commit messages or PR text.
- **Usage normalization (load-bearing, verified against provider docs):** `Usage.InputTokens` is the TOTAL input footprint INCLUDING cached tokens; `Usage.CachedInputTokens` is the cache-READ subset. Anthropic reports `input_tokens` as the *non-cached remainder*, so `InputTokens = input_tokens + cache_read_input_tokens + cache_creation_input_tokens`. OpenAI `prompt_tokens` and Gemini `promptTokenCount` already include the cached subset, so `InputTokens` = that value unchanged.
- Cache fields default to 0 ("unknown / none") when a provider omits them — never fabricate.
- Follow existing per-package test patterns: model packages use the `sseServer(t, capture, events)` helper and decode the captured request body into the provider's request struct; telemetry uses the `metrics_test.go` style.
- Spec: `dev/superpowers/specs/2026-06-30-cross-step-prompt-caching-design.md`.

---

### Task 1: Anthropic — usage cache fields + T1 rolling breakpoint

**Files:**
- Modify: `internal/providers/providers.go` (extend `Usage` — first task to need it)
- Modify: `internal/model/anthropic/anthropic.go` (`block` struct, `usageDelta`, `Complete` history marking, `accumulate` message_start)
- Test: `internal/model/anthropic/anthropic_test.go`

**Interfaces:**
- Produces: `providers.Usage` with two new int fields `CachedInputTokens`, `CacheWriteTokens` (consumed by Tasks 2, 3, 4).

- [ ] **Step 1: Write the failing tests**

Add to `internal/model/anthropic/anthropic_test.go`:

```go
// TestPromptCacheHistoryBreakpoint asserts the rolling breakpoint: the last content
// block of the last message carries cache_control, alongside system + last tool.
func TestPromptCacheHistoryBreakpoint(t *testing.T) {
	var gotReq msgRequest
	srv := sseServer(t, func(r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
	}, []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	})
	defer srv.Close()

	_, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		System: "sys",
		Messages: []providers.Message{
			{Role: "user", Content: "incident"},
			{Role: "assistant", Content: "thinking", ToolCalls: []providers.ToolCall{{ID: "t1", Name: "a", Args: "{}"}}},
			{Role: "tool", ToolCallID: "t1", Content: "result"},
		},
		Tools: []providers.ToolSpec{{Name: "a", Description: "d", Schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// system marked
	if len(gotReq.System) == 0 || gotReq.System[0].CacheControl == nil {
		t.Fatalf("system block should be a cache breakpoint: %+v", gotReq.System)
	}
	// last tool marked
	lt := gotReq.Tools[len(gotReq.Tools)-1]
	if lt.CacheControl == nil || lt.CacheControl.Type != "ephemeral" {
		t.Fatalf("last tool should be a cache breakpoint: %+v", lt)
	}
	// last block of the last message marked (the rolling breakpoint)
	last := gotReq.Messages[len(gotReq.Messages)-1]
	lb := last.Content[len(last.Content)-1]
	if lb.CacheControl == nil || lb.CacheControl.Type != "ephemeral" {
		t.Fatalf("last message's last block should be the rolling cache breakpoint: %+v", last)
	}
	// an earlier message block must NOT be marked
	if gotReq.Messages[0].Content[0].CacheControl != nil {
		t.Fatalf("earlier message blocks must not be marked: %+v", gotReq.Messages[0])
	}
}

// TestUsageCacheFields asserts the Anthropic usage normalization: InputTokens is the
// sum of input + cache_read + cache_creation; the read/creation subsets are reported.
func TestUsageCacheFields(t *testing.T) {
	srv := sseServer(t, nil, []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":30,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":20}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 150 || resp.Usage.CachedInputTokens != 100 || resp.Usage.CacheWriteTokens != 20 {
		t.Fatalf("usage = %+v, want in=150 cached=100 write=20", resp.Usage)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/model/anthropic/ -run 'TestPromptCacheHistoryBreakpoint|TestUsageCacheFields' -v`
Expected: compile error first (the new `Usage` / `block.CacheControl` fields don't exist), then FAIL.

- [ ] **Step 3: Extend `providers.Usage`**

In `internal/providers/providers.go`, replace the `Usage` struct:

```go
// Usage is the provider-reported token accounting for one completion.
type Usage struct {
	InputTokens  int // total prompt/input tokens billed, INCLUDING any served from cache (normalized across providers)
	OutputTokens int // generated/output tokens in the reply
	// CachedInputTokens is the subset of InputTokens that was a cache READ (Anthropic
	// cache_read_input_tokens, OpenAI prompt_tokens_details.cached_tokens, Gemini
	// cachedContentTokenCount) — the saving. 0 when the provider reports none.
	CachedInputTokens int
	// CacheWriteTokens is input tokens WRITTEN to the cache this request (Anthropic
	// cache_creation_input_tokens, billed ~1.25x). 0 for providers that don't report it.
	CacheWriteTokens int
}
```

- [ ] **Step 4: Add `CacheControl` to the `block` struct**

In `internal/model/anthropic/anthropic.go`, add the field to `block` (after the tool_result fields):

```go
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	// cache breakpoint (set on the last block of the last message — the rolling history breakpoint)
	CacheControl *cacheControl `json:"cache_control,omitempty"`
```

- [ ] **Step 5: Add cache fields to `usageDelta`**

In `internal/model/anthropic/anthropic.go`, extend `usageDelta`:

```go
type usageDelta struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}
```

- [ ] **Step 6: Mark the last message block in `Complete`, and normalize usage in `accumulate`**

In `Complete`, replace the `areq := msgRequest{...}` construction so the history is marked first:

```go
	msgs := toMessages(req.Messages)
	// Rolling cache breakpoint: mark the last content block of the last message, so the
	// growing conversation prefix is a cache READ on the next step. The loop only ever
	// APPENDS to history, so the prefix is byte-identical step to step — a guaranteed
	// rolling hit. Total breakpoints stay <= 4 (system + last tool + this one). Below
	// Anthropic's minimum cacheable size the marker is ignored, so early steps are fine.
	if n := len(msgs); n > 0 {
		if blocks := msgs[n-1].Content; len(blocks) > 0 {
			blocks[len(blocks)-1].CacheControl = ephemeral
		}
	}
	areq := msgRequest{Model: c.model, MaxTokens: c.maxTokens, Stream: true, Messages: msgs}
```

In `accumulate`, replace the `message_start` usage handling:

```go
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				u := ev.Message.Usage
				// Anthropic reports input_tokens as the NON-cached remainder; total input is
				// the sum of input + cache_read + cache_creation (per Anthropic docs).
				out.Usage.InputTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				out.Usage.CachedInputTokens = u.CacheReadInputTokens
				out.Usage.CacheWriteTokens = u.CacheCreationInputTokens
			}
```

- [ ] **Step 7: Run tests to verify pass + no regressions**

Run: `go test ./internal/model/anthropic/ -v`
Expected: PASS — the two new tests pass; existing tests including `TestPromptCacheToolsOnly`, `TestComplete`, `TestUsageAndStopReason` still pass (those send a single user message; the rolling marker lands on that message's block but they assert system/tool breakpoints and in/out token counts, which are unchanged — verify `TestComplete`'s `Usage.InputTokens` still equals its expected value since its message_start has no cache fields, so the sum equals plain input_tokens).

- [ ] **Step 8: Commit**

```bash
git add internal/providers/providers.go internal/model/anthropic/
git commit -m "feat(anthropic): cache conversation history + report cache tokens

Add a rolling cache_control breakpoint on the last block of the last message so the
growing tool-output history is a cache read across ReAct steps (was re-billed in full
every step). Extend providers.Usage with CachedInputTokens/CacheWriteTokens and
populate them from the Anthropic message_start usage (input normalized to the total)."
```

---

### Task 2: OpenAI — parse cached_tokens

**Files:**
- Modify: `internal/model/openai/openai.go` (`chatChunk.Usage`, `accumulate`)
- Test: `internal/model/openai/openai_test.go`

**Interfaces:**
- Consumes: `providers.Usage.CachedInputTokens` (Task 1).

- [ ] **Step 1: Write the failing test**

Add to `internal/model/openai/openai_test.go`:

```go
// TestUsageCachedTokens asserts prompt_tokens_details.cached_tokens maps to
// CachedInputTokens (OpenAI prompt_tokens already includes the cached subset).
func TestUsageCachedTokens(t *testing.T) {
	srv := sseServer(t, nil, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":200,\"completion_tokens\":9,\"prompt_tokens_details\":{\"cached_tokens\":160}}}\n\n",
		"data: [DONE]\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "m", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 200 || resp.Usage.CachedInputTokens != 160 {
		t.Fatalf("usage = %+v, want in=200 cached=160", resp.Usage)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/model/openai/ -run TestUsageCachedTokens -v`
Expected: FAIL — `CachedInputTokens` is 0 (field not yet populated).

- [ ] **Step 3: Add `prompt_tokens_details` to the usage chunk**

In `internal/model/openai/openai.go`, extend the `chatChunk.Usage` anonymous struct:

```go
	Usage *struct {
		PromptTokens       int `json:"prompt_tokens"`
		CompletionTokens   int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
```

- [ ] **Step 4: Populate `CachedInputTokens` in `accumulate`**

In `internal/model/openai/openai.go`, replace the `if ck.Usage != nil { ... }` block:

```go
		if ck.Usage != nil {
			u := providers.Usage{InputTokens: ck.Usage.PromptTokens, OutputTokens: ck.Usage.CompletionTokens}
			if ck.Usage.PromptTokensDetails != nil {
				u.CachedInputTokens = ck.Usage.PromptTokensDetails.CachedTokens
			}
			out.Usage = u
		}
```

- [ ] **Step 5: Run tests to verify pass + no regressions**

Run: `go test ./internal/model/openai/ -v`
Expected: PASS — new test passes; `TestComplete`/`TestTruncation` unchanged (their usage chunks have no `prompt_tokens_details`, so `CachedInputTokens` stays 0 and `InputTokens` is unchanged).

- [ ] **Step 6: Commit**

```bash
git add internal/model/openai/
git commit -m "feat(openai): report cached prompt tokens (prompt_tokens_details.cached_tokens)"
```

---

### Task 3: Gemini — parse cachedContentTokenCount + prefix-stability guard

**Files:**
- Modify: `internal/model/gemini/gemini.go` (`UsageMetadata`, `accumulate`, client doc comment)
- Test: `internal/model/gemini/gemini_test.go`

**Interfaces:**
- Consumes: `providers.Usage.CachedInputTokens` (Task 1).

- [ ] **Step 1: Write the failing tests**

Add to `internal/model/gemini/gemini_test.go` (it already imports `reflect`? if not, add `"reflect"` to imports):

```go
// TestUsageCachedContent asserts cachedContentTokenCount maps to CachedInputTokens
// (Gemini promptTokenCount already includes the cached subset).
func TestUsageCachedContent(t *testing.T) {
	srv := sseServer(t, nil, []string{
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":220,\"candidatesTokenCount\":8,\"cachedContentTokenCount\":180}}\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 220 || resp.Usage.CachedInputTokens != 180 {
		t.Fatalf("usage = %+v, want in=220 cached=180", resp.Usage)
	}
}

// TestRequestPrefixStable guards implicit caching: across two successive Complete calls
// for an append-only conversation, the system instruction, tools, and earlier contents
// must be byte-identical (only new turns appended), so Gemini's implicit cache hits.
func TestRequestPrefixStable(t *testing.T) {
	var bodies [][]byte
	capture := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
	}
	events := []string{"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1}}\n\n"}
	srv := sseServer(t, capture, events)
	defer srv.Close()
	c := New(srv.URL, "gemini-x", "k", 0)
	tools := []providers.ToolSpec{{Name: "a", Description: "d", Schema: `{"type":"object"}`}}

	// Step N
	if _, err := c.Complete(context.Background(), providers.CompletionRequest{
		System: "sys", Tools: tools,
		Messages: []providers.Message{{Role: "user", Content: "incident"}},
	}); err != nil {
		t.Fatalf("Complete N: %v", err)
	}
	// Step N+1: same prefix, one appended assistant turn + tool result
	if _, err := c.Complete(context.Background(), providers.CompletionRequest{
		System: "sys", Tools: tools,
		Messages: []providers.Message{
			{Role: "user", Content: "incident"},
			{Role: "assistant", Content: "", ToolCalls: []providers.ToolCall{{ID: "t1", Name: "a", Args: "{}"}}},
			{Role: "tool", ToolCallID: "t1", Content: "result"},
		},
	}); err != nil {
		t.Fatalf("Complete N+1: %v", err)
	}

	var r0, r1 genRequest
	if err := json.Unmarshal(bodies[0], &r0); err != nil {
		t.Fatalf("unmarshal r0: %v", err)
	}
	if err := json.Unmarshal(bodies[1], &r1); err != nil {
		t.Fatalf("unmarshal r1: %v", err)
	}
	if !reflect.DeepEqual(r0.SystemInstruction, r1.SystemInstruction) {
		t.Fatal("system instruction must be byte-stable across steps (implicit-cache prefix)")
	}
	if !reflect.DeepEqual(r0.Tools, r1.Tools) {
		t.Fatal("tools must be byte-stable across steps (implicit-cache prefix)")
	}
	if len(r1.Contents) <= len(r0.Contents) {
		t.Fatalf("step N+1 must append contents: len0=%d len1=%d", len(r0.Contents), len(r1.Contents))
	}
	if !reflect.DeepEqual(r0.Contents, r1.Contents[:len(r0.Contents)]) {
		t.Fatal("earlier contents must be unchanged (append-only) so the prefix stays cacheable")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/model/gemini/ -run 'TestUsageCachedContent|TestRequestPrefixStable' -v`
Expected: `TestUsageCachedContent` FAILs (CachedInputTokens 0). `TestRequestPrefixStable` should already PASS if the request is already append-only/deterministic — that is acceptable and expected (it is a guard, not a RED-first behavior test); note that in the report. If it fails, the request has unexpected volatility — stop and report.

- [ ] **Step 3: Add `cachedContentTokenCount` to `UsageMetadata`**

In `internal/model/gemini/gemini.go`, extend the `UsageMetadata` anonymous struct:

```go
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
```

- [ ] **Step 4: Populate `CachedInputTokens` in `accumulate`**

In `internal/model/gemini/gemini.go`, replace the `if gr.UsageMetadata != nil { ... }` assignment:

```go
		if gr.UsageMetadata != nil {
			out.Usage = providers.Usage{
				InputTokens:       gr.UsageMetadata.PromptTokenCount,
				OutputTokens:      gr.UsageMetadata.CandidatesTokenCount,
				CachedInputTokens: gr.UsageMetadata.CachedContentTokenCount,
			}
		}
```

- [ ] **Step 5: Add the implicit-caching doc comment**

In `internal/model/gemini/gemini.go`, append to the package/`Client` doc (above `type Client struct`) a short note:

```go
// Caching: RunLore relies on Gemini's automatic IMPLICIT prefix caching (enabled on
// Gemini 2.5+). No explicit CachedContent lifecycle is used. This depends on the request
// prefix (system_instruction + tools + earlier contents) being byte-stable and append-only
// across the loop's steps; TestRequestPrefixStable guards that invariant.
```

- [ ] **Step 6: Run tests to verify pass + no regressions**

Run: `go test ./internal/model/gemini/ -v`
Expected: PASS — both new tests pass; existing tests unchanged (their usageMetadata has no cachedContentTokenCount, so CachedInputTokens stays 0).

- [ ] **Step 7: Commit**

```bash
git add internal/model/gemini/
git commit -m "feat(gemini): report cached-content tokens + guard implicit-cache prefix

Parse usageMetadata.cachedContentTokenCount into Usage.CachedInputTokens, and add a
prefix-stability test so a future change can't silently break the byte-stable,
append-only request prefix that Gemini's implicit caching depends on."
```

---

### Task 4: Telemetry counters + loop recording

**Files:**
- Modify: `internal/telemetry/metrics.go` (2 counters in the struct + `NewMetrics`)
- Modify: `internal/investigate/loop.go` (record after each `Complete`)
- Test: `internal/telemetry/metrics_test.go`

**Interfaces:**
- Consumes: `providers.Usage.InputTokens`, `providers.Usage.CachedInputTokens` (Task 1); `telemetry.Metrics` (existing).

- [ ] **Step 1: Write the failing test**

Add to `internal/telemetry/metrics_test.go` (match the file's existing assertion style; this asserts the two new counters are constructed):

```go
func TestModelTokenCountersConstructed(t *testing.T) {
	m := NewMetrics()
	if m.ModelInputTokens == nil || m.ModelCachedInputTokens == nil {
		t.Fatal("NewMetrics must construct ModelInputTokens and ModelCachedInputTokens")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/telemetry/ -run TestModelTokenCountersConstructed -v`
Expected: compile error — the fields don't exist yet.

- [ ] **Step 3: Add the two counters to the `Metrics` struct**

In `internal/telemetry/metrics.go`, add to the struct near `ModelResponsesTruncated`:

```go
	ModelInputTokens        metric.Int64Counter // total input tokens across LLM requests (label: provider)
	ModelCachedInputTokens  metric.Int64Counter // input tokens served from cache (label: provider)
```

- [ ] **Step 4: Construct them in `NewMetrics`**

In `internal/telemetry/metrics.go`, add to the returned struct literal near `ModelResponsesTruncated`:

```go
		ModelInputTokens:        ctr("model_input_tokens_total", "total LLM input tokens, including cached (label: provider)"),
		ModelCachedInputTokens:  ctr("model_cached_input_tokens_total", "LLM input tokens served from cache (label: provider)"),
```

- [ ] **Step 5: Record in the loop**

In `internal/investigate/loop.go`, inside the existing `if li.Metrics != nil { ... }` block that follows the `li.Model.Complete(...)` call (the one recording `ModelRequests` + `ModelRequestDuration`), after the duration `Record`, add:

```go
			if err == nil {
				provAttr := metric.WithAttributes(attribute.String("provider", li.ModelProvider))
				li.Metrics.ModelInputTokens.Add(ctx, int64(resp.Usage.InputTokens), provAttr)
				li.Metrics.ModelCachedInputTokens.Add(ctx, int64(resp.Usage.CachedInputTokens), provAttr)
			}
```

(`metric` and `attribute` are already imported in `loop.go`. Record only on success — on error `resp` is the zero value.)

- [ ] **Step 6: Run tests to verify pass + no regressions**

Run: `go test ./internal/telemetry/ ./internal/investigate/ -v 2>&1 | tail -25`
Expected: PASS — the new telemetry test passes; the existing investigate loop tests (which pass `Metrics: NewMetrics()` with `ModelProvider: "anthropic"`) still pass, exercising the new `.Add` calls without panic. (Per the codebase convention, metric *values* are not asserted — the existing `ModelRequests` recording is likewise value-untested.)

- [ ] **Step 7: Commit**

```bash
git add internal/telemetry/metrics.go internal/investigate/loop.go
git commit -m "feat(telemetry): record LLM input + cached-input tokens per provider

Two counters (model_input_tokens_total, model_cached_input_tokens_total, labelled by
provider) recorded after each completion, so the prompt-cache hit ratio is observable
and a cache regression (cached drops to ~0) is visible."
```

---

### Task 5: Full verification

**Files:** none (verification only).

- [ ] **Step 1: Build, full suite, vet**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: all green. If anything fails, stop and report BLOCKED with the exact output.

- [ ] **Step 2: Confirm no stray files / formatting**

Run: `gofmt -l internal/providers internal/model internal/telemetry internal/investigate`
Expected: no output (all formatted).

---

## Self-Review

**Spec coverage:**
- ① T1 Anthropic rolling breakpoint (one breakpoint, last block of last message, ≤4 total) → Task 1 Steps 4,6 + test. ✓
- ② T2 Gemini implicit-cache reliance + prefix-stability guard + doc → Task 3 Steps 4,5 + `TestRequestPrefixStable`. ✓
- ③ Usage extension with normalization (Anthropic sum; OpenAI/Gemini passthrough) → Task 1 Step 3, Tasks 1/2/3 population + tests. ✓
- ③ Metrics (2 counters, provider label) + loop recording → Task 4. ✓
- OpenAI cached_tokens → Task 2. ✓

**Placeholder scan:** No TBD/TODO; every code step is complete; commands have expected output. ✓

**Type consistency:** `Usage.CachedInputTokens`/`CacheWriteTokens` defined in Task 1 Step 3 and consumed identically in Tasks 2/3/4; `ModelInputTokens`/`ModelCachedInputTokens` defined and used with matching names; test field names match the structs (`msgRequest`, `genRequest`, `chatChunk`). ✓

**Note on `TestRequestPrefixStable`:** it is a guard that is expected to PASS immediately (the request is already append-only). This is intentional (a regression guard, not RED-first) and is called out in Task 3 Step 2 so the implementer doesn't treat the absent RED as a problem.
