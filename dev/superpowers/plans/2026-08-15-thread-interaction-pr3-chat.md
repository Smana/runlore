# Thread Interaction PR3 — Conversational Chat Layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a human addresses RunLore in an investigation thread without a `note:` prefix, a cheap model answers them from the investigation's own evidence — and may propose a knowledge-base note, which is written through the existing reviewed-PR path.

**Architecture:** One model call per mention, no agent loop. The call's context is assembled deterministically before it (thread context, the investigation's evidence, and a BM25 catalog search run in Go, not as a tool), and its output is forced through a structured `submit_thread_reply` tool returning `{reply, kb_note}`. The layer lives in the shared `thread.Responder`, so both transports gain it at once. Every spend guard sits **upstream** of the model call.

**Tech Stack:** Go 1.26, stdlib only. Existing internals: `internal/thread` (the transport-agnostic core from PR1/PR2), `internal/providers` (`ModelProvider`, `CompletionRequest`, forced `ToolChoice`), `internal/catalog` (BM25 search), `internal/ratelimit`, `internal/telemetry`, `internal/app` (model builders).

Source spec: `dev/superpowers/specs/2026-08-14-thread-interaction-design.md` (§Chat layer).
Predecessors: PR1 (`…-pr1.md`, Slack), PR2 (`…-pr2-matrix.md`, Matrix).
**Branch:** stacked on `feat/thread-interaction-pr2-matrix`.

## Global Constraints

