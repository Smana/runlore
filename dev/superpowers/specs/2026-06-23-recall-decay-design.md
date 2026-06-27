# RunLore Outcome-Driven Recall Decay — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-23 |
| **Scope** | Bias instant-recall confidence by each catalog entry's resolution track record, and **reject** (re-investigate) entries that recall-but-never-resolve. The make-or-break "learns, not accumulates" edge. Confined to `internal/investigate/recall.go` + serve-path wiring + config; A1 recording unchanged. |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | Deep-analysis report (retired report) roadmap #9 (the thesis); builds on the merged Episodes/`OpenCounts()` read API (`2026-06-23-outcome-episodes-design.md`); recall-trustworthiness (`2026-06-22-recall-trustworthiness-design.md`, the gate/`deriveRecallConfidence` this extends); outcome-capture A1 (`2026-06-23-outcome-capture-design.md`, the correlation-not-causation caveat) |

---

## 1. Why this exists

The learning loop captures outcomes (A1) and can now read them per entry (`OpenCounts()`), but **nothing feeds them back into recall**: `deriveRecallConfidence` is computed from BM25/structural signals alone, with zero outcome input. So a catalog entry that is recalled repeatedly and whose incident **never resolves** keeps winning the cache at full confidence — the loop accumulates but does not learn. This slice closes the edge: an entry's confidence decays with its unresolved-recall track record, and once it decays past a floor the recall is **rejected**, forcing a fresh investigation that can overturn a stale/wrong entry. A correct entry (or one with no history yet) is never penalized.

This is the first edge that makes "self-improving" true. It is deliberately conservative about a noisy signal (a resolved alert is correlation, not proof our answer worked — A1 §5): the only consequence of decay is a *rejected recall → full investigation*, which is always safe.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **Decay + gate** (not report-only) | Lowering the reported number alone never self-heals; the gate (reject → re-investigate) is what lets a wrong entry be overturned. The gate is fail-safe — a wrong rejection just runs a correct full investigation. |
| D2 | **Optimistic Beta prior** `factor = (resolved+k)/(recalls+k)` | `=1.0` at zero history and when an entry always resolves; decays only as *unresolved* recalls accumulate. Fixes the roadmap's `(resolved+1)/(recalls+2)`, which scores 0.5 at zero data and would penalize correct-but-new knowledge. Since `resolved ≤ recalls`, the factor is always in `(0, 1]` — decay-only, never inflation. |
| D3 | **Per-decision compute** (call `OpenCounts()` inside `lookup`) | Freshest + simplest. Recall frequency is low (one per qualifying alert, after trigger policy + dedup + coalescing) and the ledger is bounded; the Episodes spec already accepted replay-per-call. Caching is a clean follow-up if profiling warrants. |
| D4 | **The prior encodes evidence — no separate min-count gate** | `factor < floor` is only reachable after several unresolved recalls (k=2, floor=0.5 ⇒ ≈3), so the prior itself is the evidence gate. |
| D5 | **Serve path only** (`buildInvestigator`) for this slice | The dominant alert→investigate path is where recall matters most. Reinvestigate/chat leave `Outcome` nil (no decay = current behavior) — a small follow-up. |
| D6 | **Defaults `k=2.0`, `floor=0.5`** (≈3 consecutive unresolved recalls to gate out), config-tunable | Conservative starting shape; tune from `recall_rejections_total{reason="low_outcome"}`. |

## 3. Design

### 3.1 How `Recall` reads outcomes (`recall.go`, `main.go`)

A nil-safe interface, satisfied by `*outcome.Ledger` directly (no adapter):

```go
// OutcomeStats reports per-entry recall outcomes for confidence decay.
// *outcome.Ledger satisfies it.
type OutcomeStats interface {
	OpenCounts() (map[string]outcome.Aggregate, error)
}
```

New fields on `Recall`:

```go
Outcome      OutcomeStats // optional; nil ⇒ no decay (current behavior)
OutcomePrior float64      // k — Beta prior strength (default 2.0)
OutcomeFloor float64      // reject the recall when the factor drops below this (default 0.5)
```

`investigate` gains an import of `internal/outcome` (new, acyclic edge — `outcome` imports only stdlib). Wiring: `buildModelAndTools` sets `OutcomePrior`/`OutcomeFloor` from `config.InstantRecall`; `buildInvestigator` sets `recall.Outcome = ledger` (the only site where the ledger is in scope). Other `buildModelAndTools` callers (eval, reinvestigate, chat) leave `Outcome` nil.

### 3.2 The decay factor

```go
// outcomeFactor decays a recall's confidence by its track record using an
// optimistic Beta prior: an entry with no history (or that always resolves)
// scores 1.0; one that recalls-but-never-resolves decays toward 0. k is the
// prior strength. Always in (0, 1] since resolved ≤ recalls.
func outcomeFactor(recalls, resolved int, k float64) float64 {
	return (float64(resolved) + k) / (float64(recalls) + k)
}
```

### 3.3 Where it plugs in (`lookup`, after the structural gate, before returning)

