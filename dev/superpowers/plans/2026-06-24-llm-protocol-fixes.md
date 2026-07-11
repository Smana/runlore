# Plan — LLM-protocol correctness fixes (Item R15)

Spec: `dev/superpowers/specs/2026-06-24-llm-protocol-fixes-design.md`
Branch: `r15-llm-protocol-fixes`

## Steps

1. **OpenAI — test first.** Add a test asserting the raw marshaled request body keeps
   `"content":""` for an empty-content tool message (capture the body in the httptest
   handler, not the decoded struct). Confirm it fails against current code.
2. **OpenAI — fix.** Make the tool-role message always serialize `content` (dedicated
   tool-message shape with non-omitempty `content`), keeping `omitempty` for assistant turns.
   Gate green → commit.
3. **Gemini — test first.** Add tests: (a) two parallel responses to the same function name
   map to the correct originating ids on the request `functionResponse` parts; (b) a response
   `functionCall` with an `id` carries that id into `ToolCall.ID`, with name fallback when the
   id is absent. Confirm failure against current code.
4. **Gemini — fix.** Parse `functionCall.id` on responses; add `id` to the request
   `functionCall`/`functionResponse` shapes; emit the originating id on each
   `functionResponse`; update the package doc. Gate green → commit.

## Gate (before each commit)

`go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
(expect `0 issues`).
