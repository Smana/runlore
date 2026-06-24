# Honor `Retry-After` on 429 in the shared HTTP retry path — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation |
| **Date** | 2026-06-24 |
| **Scope** | Make `internal/httpx.DoWithRetry` respect a 429 server's backoff hint (`Retry-After` delta-seconds + HTTP-date, and `retry-after-ms`) instead of always sleeping a fixed exponential, capped and ctx-aware |
| **Author** | Smana (with Claude) |
| **Related** | `internal/httpx/retry.go` (the only shared retry loop); consumers `internal/model/{anthropic,openai,gemini}` all call `DoWithRetry`; `internal/notify/{slack,matrix}.go` (single-shot `http.Do`, **no** retry loop — out of scope, see §6) |

---

## 1. Why this exists

`DoWithRetry` retries on network error / 429 / 5xx with a **fixed** `200·2^i ms` backoff (`retry.go:27`). On a 429 it throws away the server's own pacing hint. The LLM providers and chat sinks all advertise one:

- **Anthropic / OpenAI** rate-limit responses carry `retry-after` (delta-seconds or HTTP-date) and, on OpenAI, `retry-after-ms` headers.
- **Slack / Matrix** rate-limit responses carry `Retry-After` (Slack, delta-seconds) / `retry_after_ms` (Matrix `M_LIMIT_EXCEEDED`).

A bursty incident fan-out (a storm → several model calls in quick succession) makes RunLore guess a backoff that is usually **shorter** than the server asked for, so it retries early, gets 429'd again, and burns its 3 attempts without ever waiting the hinted interval. Honoring the hint turns those wasted retries into one correctly-timed retry.

The existing loop already gets one thing right — it `select`s on `ctx.Done()` while sleeping (`retry.go:24-28`). That property must be preserved: a long server-hinted wait must still abort promptly on cancellation.

## 2. Decisions locked

| # | Decision | Rationale |
|---|---|---|
| D1 | **Extract a pure `retryDelay(attempt int, resp *http.Response) time.Duration`** and unit-test *that* against crafted responses | The acceptance criterion. Pure function = no sleeping in tests; the loop just sleeps whatever it returns. |
| D2 | **Honor three header forms**, in priority order: `retry-after-ms` (ms) → `retry-after` delta-seconds → `retry-after` HTTP-date | ms is the most precise (OpenAI), then the RFC `Retry-After` in its two legal shapes (RFC 9110 §10.2.3). First parseable wins. |
| D3 | **Cap the honored delay** at a `maxDelay` (default 30s) | A hostile/buggy server could send `Retry-After: 3600`; we must not block an investigation worker for an hour. The cap also bounds the date-form. |
| D4 | **Fall back to the existing capped exponential** when no usable hint is present (no header, unparseable, header on a non-429, negative/zero) | Preserves today's behavior for 5xx and network errors, which carry no hint. |
| D5 | **Scope the change to `DoWithRetry` only**; do NOT add a retry loop to the notifiers | The notifiers do a single `http.Do` with no retry (slack.go:42, matrix.go:55) — there is no retry loop to make hint-aware. Adding one is unrequested scope (R7 says "any notifier that *has* its own retry loop" — none do). Documented as a deviation in §6; a follow-up could route notifiers through `DoWithRetry`. |
| D6 | **Keep ctx-cancellation during the wait** (the `select` on `ctx.Done()`) | Already correct; a hinted 30s wait must still abort on cancel. Covered by a test that cancels mid-wait. |
| D7 | **Parse only on a 429 response** for the hint; ignore `Retry-After` on 5xx | RFC allows `Retry-After` on 503 too, but the finding and the real providers send it on 429; restricting to 429 keeps the rule simple and avoids honoring a stray header on a transient 5xx. 5xx → exponential, unchanged. |

## 3. The `retryDelay` function

