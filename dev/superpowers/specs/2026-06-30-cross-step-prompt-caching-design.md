# Cross-step prompt caching + cache observability (T1 + T2 + ③)

Status: draft for review
Date: 2026-06-30
Scope: one PR, dedicated worktree (`feat/model-prompt-caching`). Cost optimization + the observability to prove it.

## Problem

RunLore's investigation loop (`internal/investigate/loop.go`) resends the *entire* growing message history on every ReAct step (`loop.go:238`) with no window or summarization, so cumulative input is roughly O(N²) in steps × tool-output size. Prompt caching is the lever — but today it is only partly used, and entirely unmeasured:

- **Anthropic** marks only the *static* prefix (system + tool schemas) as cacheable (`anthropic.go:161,173-175`). The growing tool-output history carries no cache breakpoint, so every accumulated tool result is re-billed at full input price on every later step — the dominant cost.
- **Gemini** uses no caching. Implicit caching is automatic on Gemini 2.5+ models from a common prompt prefix (confirmed: Gemini docs, "Context caching > Implicit caching"; min 2048 tokens on 2.5, 4096 on 3.x). Our requests are already deterministic and append-only, so the prefix is *already* cache-eligible — but nothing guarantees that stays true, and nothing measures it.
- **OpenAI** already gets automatic server-side prefix caching (covers both the static prefix and the unchanged history) with zero client work.
- **All three** report cache usage we discard: `providers.Usage` (`providers.go:465-469`) carries only `InputTokens`/`OutputTokens`. So we cannot tell whether any caching is working, nor catch a regression that silently drops the hit rate to zero.

## Design

One PR, three parts, shared theme: make cross-step caching active (Anthropic), automatic-and-guarded (Gemini), and **measurable across all three providers**.

### ① T1 — Anthropic: rolling cache breakpoint on conversation history

Anthropic allows up to 4 `cache_control` breakpoints and caches the request prefix up to and including each marked block. Today 2 are used (system block + last tool). Add a third: mark the **last content block of the last message** in the history each step.

- Add a `CacheControl *cacheControl` field (json `cache_control,omitempty`) to the `block` struct (`anthropic.go:98-109`) — it currently has none; only `systemBlock` and `tool` do.
- In `Complete`, after `toMessages(req.Messages)`, mark the last block of the last message with the shared `ephemeral` marker. Guard the empty case (no messages, or a message with no blocks) — skip silently.
- **One** rolling breakpoint (decision locked), giving 3 total (system + tools + history-tail), one under the limit. Each step: the previous step's marked prefix is a cache *read* (~0.1×), and Anthropic *writes* the newly-extended prefix. Below Anthropic's minimum cacheable size the breakpoint is ignored, so early steps are unaffected.

Why the last message's last block: the loop only ever *appends* to `messages` (`loop.go:320,359`), so the prefix up to the prior step's final block is byte-identical on the next step — a guaranteed rolling cache hit.

### ② T2 — Gemini: rely on implicit caching, guard the prefix