- **Quality gate, run before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`. `gofmt -l .` must print nothing; golangci-lint must report **`0 issues`**. Plus `go test -race ./internal/thread/ ./internal/notify/ ./internal/server/ ./internal/app/`.
- **TDD.** Failing test first. Tests verify behaviour, not mocks. Table-driven where natural.
- Every file starts with `// SPDX-License-Identifier: Apache-2.0`. Every exported symbol carries a doc comment (`revive`). Errors wrap with `%w` (`errorlint`). `context.Context` is the first parameter of any function that does I/O.
- **No new third-party dependencies.** Module path `github.com/Smana/runlore`.
- **Nothing may block or fail investigation delivery.**
- **The model never chooses the write target.** The route is derived from `thread.Context` alone — an invariant PR1 established and this PR must not weaken, now that model output reaches the write path.
- **Chat is opt-in and off by default.** With no `model.chat` configured, behaviour is byte-identical to PR2.
- Known lint traps: `revive` flags identifiers shadowing Go builtins (`max`, `min`, `clear`); staticcheck QF1012 flags `b.WriteString(fmt.Sprintf(...))` (use `fmt.Fprintf(&b, …)`); `unused` flags a function with no non-test caller.
- The docs site cannot be built with the `hugo` on `PATH` (0.139.0 < the theme's 0.146.0 floor). Use `/usr/bin/hugo` (0.164.0) and build to a directory outside the repo.

---

## Why this PR carries four guardrails that are not in the spec

A security audit of PR1+PR2 asked one question: *can an operator cap what this costs them?* The answer was **no** — not in money, and not for this feature at all. Three of its preconditions were fixed during that audit and are already on `feat/thread-interaction-pr1`:

- the global forge-write window now covers **both** write routes, checked once upstream of the route choice (`thread.Responder.ForgeWrites`);
- `Registry.Update` can no longer silently no-op, so an uncountable write is surfaced rather than swallowed;
- writes and throttles are both counted (`ThreadNotesWritten`, `ThreadWritesThrottled`).

**Four remain, and they all bite for the first time in this PR**, because this is where a mention stops costing a forge write and starts costing a model call:

1. **No per-mention input cap.** `NoteBody` writes the human's text verbatim with no truncation. A 64 KiB Matrix event or a ~1 MiB Slack body becomes ~16k–250k input tokens per mention. `model.max_tokens` caps **output only** — no client in `internal/model` caps input.
2. **No cumulative token budget.** `investigation.max_tokens_per_investigation` does not sum: `enforceBudget` compares the size of the *next request*. The genuine running total (`loopTotals`) is written and never compared to anything.
3. **Every thread-capture bound is a Go constant** with no YAML key. An operator who finds the volume too high has exactly one lever: `thread_capture: false`.
4. **`model.chat: {}` inherits the expensive model.** Following `BuildVerifyModel`'s `cmp.Or` inheritance, a present-but-empty block inherits provider, model name, `effort` and `thinking` from `model.*` — so the cheapest way to *enable* the feature is the most expensive way to *run* it, on a path any channel member can trigger.

Tasks 1–3 close these before Task 4 makes the first model call. That ordering is deliberate: a budget written after the spender is a budget nobody trusts.

---

## File Structure

**Create:**

| File | Responsibility |
|---|---|
| `internal/thread/chat.go` | `Chat` — assembles context, makes the one call, decodes the structured reply. Knows nothing about transports or forges. |
| `internal/thread/chat_test.go` | Its tests, against a fake `ModelProvider`. |
| `internal/thread/budget.go` | `Budget` — the cumulative token/call ceiling for the chat path, checked before spending. |
| `internal/thread/budget_test.go` | Its tests. |

**Modify:**

| File | Change |
|---|---|
| `internal/config/config.go` | `Model.Chat *ModelOverride`; `notify.thread` block for the previously-hardcoded bounds; `Validate` rules. |
| `internal/app/model.go` | `BuildChatModel`, mirroring `BuildVerifyModel`. |
| `internal/thread/note.go` | Enforce the per-mention input cap at the single point all text passes through. |
| `internal/thread/responder.go` | `Chat` + `Budget` fields; route `IntentFreeform` to the chat layer when configured. |
| `internal/app/notify.go`, `serve.go` | Wire the model, the budget and the configurable bounds. |
| `internal/telemetry/metrics.go` | Chat call / token / denial counters. |
| `website/content/docs/…`, `SECURITY.md` | Setup, the cost disclosure, and what an operator can actually cap. |

---

### Task 1: `model.chat` config, `BuildChatModel`, and inheritance safety

**Files:**
- Modify: `internal/config/config.go`, `internal/app/model.go`
- Test: `internal/config/config_test.go`, `internal/app/model_test.go`

**Interfaces:**
- Produces: `config.Model.Chat *ModelOverride`; `func BuildChatModel(cfg *config.Config) providers.ModelProvider` — returns `nil` when `cfg.Model.Chat == nil`, exactly as `BuildVerifyModel` returns nil for an unset `Verify`.

- [ ] **Step 1: Write the failing test**

In `internal/config/config_test.go`, table-driven, mirroring the existing `TestValidateThreadCapture`:

- `model.chat` unset → valid, and `BuildChatModel` returns nil (feature off).
- `model.chat` set with a non-empty `model:` → valid.
- **`model.chat: {}` (present but empty `model:`) → `Validate` returns an error naming `model.chat.model`**, because inheriting the investigation model onto a path any channel member can trigger is a cost trap, not a convenience.
- `model.chat` set but `notify.slack.thread_capture` and `notify.matrix.thread_capture` both false → valid but pointless; assert a warning is produced rather than an error (the operator may be staging config).
- `model.chat` inherits `provider`, `base_url` and `api_key_env` from `model.*` when unset — assert the built client reflects that.

In `internal/app/model_test.go`, assert `BuildChatModel` returns nil for an unset block and a non-nil provider for a configured one.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestValidateModelChat -v && go test ./internal/app/ -run TestBuildChatModel -v`
Expected: FAIL — `unknown field Chat in struct literal`, `undefined: BuildChatModel`.

- [ ] **Step 3: Add the config field**

In `internal/config/config.go`, beside `Model.Verify`:

```go
	// Chat routes the thread-conversation layer to its own (cheaper) model.
	// PRESENT ⇒ conversational replies are enabled; ABSENT ⇒ thread capture
	// stays deterministic note-capture only. Same inherit-when-empty semantics
	// as Verify — with one deliberate exception: `model` may not be empty.
	//
	// Verify inherits the investigation model harmlessly, because verify runs
	// once per investigation on a path RunLore itself initiates. Chat runs once
	// per addressed message, on a path any channel or room member can trigger.
	// Silently inheriting a frontier model there makes the cheapest way to turn
	// the feature on the most expensive way to run it, so Validate requires the
	// model be named explicitly.
	Chat *ModelOverride `yaml:"chat"`
```

- [ ] **Step 4: Add the validation rules**

Immediately after the existing `model.verify` validation block, following its structure:

```go
	if c := cfg.Model.Chat; c != nil {
		if strings.TrimSpace(c.Model) == "" {
			return fmt.Errorf("model.chat.model must name a model explicitly: chat runs once per addressed thread message, on a path any channel member can trigger, so it must not silently inherit model.model")
		}
	}
```

Apply the same `validateEffort` / `validateThinking` / `checkSecureKeyEndpoint` treatment `model.verify` already gets (`config.go` — search for those calls on `c.Model.Verify` and mirror each for `Chat`, resolving the effective provider/base-URL/key with the same `cmp.Or` inherit semantics `BuildVerifyModel` uses).

- [ ] **Step 5: Add the builder**

In `internal/app/model.go`, directly below `BuildVerifyModel`, mirroring it exactly:

```go
// BuildChatModel builds the thread-conversation model from model.chat, or nil
// when the block is absent — the presence of the block IS the feature switch,
// the same contract BuildVerifyModel follows for model.verify.
//
// Unlike verify, config.Validate requires model.chat.model to be named
// explicitly, so this never silently resolves to the investigation model.
func BuildChatModel(cfg *config.Config) providers.ModelProvider {
	c := cfg.Model.Chat
	if c == nil {
		return nil
	}
	apiKey := os.Getenv(cmp.Or(c.APIKeyEnv, cfg.Model.APIKeyEnv))
	return NewModelClient(cmp.Or(c.Provider, cfg.Model.Provider),
		cmp.Or(c.BaseURL, cfg.Model.BaseURL), c.Model, apiKey,
		chatMaxTokens(cfg), cmp.Or(c.Effort, cfg.Model.Effort), cmp.Or(c.Thinking, cfg.Model.Thinking))
}
```

Add `chatMaxTokens(cfg)` alongside the existing `verifyMaxTokens`, following its shape. A chat reply is short — default it well below the investigation ceiling (1024 is reasonable) and say why in its doc comment.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/config/ ./internal/app/ -v -run 'ModelChat|BuildChatModel'`
Expected: PASS.

- [ ] **Step 7: Full gate and commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/app/model.go internal/app/model_test.go
git commit -m "feat(config): model.chat, with the model named explicitly

Presence of the block enables conversational thread replies, mirroring
model.verify. Unlike verify, the model name may not be inherited: verify runs
once per investigation on a path RunLore initiates, chat runs once per
addressed message on a path any channel member can trigger, so silent
inheritance would make the cheapest way to enable it the most expensive way to
run it."
```

---

### Task 2: A per-mention input cap, and the bounds an operator can finally set

**Files:**
- Modify: `internal/config/config.go`, `internal/thread/note.go`, `internal/thread/responder.go`, `internal/app/notify.go`
- Test: `internal/thread/note_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.NotifyConfig.Thread` block carrying `max_notes_per_thread`, `forge_writes_per_hour`, `registry_ttl`, `registry_max`, `max_note_bytes`; `thread.DefaultMaxNoteBytes`.

- [ ] **Step 1: Write the failing test**

In `internal/thread/note_test.go`:
- A note whose text exceeds the cap is truncated to the cap, on a **rune boundary**, with a visible marker saying it was truncated and by how much — the human must be able to tell their words were cut, and the reviewer must not read a silently-shortened note as complete.
- A note at exactly the cap is untouched.
- The cap applies to the text used for the **model call** as well as the text written to the forge, so both are bounded by one number.
- `utf8.ValidString` holds on every truncated output, including when the cut lands mid-rune.

In `internal/config/config_test.go`: each new key round-trips; each defaults correctly when absent; a negative or zero value is rejected or documented as "unlimited" — pick one, state it in the doc comment, and test it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestNoteInputCap -v`
Expected: FAIL — `undefined: DefaultMaxNoteBytes`.

- [ ] **Step 3: Add the cap**

`internal/thread/note.go` is where every path's text converges, so the cap belongs there — applied once, not at each call site:

```go
// DefaultMaxNoteBytes bounds one human message, in bytes, before it is written
// to the knowledge base or shown to a model.
//
// It exists because neither bound above it is a bound on this: a Matrix event
// can carry 64 KiB and a Slack request body 1 MiB, while model.max_tokens caps
// only what a model WRITES. Without this, one message is ~16k–250k input
// tokens, on a path any channel member can trigger, at a cost nothing else
// counts. 8 KiB is far more than a human types into a chat reply and far less
// than a pathological one.
const DefaultMaxNoteBytes = 8 << 10
```

Truncate on a rune boundary and append a marker naming the number of bytes dropped. `internal/thread/note.go` already has a byte-counting, rune-safe `truncate` helper — reuse it rather than writing a second one.

- [ ] **Step 4: Expose the bounds as config**

Add a `notify.thread` block. **Check for a name collision first**: `NotifyConfig` has an `Extra map[string]yaml.Node` inline field that catches `notify.<name>` blocks for registered notifiers, so a key named `thread` would shadow a notifier of that name. Confirm no registered notifier is called `thread` (`grep -rn 'Register(Descriptor{' internal/notify/`), and note the constraint in the field's doc comment.

Keys: `max_notes_per_thread`, `forge_writes_per_hour`, `registry_ttl`, `registry_max`, `max_note_bytes`. Each defaults to the constant it replaces, so an absent block changes nothing. Wire them through `internal/app/notify.go` where those constants are currently read.

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/thread/ ./internal/config/ ./internal/app/ -race`

```bash
git add internal/thread/note.go internal/thread/note_test.go internal/config/config.go internal/config/config_test.go internal/app/notify.go
git commit -m "feat(thread): cap one message's size, and let an operator set the bounds

model.max_tokens caps what a model writes, not what it reads: a 1 MiB chat
message is ~250k input tokens on a path any channel member can trigger. Caps
the text once, where every path converges, and turns the hardcoded thread
bounds into config so an operator has a dial short of turning the feature off."
```

---

### Task 3: A cumulative spend budget, checked before the model is called

**Files:**
- Create: `internal/thread/budget.go`, `internal/thread/budget_test.go`
- Modify: `internal/telemetry/metrics.go`, `internal/config/config.go`

**Interfaces:**
- Produces:
  - `type Budget struct { … }`
  - `func NewBudget(maxCalls int, maxTokens int64, window time.Duration, log *slog.Logger) *Budget`
  - `func (b *Budget) Allow() bool` — reports whether another call fits, non-blocking, recording the attempt
  - `func (b *Budget) Record(u providers.Usage)` — folds actual reported usage back in after a call
  - `func (b *Budget) Remaining() (calls int, tokens int64)`

- [ ] **Step 1: Write the failing test**

- A fresh budget allows a call; after `maxCalls` in the window it denies.
- Recorded usage accumulates; once the token total crosses `maxTokens` the next `Allow` denies **even if the call count has room** — the two limits are independent ceilings, not alternatives.
- Both limits slide with the window, like `ratelimit.Window`.
- `maxCalls <= 0` and `maxTokens <= 0` each mean unlimited **for that dimension only** — document and test it, because "0 means unlimited" is the convention `ratelimit.Window` already uses and diverging would be a trap.
- A nil `*Budget` allows everything, so an unconfigured budget is not a hidden deny.
- Concurrent `Allow`/`Record` are safe (`-race`, real goroutines).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestBudget -v`
Expected: FAIL — `undefined: NewBudget`.

- [ ] **Step 3: Implement**

Follow `internal/ratelimit/window.go`'s sliding-window shape and its `now func() time.Time` injection for testability. `Allow` must be non-blocking and must not hold a lock across anything but its own bookkeeping.

The distinction that matters: `Allow` is checked **before** the call, on the estimate that one call fits; `Record` folds in what the call actually cost, from `CompletionResponse.Usage`. That closes the gap where a provider reports far more usage than expected and the next check is still made against a stale total.

- [ ] **Step 4: Add the metrics**

In `internal/telemetry/metrics.go`, following the naming and doc-comment style of `ThreadNotesWritten` / `ThreadWritesThrottled` added in PR1:

- `ThreadChatCalls` — model calls made by the chat layer
- `ThreadChatTokens` — tokens the chat layer actually spent, from provider-reported usage
- `ThreadChatDenied` — calls the budget refused, labelled by which ceiling was hit

An operator must be able to see spend *and* refusals; PR1's audit found the one existing global cap fired with no log and no metric at all.

- [ ] **Step 5: Add the config keys**

Under the `notify.thread` block from Task 2: `chat_calls_per_hour` and `chat_tokens_per_hour`, both defaulting to values that a single active incident would not hit but a runaway would. State the reasoning in the doc comment rather than picking round numbers silently.

- [ ] **Step 6: Run tests and commit**

Run: `go test ./internal/thread/ -race -run TestBudget -v`

```bash
git add internal/thread/budget.go internal/thread/budget_test.go internal/telemetry/metrics.go internal/config/config.go
git commit -m "feat(thread): a cumulative spend budget for the chat path

max_tokens_per_investigation compares the size of the NEXT REQUEST, not a
running total, so it cannot bound a per-message path. This is a real cumulative
ceiling on calls and on provider-reported tokens, checked before the spend and
reconciled after it, with a metric on every denial."
```

---

### Task 4: The chat call

**Files:**
- Create: `internal/thread/chat.go`, `internal/thread/chat_test.go`

**Interfaces:**
- Consumes: `providers.ModelProvider`, `providers.CompletionRequest{System, Messages, Tools, ToolChoice}`, `providers.CompletionResponse{Text, ToolCalls, Usage, Truncated}`, `providers.ToolSpec{Name, Description, Schema}`, `providers.ToolCall{ID, Name, Args}`; `Budget` (Task 3); `Context` (PR1).
- Produces:
  - `type Chat struct { Model providers.ModelProvider; Budget *Budget; Catalog ChatSearcher; Metrics *telemetry.Metrics; Log *slog.Logger }`
  - `type ChatSearcher interface { Search(query string, k int) ([]catalog.Entry, error) }` — narrowed to what this needs, and matching `*catalog.Catalog`'s real method exactly (verified: `internal/catalog/catalog.go:421`). **There is no `catalog.Hit` type** — an earlier draft of this plan invented one. `SearchScored`/`SearchHybrid` also exist and return `[]ScoredEntry`; plain `Search` is enough for a prompt, so prefer it and do not widen the interface
  - `type ChatReply struct { Reply string; KBNote string }`
  - `func (c *Chat) Answer(ctx context.Context, tc Context, author, text string) (ChatReply, bool)` — the bool is false when the layer declined or failed and the caller must fall back

- [ ] **Step 1: Write the failing test**

Against a fake `ModelProvider`, asserting behaviour rather than prompt text:

- A well-formed `submit_thread_reply` tool call yields `{Reply, KBNote}` and `true`.
- **An empty `kb_note` means nothing is filed** — assert the caller receives an empty `KBNote`, not a whitespace string that later looks non-empty.
- A model **error** returns `false` so the caller degrades; the human's note is never lost to a model outage.
- A model returning **prose instead of the tool call** returns `false` — forced `ToolChoice` should prevent it, but a provider that ignores the directive must not produce an unstructured write.
- **Malformed JSON** in `ToolCall.Args` returns `false` and is logged, not panicked on.
- A `Truncated` response returns `false` — a cut-off reply must not be presented as complete.
- Exactly **one** `Complete` call is made per `Answer` — assert the fake's call count. There is no agent loop, and a regression that introduced one would be an unbounded cost path.
- The budget is consulted **before** `Complete`, and `Record` is called with the response's `Usage` after — assert ordering with a fake that records the sequence.
- With the budget exhausted, `Complete` is **never called**.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestChat -v`
Expected: FAIL — `undefined: Chat`.

- [ ] **Step 3: Implement**

The tool schema is the contract:

```go
const submitThreadReplyTool = "submit_thread_reply"

var chatToolSchema = `{
  "type": "object",
  "properties": {
    "reply":   {"type": "string", "description": "What to say back to the human, in one or two sentences."},
    "kb_note": {"type": "string", "description": "The durable fact worth recording in the knowledge base, or an empty string when the message was a question and there is nothing to record."}
  },
  "required": ["reply", "kb_note"]
}`
```

Set `ToolChoice: submitThreadReplyTool` on the request — `internal/providers` documents this as the mechanism for turns where a prose reply is never acceptable, and every structured-output path in this codebase (`submit_verdicts`, `submit_review`, `submit_grade`) uses it.

Order inside `Answer`, and it is load-bearing:

1. `Budget.Allow()` — deny returns `false` immediately, and increments `ThreadChatDenied`. **Nothing is spent before this.**
2. Assemble context (Task 5).
3. One `Complete`.
4. `Budget.Record(resp.Usage)` and `ThreadChatTokens`, whatever the outcome — a failed call still cost tokens.
5. Decode; any failure returns `false`.

- [ ] **Step 4: Run tests and commit**

Run: `go test ./internal/thread/ -race -run TestChat -v`

```bash
git add internal/thread/chat.go internal/thread/chat_test.go
git commit -m "feat(thread): one structured model call per addressed message

Forced tool choice so a provider cannot answer with prose that would reach the
write path unstructured, exactly one call per message with no agent loop, and
the budget consulted before the spend and reconciled after it. Every failure
mode returns false so the caller degrades to deterministic capture rather than
losing the human's words to a model outage."
```

---

### Task 5a: Carry the investigation's evidence into the thread registry

**Why this task was added.** The plan assumed the evidence the chat layer answers from was
already reachable. It is not. `thread.Context` carries identity only, and
`providers.Investigation.RuledOut` / `.DataGaps` are persisted **nowhere in the repo** —
the curator's KB draft (`internal/curator/draft.go`) never writes them, the outcome ledger
(`internal/outcome/ledger.go:61`) has no such fields, and the rendered chat message is
write-only egress with no read-back path. The data exists exactly once, at
`Registry.Register` (`internal/thread/registry.go:416`), which receives the full
`Investigation` and discards all but six fields.

Load-bearing rather than cosmetic: the design spec makes `@runlore reinvestigate:` a
**non-goal** precisely *because* "the chat layer answers most 'you're wrong' challenges
without one, because it already holds the investigation's ruled-out hypotheses, open
questions and data gaps" (spec:48-58).

**Files:**
- Modify: `internal/thread/thread.go` (a bounded `Evidence` type on `Context`)
- Modify: `internal/thread/registry.go` (`Register` populates it; the record round-trips it)
- Test: the file where the existing `Context` round-trip is asserted

**Decided, not open:**
- The evidence lives in the **registry, never on the event stamp**. A Matrix event caps at
  64 KiB, and `contextFromStamp`'s own doc comment establishes that stamped content is
  forgeable — it deliberately takes root and room from the event so "a forged stamp cannot
  redirect where a note is written". Evidence read off a stamp would be attacker-controlled
  text flowing straight into a model prompt.
- A registry miss therefore degrades to identity-only, which the spec already documents for
  that case (spec:469). Task 5b must render cleanly with empty evidence.
- Every list and string is bounded **at capture**, not at render, so the ceiling is a
  property of what is stored.

Must also assert: the evidence survives a write/reopen round-trip through the persisted
JSONL, and a record written before this change still loads — the log is append-only and
replayed across binary versions.

---

### Task 5b: Deterministic context assembly

**Files:**
- Modify: `internal/thread/chat.go`
- Test: `internal/thread/chat_test.go`

Folds in two defects proved by the Task 4 review, both on exactly these lines: the human's
message reaches the model **uncapped**, violating `note.go:19-27`'s explicit "or shown to a
model" guarantee (a 900 KiB message is one ~250k-token call, more than the entire hourly
ceiling); and the prompt carries **no UNTRUSTED-DATA instruction, no fence around untrusted
text, and no `redact.Secrets`**, unlike every other model-facing prompt in this repo
(`investigate/loop.go:85`, `rerank.go:103`, `loop.go:1215`). Forged framing was
demonstrated against the current renderer.

- [ ] **Step 1: Write the failing test**

- The assembled context carries the investigation's identity and evidence from `thread.Context`, and the human's message.
- A BM25 catalog search is run **in Go, before the call**, and its top hits are included — **the model is given no tools other than `submit_thread_reply`.** Assert the request's `Tools` contains exactly one spec. A search tool would create an agent loop and an unbounded call count; this is the single most important assertion in the task.
- The assembled prompt is bounded: with a catalog returning many long hits, the context stays under a stated ceiling. Assert the ceiling, not the exact bytes.
- A nil or failing catalog degrades to no hits rather than failing the call.
- The human's text is included **after** truncation to `MaxNoteBytes` (Task 2), so one bound governs both the write and the prompt.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestChatContext -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Assemble: the thread's finding (title, resource, verdict), its evidence — root causes, ruled-out hypotheses, open questions, data gaps, which is what lets the model answer *"did you check the NetworkPolicies?"* without a re-run — the top 3 catalog hits for the human's text, and the message itself.

**Bound every variable-length section.** The catalog hit excerpts, the evidence lists and the message all have caps; the total is the sum of known ceilings, not whatever the data happens to be.

- [ ] **Step 4: Run tests and commit**

```bash
git add internal/thread/chat.go internal/thread/chat_test.go
git commit -m "feat(thread): assemble the chat context deterministically

The catalog search runs in Go before the call rather than as a tool the model
may invoke: a tool would make the call count unbounded, which is the one
property this layer cannot have. Every variable-length section is capped, so
the prompt size is the sum of known ceilings."
```

---

### Task 6: Route freeform to the chat layer

**Files:**
- Modify: `internal/thread/responder.go`
- Test: `internal/thread/responder_test.go`

**Note on the current shape:** PR1's security-audit fixes changed `Handle` — `IntentFreeform` no longer writes; it replies telling the human to use `note:`. **Read `Handle` before you start**; this task replaces that reply with the chat layer *when a model is configured*, and leaves it exactly as-is when one is not.

- [ ] **Step 1: Write the failing test**

- With **no** `Chat` configured, behaviour is byte-identical to PR2: freeform replies with the how-to and writes nothing.
- With `Chat` configured, freeform calls it and returns the model's `Reply`.
- A non-empty `KBNote` is written **through the existing routing** — same `write()` path, same forge, same per-thread cap, same `ForgeWrites` window. The model's output is note *content*; it never selects the target.
- An empty `KBNote` writes nothing and still replies.
- `Chat` returning `false` (error, budget, malformed) **degrades to the deterministic reply** — never a silent drop.
- `note:` is unchanged: it writes with **no model call**. Assert the fake model's call count is zero.
- `reinvestigate:` is unchanged: refused, no model call, no write.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestHandleChat -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add `Chat *Chat` to `Responder` (nil ⇒ layer off). In `Handle`, on `IntentFreeform` with a non-nil `Chat`, call `Answer`; on `ok`, use its `Reply` and write its `KBNote` when non-empty; on `!ok`, fall through to today's behaviour unchanged.

**Do not move the `ForgeWrites` check.** It already sits in `write()`, upstream of the route choice, and its doc comment anticipates this PR. The chat *call* is separately bounded by `Budget`. Two ceilings, two spenders — do not collapse them: a chatty channel that opens no PRs must still be bounded on tokens.

- [ ] **Step 4: Run tests and commit**

Run: `go test ./internal/thread/ ./internal/notify/ ./internal/server/ -race`

```bash
git add internal/thread/responder.go internal/thread/responder_test.go
git commit -m "feat(thread): answer freeform messages with the chat layer

note: still writes with no model call and reinvestigate: is still refused, so
the deterministic paths are untouched. A proposed note is written through the
existing routing — the model supplies content, never the target."
```

---

### Task 7: Wiring, docs, and the cost disclosure

**Files:**
- Modify: `internal/app/notify.go`, `internal/app/serve.go`
- Test: `internal/app/notify_test.go`
- Modify: `website/content/docs/…`, `SECURITY.md`

- [ ] **Step 1: Write the failing test**

- `model.chat` set + thread capture on + a catalog available ⇒ the responder carries a non-nil `Chat` and `Budget`.
- `model.chat` unset ⇒ both nil, and the responder behaves exactly as PR2.
- Each degraded case (no catalog, no model credential at runtime) logs clearly and leaves the deterministic path working — never a panic, never silent inertness.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run TestBuildThreadChat -v`

- [ ] **Step 3: Wire it**

Build the model with `BuildChatModel`, the budget from the new config keys, and hand both to the single shared `*thread.Responder` that both transports already use. Follow the established guard-and-warn structure in `serve.go` rather than inventing a new shape.

- [ ] **Step 4: Write the docs**

The Slack and Matrix integration pages get a "Conversational replies" section: what changes when `model.chat` is set, that `note:` still costs nothing, and the config block.

**The cost disclosure is the point of this step.** State plainly, in the configuration reference and `SECURITY.md`:

- with `model.chat` set, **every addressed message that is not a `note:` costs one model call**;
- on Matrix, "addressed" means the bot's localpart appearing as a whole word — no mention entity required;
- what is capped (calls/hour, tokens/hour, bytes per message, forge writes/hour, notes per thread) **and what is not** — there is still no cost ceiling in currency, and `model.pricing` remains a reporting table, not a limit;
- the exact config keys that bound each.

An operator deciding whether to enable a paid, member-triggerable path deserves the complete picture, including its edges. Do not soften it.

- [ ] **Step 5: Verify the docs build**

Run: `cd website && /usr/bin/hugo --quiet --destination /tmp/hugo-pr3` — exit 0, no warnings. Then confirm the new content rendered.

- [ ] **Step 6: Full gate and commit**

```bash
git add internal/app/ website/ SECURITY.md
git commit -m "feat(app): wire the thread chat layer

Documents what the feature costs and, explicitly, what is not capped: calls,
tokens, message size, forge writes and notes per thread all have ceilings; the
spend has no ceiling in currency, and model.pricing remains a reporting table."
```

---

## Self-Review

**Spec coverage** (§Chat layer):

| Requirement | Task |
|---|---|
| Enabled by the presence of `model.chat`, `ModelOverride` inherit semantics | 1 |
| One call per mention, no agent loop | 4 (asserted by call count) |
| Context assembled deterministically, BM25 pre-fetched not tool-called | 5 |
| Forced `submit_thread_reply` returning `{reply, kb_note}` | 4 |
| Empty `kb_note` files nothing | 4, 6 |
| Failure degrades to deterministic capture | 4, 6 |
| Lives in the shared responder, both transports gain it | 6, 7 |

**Audit preconditions** (not in the spec; added here): per-mention input cap → Task 2; cumulative budget → Task 3; bounds as config → Tasks 2, 3; `model.chat: {}` inheritance trap → Task 1; every spend guard upstream of the spender → Tasks 3, 4, 6.

**Placeholder scan:** none. Two tasks (2, 7) reference existing helpers and doc pages by name rather than pasting them — those are concrete pointers, not deferred work.

**Type consistency:** `Chat`, `ChatReply{Reply, KBNote}`, `ChatSearcher`, `Answer(ctx, tc, author, text) (ChatReply, bool)`, `Budget`, `NewBudget`, `Allow`, `Record`, `Remaining`, `DefaultMaxNoteBytes`, `BuildChatModel`, `config.Model.Chat` are each declared once and used identically throughout. `Answer`'s `bool` means "the caller may use this result"; `false` always means degrade.

**Risk to flag at execution time:** Task 6 depends on `Handle`'s post-audit shape, which was changing while this plan was written. **Re-read `Handle` before starting Task 6** — if `IntentFreeform` still writes, the audit fix did not land and this plan's premise is wrong; stop and report rather than building on it.