```go
const (
	baseBackoff = 200 * time.Millisecond // matches the prior fixed schedule
	maxDelay    = 30 * time.Second       // cap on any honored / computed delay
)

// retryDelay returns how long to wait before the next attempt. attempt is the
// 1-based index of the wait (i.e. the wait BEFORE attempt #attempt; attempt>=1).
// On a 429 it honors the server hint — retry-after-ms, then Retry-After as
// delta-seconds, then Retry-After as an HTTP-date — capped at maxDelay. With no
// usable hint it falls back to capped exponential backoff: baseBackoff*2^(attempt-1).
func retryDelay(attempt int, resp *http.Response) time.Duration
```

**Algorithm:**
1. If `resp != nil && resp.StatusCode == 429`, try `parseRetryAfter(resp.Header, now)`:
   - `retry-after-ms` header → integer milliseconds.
   - `retry-after` header → integer delta-seconds; else an HTTP-date (`http.ParseTime`) → `date.Sub(now)`.
   - First one that yields a value `> 0` wins; clamp to `[0, maxDelay]`; return it.
2. Otherwise (or no usable hint): `d := baseBackoff << (attempt-1)`; if `d > maxDelay || d <= 0` (overflow) → `maxDelay`; return `d`.

`now` is injected as a `func() time.Time` argument to `parseRetryAfter` so the HTTP-date branch is deterministic in tests. `retryDelay` itself reads `time.Now` and forwards it; tests exercise the date branch through `parseRetryAfter` directly with a fixed clock.

The header lookups are case-insensitive (Go's `http.Header.Get` already canonicalizes), covering `Retry-After`, `retry-after`, `Retry-After-Ms`, etc.

## 4. The loop change

`DoWithRetry` keeps its structure; only the sleep duration changes:

```go
for i := 0; i < attempts; i++ {
	if i > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryDelay(i, resp)): // was: fixed 200·2^(i-1) ms
		}
	}
	...
}
```

`resp` here is the previous attempt's response (a 429/5xx, or nil after a network error). On the first 429, `retryDelay(i, resp)` sees the 429 and returns the hint; on a 5xx or network error it falls back to exponential. The `select` is untouched, so D6 holds for free.

## 5. Testability / acceptance

- `parseRetryAfter` unit tests (fixed clock): delta-seconds, HTTP-date, `retry-after-ms`, ms-takes-priority-over-seconds, none → zero, malformed → zero, date in the past → zero/clamped.
- `retryDelay` unit tests: each header form returns the hinted (capped) duration; over-cap honored value clamps to `maxDelay`; no hint / non-429 → `baseBackoff<<(attempt-1)`; high attempt overflow → `maxDelay`. **No test sleeps for the hinted duration** — they assert the *computed* value.
- An `httptest` integration test: a server that 429s once with `Retry-After: 0` (so the test doesn't actually wait) then 200, proving the header path is exercised and the request is retried. (`Retry-After: 0` keeps the test fast while still routing through the parse path; a small positive value would be honored and slow the test, so 0 is the right probe.)
- A cancellation test: a server that always 429s with a large `Retry-After`; cancel the ctx mid-wait and assert `DoWithRetry` returns `ctx.Err()` promptly (well under the hinted wait).

## 6. Out of scope / deviation from the finding

- **Notifiers.** R7 says "apply in `DoWithRetry` and any notifier that has its own retry loop." `slack.go` and `matrix.go` do a single `s.http.Do` with **no retry** (slack.go:42, matrix.go:55) — there is no loop to make hint-aware, and adding a full retry+backoff loop to them is a behavior change R7 doesn't ask for. **Deliberately skipped.** A clean follow-up is to route notifier requests through `DoWithRetry` (they already build `*http.Request` via `http.NewRequestWithContext`), which would give them hint-aware retries for free — tracked as future work, not this change.
- **5xx `Retry-After`.** RFC permits it on 503; restricted to 429 (D7).
- **`Retry-After` on success/3xx.** Ignored — only consulted on a 429 we are about to retry.
