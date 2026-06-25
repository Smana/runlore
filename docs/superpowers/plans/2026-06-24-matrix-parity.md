# Plan — Matrix parity (HTML + txn durability), R16

Spec: `docs/superpowers/specs/2026-06-24-matrix-parity-design.md`
Branch: this worktree only (never `main`).

## Steps (test-first, commit incrementally)

1. **Spec + plan** (this commit). Document the CHALLENGE verdicts: HTML fix and
   txn seed in scope; Matrix interactive approval scoped out with a follow-up.

2. **HTML formatting (red → green).**
   - Tests: sent event has `format=org.matrix.custom.html`, non-empty
     `formatted_body` with `<strong>`/`<a href>`, plaintext `body` with no raw
     `*`; HTML-escaping of `<script>`; `mrkdwnToHTML` table cases.
   - Impl: `mrkdwnToHTML(s)` — escape → bold `*…*` → code `` `…` `` → linkify
     bare URLs → `\n`→`<br/>`; `plainFallback(s)` strips `*`/backticks; `Deliver`
     marshals `{msgtype, body, format, formatted_body}`.

3. **Txn durability seed (red → green).**
   - Test: two notifiers (simulated restart) yield non-colliding, increasing
     txn ids.
   - Impl: seed `m.txn` from `time.Now().UnixNano()` in `NewMatrix`; keep
     `Add(1)` monotonic in-process. Comment the wall-clock-regression caveat.

4. **Gate green** before each commit: `go build/vet/test ./... && gofmt -l . &&
   golangci-lint run ./...` and `golangci-lint run --enable gosec ./...`.

## Out of scope (follow-up)
Matrix interactive approval (sync reader + `!approve <id>` convention +
authorization + server wiring). Operators use `POST /actions/{id}/approve`.
