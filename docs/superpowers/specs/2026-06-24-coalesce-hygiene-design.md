# Coalesce / dedup hygiene (R17)

Date: 2026-06-24
Branch: worktree-agent-a7aae1794a8d69bdf
Status: implementing

A cluster of trigger-pipeline correctness nits in `internal/coalesce` and
`internal/trigger`. Each was challenged against the current code; the verdict
and the chosen fix are recorded below.

## Scope

Four findings. All four held up under challenge; finding 2 holds only in a
narrower form than stated, so the fix is correspondingly narrow.

### Nit 1 — `recent` map never evicted (leak) — HOLDS

`coalescer.go` writes `recent[k]` at three sites (critical flush, MaxBatch
flush, sweep flush) and never deletes. `sweep` evicts only `pending`;
`withinCooldown` reads `recent` but never trims it. `Run`/`sweep` ticks for the
whole process lifetime (`main.go` wires `cz.Run(ctx, …)`), so over a long serve
with churning namespaces/labels `recent` grows without bound — one permanent
entry per distinct key ever seen.

**Fix.** In `sweep`, after the pending pass, evict every `recent` entry whose
age `>= Cooldown` (it can no longer suppress anything). When `Cooldown <= 0`,
`recent` is never consulted by `withinCooldown`, so evict all of it. This
mirrors the eviction the `Deduper` already does (`dedup.go`).

### Nit 2 — cooldown drops a genuinely new incident — HOLDS (narrowed)

Within the cooldown window a same-key incident is unconditionally `return`ed
(suppressed). For same-key **repeats** this is the whole point (storm collapse;
asserted by `TestAddCriticalStormSuppressedAfterFirst`). But when
`CorrelationLabels` are configured the key is `ns/<labelvalues>`, so a
**different, genuinely new critical alert** sharing those labels is dropped too.

**Design fork.**
- (A) "don't suppress any critical during cooldown" — rejected: re-breaks
  storm collapse, contradicts the existing storm test.
- (B) "reset cooldown only on quiescence" — rejected here: invasive, changes
  flush timing semantics for the whole pipeline.
- (C, chosen) Track the set of alertnames already flushed per key. During
  cooldown, suppress as before **except** a `critical` whose alertname has not
  yet been seen for that key — that is a distinct new problem, so flush it
  (and record its alertname). Same-alertname critical repeats and all warnings
  still suppress. Storm collapse is preserved; a genuinely new critical is not
  lost.

The seen-alertname set lives alongside `recent` and is evicted on the same
schedule (Nit 1), so it adds no new leak.

### Nit 3 — all-empty correlation labels collapse unrelated incidents — HOLDS

`key()` with `CorrelationLabels` set joins `inc.Labels[l]`; a missing label is
`""`. If **every** correlation label is absent the key degenerates to `ns/`
(or `ns//…`), collapsing unrelated incidents in the same namespace.

**Fix.** If every correlation-label value is empty, fall through to the
no-labels path (`GroupKey`, else `ns/alertname`). Partial presence is a
legitimate key and is left untouched.

### Nit 4 — dedup fallback key omits environment — HOLDS

`trigger.dedupKey` falls back to `AlertName + "/" + Namespace` when
`Fingerprint` is empty. A policy admitting multiple environments lets
same-name/same-namespace alerts from different environments collide on the
fallback, deduping a distinct incident.

**Fix.** Append `inc.Environment`: `AlertName + "/" + Namespace + "/" +
Environment`. Fingerprint path unchanged.

## Tests (test-first, stdlib, table-driven where it fits)

- `recent` (and seen-alertname) eviction after cooldown in `sweep`.
- new critical alertname during cooldown is flushed, not dropped; same-alertname
  critical repeat still suppressed; warning repeat still suppressed.
- all-empty correlation labels do not collapse unrelated incidents; partial
  presence still correlates.
- env-distinct fallback dedup keys do not collide.

Existing coalesce/trigger tests stay green.

## Gate (before each commit)

`go build/vet/test ./... && gofmt -l . && golangci-lint run ./...` (0 issues,
gosec enabled) + `go test -race ./internal/coalesce/ ./internal/trigger/`.
