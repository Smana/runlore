# Plan — Coalesce / dedup hygiene (R17)

Spec: `2026-06-24-coalesce-hygiene-design.md`. Test-first, commit incrementally.
Gate before each commit:
`go build/vet/test ./... && gofmt -l . && golangci-lint run ./...` (0 issues) +
`golangci-lint run ./internal/coalesce/... ./internal/trigger/...` (gosec-clean)
+ `go test -race ./internal/coalesce/ ./internal/trigger/`.

## Step 1 — Nit 4: env in dedup fallback key (trigger)
1. Test: two incidents, same alertname+namespace, no fingerprint, different
   environment → second NOT deduped. (`engine_test.go` or new `dedupkey_test.go`)
2. Impl: `dedupKey` appends `inc.Environment`.
3. Gate, commit.

## Step 2 — Nit 3: all-empty correlation labels fall back (coalesce key)
1. Test: `CorrelationLabels` set, two incidents missing all those labels but
   different alertname/groupKey → distinct keys (no collapse). Partial presence
   still correlates.
2. Impl: in `key()`, if every label value empty → no-labels path.
3. Gate, commit.

## Step 3 — Nit 1 + Nit 2: recent eviction + new-critical-during-cooldown
   (coalesce). These share the per-key state struct, so one change set.
1. Tests:
   - eviction: after a key's entry ages past Cooldown, `sweep` drops it from
     `recent` (and the seen-alertname set).
   - new critical alertname during cooldown flushes; same-alertname critical
     repeat suppressed; warning repeat suppressed.
2. Impl: replace `recent map[string]time.Time` with a per-key record holding
   `last time.Time` + `seen map[string]struct{}` (alertnames). `withinCooldown`
   stays. `Add` cooldown branch: suppress unless critical with unseen alertname
   → flush + record. `sweep` evicts aged records.
3. Gate (incl. -race), commit.
