# Design — LLM-protocol correctness fixes (Item R15)

Date: 2026-06-24 · Status: approved (autonomous) · Worktree: `r15-llm-protocol-fixes`

## Problem

Two LLM-protocol correctness bugs in the model providers.

### Bug 1 — OpenAI drops empty tool-result content

`internal/model/openai/openai.go`: `chatMessage.Content` carries `json:"content,omitempty"`
(line 47). A `tool`-role result whose content is the empty string then marshals to
`{"role":"tool","tool_call_id":"…"}` — the `content` field is **absent**. The OpenAI
chat-completions schema (and strict OpenAI-compatible servers: vLLM, Ollama) **require
`content` to be present on a `tool` message** → the request 400s.

**Reachability (challenged):** `internal/investigate/loop.go:271` appends
`{Role:"tool", ToolCallID:tc.ID, Content:out}` where `out` is `runTool(...)`'s return
(loop.go:267,279-300). A tool's `Call` can legitimately return `""` (a tool with no output,
or an empty result), so an empty-content tool message is reachable in production, not just
in theory.

**Empirically confirmed:** Go `encoding/json` with `omitempty` marshals an empty `Content`
to `{"role":"tool"}` (field elided); without `omitempty` it marshals to
`{"role":"tool","content":""}`.

**Constraint:** the same `chatMessage.Content` field is shared by system/user/assistant/tool
messages. The OpenAI schema says an **assistant** message with `tool_calls` may *omit*
`content` (`omitempty` is correct there), but a **tool** message must *include* it. So the
fix must be path-specific to the tool role — not a blanket drop of `omitempty`.

### Bug 2 — Gemini correlates function responses by name only

`internal/model/gemini/gemini.go`: the response `functionCall` is mapped to a
`providers.ToolCall` with `ID = p.FunctionCall.Name` (line 146) — the real Gemini call `id`
(present on the newer API) is discarded and never even parsed. On the request side,
`functionResponse` carries only `Name` (struct lines 71-74; populated at line 188).

Per Gemini docs, when the model emits **parallel calls to the same function name**, results
are correlated by **`id`**, not name. Two parallel same-function calls therefore collide
under name-only matching (both `functionResponse` parts carry the same `Name`, and the
originating `id`s are lost).

**Challenge — worth fixing now?** RunLore's assistant turn *can* carry multiple
`resp.ToolCalls` (loop.go:238-272 iterates them), so parallel calls are reachable; the
collision only manifests when two of them target the *same* function. Echoing `id` makes
correlation robust in all cases at near-zero cost, and the package doc already (incorrectly)
claims name-based matching. Implemented now as a correctness hardening, with a name fallback
for older API responses that omit `id`.

## Changes

### OpenAI (`internal/model/openai/openai.go`)
- Add a dedicated `toolMessage` shape (or force `content:""` on the tool path) so a
  `tool`-role message **always** serializes `content`, even when empty. Assistant turns keep
  `omitempty` on `content` (canonical when `tool_calls` are present).

### Gemini (`internal/model/gemini/gemini.go`)
- Parse the response `functionCall.id` (new field on `functionCall`) and carry it into
  `providers.ToolCall.ID`, falling back to `Name` when absent.
- Add `id` to the `functionCall` and `functionResponse` request shapes; emit the originating
  call's `id` on each `functionResponse` so parallel same-function results correlate
  correctly. Name stays as the human-meaningful label; the `id`→name map still resolves the
  response name.
- Update the package doc to describe id-based correlation.

## Tests (stdlib `testing` + `httptest`, table-driven, no testify)

- **OpenAI:** a request containing an empty-content tool message keeps `"content":""` in the
  marshaled JSON sent to the server (assert on the raw request body, not the typed struct,
  since the struct round-trips either way).
- **Gemini:** two parallel responses to the *same* function name map to the correct `id`s
  (request `functionResponse` parts carry distinct ids matching the originating calls); a
  response `functionCall` with an `id` carries that id into the `ToolCall` (fallback to name
  when absent).

## Gate

`go build/vet/test ./... && gofmt -l . && golangci-lint run ./...` (0 issues, gosec on)
green before each commit.