```go
conf := deriveRecallConfidence(score, margin, strength)
if r.Outcome != nil {
	if counts, err := r.Outcome.OpenCounts(); err == nil {
		if agg, ok := counts[e.Path]; ok { // only entries with recall history
			f := outcomeFactor(agg.Recalls, agg.Resolved, r.OutcomePrior)
			if f < r.OutcomeFloor {
				r.reject(ctx, "low_outcome") // decayed out → re-investigate
				return nil, 0
			}
			conf = clampF(conf*f, 0, 0.90)
		}
	} else if r.Log != nil {
		r.Log.Warn("recall: outcome stats unavailable; skipping decay", "err", err)
	}
}
```

- An entry with **no** recall history is absent from `counts` → untouched (optimistic default; equivalent to factor 1.0).
- The gate reuses the existing `reject(ctx, reason)` helper → `recall_rejections_total{reason="low_outcome"}`. No new instrument.
- A stats error degrades gracefully: skip decay, recall as before (the read API failing must not break investigations).
- The factor is added to the existing "instant recall decision" log line for observability.

### 3.4 Config

Add to `config.InstantRecall` (and defaults in `config/load.go`, alongside `MinScore`/`MarginGap`/`SoloFloor`):

```yaml
catalog:
  instant_recall:
    outcome_prior: 2.0   # k; higher = more evidence before decay bites
    outcome_floor: 0.5   # reject the recall below this factor
```

Defaults applied only when `instant_recall.enabled` (matching the existing knobs): `OutcomePrior` 2.0, `OutcomeFloor` 0.5.

## 4. Components / seams

| Change | Location |
|---|---|
| `OutcomeStats` interface + `Outcome`/`OutcomePrior`/`OutcomeFloor` fields | `internal/investigate/recall.go` |
| `outcomeFactor` + decay/gate block in `lookup` | `internal/investigate/recall.go` |
| `outcome_prior` / `outcome_floor` config + defaults | `internal/config/config.go`, `internal/config/load.go` |
| Set `OutcomePrior`/`OutcomeFloor` on the built `Recall` | `cmd/lore/main.go` (`buildModelAndTools`) |
| Set `recall.Outcome = ledger` | `cmd/lore/main.go` (`buildInvestigator`) |
| Tests | `internal/investigate/recall_test.go`, `internal/config/*_test.go` |

## 5. Trade-offs accepted in v1

- **Correlation, not causation** — a resolved alert is not proof our answer worked, nor an unresolved one proof it didn't. The optimistic prior + a floor requiring several unresolved recalls keeps the signal conservative, and the only action is a fail-safe re-investigation. We act on the proxy deliberately; it is the best available signal and the downside is bounded.
- **Per-decision replay** — `OpenCounts()` re-reads the bounded JSONL and briefly locks the ledger on each recall decision. Accepted (low recall frequency); caching is a deferred follow-up.
- **Serve path only** — reinvestigate/chat recalls don't decay yet (no `Outcome` wired); they behave exactly as today. Follow-up.
- **Hand-tuned defaults** — `k`/`floor` are conservative constants, tunable from `recall_rejections_total{reason="low_outcome"}`. Not learned.

## 6. Testing

- **`outcomeFactor`**: `(0,0)→1.0`; `(5,5)→1.0`; `(3,0),k=2→0.4`; `(6,0),k=2→0.25`; monotonic decrease as unresolved recalls rise; always `≤ 1.0`.
- **`lookup` decay** (with a fake `OutcomeStats` returning a fixed `map[string]outcome.Aggregate`):
  - healthy entry (`recalls=5, resolved=5`) → recalls, confidence ≈ derived (factor 1.0);
  - stale entry (`recalls=4, resolved=0`, factor `2/6≈0.33 < 0.5`) → **rejected `low_outcome`**, `lookup` returns `(nil, 0)`;
  - entry with no history (absent from the map) → recalls, confidence unchanged by decay;
  - `Outcome == nil` → recalls, current behavior (no decay);
  - `OpenCounts()` returns an error → recalls (decay skipped), no rejection.
- **Metric**: a `low_outcome` rejection increments `recall_rejections_total{reason="low_outcome"}` (via the existing `reject` path; assert through the established `telemetry.Setup` + scrape pattern, or via the fake/log).
- **Config**: `outcome_prior`/`outcome_floor` default to 2.0/0.5 when `instant_recall.enabled` and unset; explicit values are respected.
- Existing recall tests (margin/structural/derive) must still pass — decay is additive and only engages when `Outcome` is set.

## 7. Out of scope (later slices)

- Surfacing `recall_count`/`resolved_count`/`last_confirmed` onto entry **git frontmatter** (write-path; a curator change).
- Decay on the **reinvestigate / chat** recall paths.
- **Caching** `OpenCounts()` (per-decision compute chosen).
- Calibrating `k`/`floor` from data, or a learned decay (the defaults are an explainable starting shape).
- Any change to A1 outcome **recording**.

This slice flips the learning loop from measuring outcomes to **acting on them**: a stale or wrong entry that keeps failing to resolve decays out of the cache and gets re-investigated — the first concrete instance of RunLore learning your platform.
