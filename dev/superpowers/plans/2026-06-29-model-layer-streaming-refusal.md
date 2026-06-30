# Model-layer streaming + max_tokens + refusal — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: `superpowers:subagent-driven-development`. Design detail lives in the companion spec: [`../specs/2026-06-29-model-layer-streaming-refusal.md`](../specs/2026-06-29-model-layer-streaming-refusal.md) — read it first. Steps use `- [ ]` tracking.

**Goal:** Add internal streaming, a configurable `max_tokens`, and refusal/safety handling to all three LLM providers **without changing** the `providers.ModelProvider.Complete` interface.

**Architecture:** Each provider streams its HTTP response (SSE) and accumulates into the existing `CompletionResponse`. New config `model.max_tokens` (default 8192, `verify` inherits). New `CompletionResponse.StopReason` + `Refused()`; the investigation loop turns a refusal into a first-class `unresolved` outcome.

**Tech stack:** Go 1.26; hand-rolled providers in `internal/model/{anthropic,openai,gemini}`; `internal/httpx` (SSRF-guarded client + `DoWithRetry`); `httptest` SSE servers for tests; `-race`.

**Conventions (AGENTS.md):** TDD (failing test first), table-driven tests that verify *behaviour* not mocks, wrap errors with `%w`, doc comments on exported symbols, `gofmt`+`golangci-lint v2` clean. **No `Co-Authored-By` trailers.**

**Branch:** `feat/model-streaming` off `main` (`812d923`, Wave-1 merged — `config.go` already has `containsFold`).

---

### Task 1: Config — `model.max_tokens` + verify inheritance
**Files:** `internal/config/config.go` (Model struct + `Validate`), `internal/config/config_test.go`, `internal/app/model.go` (apply default + verify inheritance at provider construction).

- [ ] **Test first** — `max_tokens: 16384` parses to `Model.MaxTokens==16384`; unset → `0`; negative → `Validate` error; a `model.verify` block with no `max_tokens` resolves to the parent's effective value, but overrides when set.
- [ ] Run → fail.
- [ ] **Implement** — add `MaxTokens int \`yaml:"max_tokens"\`` to `Model` (doc comment: "0 = use the 8192 default"); reject `< 0` in `Validate`; in `app/model.go` compute `effectiveMaxTokens := cfg.MaxTokens; if effectiveMaxTokens == 0 { effectiveMaxTokens = 8192 }` and pass it into each provider's constructor; the verify model inherits the parent's effective value unless it set its own.
- [ ] Run → pass; `gofmt`/`vet`.
- [ ] **Commit** `feat(config): add model.max_tokens (default 8192), verify inherits`.

### Task 2: `CompletionResponse` — `StopReason` + `Refused()`
**Files:** `internal/providers/providers.go` (CompletionResponse), `internal/providers/providers_test.go` (or a focused test near the type).

- [ ] **Test first** — `Refused()` is true for stop reasons `"refusal"`, `"content_filter"`, `"safety"`, `"prohibited_content"`, `"blocklist"`, `"spii"` (case-insensitive) and false for `"end_turn"`/`"stop"`/`"max_tokens"`/empty.
- [ ] Run → fail.
- [ ] **Implement** — add `StopReason string` to `CompletionResponse` (keep `Truncated` for the max_tokens case); add method `func (r CompletionResponse) Refused() bool` matching the set above via `strings.EqualFold`/lowercase compare. Doc-comment both.
- [ ] Run → pass.
- [ ] **Commit** `feat(providers): add CompletionResponse.StopReason + Refused()`.

### Task 3: `httpx` — streaming-friendly client
**Files:** `internal/httpx/client.go` (+ `retry.go` if needed), `internal/httpx/*_test.go`.

- [ ] **Test first** — a streaming response that takes longer than the old flat 2-minute deadline but keeps sending data is NOT killed; `ctx` cancellation still aborts promptly; an idle stream (no bytes) times out via a read/idle deadline.
- [ ] Run → fail.
- [ ] **Implement** — provide a `SecureStreamingClient(...)` (or an option on `SecureClient`) that keeps the SSRF redirect guard but removes the flat overall `Timeout`, relying on `ctx` + a `ResponseHeaderTimeout`/read-deadline for idle detection. `DoWithRetry` must retry only on connection establishment, not after streaming has begun. Keep existing `SecureClient` for non-streaming callers (forge, notifiers, metrics, logs).
- [ ] Run → pass.
- [ ] **Commit** `feat(httpx): streaming-friendly secure client (no flat deadline, idle timeout)`.

### Task 4: Anthropic streaming
**Files:** `internal/model/anthropic/anthropic.go`, `internal/model/anthropic/anthropic_test.go`. Mirror the existing non-streaming `Complete`; see spec §2.

