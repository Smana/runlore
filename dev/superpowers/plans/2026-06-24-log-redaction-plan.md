# R19 — implementation plan

Spec: `docs/superpowers/specs/2026-06-24-log-redaction-design.md`

Test-first, incremental commits. Gate green before each commit:
`go build/vet/test ./... && gofmt -l . && golangci-lint run ./...` and
`golangci-lint run --enable gosec ./...`.

## Step 1 — httpx helpers (test-first)

1. `internal/httpx/redact_test.go`:
   - `TestSanitizeHeader`: control chars (`\n`, `\r`, `\t`, `\x00`, ANSI ESC)
     removed; over-long input capped; clean input unchanged.
   - `TestRequestID`: precedence across `x-request-id`, `request-id`,
     `x-goog-request-id`, `x-amzn-requestid`; empty when none; value sanitized.
2. `internal/httpx/redact.go`:
   - `SanitizeHeader(s string) string` — drop runes < 0x20 and 0x7f (DEL), cap
     at a small constant (e.g. 200), trim.
   - `RequestID(h http.Header) string` — first non-empty of the known names,
     sanitized.
3. Gate + commit.

## Step 2 — providers stop logging bodies (test-first)

For each of openai / anthropic / gemini:

1. Add a test: server returns non-2xx with a malicious body
   (secret + newline + forged record); assert the error excludes the
   body/secret/newline and includes status + request-id (server sets
   `X-Request-Id`).
2. Change the non-2xx branch to
   `fmt.Errorf("<verb> status %d (request-id %q)", code, httpx.RequestID(resp.Header))`.
   Remove the `string(data[:min(len(data),512)])` body interpolation. The body
   is still read (for the 2xx parse path) — only its use in the error is removed.
3. Gate + commit (one commit per provider, or one combined — keep small).

## Step 3 — final gate + spec/plan touch-ups

Run the full gate incl. `--enable gosec`; record output in the report.