No request restructuring. The request is already deterministic (struct/slice serialization; the only map is `resultObject`'s single-key `{"result": …}`, `gemini.go:371`) and append-only, so the prefix is stable across steps and implicit caching applies on 2.5+ models.

- Add a **regression guard test**: build two successive `Complete` request bodies for a growing conversation (step N and step N+1, where N+1 appends an assistant turn + tool result) and assert the step-N serialized request is a byte-prefix of the step-N+1 request up to the divergence point — i.e. the system instruction + tools + earlier contents are byte-identical. This locks prefix stability so a future change that introduces volatility (a timestamp, map iteration, reordered field) into the cacheable prefix fails the test instead of silently killing the cache.
- Add a doc comment on the Gemini client recording that it depends on implicit caching and why prefix determinism matters.

(No change to field ordering: system_instruction is already first; Gemini processes system_instruction + tools as the stable header before the growing contents.)

### ③ Cache observability — all three providers (the backbone)

Extend the shared `Usage` contract so the cache win is provable and regressions are catchable.

**`providers.Usage` (`providers.go:465-469`) gains two fields:**

```
InputTokens       int // total input/prompt tokens billed (INCLUDING any served from cache — normalized across providers)
OutputTokens      int // generated/output tokens
CachedInputTokens int // the subset of InputTokens that was a cache READ (Anthropic cache_read, OpenAI cached_tokens, Gemini cachedContent); the saving
CacheWriteTokens  int // Anthropic only: tokens WRITTEN to cache this request (cache_creation, billed ~1.25x); 0 for providers that don't report it
```

**Critical normalization** (the one cross-provider subtlety): the providers disagree on whether `input_tokens` already includes cached tokens. Each provider MUST normalize so `InputTokens` is the *total* footprint and `CachedInputTokens` is the cached subset:

- **Anthropic** reports `input_tokens` as the *non-cached remainder*, with `cache_read_input_tokens` and `cache_creation_input_tokens` separate. So set `InputTokens = input_tokens + cache_read_input_tokens + cache_creation_input_tokens`, `CachedInputTokens = cache_read_input_tokens`, `CacheWriteTokens = cache_creation_input_tokens`. (Add these two fields to `usageDelta`, `anthropic.go:148-151`; they arrive on `message_start`.)
- **OpenAI** reports `prompt_tokens` as the *total*, with `prompt_tokens_details.cached_tokens` a subset. So `InputTokens = prompt_tokens` (unchanged), `CachedInputTokens = prompt_tokens_details.cached_tokens`. (Add `prompt_tokens_details.cached_tokens` to the usage chunk, `openai.go:122-125`.) `CacheWriteTokens = 0`.
- **Gemini** reports `promptTokenCount` as the *total*, with `cachedContentTokenCount` a subset. So `InputTokens = promptTokenCount` (unchanged), `CachedInputTokens = cachedContentTokenCount`. (Add `cachedContentTokenCount` to `UsageMetadata`, `gemini.go:135-138`.) `CacheWriteTokens = 0`.

Result: `CachedInputTokens / InputTokens` is a comparable cache-hit ratio across all three providers, and `CachedInputTokens == 0` over a multi-step investigation is the regression signal.

**Metrics (`internal/telemetry/metrics.go`):** add two `Int64Counter`s labelled by provider, following the existing `ctr(...)` pattern and the `ModelRequests` naming:

```
ModelInputTokens       metric.Int64Counter // total input tokens across LLM requests (label: provider)
ModelCachedInputTokens metric.Int64Counter // input tokens served from cache (label: provider)
```

**Recording (`loop.go`):** where model metrics are already recorded after each `Complete` (the `li.Metrics != nil` block around `loop.go:239-248`), also add `resp.Usage.InputTokens` and `resp.Usage.CachedInputTokens` to the new counters (provider label = `li.ModelProvider`). Nil-safe like the surrounding metric calls. This is recorded for the main loop's calls; the verify pass (`verify.go`) is out of scope for metric wiring in this PR (its calls are few and cheap).

### OpenAI

No caching change (server-side automatic). Only the part-③ `cached_tokens` parsing.

## Files touched

- `internal/providers/providers.go` — extend `Usage` (2 fields + doc).
- `internal/model/anthropic/anthropic.go` — T1 (block `CacheControl` + mark last message block); usage cache fields (`usageDelta` + normalized population).
- `internal/model/anthropic/anthropic_test.go` — T1 breakpoint test; usage normalization test.
- `internal/model/openai/openai.go` — `cached_tokens` parsing + population.
- `internal/model/openai/openai_test.go` — cached-token usage test.
- `internal/model/gemini/gemini.go` — `cachedContentTokenCount` parsing + population; implicit-caching doc comment.
- `internal/model/gemini/gemini_test.go` — cached-token usage test; **prefix-stability guard test**.
- `internal/telemetry/metrics.go` — 2 new counters.
- `internal/investigate/loop.go` — record the 2 new counters after each `Complete`.

## Testing

- **Anthropic T1:** marshal a `Complete` request with a multi-message history; assert `cache_control: ephemeral` is present on (a) the system block, (b) the last tool, and (c) the last block of the last message — and absent elsewhere. Assert total breakpoints == 3.
- **Anthropic usage:** feed a synthetic `message_start` usage with `input_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`; assert `InputTokens == sum`, `CachedInputTokens == cache_read`, `CacheWriteTokens == cache_creation`.
- **OpenAI usage:** feed a usage chunk with `prompt_tokens` + `prompt_tokens_details.cached_tokens`; assert `InputTokens == prompt_tokens`, `CachedInputTokens == cached_tokens`.
- **Gemini usage:** feed `usageMetadata` with `promptTokenCount` + `cachedContentTokenCount`; assert `InputTokens == promptTokenCount`, `CachedInputTokens == cachedContentTokenCount`.
- **Gemini prefix-stability guard:** serialize step-N and step-N+1 requests for an append-only conversation; assert the divergence point lands only where the new turn was appended (system instruction + tools + earlier contents byte-identical).
- All existing model + loop tests stay green; `go build ./... && go vet`.

## Non-goals (out of scope for this PR)

- Mid-loop tool-output compaction (thread T3 — designed AFTER this, since compaction rewrites history and would invalidate T1's rolling breakpoint).
- Token-aware truncation; per-task output caps.
- Explicit Gemini `CachedContent` API (rejected: per-investigation create/delete lifecycle + TTL/expiry hazards aren't worth it for sub-2-minute loops).
- Verify/judge-pass cache-metric wiring.
- Surfacing cached tokens in the Slack/PR delivery (metrics only).

## Interaction note for the next thread (T3)

T1's rolling breakpoint assumes the message history is **append-only**. The T3 compaction thread will mutate/summarize earlier history, which invalidates a cached prefix at the mutation point. T3 must therefore (a) place its compaction boundary *before* the rolling breakpoint and only rewrite below it, or (b) accept a one-time cache miss on the step compaction fires. This is called out in the T3 spec when it's written; recorded here so the dependency isn't lost.
