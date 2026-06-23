# RunLore Outcome Episodes Read API — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-23 |
| **Scope** | Add a read-only API over the append-only outcome ledger that reconstructs open→resolve **episodes** (recurrence-aware) and rolls them up **per catalog entry** — the A1→A2 seam the recall-decay edge will consume. Pure addition; no behavior change to `Open`/`Resolve`. |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | Deep-analysis report (`docs/analysis/2026-06-23-deep-analysis.md`) Slice 3 / roadmap #8; the outcome-capture spec (`2026-06-23-outcome-capture-design.md`, which named `Episodes()` at §3.1 but left it unimplemented); `internal/outcome/ledger.go`; downstream consumer #9 (bias `deriveRecallConfidence` by resolve-rate) and #10/#12 (TTL/recurrence) |

---

## 1. Why this exists

A1 (outcome capture) records `open`/`resolve` events in an append-only JSONL ledger, but **nothing reads the history**: the in-memory `open` map is lossy by design (it keeps only the latest open per fingerprint, `ledger.go:67,102`), and `Episodes()` — named in the outcome-capture spec (§3.1) as the A2 reader — was never implemented. So the learning loop cannot yet compute the signal it needs: *for a recalled catalog entry, how often did the incident actually resolve?* This slice adds that read API. It is the prerequisite for the make-or-break decay edge (#9): an entry that recalls-but-never-resolves must be demotable, and that requires per-entry `recalls`/`resolved` counts derived from the full ledger history.

This is **pure read plumbing** — it does not change recording, does not wire decay, and emits no metrics. It unblocks #9 without prematurely committing to its policy.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | Expose **both** `Episodes()` (recurrence-aware raw list) **and** `OpenCounts()` (per-entry aggregate) | `OpenCounts` is exactly what #9 consumes; `Episodes` is the raw material future consumers (#10 recurrence, A2 frontmatter surfacing) need. Matches the spec's named seam and the roadmap's test names. |
| D2 | `Episodes()` **replays the full JSONL**, not the in-memory `open` map | The in-memory map is lossy (latest-only); the append-only file is the complete history — the only source that preserves recurrence (multiple opens per fingerprint). |
| D3 | **LIFO** resolve-pairing | The live `Resolve` matches the latest open for a fingerprint (the overwriting map); reconstruction pops the most-recent unresolved open to mirror that attribution. Aggregate counts are pairing-order-independent regardless. |
| D4 | `OpenCounts()` keys on **entry path**, counts **recall** episodes only | #9 decays *per-entry recall confidence*; fresh opens carry no entry (`Entry == ""`) and are skipped. |
| D5 | **Drop `expired_count`** from the aggregate for now | No `expired`/TTL events exist yet (that is slice #10). The unresolved-episode count (`Recalls > Resolved`) already gives the negative signal #9 needs. |
| D6 | Add an explicit `Resolved bool` to `Episode` | Cleaner for consumers than inferring from a zero `ResolvedAt`; the existing `Resolve()` (which only ever returns a matched pair) sets it `true` — a behavior-preserving one-liner. |

## 3. Design

### 3.1 `readEvents()` helper (DRY)

Factor the JSONL scan currently inlined in `New()` (`ledger.go:58-72`) into:

```go
// readEvents replays the ledger file in order, skipping corrupt lines. Returns an
// empty slice for a disabled (path=="") or absent file.
func (l *Ledger) readEvents() ([]Event, error)
```

`New()` is refactored to build its open-index from `readEvents()`; `Episodes()` reconstructs from the same source. Same buffered-scanner config and corrupt-line tolerance as today.

### 3.2 `Episodes() []Episode`

```go
func (l *Ledger) Episodes() ([]Episode, error)
```

- Disabled/empty → `(nil, nil)`.
- Under `l.mu` (consistent snapshot vs concurrent appends; not hot-path), `readEvents()` then reconstruct, accumulating into `out []Episode`:
  - `pending := map[string][]int{}` — per-fingerprint stack of **indices into `out`** for still-unresolved opens.
  - `open` → build an `Episode` from the event (`Kind`, `Entry`, `Title`, `Resource`, `OpenedAt = e.At`, `Resolved=false`), append it to `out`, and push its index (`len(out)-1`) onto `pending[fp]`.
  - `resolve` → pop the most-recent (LIFO) index from `pending[fp]`, if any, and mutate that `out[i]` in place: `ResolvedAt = e.At`, `Duration = e.At − out[i].OpenedAt`, `Resolved = true`. A resolve with no pending open is ignored (mirrors live `ok=false`).
- Result preserves open-order; all kinds (recall **and** fresh) included. **3 opens + 1 resolve for one fingerprint ⇒ 3 episodes, exactly 1 `Resolved`.**

`Episode` gains `Resolved bool` (added to the struct at `ledger.go:30-34`); `Resolve()` sets `Resolved: true` in the episode it returns.

### 3.3 `OpenCounts() map[string]Aggregate`

```go
type Aggregate struct {
	Recalls       int       // recall episodes for this entry
	Resolved      int       // of those, how many resolved
	LastConfirmed time.Time // latest ResolvedAt among them (zero if none)
}

func (l *Ledger) OpenCounts() (map[string]Aggregate, error)
```

- Derived from `Episodes()` (single source of truth): iterate, skip episodes with empty `Entry` or non-recall `Kind`, and for each `Entry` accumulate `Recalls++`, `Resolved++` when resolved, and `LastConfirmed = max(LastConfirmed, ResolvedAt)`.
- Disabled/empty → empty (non-nil) map.
- This is #9's direct input: smoothed resolve-rate `(Resolved+1)/(Recalls+2)`; `Recalls > Resolved` is the decay signal.

## 4. Components / seams

| Change | Location |
|---|---|
| `readEvents()` helper; `New()` refactored to use it | `internal/outcome/ledger.go` |
| `Resolved bool` on `Episode`; `Resolve()` sets it `true` | `internal/outcome/ledger.go` |
| `Episodes()` (replay + LIFO reconstruction) | `internal/outcome/ledger.go` |
| `Aggregate` type + `OpenCounts()` | `internal/outcome/ledger.go` |
| Tests | `internal/outcome/ledger_test.go` |

## 5. Trade-offs accepted in v1

- **Replay cost per call** — `Episodes()`/`OpenCounts()` re-read the whole file each call. Acceptable: these are analysis/decision-time calls (a recall decision or a periodic decay pass), not the alert hot-path, and the ledger is bounded by incident volume. Caching/compaction is a later concern.
- **No fresh→entry attribution** — fresh opens (`Entry == ""`) don't contribute to `OpenCounts`; only recall episodes do. The `link` event that would credit a fresh investigation's eventual curated entry is a deferred write-path change (roadmap #10), not this read API.
- **No `expired` signal** — unresolved episodes are represented (a recall episode with `Resolved=false`), which suffices for resolve-rate. An explicit `expired` terminal state waits for the TTL sweep (#10).
- **Mutex held during file read** — briefly blocks appends. Acceptable given the call frequency; avoids a torn read against a concurrent append.

## 6. Testing

- `TestEpisodesReconstructsRecurrence`: 3 `open` (same fingerprint, `kind=recall`, `entry="x.md"`) then 1 `resolve` → `Episodes()` returns 3 episodes for the fingerprint with exactly 1 `Resolved`; `OpenCounts()["x.md"]` = `{Recalls:3, Resolved:1, LastConfirmed:<resolve time>}`.
- `TestEpisodesResolvedPairingAndDuration`: one `open`+`resolve` → 1 resolved episode with the correct `Duration`; with two opens before one resolve, the **most-recent** open is the resolved one (LIFO).
- `TestOpenCountsSkipsFresh`: `kind=fresh` opens (empty `Entry`) do not appear in `OpenCounts()`.
- `TestEpisodesEmptyAndDisabled`: a disabled ledger (`path==""`) and a fresh empty file → `Episodes()` `nil`, `OpenCounts()` empty non-nil map, no error.
- `TestEpisodesRoundTrip`: events written via `Open`/`Resolve` are reproduced by `Episodes()` (counts + kinds + durations).
- Existing `ledger_test.go` tests must still pass unchanged (the `Resolved` field addition is behavior-preserving).

## 7. Out of scope (later slices)

- Wiring `OpenCounts()` into `deriveRecallConfidence` (the decay edge, #9) — the make-or-break, next.
- The `link` event for fresh→curated-entry attribution + coalesce/race/TTL fixes (#10).
- A ledger-backed `RecurrenceStore` for the dormant curate Recurrence pass (#12).
- Metrics for the read API (added when #9 consumes it).
- Ledger compaction / bounding growth.

This slice makes the ledger's history *readable* — turning "the absence of a resolve line" into a queryable per-entry `recalls`/`resolved` fact — without yet acting on it.
