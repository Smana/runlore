# Design — LLM token-usage accounting + stop_reason (Item R14)

Date: 2026-06-24 · Status: approved (autonomous) · Worktree: `agent-a985a7e86993947f8`

## Problem

No model provider parses the provider-reported `usage` (input/output token counts) or
the stop/finish reason. Two consequences:

1. **A truncated answer is treated as a complete one.** When the model hits its output
   ceiling (Anthropic `stop_reason == "max_tokens"`, OpenAI `finish_reason == "length"`,
   Gemini `finishReason == "MAX_TOKENS"`), the response text is cut mid-thought, but the
   loop has no way to tell it apart from a normal `end_turn` and reports it as a finished
   investigation step.
2. **The investigation budget is a `~4 chars/token` estimate** (`internal/investigate/budget.go`)
   rather than real counts. `estimateTokens` even documents the gap: *"Provider-reported
   usage is not exposed in CompletionResponse today."*

### CHALLENGE — is this genuinely unparsed today? (verdict: yes)

`grep` over `internal/model` + `internal/providers/providers.go` for
`usage|stop_reason|finish_reason|finishReason|Usage|InputTokens|OutputTokens|Truncated`
returns **zero matches**. Confirmed per provider:

- `internal/model/anthropic/anthropic.go:87-92` — `msgResponse` is `{Content, Error}` only;
  no `usage`, no `stop_reason`.
- `internal/model/gemini/gemini.go:90-97` — `genResponse` is `{Candidates[].Content, Error}`
  only; no `usageMetadata`, no `finishReason`.
- `internal/model/openai/openai.go:84-90` — `chatResponse`/`chatChoice` are `{Choices[].Message}`
  only; no `usage`, no `finish_reason`.
- `internal/providers/providers.go:413-417` — `CompletionResponse` is `{Text, ToolCalls}` only.

The premise holds. This slice adds the parsing and surfaces it.

### CHALLENGE — streaming (verdict: DEFER, documented follow-up)

The task flags streaming as "a separate, bigger question". Adding streaming to all three
providers is a response-handling rewrite: SSE/event parsing, partial-block accumulation,
and a streaming variant of `Complete` plus loop wiring — none of which is needed to read
`usage` or the stop reason, both of which are already present on the **non-streaming** JSON
response each provider returns today. Biting off streaming here would bloat the diff with a
concern orthogonal to accounting.

**Decision: usage + stop_reason now (non-streaming), streaming deferred.** Follow-up tracked
below. This is the most defensible split — it delivers the accounting value (real counts +
truncation detection) at small, well-tested cost, and leaves streaming as an isolated future
change.

### CHALLENGE — how much should the loop *act* on truncation? (verdict: expose + observe + one nudge)

The task says "at minimum expose it; wiring it into the budget/hard-kill can be incremental."
Three options:

- **(A) Expose only** — add the field, log nothing. Cheapest, but a truncated final answer
  still silently flows through.
- **(B) Expose + observe + one-shot nudge** — surface `Truncated`; when a step's response is
  truncated, log a warning, record a metric, and inject a single follow-up asking the model
  to wrap up concisely. The model gets one chance to recover instead of the loop accepting a
  cut-off answer. (chosen)
- **(C) Hard-kill on truncation** — treat a truncated step as terminal. Too aggressive: a
  truncated *tool-call* turn is common and recoverable; killing the investigation discards
  good evidence.

**Decision: (B).** It is the minimal action that stops a truncated answer from being treated
as complete, mirrors the existing single-use-nudge pattern already in `loop.go` (the
prose-turn nudge and the budget nudge), and stays incremental. Real token counts are surfaced
on the response and recorded as a metric; switching the budget guard's *input* from the
estimate to real counts is left as a follow-up (the estimate must remain for the pre-request
guard, since real counts only exist post-response).

## Changes

### Providers contract (`internal/providers/providers.go`)

- Add a `Usage` struct: `{InputTokens, OutputTokens int}`.
- Add two fields to `CompletionResponse`:
  - `Usage Usage` — provider-reported token counts (zero when the provider omits them).
  - `Truncated bool` — true when the provider stopped on its output-token ceiling
    (the actionable, provider-agnostic truncation signal).

### Anthropic (`internal/model/anthropic/anthropic.go`)

- Extend `msgResponse` with `StopReason string \`json:"stop_reason"\`` and
  `Usage *struct{ InputTokens, OutputTokens int }` (`input_tokens`/`output_tokens`).
- Map `Usage` onto `CompletionResponse.Usage`; set `Truncated = (StopReason == "max_tokens")`.

### OpenAI (`internal/model/openai/openai.go`)

- Add `FinishReason string \`json:"finish_reason"\`` to `chatChoice`, and a top-level
  `Usage *struct{ PromptTokens, CompletionTokens int }` (`prompt_tokens`/`completion_tokens`)
  on `chatResponse`.
- Map `Usage` onto `CompletionResponse.Usage` (prompt→input, completion→output);
  set `Truncated = (choice.FinishReason == "length")`.

### Gemini (`internal/model/gemini/gemini.go`)

- Add `FinishReason string \`json:"finishReason"\`` to the candidate, and a top-level
  `UsageMetadata *struct{ PromptTokenCount, CandidatesTokenCount int }` on `genResponse`.
- Map `UsageMetadata` onto `CompletionResponse.Usage` (prompt→input, candidates→output);
  set `Truncated = (candidate.FinishReason == "MAX_TOKENS")`.

### Investigation loop (`internal/investigate/loop.go`)

- After a successful `Complete`, when `resp.Truncated`:
  - Log a warning (`title`, `step`, real `input_tokens`/`output_tokens`).
  - Record a new metric `model_responses_truncated_total` (label: `provider`).
  - Inject the truncation nudge **once** (single-use, mirroring `budgetNudged`/`nudged`):
    a `user` turn asking the model to continue concisely / wrap up. The assistant's partial
    text is appended first so the model sees its own cut-off turn.
- Record the real `output_tokens`/`input_tokens` via the existing token histogram path is
  *not* changed here (estimate stays authoritative for the budget guard); a follow-up may
  switch the `InvestigationTokens` record to summed real counts.

### Telemetry (`internal/telemetry/metrics.go`)

- Add `ModelResponsesTruncated metric.Int64Counter`
  (`model_responses_truncated_total`, label `provider`).

## Tests (stdlib `testing` + `httptest`, table-driven, no testify)

Per provider:
- A crafted `httptest` response carrying `usage` + a normal stop/finish reason → assert
  `resp.Usage.InputTokens`/`OutputTokens` and `resp.Truncated == false`.
- A crafted response carrying the truncation stop/finish reason → assert `resp.Truncated == true`.
- A response *omitting* `usage` → assert `resp.Usage` is the zero value and no panic
  (defensive: provider may omit it).

Loop:
- A truncated first step injects the nudge once and continues (does not hard-kill); a second
  truncation does not re-inject. (Asserted via the existing `loop_test.go` fake-model harness.)

## Gate

`go build/vet/test ./... && gofmt -l . && golangci-lint run ./...` (0 issues) and
`golangci-lint run --enable gosec ./...` (gosec-clean on new code) green before each commit.

## Deferred follow-ups

1. **Streaming** for all three providers (SSE/event parsing + a streaming `Complete` path) —
   the larger change this slice deliberately excludes.
2. **Budget guard on real counts** — feed summed `Usage.InputTokens` back into the
   `overBudget` decision once a step has run, complementing the pre-request char estimate.
