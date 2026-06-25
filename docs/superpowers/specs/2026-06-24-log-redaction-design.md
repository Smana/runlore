# R19 — Stop logging upstream LLM error bodies + structural redaction (design)

Date: 2026-06-24
Branch: worktree-agent-a7b21b8c56b065032
Item: R19

## Problem

Two claims from the security review.

### Part 1 — upstream LLM response body echoed into a logged Error

`internal/model/{openai,anthropic,gemini}` each build, on a non-2xx response:

```go
fmt.Errorf("…status %d: %s", resp.StatusCode, string(data[:min(len(data), 512)]))
```

— up to 512 bytes of an **upstream-controlled** body reach the returned error.

### Part 2 — no structural log redaction

`internal/logging/logging.go` is a thin slog wrapper. "No secrets in logs"
holds only by call-site discipline. The review asks whether a
`slog.LogValuer`-based redacting type is worth adding now.

## CHALLENGE — verdicts vs current code

### Part 1: CONFIRMED — body reaches an Error-level log.

The error path is concrete and traced end-to-end:

1. `internal/model/openai/openai.go:130` (and `anthropic.go:121`, `gemini.go:129`)
   build `fmt.Errorf("chat status %d: %s", code, body[:512])`.
2. The caller `internal/investigate/loop.go:218` wraps it:
   `return fmt.Errorf("model: %w", err)` — returned from `Investigate`.
3. The top-level consumer `internal/investigate/investigate.go:246` logs it:
   `q.log.Error("investigation failed; retrying", "title", …, "err", err)`.

So up to 512 bytes of upstream body land at **Error** level, structured, with
no sanitization → information disclosure + log injection (newlines, ANSI,
fake-record forging in text logs; controlled string in JSON logs).

Threat model holds: the OpenAI `base_url` is operator-configurable
(`internal/config` `BaseURL`, wired at `cmd/lore/main.go:423-427`), so a
misbehaving/compromised proxy or any non-2xx upstream can inject arbitrary
bytes. Even the public APIs return attacker-influenceable strings in some 4xx
bodies. The vector is real. **Fix it.**

### Part 2: DECLINE the redacting type — over-engineering *for this codebase now*.

Evidence gathered (greps over `internal/`, `cmd/`):

- **No secret is ever logged by value.** A sweep of every `.Info/.Debug/.Warn/.Error/.Log(`
  call for `apikey|token|password|secret|bearer|authorization` values returns
  only two false positives: `"investigation hard-stopped at token budget"`
  (LLM tokens) and `"rejected … missing/invalid bearer token"` (a static
  message string, no value).
- **Config is env-name-only by construction.** Secret-ish config fields are all
  `*Env string` — `WebhookTokenEnv`, `APIKeyEnv`, `BotTokenEnv`,
  `SigningSecretEnv`, `ApprovalTokenEnv` (`internal/config/config.go`). Config
  carries the *name of the env var*, never the secret. The actual secret
  (`apiKey`) is read into a local var and passed straight to the client; it
  never reaches a `slog` arg.

A `slog.LogValuer` redacting type only earns its keep when secrets actually
flow to log call sites. Here the architecture already prevents that. Adding the
type now produces an **unused abstraction with no call site to attach to** —
the precise "hardening vs over-engineering" fork the item flagged. Decision:
**do not add it.** If a future change starts logging a struct that carries a
secret, revisit. The defensible, minimal hardening is to fully close the one
*actually-exploitable* vector (Part 1), which this change does.

## Design — Part 1 fix

Replace the body-bearing error with a sanitized, correlation-friendly one:

```
<verb> status <code> (request-id <id>)
```

- **Drop the body from the returned error entirely.** The returned error goes
  straight to an Error-level log; the body has no business there. This closes
  info-disclosure + log-injection with zero new plumbing.
- **Surface the upstream request-id instead** — the actionable correlation key
  for filing/diagnosing an upstream issue, and it is operator-trusted metadata,
  not body content. Header names differ per provider; a shared helper tries the
  common ones (`x-request-id`, `request-id`, `x-goog-request-id`,
  `x-amzn-requestid`) and returns the first non-empty, sanitized value.
- **Sanitize the request-id too** (defense in depth — a header is still
  upstream-controlled): strip control chars (incl. CR/LF), cap length. Quote it
  with `%q` so even a surviving odd byte can't forge log structure.

### Design fork: emit the body at Debug?

The item says "if the body is genuinely needed, … emit at Debug only." The
model `Client` structs hold **no `*slog.Logger`** (verified). Threading one in
touches three structs, three `New` signatures, six construction sites
(`cmd/lore/main.go:423-427`, `618-622`) and their tests — a meaningfully larger
change for marginal value, since the request-id is the actionable key and the
raw body is rarely needed for triage.

**Decision:** do **not** thread a logger in; the returned error carries
`status + request-id`, no body. This is the most defensible minimal fix. A
`sanitize` helper (strip control chars + cap) is added and used for the
request-id, so the same primitive is ready if a body-at-Debug path is ever
wanted. Documented as a deliberate deviation from the "emit at Debug" option.

### Helper placement

Both helpers are tiny and shared by all three providers. Put them in the
existing shared `internal/httpx` package (already imported by all three for
`DoWithRetry`), as `httpx.RequestID(resp.Header)` and `httpx.SanitizeHeader(s)`.
No new package, no new import in the providers.

## Tests (test-first, stdlib, table-driven)

- `internal/httpx`: `SanitizeHeader` strips CR/LF/control chars and caps length;
  `RequestID` returns the first present header (precedence) and "" when none.
- Each provider (`openai`, `anthropic`, `gemini`): a non-2xx response whose body
  contains a secret + a forged log line (`"\nfake=record secret=sk-LEAK…"`)
  yields an error that (a) does **not** contain the body/secret/newline, and
  (b) **does** contain the status code and the upstream request-id.

## Gate

`go build/vet/test ./...`, `gofmt -l .`, `golangci-lint run ./...`, plus
`golangci-lint run --enable gosec ./...` (gosec not in this branch's
`.golangci.yml`; run explicitly so new code is gosec-clean for integration).

## Coordinate-with

R15 also edits `openai.go`/`gemini.go` but in the request-marshaling region;
this change only touches the non-2xx error-construction lines + adds helpers in
`httpx`. Non-overlapping; integration reconciles.
