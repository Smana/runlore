# RunLore Recall Trustworthiness — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-22 |
| **Scope** | Make instant recall (the KB cache) trustworthy enough to short-circuit an investigation: a multi-signal confident-recall gate, *derived* recall confidence, no duplicate-PR on a recall, and a minimal write-side resource field. First slice of the learning-loop critique. |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | The "accumulates ≠ learns" learning-loop critique; `internal/investigate/recall.go` + `loop.go`; `internal/catalog`; `internal/curator/draft.go`; the storm PR's recall metrics (`recall_score`, `recall_hits{result}`) which this builds on; phase-2: embeddings behind `catalog.Searcher` |

---

## 1. Why this exists

Instant recall is RunLore's KB cache: a BM25 match on the alert *symptom* short-circuits the whole investigation with **0 LLM calls**, returning a finding with a **hardcoded `Confidence: 0.8`** (`recall.go:59,64`). Per the project critique this is the "deepest flaw": **symptoms are many-to-one with root causes** (CrashLoopBackOff = OOM | bad image | missing secret | failed migration | probe regression), so a lexical symptom match can confidently return the *wrong* root cause for *this* occurrence. The absolute `MinScore` threshold (`recall.go:47`) isn't portable across corpora (a 5-entry seed vs a 500-entry catalog), and the `0.8` is fabricated.

The just-merged storm PR added the **measurement** layer — `recall_score` (histogram of the BM25 score at every decision) and `recall_hits{result=verified|downgraded|rejected}`. This spec adds the **trust** layer on top: a recall short-circuits only when multiple independent signals agree, and its confidence is *derived* from those signals rather than asserted.

This is the **first** of several learning-loop slices. Deliberately out of scope (separate specs): the outcome loop / decay (the "does it actually learn" core), embeddings, the broader confidence-de-overloading on the investigation path, and the full KB write-format (Type/Tags) fix.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **First slice = recall trustworthiness** (not the outcome loop) | Self-contained; builds directly on the recall metrics just shipped; de-risks the outcome loop, which needs trustworthy recall before it can sensibly decay/demote. |
| D2 | **Multi-signal confident-recall**: similarity margin **AND** structural agreement **AND** verify-survives | A single lexical signal can't separate many-to-one symptom→cause; a structural-agreement gate (resource match) is the lever that can. |
| D3 | **Similarity = BM25 + a *relative* margin (v1); embeddings deferred** | A margin (gap to the runner-up) is corpus-portable, unlike an absolute `MinScore`. Embeddings add a provider-coverage gap (no Anthropic embeddings endpoint) + a vector index → phase-2 behind `catalog.Searcher`. |
| D4 | **Recall confidence is *derived*** from match signals + verify (replaces the hardcoded `0.8`); capped < 1.0 | A cache hit must never assert certainty, and the number must be explainable — not an LLM scalar, not a constant. |
| D5 | **Don't curate recalled findings** | A recall matched an existing entry → not novel; skip `Curate()` on the recall path. |
| D6 | **Entries store their originating resource** (minimal write-side add) | Structural agreement needs it; the broader Type/Tags write-format fix stays a separate slice. |
| D7 | **Confidence scope = recall path only** | Keep the spec tight; full-investigation confidence derivation + gate-separation is a follow-up that pairs with the outcome loop. |

## 3. Design

### 3.1 The confident-recall gate (`recall.go`)

**Today:** BM25 search on `Title + " " + Message`; take the top hit; `if score < MinScore` → no recall; else return a finding with `Confidence: 0.8`.

**New:** short-circuit ONLY when all three hold (otherwise fall through to a normal investigation):

