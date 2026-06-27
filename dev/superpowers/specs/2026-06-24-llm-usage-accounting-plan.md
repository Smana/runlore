# Plan — LLM token-usage accounting + stop_reason (Item R14)

Spec: `2026-06-24-llm-usage-accounting-design.md`. Test-first, commit incrementally.
Gate before each commit: `go build/vet/test ./... && gofmt -l . && golangci-lint run ./...`
plus `golangci-lint run --enable gosec ./...`.

## Step 1 — Providers contract
- Add `Usage{InputTokens,OutputTokens int}` and `Usage`/`Truncated` fields on
  `CompletionResponse` in `internal/providers/providers.go`.
- Commit: `feat(providers): expose token Usage + Truncated on CompletionResponse`.

## Step 2 — Anthropic parsing (test-first)
- Test: usage + non-truncated; `stop_reason:"max_tokens"` → Truncated; usage omitted → zero.
- Impl: extend `msgResponse`, map onto response.
- Commit: `feat(model/anthropic): parse usage + max_tokens stop_reason`.

## Step 3 — OpenAI parsing (test-first)
- Test: usage + non-truncated; `finish_reason:"length"` → Truncated; usage omitted → zero.
- Impl: extend `chatChoice` + `chatResponse`, map onto response.
- Commit: `feat(model/openai): parse usage + length finish_reason`.

## Step 4 — Gemini parsing (test-first)
- Test: usageMetadata + non-truncated; `finishReason:"MAX_TOKENS"` → Truncated; omitted → zero.
- Impl: extend candidate + `genResponse`, map onto response.
- Commit: `feat(model/gemini): parse usageMetadata + MAX_TOKENS finishReason`.

## Step 5 — Metric + loop wiring (test-first)
- Add `ModelResponsesTruncated` to telemetry.
- Loop: single-use truncation nudge + warn + metric on `resp.Truncated`.
- Test: truncated step nudges once, continues; second truncation does not re-nudge.
- Commit: `feat(investigate): surface response truncation (warn + metric + one-shot nudge)`.

## Step 6 — Final gate sweep + spec/plan commit
- Full gate incl. gosec; commit spec+plan if not already.
