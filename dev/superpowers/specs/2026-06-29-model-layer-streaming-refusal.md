# Model layer: internal streaming, configurable max_tokens, refusal handling

- **Date:** 2026-06-29
- **Status:** design approved; ready for implementation plan
- **Scope:** `internal/model/{anthropic,openai,gemini}`, `internal/httpx`, `internal/config`, `internal/investigate` (loop refusal handling)
- **Origin:** Wave-2 of the 2026-06-29 audit follow-ups ([../../plans/2026-06-29-audit-followups.md](../../plans/2026-06-29-audit-followups.md), item `MODEL-MODERNIZE`).

## Context & goal

RunLore's LLM client is a hand-rolled, SDK-free multi-provider HTTP client (Anthropic, OpenAI-compatible, Gemini) behind a one-method `providers.ModelProvider` interface. Today it is **non-streaming** with a flat 2-minute HTTP timeout and a hardcoded Anthropic `max_tokens: 4096` (OpenAI/Gemini send none), and it does **not** handle model refusal/safety stop reasons. Consequences: truncation/timeout risk on a long `submit_findings`; no control over output length/cost; and a refusal is misread as an empty prose turn, burning a nudge step.

**Goal:** add (1) internal streaming, (2) a configurable `max_tokens`, and (3) refusal/safety handling — **without changing the `ModelProvider.Complete` interface** (design decision A).

## Non-goals (deferred to a follow-up)

- Caller-facing token streaming / a `CompleteStream` method.
- Adaptive thinking / `effort`, `count_tokens`-based budgeting, `cache_read_input_tokens` parsing, cost telemetry.

## Approach

`ModelProvider.Complete(ctx, CompletionRequest) (CompletionResponse, error)` stays **unchanged**. Each provider switches its HTTP request to streaming (SSE) and **accumulates** the full response internally, returning the same `CompletionResponse`. The investigation loop is unchanged except for handling a new refusal signal.

## Components

### 1. Config (`internal/config`)
- Add `MaxTokens int` to `config.Model` (YAML `max_tokens`), optional; default **8192** when unset.
- `model.verify` (cheaper judge model) **inherits** `max_tokens` unless it sets its own.
- Validation: `max_tokens >= 0`; `0` → use the 8192 default. Field must work under the strict `KnownFields(true)` decoder.

### 2. Internal streaming (per provider + `internal/httpx`)
General: build the request with provider-specific `stream:true`, POST, read the SSE body incrementally, accumulate into `CompletionResponse{Text, ToolCalls, Usage, StopReason, Truncated}`. Reuse `httpx.SecureClient` (SSRF guard) but with a **streaming-appropriate timeout**: drop the flat 2-minute overall deadline; rely on `ctx` + a per-read/idle timeout so a long generation isn't killed mid-stream. `DoWithRetry` applies to **connection establishment only**, not mid-stream (a mid-stream drop returns an error → the loop retries the whole step).
- **Anthropic** `/v1/messages` `stream:true`: handle SSE events `message_start` (input usage), `content_block_start`/`content_block_delta` (`text_delta` → text; `input_json_delta` → accumulate tool_use partial JSON per block), `content_block_stop`, `message_delta` (`stop_reason`, output usage), `message_stop`. Reassemble `tool_use` blocks into `ToolCall{ID,Name,Args}`. Send `max_tokens`. Keep prompt-cache breakpoints + `anthropic-version`.
- **OpenAI** `/chat/completions` `stream:true` + `stream_options:{include_usage:true}`: accumulate `choices[0].delta.content` and `delta.tool_calls[i]` by index (id/name/arguments fragments), `finish_reason`, and the final usage chunk. Send `max_tokens`. Keep the non-`omitempty` tool-message `content`.
- **Gemini** `:streamGenerateContent?alt=sse`: accumulate `candidates[0].content.parts` (text + functionCall), `finishReason`, `usageMetadata`. Send `generationConfig.maxOutputTokens`. Keep functionCall id-correlation + `resultObject` wrapping.

### 3. Refusal / safety handling (`internal/model/*` + `internal/investigate/loop.go`)
- Add `StopReason string` to `CompletionResponse` (keep `Truncated` for the max_tokens case). Set it from each provider's terminal reason.
- Detect refusal/safety: Anthropic `stop_reason == "refusal"`; OpenAI `finish_reason == "content_filter"`; Gemini `finishReason ∈ {SAFETY, PROHIBITED_CONTENT, BLOCKLIST, SPII}`. Expose `CompletionResponse.Refused() bool`.
- In the loop: a `Refused()` turn ends the investigation as a first-class **`unresolved`** result with a clear note ("the model declined / safety-filtered the content; no root cause produced") — **not** an empty prose turn (no nudge), and **not** a retry.

## Error handling
- Mid-stream connection error → wrap with `%w`, return; the loop's existing error path retries/surfaces. Discard partial output.
- `max_tokens` reached (`Truncated`) → keep the existing "re-prompt once to continue concisely".
- Refusal → terminal `unresolved` outcome (not an error, not a retry).
- Malformed SSE/JSON → wrap + return; never panic.

## Testing (TDD)
- Per provider: `httptest` server emitting a canned SSE stream; assert `Complete` accumulates text, multi-tool-call args (by index/block), usage, stop_reason, truncation. Mid-stream connection drop → error.
- Refusal per provider: stream a refusal/safety terminal reason → `Refused()` true; loop test → investigation is `unresolved` with the note, model called exactly once (no retry storm).
- `max_tokens`: assert it is sent in each provider's request; `model.verify` inherits when unset, overrides when set.
- All existing model + loop tests stay green; run under `-race`.

## Acceptance criteria
- `Complete` interface unchanged; callers untouched.
- All 3 providers stream internally; long outputs no longer hit the 2-minute timeout; `max_tokens` configurable (default 8192); verify inherits.
- Refusal → first-class `unresolved` with a note; no nudge/retry burned.
- `go build && go vet && gofmt -l . && go test -race ./...` green; `golangci-lint` clean.

## Rollout
Wave-1 is merged (`origin/main` @ `812d923`, includes the `config.go` severity change this also edits). Implement on a branch off current `main`. Single cohesive PR; split per provider if the diff gets large. Commit this spec + the implementation plan alongside the code.