- [ ] **Test first** — `httptest` server emits an Anthropic SSE stream (`message_start`→`content_block_delta` text + a `tool_use` block with `input_json_delta` fragments→`message_delta` with `stop_reason`+usage→`message_stop`). Assert `Complete` returns the accumulated text, a `ToolCall{ID,Name,Args}` with reassembled JSON args, `Usage{Input,Output}`, and `StopReason`. Add a refusal case (`stop_reason:"refusal"`, empty content) → `Refused()` true. Add a mid-stream connection-drop case → error. Keep `TestNon2xxErrorOmitsBody`, `TestMessageCoalescing`, `TestPromptCacheToolsOnly` green.
- [ ] Run → fail.
- [ ] **Implement** — set `stream:true` and `max_tokens` (from config) on the request; consume SSE incrementally with the streaming client; accumulate text + per-block `input_json_delta` into tool_use args; map `stop_reason`→`StopReason` (+ `Truncated` when `max_tokens`). Preserve `anthropic-version`, prompt-cache breakpoints, request-id redaction.
- [ ] Run → pass.
- [ ] **Commit** `feat(model/anthropic): stream /v1/messages and accumulate; send max_tokens`.

### Task 5: OpenAI-compatible streaming
**Files:** `internal/model/openai/openai.go`, `internal/model/openai/openai_test.go`.

- [ ] **Test first** — SSE stream of `chat.completion.chunk`s: `delta.content` fragments + `delta.tool_calls[i]` (id/name/arguments fragments by index) + a final `finish_reason` + a usage chunk (`stream_options.include_usage`). Assert accumulation, multi-tool-call reassembly by index, `Usage`, `StopReason`. Refusal case: `finish_reason:"content_filter"` → `Refused()`. Mid-stream drop → error. Keep the tool-message `content` (non-omitempty) test green.
- [ ] Run → fail.
- [ ] **Implement** — `stream:true` + `stream_options:{include_usage:true}` + `max_tokens`; accumulate deltas by choice/tool-call index; map `finish_reason`→`StopReason`.
- [ ] Run → pass.
- [ ] **Commit** `feat(model/openai): stream chat/completions and accumulate; send max_tokens`.

### Task 6: Gemini streaming
**Files:** `internal/model/gemini/gemini.go`, `internal/model/gemini/gemini_test.go`.

- [ ] **Test first** — `:streamGenerateContent?alt=sse` SSE of partial `candidates`: text parts + `functionCall` parts (with id correlation) + `finishReason` + `usageMetadata`. Assert accumulation, functionCall→`ToolCall`, `Usage`, `StopReason`. Refusal: `finishReason:"SAFETY"` → `Refused()`. Mid-stream drop → error.
- [ ] Run → fail.
- [ ] **Implement** — switch endpoint to `:streamGenerateContent?alt=sse`; set `generationConfig.maxOutputTokens`; accumulate parts; map `finishReason`→`StopReason`. Preserve functionCall id-correlation + `resultObject` wrapping.
- [ ] Run → pass.
- [ ] **Commit** `feat(model/gemini): stream generateContent and accumulate; send maxOutputTokens`.

### Task 7: Loop refusal handling
**Files:** `internal/investigate/loop.go`, `internal/investigate/loop_test.go`.

- [ ] **Test first** — a scriptModel whose response is `Refused()` (e.g. `StopReason:"refusal"`, no tool calls) → the investigation is delivered as `unresolved` with a note like "model declined / safety-filtered; no root cause produced"; the model is called **exactly once** (no nudge, no retry storm); `Confidence==0`, `RootCauses` empty.
- [ ] Run → fail.
- [ ] **Implement** — in the loop's response handling, before the prose-turn nudge path, check `resp.Refused()`: if so, build a synthetic `unresolved` investigation (reuse the timeout/budget synthetic-result pattern) with the refusal note and return it. Redaction + delivery unchanged.
- [ ] Run → pass.
- [ ] **Commit** `feat(investigate): treat model refusal as a first-class unresolved outcome`.

### Task 8: Integration + gate
**Files:** none new — verification.

- [ ] Confirm `app/model.go` passes `max_tokens` to all 3 providers and the verify model inherits (add an `app` test if not already covered by Task 1).
- [ ] Run the full gate: `go build ./... && go vet ./... && gofmt -l . && go test -race ./...` → all green; `golangci-lint run ./...` → 0 issues.
- [ ] **Commit** any fixups; ensure the spec + this plan are committed on the branch.

---

## Self-review
- **Spec coverage:** config max_tokens+inheritance (T1, T8), interface-unchanged streaming per provider (T3–T6), StopReason/Refused (T2), refusal→unresolved (T7), error handling (drop tests in T4–T6), testing strategy (httptest SSE per task) — all spec sections map to a task.
- **Type consistency:** `CompletionResponse.StopReason` / `Refused()` defined in T2 and consumed in T4–T7; `MaxTokens` defined in T1 and consumed in T4–T6/T8.
- **No placeholders:** SSE plumbing references spec §2 + the existing provider files to mirror (the implementer reads real code); new/critical code (config field, Refused set, loop branch) is specified inline.
