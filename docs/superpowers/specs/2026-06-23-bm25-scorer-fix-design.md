# RunLore BM25 Scorer Fix — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-23 |
| **Scope** | Make the catalog index actually run **BM25** (it silently runs legacy TF-IDF), prove it with a test, and add observability for the always-on curator dedup so the score floors can be re-fit from live data in a follow-up. **Re-tuning any floor is explicitly out of scope.** |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | The deep-analysis report (`docs/analysis/2026-06-23-deep-analysis.md`) finding #1 / Slice 1 — corroborated by 3 critique lenses; `internal/catalog/catalog.go`; `internal/investigate/recall.go` (recall floors, opt-in); `internal/curator/curator.go` + `internal/curator/fingerprint.go` (`DupScore`, always-on); `internal/telemetry/metrics.go` (`recall_score` pattern this mirrors); the recall-trustworthiness spec (`2026-06-22-recall-trustworthiness-design.md`) whose "corpus-portable margin" premise depends on the scorer |

---

## 1. Why this exists

The catalog's bleve index is built with a bare default mapping that **never sets `ScoringModel`**, so bleve silently falls back to its legacy **TF-IDF** scorer — while every comment, the `recall_score` metric, the config docs, and the entire "corpus-portable margin" design premise call it **BM25**.

- `internal/catalog/catalog.go:36` (`NewEmpty`) and `:68` (`buildIndex`) both call `bleve.NewMemOnly(bleve.NewIndexMapping())` with no `ScoringModel`.
- In bleve `v2.6.0` / `bleve_index_api v1.3.11`, `DefaultScoringModel = TFIDFScoring` (`indexing_options.go:37`); `isBM25Enabled` returns true only when `ScoringModel == "bm25"`, so the empty default resolves to TF-IDF at query time (`index_impl.go:714-717`).
- No test asserts the active scoring model, so the silent fallback was never caught.

This matters because the recall floors (`MinScore`, `SoloFloor`, `MarginGap`) and the curator's `DupScore` are reasoned/tuned against BM25's bounded, length-normalized, term-frequency-saturating scores — but the index produces TF-IDF scores, which have a different distribution. The relative `MarginGap` is partly insulated; the absolute floors are not. **This is the foundational fix that every later retrieval/recall improvement depends on** — you cannot honestly tune a scorer the code does not run.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **First slice = the scorer fix alone** | Smallest, highest-leverage, fully independent; unblocks all retrieval tuning; proves the spec→plan→TDD flow on one safe change. |
| D2 | **Flip the scorer + add observability; DEFER all re-tuning** | Re-fitting `MinScore`/`SoloFloor`/`MarginGap`/`DupScore` needs the BM25 score distribution from live data. Ship the flip + a way to *see* the new scores now; re-tune in a follow-up so the two concerns are independently revertible. |
| D3 | **Single source of truth for the mapping** | The bug existed because two index-construction sites drifted. A shared `newIndexMapping()` helper prevents recurrence. |
| D4 | **Use the literal `"bm25"`, validated by bleve** | bleve validates `ScoringModel` against `SupportedScoringModels` and fails loudly on an unsupported value — no need to import `bleve_index_api` just for the constant. |
| D5 | **Mirror the existing `recall_score` observability pattern** | A nil-safe `*telemetry.Metrics` + a histogram recorded on every decision is already the house style (`Recall`). Consistency + the full score distribution needed to re-tune. |

## 3. Design

### 3.1 The scorer flip (`internal/catalog/catalog.go`)

Add one unexported helper and route both construction sites through it:

```go
// newIndexMapping returns the index mapping used everywhere in the catalog. It
// pins the scoring model to BM25 — bleve defaults to legacy TF-IDF when unset,
// whose unbounded, non-saturating scores are not corpus-portable.
func newIndexMapping() *mapping.IndexMappingImpl {
	im := bleve.NewIndexMapping()
	im.ScoringModel = "bm25" // validated by bleve against SupportedScoringModels
	return im
}
```

- `NewEmpty` (`:36`) and `buildIndex` (`:68`) both call `bleve.NewMemOnly(newIndexMapping())`.
- The now-accurate comments at `catalog.go:12,91,107` (and the "BM25" references in `recall.go:18,21,35`, `config.go:120,389`) stay — they become true rather than aspirational. No comment text changes are required beyond confirming accuracy.

### 3.2 Dedup observability (`internal/telemetry/metrics.go`, `internal/curator/curator.go`)

The curator's catalog dedup (`Curate` → `Novelty.IsDuplicate`) is **always on** (wired at `cmd/lore/main.go`), so the scorer flip changes the meaning of `DupScore=5.0` immediately. Make that observable, mirroring `recall_score`:

