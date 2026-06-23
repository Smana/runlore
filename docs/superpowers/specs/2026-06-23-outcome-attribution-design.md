# Outcome attribution: coalesce + order-independent Episodes

Date: 2026-06-23
Slice: #10 outcome attribution

## Problem

The outcome ledger ("did the answer actually work?") has two attribution bugs:

- **(A) Coalesced batches orphan fingerprints.** When the coalescer folds N
  correlated alerts into one investigation, the `OnComplete` hook records a
  single `open` for `incs[0]`'s fingerprint only. The other N-1 fingerprints are
  never opened, so their resolved-alert webhooks match nothing and their
  resolutions are silently lost — under-counting `Resolved` and skewing recall
  decay.

- **(B) `Episodes()` drops a resolve that arrives before its open.** `Episodes()`
  replays the file in order and pairs each `resolve` with the most-recent
  unresolved `open` (LIFO). A `resolve` seen before any `open` for that
  fingerprint is dropped. This happens for a transient incident that fires,
  triggers an investigation, then clears *mid-investigation* — the resolve
  webhook lands before the investigation's `open` is written. The episode then
  looks unresolved even though it resolved.

## Approach

### Part A — record an open per coalesced fingerprint

- `investigate.Request` gains `Fingerprints []string` (the batch's fingerprints),
  keeping the existing single `Fingerprint` field.
- `FromIncident` sets `Fingerprints` to `[inc.Fingerprint]` (or nil when empty).
- The coalescer flush sink (`cmd/lore/main.go`) sets `rep.Fingerprints` to every
  non-empty fingerprint in the batch. A single incident stays one fingerprint.
- `Request.Fingerprints` is threaded through `loop.go` and `recall.go` onto
  `providers.Investigation.Fingerprints`.
- `OnComplete` records one `ledger.Open(...)` per fingerprint (falling back to the
  singular `Fingerprint` when the slice is empty), so each constituent alert's
  resolve webhook can match.

### Part B — order-independent `Episodes()` pairing

Replay maintains two per-fingerprint queues:

- `pendingOpens map[string][]int` — indices into the result `out` slice (LIFO).
- `pendingResolves map[string][]time.Time` — resolve times seen before any open.

On `open`: if a pending resolve exists, pop one and resolve the just-appended
episode (guard `Duration` ≥ 0: if the resolve predates the open, use 0).
Otherwise push the episode's index onto `pendingOpens`.

On `resolve`: if a pending open exists, pop the most-recent (LIFO) and resolve it
(unchanged behavior). Otherwise buffer the resolve time on `pendingResolves`.

Recurrence semantics are unchanged (N opens + 1 resolve ⇒ N episodes, 1 resolved).

The existing `TestEpisodesOrphanResolveSkipped` asserted the old drop behavior;
it is replaced by `TestEpisodesResolveBeforeOpenPairs` asserting the new pairing.

## Deferred (out of scope)

- **TTL expiry** of never-resolving opens (follow-up): opens with no resolve grow
  the in-memory index unbounded.
- **Live metrics on a buffered (early) resolve.** `Resolve()` still returns
  `ok=false` when the open hasn't been written yet (live path unchanged); only the
  replay-based `Episodes()` now reconciles the ordering. The metric/learning signal
  is recovered at replay time, not at the moment of the early webhook.

## Note on disjointness

This slice touches `internal/outcome/ledger.go`, `internal/investigate/{investigate,
loop,recall}.go`, `internal/providers/providers.go`, and `cmd/lore/main.go`. The
edited `loop.go` region (the single `inv.Fingerprint = req.Fingerprint` line) is
disjoint from other in-flight work in that file.