1. **Similarity margin** — over the top-2 bleve results:
   - `score₀ ≥ floor` (a low sanity floor — keeps `MinScore`'s role but lower), AND
   - `score₀ − score₁ ≥ margin_gap` (or `score₀ / score₁ ≥ margin_ratio`) — the top hit is an *unambiguous* winner. The margin is *relative*, so it's portable across corpus sizes.
   - If only one hit exists, require `score₀ ≥ solo_floor` (a higher bar — a single weak match is not confident).
2. **Structural agreement** — the incoming alert's resource (`namespace`, plus `workload` kind/name from labels) matches the matched entry's stored `Resource` (§3.4). An entry with no stored resource (predating §3.4) fails this gate → fall through. (Fail-safe: a missed recall just means a correct full investigation.)
3. **Verify survives** (unchanged) — the recalled finding still passes the adversarial `verifyFindings` pass already wired in `loop.go`.

Emit `recall_rejections_total{reason="low_margin"|"no_resource_match"}` (a new counter; verify-rejection is already `recall_hits{result=rejected}`) so the thresholds are tunable from live data alongside `recall_score`.

### 3.2 Derived recall confidence (replaces the hardcoded `0.8`)

A transparent, documented function — not an LLM number, not a constant:

```
base   = lerp(marginStrength)        → [0.55 .. 0.85]   // unambiguous winner higher, marginal lower
struct = +0.05 if exact (ns+workload) | +0.00 if namespace-only
verify = ×1.0 verified | ×0.5 downgraded | (rejected ⇒ recall fails — no value returned)
recallConfidence = clamp(base + struct, 0.50, 0.90) × verify
```

The exact constants are config knobs (§3.5); the *shape* is the contract. Capped at 0.90 — a cache hit never claims certainty. This value flows wherever the old `0.8` did.

### 3.3 Don't-curate-recalled (`loop.go`)

On the recall short-circuit path, do **not** call `Curate()` (the `OnComplete` → curation hook). A recall is by definition a match against an existing entry; re-curating would draft a near-duplicate PR (the curator's novelty dedup *might* catch it, but skipping is both correct and cheaper). The `recall_hits` metric already records the hit for visibility; per-entry recall bookkeeping (recall counts, last-recalled) is deferred to the outcome-loop spec.

### 3.4 Write-side: entries store their originating resource (`curator/draft.go`, `catalog`)

`draftKBEntry` currently leaves `Resource` empty (`curator/draft.go`). Populate it from the investigation's incident — `{Namespace, Kind, Name}` of the affected workload, which the curator already has (the dedup fingerprint already uses workload namespace/name). Add a structured `Resource` to the catalog `Entry` (front-matter) and include it in the bleve index. This is the **minimal** write-side change that makes §3.1's structural agreement possible; the broader Type/Tags fix (still hardcoded `Incident` / `["runlore","incident"]`) remains a separate "KB write quality" slice.

### 3.5 Config

New knobs (under the catalog/recall config): `margin_gap` (or `margin_ratio`), `floor`, `solo_floor`, and structural strictness (`require_workload_match` — exact `ns+workload` vs namespace-only). Conservative defaults; tune from `recall_score` + `recall_rejections_total`.

## 4. Components / seams

| Change | Location |
|---|---|
| Confident-recall gate (margin + structural + derived confidence) | `internal/investigate/recall.go` |
| Skip curation on the recall short-circuit | `internal/investigate/loop.go` |
| `Resource` field on `Entry` + index it | `internal/catalog/` (entry + index) |
| Populate `Resource` on draft | `internal/curator/draft.go` |
| `recall_rejections_total{reason}` counter | `internal/telemetry/metrics.go` + `recall.go` |
| Margin / floor / strictness config | `internal/config/config.go` |

## 5. Trade-offs accepted in v1

- **BM25 margin, not embeddings** — a reworded symptom with low lexical overlap won't recall even if semantically identical. Acceptable: a missed recall is just a (correct) full investigation; embeddings are the phase-2 upgrade behind `catalog.Searcher`.
- **Structural agreement requires a stored resource** — entries written before §3.4 (empty `Resource`) won't recall until re-curated. Acceptable: fail-safe (fall through to investigate); the corpus heals as new entries are written.
- **Derived confidence is hand-tuned, not learned** — the outcome loop (later slice) will calibrate it from real resolution data. For now it's an explainable formula, which already beats a fabricated `0.8`.

## 6. Testing

- **`recall_test.go`**: clear winner (large margin) short-circuits; near-tie (small margin) falls through; lone weak hit falls through; structural match required (resource mismatch → fall through); entry with no stored resource → fall through; derived-confidence formula across margin/struct/verify combinations.
- **`loop_test.go`**: the recall short-circuit path does **not** call `Curate`.
- **`curator/draft` test**: `Resource` populated from the incident.
- **Live tuning**: `recall_score` + `recall_rejections_total` after seeding the catalog.

## 7. Out of scope (other learning-loop slices)

- The **outcome loop** / decay / demotion + per-entry recall bookkeeping — the "learns, not accumulates" core. The next slice.
- **Embeddings** — hybrid BM25+embeddings behind `catalog.Searcher`; the provider-coverage problem (no Anthropic endpoint) is solved there.
- **Full investigation-confidence derivation + gate-separation** (the broader half of critique fix #5) — pairs naturally with the outcome loop.
- **KB write-format Type/Tags fix** — this spec adds only the `Resource` field needed for structural agreement.

This is a focused, shippable first slice: it makes the cache *defensible* and *measured* without yet claiming it *learns*.