- **Metric:** add a `CurationDedupScore` histogram to `telemetry.Metrics` ("catalog top-hit relevance score at the curation dedup decision"), constructed alongside `RecallScore`.
- **Wiring:** add a nil-safe `Metrics *telemetry.Metrics` field to `Curator` (exactly as `Recall` has one); pass the existing `metrics` handle in at the `cmd/lore/main.go` build site.
- **Record:** in `Curate`, record the **top-hit score on every call** (whether or not it crosses `DupScore`) so the follow-up sees the full distribution — not only the firing tail. Enrich the existing "duplicates a catalog entry" log line (`curator.go:43`) with the score.
- This requires `Novelty.IsDuplicate` (or a thin variant) to surface the top-hit score even when it does not declare a duplicate; today it returns the hit only on a positive match. The minimal change: have it return the top `ScoredEntry` (and a bool) regardless, and let `Curate` both decide and record.

### 3.3 No behavior change beyond the scorer

Recall (opt-in, off by default), curation dedup *decisions*, and the investigation loop are otherwise untouched. The only default-config behavioral consequence is that the always-on curator dedup now compares BM25 scores against the unchanged `DupScore=5.0` — a known, now-observable shift, with re-tuning deferred to a follow-up.

## 4. Components / seams

| Change | Location |
|---|---|
| `newIndexMapping()` helper; both index sites use it | `internal/catalog/catalog.go` |
| `TestNewIndexMappingUsesBM25` (the load-bearing guard) + an end-to-end scoring sanity check | `internal/catalog/catalog_test.go` |
| `CurationDedupScore` histogram (nil-safe instrument) | `internal/telemetry/metrics.go` |
| Surface the top hit + score from `IsDuplicate` regardless of match | `internal/curator/fingerprint.go` |
| Nil-safe `Metrics` field; record top-hit dedup score every `Curate`; enrich dup log | `internal/curator/curator.go` |
| Pass `metrics` into the `Curator` | `cmd/lore/main.go` (build site) |
| Curator test asserting the score is recorded / logged | `internal/curator/curator_test.go` |

## 5. Trade-offs accepted in v1

- **Floors stay numerically unchanged** — the scorer flip shifts score magnitudes, so the existing `MinScore`/`SoloFloor`/`MarginGap`/`DupScore` are, strictly, mis-calibrated until the follow-up re-fits them from the new `recall_score` / `CurationDedupScore` distributions. Accepted: instant recall is opt-in/off by default (so recall floors affect only opt-in users), and the always-on dedup shift is now *observable*, which is the prerequisite for tuning it. Shipping the flip + observability separately keeps both reversible.
- **No synthetic-corpus measurement in this slice** — interim BM25 defaults could be derived now, but that adds a fixture and is less reversible. Deferred to the re-tuning follow-up.
- **`MarginGap` is relative** and therefore largely insulated from the absolute-scale change; the floors most affected are the absolute ones, which the follow-up targets.

## 6. Testing

- **`catalog_test.go`**: `TestNewIndexMappingUsesBM25` asserts `newIndexMapping().ScoringModel == "bm25"` — the **load-bearing regression guard**. Because both index-construction sites (`NewEmpty`, `buildIndex`) are forced through this single helper (D3), asserting the helper guarantees BM25 everywhere. Plus an end-to-end sanity check (`TestBuildIndexScores`): an index built through the helper accepts the BM25 model (bleve validates `ScoringModel` at build time and errors on an unsupported value) and returns ordered, positive scores for a discriminating query. We deliberately do **not** assert score *magnitudes* to distinguish BM25 from TF-IDF — TF-IDF also length-normalizes, so a magnitude-based discriminator is brittle; the direct mapping assertion is the reliable signal.
- **`curator_test.go`**: a dedup case asserts the top-hit score is recorded to the metric and included in the log line on every `Curate` (firing and non-firing).
- **Whole tree**: `go build ./...`, `go test ./...`, `golangci-lint run` all green.

## 7. Out of scope (follow-up slices)

- **Re-fitting** `MinScore` / `SoloFloor` / `MarginGap` / `DupScore` from the live BM25 distributions (the immediate follow-up to this slice).
- Indexing `cause`/`resolution` as weighted fields and `resource`/`namespace` as filterable fields (deep-analysis #3).
- The structural-agreement pre-filter and widened internal `k` (#3).
- Phase-2 vectors (chromem-go + RRF).

This slice makes the catalog *run the scorer it claims to run*, and makes the one always-on consumer's behavior *visible* — the cheapest high-leverage fix and the foundation the rest of the retrieval roadmap stands on.
