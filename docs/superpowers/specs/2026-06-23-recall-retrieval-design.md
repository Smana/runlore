# RunLore Recall Retrieval — Wider Candidates + Structural Pre-filter — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-23 |
| **Scope** | Make instant recall *find* the structurally-correct catalog entry instead of only checking the single top lexical hit: fetch a wider candidate set, pre-filter by structural agreement, and apply the margin gate among the structurally-agreeing candidates. Confined to `internal/investigate/recall.go`. |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | Deep-analysis report (`docs/analysis/2026-06-23-deep-analysis.md`) roadmap #3 / "structural agreement is a post-rank filter at k=2 — the correct entry ranked #3 is never seen"; builds directly on the disambiguation slice (`2026-06-23-recall-disambiguation-design.md`, which made `resourceAgrees` discriminate) and the BM25 scorer fix (`2026-06-23-bm25-scorer-fix-design.md`) |

---

## 1. Why this exists

The disambiguation slice fixed *how* recall decides a workload matches (`resourceAgrees`), but recall still only ever inspects `hits[0]` from a `k=2` lexical search (`recall.go:54,78`). So the structural check is a **post-rank filter on a single candidate**: if the entry that actually matches the alert's workload is the 3rd-best lexical hit (because two other entries have more symptom-token overlap), recall never sees it and falls through to a full investigation. Symptom text is many-to-one with root causes, so the lexically-top entries are frequently the *wrong* workload — exactly when the right one is buried.

This slice turns structural agreement into a **pre-filter over a wider candidate set**: fetch more candidates, keep those whose stored resource agrees with the alert's workload, and only then ask "is there a clear winner among them?" The right entry becomes findable; an irrelevant lexical neighbour can no longer block a correct recall.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **Widen the internal candidate `k` + pre-filter in Go** (not a bleve filterable field) | Candidates already carry the full `Entry` (incl. `Resource`); filtering a wider slice in Go needs no index/schema change and is sufficient at current scale. A bleve `ConjunctionQuery` over an indexed `resource` field is the scale-version follow-up. |
| D2 | **Margin gate operates among the structurally-agreeing candidates** | After pre-filtering by workload, the margin should ask "is there one clear winner *for this workload*", not "is the top lexical hit a clear winner". A lexically-similar but structurally-irrelevant neighbour must not suppress a correct recall. |
| D3 | **`recallCandidateK = 20` as an internal constant** | Not user-facing; YAGNI on a config knob. Large enough that the structurally-correct entry is in the candidate window for realistic catalog sizes; small enough to stay cheap. |
| D4 | **No bleve index change; no field boosting; no cause/resolution re-indexing** | Cause/resolution text is already in the indexed `text` field (via the entry `Body`). The gap was candidate *selection*, not indexed *content*. Field boosting (title^3…) is a separate concern. |

## 3. Design

### 3.1 Restructured `lookup` (`recall.go`)

Today (`recall.go:48-108`): `SearchScored(query, 2)` → record `hits[0].Score` → Gate 1 margin over top-2 lexical → Gate 2 structural on `hits[0]` → derive + decay → return.

New flow:

1. **Fetch wider:** `hits, err := r.Catalog.SearchScored(query, recallCandidateK)` (`recallCandidateK = 20`). Empty/err → return `(nil, 0)` (unchanged).
2. **Structural pre-filter:** build `agreeing []catalog.ScoredEntry` preserving lexical order, keeping each hit whose `resourceAgrees(req.Workload, hit.Entry.Resource, r.RequireWorkloadMatch) != matchNone`. If `len(agreeing) == 0`: record the top lexical score (`hits[0].Score`) for miss visibility, `reject(ctx, "no_resource_match")`, return `(nil, 0)`.
3. **Pick the winner + record:** `winner := agreeing[0]`; `score := winner.Score`; record `score` to `RecallScore` (the value the floors gate).
4. **Gate — margin among agreeing:**
   - `len(agreeing) == 1`: `confident = score >= r.SoloFloor && score >= r.MinScore`; `margin = score`.
   - else: `margin = score - agreeing[1].Score`; `confident = score >= r.MinScore && margin >= r.MarginGap`.
   - `!confident` → `reject(ctx, "low_margin")`, return `(nil, 0)`.
