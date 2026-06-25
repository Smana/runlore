# Honor `Retry-After` on 429 in `DoWithRetry` — Implementation Plan

> **For agentic workers:** implement task-by-task, test-first. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make the shared `internal/httpx.DoWithRetry` honor a 429 server's backoff hint (`Retry-After` delta-seconds + HTTP-date, and `retry-after-ms`), capped and ctx-aware, instead of always sleeping a fixed exponential. The LLM clients (`internal/model/*`) all flow through this one path, so the fix lands once and benefits all three.

**Architecture:** Extract the wait computation into a pure `retryDelay(attempt int, resp *http.Response) time.Duration` (+ a `parseRetryAfter(http.Header, now func() time.Time)` helper) and unit-test those. `DoWithRetry`'s `select`-on-`ctx.Done()` sleep just swaps its fixed duration for `retryDelay(i, resp)`. No consumer changes; notifiers are out of scope (they have no retry loop — see design §6).

**Tech Stack:** Go stdlib only (`net/http`, `time`, `strconv`); plain `testing` (no testify); table-driven; errors wrapped with `%w`; `context.Context` first param of I/O funcs.

**Design spec:** `docs/superpowers/specs/2026-06-24-respect-retry-after-design.md`

**Conventions:**
- Module path `github.com/Smana/runlore`.
- Full gate before commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` (must print `0 issues`); plus `go test -race ./internal/httpx/...` (the loop spawns goroutines via `time.After`/ctx).
- Conventional commits; **no co-author / attribution lines.**

---

## Part 1 — `retryDelay` + `parseRetryAfter` (pure, tested)

### Task 1.1: Failing tests for `parseRetryAfter` and `retryDelay`

**Files:**
- Test: `internal/httpx/retry_test.go` (extend)

- [ ] **Step 1: Write table-driven tests** covering:
  - `parseRetryAfter` with a fixed clock: `Retry-After: 5` → 5s; HTTP-date 10s in the future → ~10s; `Retry-After-Ms: 1500` → 1.5s; ms beats seconds when both present; missing → 0; malformed → 0; past date → ≤ 0.
  - `retryDelay`: a 429 with each header form returns the hinted (capped) duration; a 429 with `Retry-After: 9999` clamps to `maxDelay`; nil resp / non-429 / 200 → `baseBackoff<<(attempt-1)`; a huge `attempt` overflows → `maxDelay`.
- [ ] **Step 2: Run — expect FAIL** (`retryDelay`/`parseRetryAfter` undefined): `go test ./internal/httpx/ -run 'RetryDelay|RetryAfter'`

### Task 1.2: Implement the helpers

**Files:**
- Modify: `internal/httpx/retry.go`

- [ ] **Step 1:** Add `baseBackoff`/`maxDelay` consts, `parseRetryAfter(h http.Header, now func() time.Time) time.Duration`, and `retryDelay(attempt int, resp *http.Response) time.Duration` per design §3. `retryDelay` passes `time.Now` into `parseRetryAfter`.
- [ ] **Step 2: Run — expect PASS:** `go test ./internal/httpx/ -run 'RetryDelay|RetryAfter'`

---

## Part 2 — Wire it into the loop

### Task 2.1: Swap the fixed sleep for `retryDelay`

**Files:**
- Modify: `internal/httpx/retry.go`

- [ ] **Step 1:** Replace the `time.After(time.Duration(200*(1<<uint(i-1))) * time.Millisecond)` in the `select` with `time.After(retryDelay(i, resp))`. Update the doc comment to note it honors `Retry-After` on a 429.
- [ ] **Step 2: Run — expect PASS** (existing transient/4xx tests still green): `go test ./internal/httpx/`

### Task 2.2: httptest 429+`Retry-After` retry test + cancellation test

**Files:**
- Test: `internal/httpx/retry_test.go` (extend)

- [ ] **Step 1:** `TestDoWithRetryHonorsRetryAfterHeader`: a server that 429s once with `Retry-After: 0` then 200; assert it retried (2 hits) and returned 200 — proves the header path is wired without a real wait.
- [ ] **Step 2:** `TestDoWithRetryCancelDuringHintedWait`: a server that always 429s with `Retry-After: 30`; cancel the ctx shortly after the call starts; assert `DoWithRetry` returns `context.Canceled` (or `DeadlineExceeded`) promptly (< the 30s hint).
- [ ] **Step 3: Run — expect PASS:** `go test -race ./internal/httpx/`

---

## Final verification

- [ ] `go build ./...` → clean
- [ ] `go vet ./...` → clean
- [ ] `go test ./...` → all green
- [ ] `go test -race ./internal/httpx/...` → green
- [ ] `gofmt -l .` → no output
- [ ] `golangci-lint run ./...` → `0 issues`

---

## Commits (conventional, no trailers)

1. `test(httpx): cover retryDelay + Retry-After parsing` (failing tests — or fold into the feat commit if implementing same-session)
2. `feat(httpx): honor Retry-After / retry-after-ms on 429 in DoWithRetry`

(Tests and implementation may be squashed into the single `feat` commit since they land together; keep the message conventional and trailer-free.)
