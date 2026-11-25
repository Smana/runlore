# Gemini 3 & Gemini 4 Support Implementation Plan

> **Status:** Implemented (PR #627, merged 2026-11-24; closes #392)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make RunLore's native Gemini client work end-to-end on Gemini 3 and Gemini 4 (`gemini-4-neon`, `gemini-4-carbon`) by capturing and replaying `thoughtSignature` via `providers.Message.Opaque`, unlocking `model.effort` (`thinkingConfig.thinkingLevel`), folding `thoughtsTokenCount` into output token usage, filtering `thought: true` parts out of visible text, and updating docs.

**Architecture:** All wire mechanics stay isolated inside `internal/model/gemini`. On responses carrying any `thoughtSignature`, `accumulate` serializes a versioned JSON envelope (`{"v":1,"provider":"gemini","parts":[...]}`) into `CompletionResponse.Opaque`, which `internal/investigate/loop.go` already forwards onto assistant turns in history. `toContents` replays valid Gemini envelopes verbatim and falls back to `Content` + `ToolCalls` otherwise. `internal/config` and `internal/app` unlock and plumb `model.effort` for `provider: gemini`.

**Tech Stack:** Go 1.26, stdlib `net/http` + `encoding/json`, `internal/model/clientcore`, `httptest`.

**Spec:** [`docs/superpowers/specs/2026-11-24-gemini-4-support-design.md`](../specs/2026-11-24-gemini-4-support-design.md)

## Global Constraints

- **No changes to `providers.Message`, `providers.ToolCall`, or `providers.Usage`.** Reuse existing `Message.Opaque` and `Usage.OutputTokens`.
- **No fabricated bypass sentinels.** Never emit `skip_thought_signature_validator` when `Opaque` is missing or foreign.
- **No `includeThoughts: true`.** Never request thought summaries on the wire; if a gateway or model emits `thought: true` parts anyway, keep their text out of `CompletionResponse.Text`.
- **Byte-stable prefix preservation.** Keep `opaqueEnvelope` and `part` deterministic (no map iteration) so Gemini's implicit prefix cache stays intact across turns.
- **Zero behavior change when `effort == ""` and no `thoughtSignature` is present** (Gemini 2.5 compatibility).
- **Keep uniform `defaultMaxTokens` (8192) and `chatDefaultMaxTokens` (1024).**
- **Quality gate before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh` (`gofmt -l .` prints nothing; `hack/lint.sh` reports `0 issues`).
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/model/gemini/gemini.go` (modify) | Add `effort` param to `New`, `thinkingConfig` wire struct, `Thought`/`ThoughtSignature` fields on `part`, `ThoughtsTokenCount` on `usageMetadata`, `opaqueEnvelope` encode/decode, `accumulate` part filtering/coalescing, and `toContents` opaque replay |
| `internal/model/gemini/gemini_test.go` (modify) | Update `New(...)` call sites; add TDD tests for thoughtSignature replay, parallel calls, trailing signature parts, `thought: true` filtering, foreign `Opaque` fallback, `thoughtsTokenCount`, `thinkingLevel`, forced `ToolChoice`, and signed prefix stability |
| `internal/config/config.go` (modify) | Add `"gemini"` to `effortLevels` (`minimal\|low\|medium\|high\|xhigh`); remove the gemini rejection branch in `validateEffort`; update comments |
| `internal/config/config_test.go` (modify) | Update `TestValidateEffort` for Gemini's valid and invalid effort values (including `verify`/`chat` inheritance) |
| `internal/app/model.go` (modify) | Pass `effort` into `gemini.New(...)`; update doc comment |
| `internal/app/model_test.go` (modify) | Assert `NewModelClient` passes `effort` through to the Gemini client |
| `internal/eval/compare.go`, `internal/eval/compare_test.go` (modify) | Update comment and test cases for Gemini effort validation |
| `website/content/docs/integrations/llm/gemini.md` (modify) | Document Gemini 2.5 / 3 / 4 (`gemini-4-neon`, `gemini-4-carbon`), `model.effort`, and `max_tokens` headroom |
| `website/content/docs/configuration/configuration.md` (modify) | Document `gemini` under `model.effort` |
| `ROADMAP.md` (modify) | Remove/update the #392 blocker entry once shipped |

---

## Task 1: Gemini wire types, `effort` (`thinkingLevel`), and `thoughtsTokenCount`

**Files:**
- Modify: `internal/model/gemini/gemini.go`
- Modify: `internal/model/gemini/gemini_test.go`
- Modify: `internal/app/model.go` (update single call site so package compiles)

- [x] **Step 1: Write failing tests for `thinkingConfig.thinkingLevel` and `thoughtsTokenCount`**

In `internal/model/gemini/gemini_test.go`, update existing `New(srv.URL, ..., 0)` calls to `New(srv.URL, ..., 0, "")` and add two failing tests:
1. `TestEffortSendsThinkingLevel`: verifies `effort: "high"` sends `generationConfig.thinkingConfig.thinkingLevel == "HIGH"`, `effort: "xhigh"` sends `"XHIGH"`, and `effort: ""` omits `thinkingConfig` (`gotReq.GenerationConfig.ThinkingConfig == nil`).
2. `TestUsageIncludesThoughtsTokenCount`: streams a final chunk with `{"promptTokenCount":200,"candidatesTokenCount":50,"thoughtsTokenCount":350,"cachedContentTokenCount":120}` and asserts `resp.Usage == providers.Usage{InputTokens: 200, OutputTokens: 400, CachedInputTokens: 120}`.

- [x] **Step 2: Run targeted tests to verify they fail**

```bash
go test ./internal/model/gemini/... -run 'TestEffortSendsThinkingLevel|TestUsageIncludesThoughtsTokenCount'
```

Expected: compile/test failure (`New` signature mismatch and missing `ThinkingConfig` / `ThoughtsTokenCount`).

- [x] **Step 3: Implement `effort`, `thinkingConfig`, and `ThoughtsTokenCount` in `internal/model/gemini/gemini.go` and `internal/app/model.go`**

In `internal/model/gemini/gemini.go`:
- Add `effort string` field to `Client` and accept `effort string` as the 5th parameter of `New`.
- Add `thinkingConfig`:
  ```go
  type generationConfig struct {
  	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
  	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
  }

  type thinkingConfig struct {
  	ThinkingLevel string `json:"thinkingLevel,omitempty"`
  }
  ```
- In `Complete`, set `ThinkingConfig: &thinkingConfig{ThinkingLevel: strings.ToUpper(c.effort)}` only when `c.effort != ""`.
- Add `ThoughtsTokenCount int` (`json:"thoughtsTokenCount"`) to `genResponse.UsageMetadata` and fold it into `out.Usage.OutputTokens` (`gr.UsageMetadata.CandidatesTokenCount + gr.UsageMetadata.ThoughtsTokenCount`).
- Update `internal/app/model.go:74` to call `gemini.New(baseURL, model, apiKey, maxTokens, effort)` and update `NewModelClient`'s doc comment.

- [x] **Step 4: Run tests to verify Task 1 passes**

```bash
go test ./internal/model/gemini/... ./internal/app/...
```

Expected: PASS.

---

## Task 2: Capture and replay `thoughtSignature` and `thought: true` filtering via `Message.Opaque`

**Files:**
- Modify: `internal/model/gemini/gemini.go`
- Modify: `internal/model/gemini/gemini_test.go`

- [x] **Step 1: Write failing tests covering all replay & filtering contracts**

Add table-driven and multi-turn tests in `internal/model/gemini/gemini_test.go`:
1. `TestThoughtSignatureMultiTurnReplay`:
   - Turn 1 returns a `functionCall` part with `"thoughtSignature": "sig-turn-1"`.
   - Assert `resp.Opaque` is non-empty, valid JSON with `v:1`, `provider:"gemini"`.
   - Feed assistant turn (with `Opaque: resp.Opaque`) + tool result back into Turn 2 (including a Turn 2 with `ToolChoice: "submit_findings"`).
   - Inspect captured Turn 2 wire JSON and assert the `model` turn's `parts[0]` carries `"thoughtSignature": "sig-turn-1"` as a sibling of `functionCall`.
2. `TestParallelFunctionCallsPreserveFirstSignatureOnly`:
   - Turn 1 streams two `functionCall` parts: first with `"thoughtSignature": "sig-par-1"`, second without `thoughtSignature`.
   - Turn 2 replays history and asserts wire `parts[0].thoughtSignature == "sig-par-1"` and `parts[1].thoughtSignature == ""` (never duplicated), and both `functionResponse` parts correlate properly.
3. `TestThoughtPartsFilteredAndSignedRetained`:
   - Stream includes `{"thought": true, "text": "internal scratchpad"}` (unsigned), `{"thought": true, "text": "signed scratch", "thoughtSignature": "sig-t"}`, `{"text": "Hello "}`, `{"text": "world"}`, and trailing `{"text": "", "thoughtSignature": "sig-end"}`.
   - Assert `resp.Text == "Hello world"` (zero thought text leaked).
   - Assert decoded `Opaque` omits the unsigned `thought: true` part, retains the signed `thought: true` part verbatim, coalesces `"Hello "` + `"world"` into one plain text part `"Hello world"`, and retains the trailing empty-text part with `"sig-end"`.
4. `TestNoSignatureLeavesOpaqueNil`:
   - Stream with plain text + unsigned `functionCall` (Gemini 2.5 shape) produces `resp.Opaque == nil`.
5. `TestForeignOrCorruptOpaqueFallsBackCleanly`:
   - Assistant turn carrying Anthropic-format `Opaque` (`[{"type":"thinking","thinking":"x","signature":"s"}]`) or invalid JSON falls back to synthesizing `parts` from `Content` + `ToolCalls` with NO `skip_thought_signature_validator` sentinel anywhere in the serialized request body.
6. `TestRequestPrefixStableWithThoughtSignature`:
   - Multi-step conversation with signed `Opaque` turns produces byte-identical prefixes across Turn 2 and Turn 3 (guards Gemini implicit prefix caching).

- [x] **Step 2: Run targeted tests to verify they fail**

```bash
go test ./internal/model/gemini/... -run 'TestThoughtSignature|TestParallelFunctionCalls|TestThoughtParts|TestNoSignature|TestForeignOrCorrupt|TestRequestPrefixStable'
```

Expected: FAIL.

- [x] **Step 3: Implement `part` fields, `opaqueEnvelope`, `accumulate`, and `toContents` in `internal/model/gemini/gemini.go`**

- Extend `part`:
  ```go
  type part struct {
  	Text             string            `json:"text,omitempty"`
  	Thought          bool              `json:"thought,omitempty"`
  	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
  	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
  	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
  }
  ```
- Define envelope and helper:
  ```go
  const (
  	opaqueVersion  = 1
  	opaqueProvider = "gemini"
  )

  type opaqueEnvelope struct {
  	Version  int    `json:"v"`
  	Provider string `json:"provider"`
  	Parts    []part `json:"parts"`
  }

  func decodeOpaqueParts(raw json.RawMessage) ([]part, bool) {
  	if len(raw) == 0 {
  		return nil, false
  	}
  	var env opaqueEnvelope
  	if err := json.Unmarshal(raw, &env); err != nil {
  		return nil, false
  	}
  	if env.Version != opaqueVersion || env.Provider != opaqueProvider || len(env.Parts) == 0 {
  		return nil, false
  	}
  	return env.Parts, true
  }
  ```
- Update `accumulate`:
  - Track `var replayParts []part` and `hasSignature := false`.
  - Apply the exact rules from the spec (never append `p.Thought` text to `out.Text`; drop unsigned `p.Thought` from `replayParts`; coalesce adjacent unsigned non-thought text parts; preserve every `functionCall` and every signed part in order).
  - If `hasSignature && len(replayParts) > 0`, marshal `opaqueEnvelope{Version: opaqueVersion, Provider: opaqueProvider, Parts: replayParts}` into `out.Opaque`.
- Update `toContents`:
  - In the `idToName` pre-pass, also index any `FunctionCall` inside decoded `m.Opaque` parts when `m.ToolCalls` is empty.
  - In `case "assistant"`: if `parts, ok := decodeOpaqueParts(m.Opaque); ok`, emit `content{Role: "model", Parts: parts}` directly; otherwise run the existing `m.Content` + `m.ToolCalls` synthesis.

- [x] **Step 4: Run gemini package tests with race detector**

```bash
go test -race -v ./internal/model/gemini/...
```

Expected: all PASS.

---

## Task 3: Unlock `model.effort` for Gemini in config, app, and eval

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/app/model_test.go`
- Modify: `internal/eval/compare.go`
- Modify: `internal/eval/compare_test.go`

- [x] **Step 1: Update/add failing tests in config, app, and eval**

- In `internal/config/config_test.go` (`TestValidateEffort`):
  - Assert `provider: "gemini"` accepts `minimal`, `low`, `medium`, `high`, `xhigh`, and `""`.
  - Assert `provider: "gemini"` rejects `"max"` and unknown levels with a clear error naming `model.effort`.
  - Assert `verify` and `chat` overrides inherit/validate Gemini effort against their effective provider (e.g., `provider: "gemini", effort: "xhigh"` with `verify: {provider: "anthropic"}` fails because Anthropic doesn't support `xhigh`; `verify: {model: "gemini-4-neon", effort: "low"}` passes).
- In `internal/app/model_test.go`:
  - Add a test verifying `NewModelClient("gemini", srv.URL, "gemini-4-neon", "k", 8192, "xhigh", "")` sends `generationConfig.thinkingConfig.thinkingLevel == "XHIGH"` on the wire.
- In `internal/eval/compare_test.go`:
  - Replace the `"effort on gemini"` rejection case (`effort: high`) with an accepted `"effort on gemini"` case (`effort: xhigh`) and an invalid-level case (`effort: max`).

- [x] **Step 2: Run targeted tests to verify they fail**

```bash
go test ./internal/config/... ./internal/app/... ./internal/eval/... -run 'TestValidateEffort|TestNewModelClient|TestLoadCompare'
```

Expected: FAIL on Gemini effort assertions.

- [x] **Step 3: Implement config & comment updates**

- In `internal/config/config.go`:
  - Add `"gemini": {"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}` to `effortLevels`.
  - Remove the `if provider == "gemini"` error branch in `validateEffort`.
  - Update comments on `Model.Effort` and `effortLevels`.
- In `internal/eval/compare.go`:
  - Update comment near `ValidateEffort` to reflect Gemini's supported vocabulary.

- [x] **Step 4: Run package tests**

```bash
go test -race ./internal/config/... ./internal/app/... ./internal/eval/...
```

Expected: PASS.

---

## Task 4: Documentation & Docsguard Verification

**Files:**
- Modify: `website/content/docs/integrations/llm/gemini.md`
- Modify: `website/content/docs/configuration/configuration.md`
- Modify: `ROADMAP.md`

- [x] **Step 1: Update Gemini integration documentation**

In `website/content/docs/integrations/llm/gemini.md`:
- Remove the `## Gemini 3.x does not work` blocker section.
- Update the intro and minimal config to showcase Gemini 4 (`gemini-4-neon`, `gemini-4-carbon`) alongside Gemini 3 and Gemini 2.5.
- Document `model.effort` (`minimal|low|medium|high|xhigh` → `generationConfig.thinkingConfig.thinkingLevel`; empty omits it so `gemini-4-neon` defaults to `high` and `gemini-4-carbon` to `medium`; note `minimal` is Gemini 3 Flash only and rejected by Gemini 4).
- Document `max_tokens` headroom: `maxOutputTokens` caps both `thoughtsTokenCount` and visible output, so raise `model.max_tokens` (and `model.chat.max_tokens`, which defaults to `1024`) when running high/xhigh thinking models.
- Document that `thoughtSignature` is replayed automatically via `Message.Opaque` and `thoughtsTokenCount` is included in `OutputTokens` and cost ceilings.

- [x] **Step 2: Update configuration reference and roadmap**

- In `website/content/docs/configuration/configuration.md`, update the `model.effort` paragraph to list `gemini`: `minimal`·`low`·`medium`·`high`·`xhigh` (sent as `generationConfig.thinkingConfig.thinkingLevel`).
- In `ROADMAP.md`, remove the #392 row from `## Now`.

- [x] **Step 3: Run the full quality gate (including `internal/docsguard`)**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected:
- `go build ./...`, `go vet ./...`, `go test ./...` all PASS (including `internal/docsguard` config-fence checks!).
- `gofmt -l .` prints nothing.
- `hack/lint.sh` reports `0 issues`.

- [x] **Step 4: Commit**

Commit using Conventional Commits (English, no co-author line), e.g.:
`feat(gemini): support Gemini 3/4 thoughtSignature replay, thinkingLevel, and thoughtsTokenCount`