5. **Derive + decay** (unchanged): `strength := resourceAgrees(req.Workload, winner.Entry.Resource, r.RequireWorkloadMatch)`; `conf := deriveRecallConfidence(score, margin, strength)`; apply the outcome-decay block keyed on `winner.Entry.Path`; log; return `(&winner.Entry, conf)`.

`recallCandidateK` is a package constant in `recall.go`. `kb_search` (the LLM grounding tool, `k=3`) is unchanged — it is a separate path, not the short-circuit.

### 3.2 The margin's meaning changes (intentional)

Previously the margin guarded against *lexical* ambiguity (two similar-looking entries, regardless of workload). Now it guards against ambiguity *among entries for the matched workload*. Consequences:

- A lexically-similar entry for a **different** workload no longer suppresses a correct recall (it's filtered out before the margin).
- A near-tie that should still fall through is one between **two entries for the same workload** (e.g. two incidents on `apps/web` with close scores) — genuinely ambiguous, so recall correctly declines.

This updates one existing test, `TestLookupMarginNearTieFallsThrough`: its near-tie runner-up currently has an empty `Resource` (so post-filter it is no longer a competitor). It will be updated to give the runner-up the **same** resource as the winner, making it a genuine same-workload near-tie that still falls through.

### 3.3 What is unchanged

`resourceAgrees`, `deriveRecallConfidence`, `outcomeFactor`, the outcome-decay block, the rejection metric/reasons (`no_resource_match`, `low_margin`), the config knobs (`MinScore`/`SoloFloor`/`MarginGap`/`RequireWorkloadMatch`/decay), the catalog index, and `kb_search`.

## 4. Components / seams

| Change | Location |
|---|---|
| Widen candidate `k`; structural pre-filter; margin among agreeing; `recallCandidateK` constant | `internal/investigate/recall.go` (`lookup`) |
| Tests (pre-filter surfaces a non-top-lexical winner; margin-among-agreeing; no-agreement; update near-tie) | `internal/investigate/recall_test.go` |

## 5. Trade-offs accepted in v1

- **Per-decision wider search** — `SearchScored(query, 20)` returns up to 20 candidates per recall; negligible over a bounded in-memory index, and recall frequency is low. A bleve-side resource filter (push-down) is the scale follow-up.
- **Margin re-interpretation** — the margin now compares same-workload candidates. This is the intended, more-correct semantics, and it updates one test. For corpora where a workload has a single curated entry (the common case), the solo-floor path governs.
- **Threshold continuity** — `MinScore`/`SoloFloor`/`MarginGap` now gate the structurally-chosen winner rather than the top lexical hit. Instant recall is opt-in/off by default and the thresholds are tunable from `recall_score` + `recall_rejections_total{reason}`, so this is acceptable; no default change.
- **`recallCandidateK` fixed at 20** — if a workload legitimately has >20 entries more lexically-relevant ahead of the right one, the right one could still be missed (a missed recall → a correct full investigation, fail-safe). Unrealistic at current scale.

## 6. Testing

- **Pre-filter surfaces a non-top-lexical winner (headline):** candidates where the two highest lexical scores are *different-workload* entries and the structurally-correct entry is, say, 3rd; assert it is recalled (previously impossible at k=2).
- **Margin among agreeing:** two **same-workload** entries with a near-tie (gap < `MarginGap`) → falls through (`low_margin`); a clear winner among same-workload entries → recalls.
- **No structurally-agreeing candidate** in the wider set → `reject("no_resource_match")`, falls through, and `recall_score` still records the top lexical score (miss visibility).
- **Single agreeing candidate** → solo-floor path (clears `SoloFloor`/`MinScore` → recalls; below → falls through).
- **Update** `TestLookupMarginNearTieFallsThrough`: runner-up given the same `Resource` as the winner so the near-tie is genuine post-filter.
- All other existing recall tests (structural matrix, decay branches, success-metric disambiguation, no-stored-resource) stay green.

## 7. Out of scope (later slices)

- Indexing `resource`/`namespace` as filterable bleve fields + a `ConjunctionQuery` push-down (the scale version of the pre-filter).
- Per-field boosting (`title^3`, etc.) — the "single conflated `text` field" finding.
- Making `recallCandidateK` (or `kb_search`'s `k`) configurable.
- Any embeddings / hybrid-retrieval work.

This slice closes the other half of the many-to-one problem: the disambiguation slice made the gate *discriminate*; this one makes the right entry *reachable*, so a correct recall is no longer lost just because a wrong-workload entry has more symptom-token overlap.
