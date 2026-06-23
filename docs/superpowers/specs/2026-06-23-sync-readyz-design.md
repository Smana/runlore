# HEAD-diff sync + readyz catalog gate — design (roadmap #18)

| | |
|---|---|
| **Date** | 2026-06-23 |
| **Roadmap item** | #18 — gate catalog `Reload` on real HEAD change; gate `readyz` on catalog warmth; stop knowledge-free cold-start serving |
| **Depends on** | #1 (BM25, merged) |
| **Effort** | M |

## Problem

Two inefficiency/robustness gaps in the catalog sync path (verified against code):

1. **Reload runs on every poll regardless of change.** `Syncer.Run`
   (`internal/catalog/sync.go:91-113`) calls `onSync()` after *every* successful
   `Sync`, and the `onSync` callback (`cmd/lore/main.go:437-447`) unconditionally
   rebuilds the **entire** in-memory BM25 index (`Catalog.Reload` →
   `buildIndex`, `catalog.go:54-92`) under a write lock — even when `git pull`
   returned `NoErrAlreadyUpToDate`. There is **no HEAD/SHA tracking**
   (`Syncer` has no revision field). At the default 5-minute interval this is a
   full rebuild + Search-blocking write-lock churn every poll on every replica,
   for nothing.

2. **`readyz` is not gated on catalog warmth.** `/readyz`
   (`internal/server/server.go:93-99`) gates only on leadership (`s.ready` =
   `leader.Load`). The git-sync path starts from `catalog.NewEmpty()`
   (`main.go:424`, `Len()==0`) and returns immediately; the leader therefore
   passes `readyz` and receives webhook traffic **before its knowledge base has
   loaded**. Instant recall then searches an empty index until the first sync
   completes. (Bounded: an empty-index recall returns no hit and the investigation
   falls through to the full ReAct loop, so this degrades recall availability
   rather than producing wrong output — but a leader should not advertise ready
   until its KB is warm.)

## Design

### A. Gate `Reload` on a real HEAD change

`Syncer` tracks the last-synced commit and only triggers `onSync` when the commit
actually moved.

- Add field `lastRev plumbing.Hash` to `Syncer` (`sync.go:24-30`).
- Change `Sync(ctx) error` → **`Sync(ctx) (changed bool, err error)`**. After the
  clone-or-pull, read `repo.Head().Hash()`; `changed = (rev != s.lastRev)`; store
  `s.lastRev = rev`. On the first sync of a process, `lastRev` is the zero hash, so
  `changed` is true (forces an initial index build). On a no-op pull the head is
  unchanged → `changed == false`.
- `Run`'s `do()` calls `onSync()` **only when `changed`** (and logs at debug when
  skipping). The initial sync still indexes once (changed=true).
- Update the one-shot caller `runCatalogSync` (`main.go` ~line 908) to
  `_, err := syncer.Sync(ctx)`.

This eliminates the every-poll rebuild while keeping the read/write loop closed:
a curator-merged PR moves HEAD → next poll detects it → one rebuild.

### B. Gate `readyz` on catalog warmth

Readiness reflects "this catalog has completed its initial load," not "has ≥1
entry" — an empty-but-synced KB is a legitimate ready state (a fresh install must
not be bricked forever).

- Add an `atomic.Bool ready` to `Catalog`; `NewEmpty()` leaves it false;
  `Reload()` sets it **true on success** (even with zero entries). `New(dir)` calls
  `Reload`, so a static mounted catalog is ready immediately. Expose
  `func (c *Catalog) Ready() bool`.
- `buildInvestigator` (`main.go:1075`) already receives the catalog from
  `buildModelAndTools` (4th return). Change its return type from
  `investigate.Investigator` to `(investigate.Investigator, *catalog.Catalog)` and
  thread the catalog up to its **single** caller (`runServe`, `main.go:221`).
- In `runServe`, compose the readiness callback and pass it to `server.New`
  (currently passed `leader.Load` directly, `main.go:302`):

  ```go
  // readyFunc gates readiness on leadership AND a warm catalog.
  // A nil catalog (none configured) imposes no catalog gate.
  func readyFunc(leader func() bool, cat *catalog.Catalog) func() bool {
      return func() bool {
          if cat != nil && !cat.Ready() {
              return false
          }
          return leader()
      }
  }
  ```

  `server.New`'s signature is **unchanged** — the composition happens at the call
  site. Standbys stay 503 (leader gate); a warmed standby that wins failover flips
  ready immediately (its catalog is already loaded).

### Deliberately out of scope (with reasons)

- **Leader-only sync:** rejected. Syncing on every replica keeps a failover
  standby warm (an existing, intentional feature — `main.go:415-417`). Part A
  removes the only cost (per-poll rebuild), so warm-standby sync is now cheap.
- **On-disk persisted (scorch) index:** YAGNI. With Part A, a full in-memory
  rebuild happens only on an actual HEAD change (rare), so persistence buys little
  and would add disk-state/migration surface.
- **Blocking investigations until warm:** unnecessary. Part B keeps the leader out
  of Service endpoints until warm, which is the right layer; the ReAct loop already
  degrades gracefully on an empty index.

### C. Cleanup (carried from #7)

A Task-5 smoke test in PR #81 accidentally committed a generated artifact. Remove
`eval/reports/2026-06-23T17-49-33Z-replay.json` and add `eval/reports/` to
`.gitignore` so generated campaign reports are never tracked.

## Testing

- `internal/catalog/sync_test.go`
  - `TestSyncReportsChangeOnClone` — first `Sync` of a fresh dir returns `changed==true`.
  - `TestSyncNoChangeOnRepeatedPull` — a second `Sync` with no upstream commit returns `changed==false`.
  - `TestSyncReportsChangeAfterNewCommit` — a new upstream commit makes the next `Sync` return `changed==true`.
  - `TestRunReloadsOnlyOnChange` — drive `Run`/`do` (or `Sync`) over a static upstream and assert the `onSync` callback fires once (initial) and not again while HEAD is unchanged.
- `internal/catalog/catalog_test.go`
  - `TestNewEmptyNotReady` — `NewEmpty().Ready()==false`.
  - `TestReloadMarksReady` — after `Reload`, `Ready()==true` (incl. a zero-entry dir).
  - `TestStaticCatalogReadyOnLoad` — `New(dir).Ready()==true`.
- `cmd/lore/main_test.go` (new or existing)
  - `TestReadyFunc` — nil catalog → gate == leader passthrough; non-ready catalog → false even when leader true; ready catalog + leader true → true; ready catalog + leader false → false.
- Existing `internal/catalog/*` and `internal/server/*` tests stay green.

## Files touched

- `internal/catalog/sync.go` — `lastRev` field; `Sync` returns `(bool, error)`; `Run` gates `onSync` on change.
- `internal/catalog/catalog.go` — `ready atomic.Bool`; `Ready()`; set in `Reload`.
- `cmd/lore/main.go` — `buildInvestigator` returns the catalog; `runServe` composes `readyFunc`; one-shot `runCatalogSync` updated for the new `Sync` signature.
- `internal/catalog/sync_test.go`, `internal/catalog/catalog_test.go`, `cmd/lore/main_test.go` — tests above.
- `.gitignore`, remove `eval/reports/2026-06-23T17-49-33Z-replay.json` — cleanup.
