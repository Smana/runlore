# RunLore Entity Precision in Track A — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-23 |
| **Scope** | Make the **replay** eval (Track A) score root-cause *correctness* deterministically at the entity level — recall-gated precision with an over-claim/false-positive penalty — instead of pure keyword presence. Confined to `internal/eval/case.go` (schema), `internal/eval/score.go` (scoring + `Result`), and their tests. Track B (live, LLM-judge) is untouched. |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | Deep-analysis report roadmap #4 ("Add `root_cause_entities`; deterministic recall-gated precision in Track A; over-claim/FP penalty; reserve judge for fuzzy dims") and §line 139 ("ITBench's finding — more turns correlate with worse scores because strong models over-claim — means an eval with no over-claim penalty cannot see the dominant failure mode"). Builds on #5 (k-of-n gate, Track B) and #6 (recall-in-eval). Unblocks #7 (eval into CI). |

---

## 1. Why this exists

RunLore has two eval tracks. **Track B** (`live.go`) runs on a live cluster and grades with an LLM judge — five fuzzy dimensions, k-of-n gate (#5). **Track A** (`eval.go`/`case.go`/`score.go`) replays recorded tool outputs through the loop and scores **deterministically** via `Score()`: every `Expected.MustContain` keyword must appear in the flattened findings text, and confidence must clear `MinConfidence`. Track A is reproducible and cluster-independent — the natural CI benchmark (#7 depends on it).

The gap is in Track A's scoring. `MustContain` checks **recall only** — "did the right keyword appear anywhere in the findings?" — with **no precision and no over-claim penalty**. An investigation that blames ten entities, one of which is correct, passes. That is exactly the dominant failure mode the literature flags: strong models *over-claim*, asserting wrong entities as causes. An eval that cannot see over-claiming cannot measure the thing that most distinguishes a good RCA from a confident-wrong one.

This slice makes Track A score **entity-level precision deterministically**: ground truth lists the entities that must be named as the cause (recall) and the plausible-but-wrong entities that must not be blamed (over-claim). A case passes the entity check only when it names all the right entities and none of the wrong ones — recall-gated precision, no LLM in the loop.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **Track A (replay) only** | The report's literal wording, and the home of deterministic/CI scoring. Track B's judge stays for fuzzy dims; over-claim correctness moves to a deterministic Track-A gate so we stop leaning on the biased same-family judge to catch it. |
| D2 | **Authored distractor set** (not structured-entity precision) | Replay is text-based — static tool outputs → findings text — so the investigation rarely populates structured `Resource`/`Changes`. Detecting an over-claim deterministically needs an explicit notion of "wrong entities." Per-scenario authored `distractors` is reliable in text replay and is appropriate for a curated gold set (ITBench-style). Structured-entity precision is thin/unreliable in Track A. |
| D3 | **Entity matching scans *claim text* only** (`Title` + each `RootCause.Summary`/`SuggestedAction`), excluding `Evidence`/`Unresolved` | Semantics: "did you *blame* this entity," not "did you mention it while ruling it out." A distractor cited as ruled-out evidence must not count as an over-claim; an expected entity must be named *as a cause*, not merely appear in raw evidence. |
| D4 | **Binary recall-gated pass** (`recall && no over-claim`), not a graded precision float | Deterministic and sufficient for a gate. A graded precision/recall metric (e.g. fractional credit) is a later refinement. |
| D5 | **Additive schema; backward compatible** | New `RootCauseEntities`/`Distractors` fields engage only when `RootCauseEntities` is non-empty. Existing `MustContain`-only cases score identically to today. |

## 3. Design

### 3.1 Schema (`case.go`, `Expected`)

Add two fields to `Expected`:

```go
type Expected struct {
	MustContain       []string `yaml:"must_contain"`        // keywords that must appear in the findings (existing)
	MinConfidence     float64  `yaml:"min_confidence"`      // confidence floor (existing)
	RootCauseEntities []string `yaml:"root_cause_entities"` // entities that MUST be named as the cause (recall)
	Distractors       []string `yaml:"distractors"`         // plausible-but-wrong entities that must NOT be blamed (over-claim/FP)
}
```

Entities are canonical, discriminative strings (e.g. `apps/web`, `rds/prod-db`, a Deployment name). Entity scoring engages only when `RootCauseEntities` is non-empty.

### 3.2 Claim text vs evidence (`score.go`)

A new helper isolates the *asserted cause + fix*, excluding evidence and unresolved notes:

```go
// claimText is what the investigation BLAMED: the title plus each hypothesis's
// summary and suggested action. It deliberately excludes Evidence and Unresolved
// so entity matching means "named as a cause", not "mentioned while ruling out".
func claimText(inv providers.Investigation) string {
	var b strings.Builder
	b.WriteString(inv.Title)
	for _, rc := range inv.RootCauses {
		b.WriteString(" " + rc.Summary + " " + rc.SuggestedAction)
	}
	return b.String()
}
```

(The existing `investigationText` — which *includes* Evidence/Unresolved — is unchanged and still used for `MustContain` recall, preserving today's behavior for keyword scoring.)

### 3.3 Scoring (`score.go`, `Score`)

Case-insensitive substring match (same style as `MustContain`). Over the lowercased claim text:

- **recall**: every string in `RootCauseEntities` appears → un-named ones are added to `Missing`.
- **over-claim**: any string in `Distractors` appears → collected into a new `OverClaimed []string`.
- **entity pass** (only when `RootCauseEntities` non-empty): `len(missingEntities) == 0 && len(OverClaimed) == 0`.

`Result` gains `OverClaimed []string`. The overall `Pass`:

```
Pass = keywordPass (all MustContain present, if any)
    && entityPass   (recall met AND no over-claim, if RootCauseEntities non-empty)
    && inv.Confidence >= MinConfidence
```

When `RootCauseEntities` is empty, `entityPass` is vacuously true → identical to today. Over-claimed entities are carried structurally in `Result.OverClaimed` **and** mirrored into `Missing` as `"over-claimed: <entity>"` entries, so existing callers that render `Missing` (the report failure note) show *why* a case failed without changing their rendering code.

### 3.4 What is unchanged

Track B (`live.go`), the `ModelJudge` and its rubric, `GroundTruth` (Track B's truth), `Coverage`, the replay harness (`eval.go` `Runner`/`runOne`/`staticTool`), and `MustContain`/`MinConfidence` semantics. The judge is "reserved for fuzzy dims" structurally — root-cause correctness is now a deterministic Track-A gate, not a fuzzy-judge score.

## 4. Components / seams

| Change | Location |
|---|---|
| `RootCauseEntities`, `Distractors` fields on `Expected` | `internal/eval/case.go` |
| `claimText` helper; entity recall + over-claim scoring; `OverClaimed` on `Result`; gated `Pass` | `internal/eval/score.go` |
| Table-driven entity-scoring tests | `internal/eval/score_test.go` (new or extend existing) |

## 5. Trade-offs accepted in v1

- **Substring matching** — an entity `web` could match `webhook`. Entities are canonical refs (`apps/web`), so authors pick discriminative strings; word-boundary matching is YAGNI.
- **Per-scenario distractor authoring** — each entity-scored case must list its distractors. Accepted: it is a curated gold set, and a distractor-free case simply has no over-claim signal (recall-only, same as a `MustContain` case).
- **Binary pass, not graded precision** — `recall && no-overclaim`. Sufficient for a gate; fractional precision/recall credit is a later refinement.
- **Claim-text scoping is heuristic** — `Title` + `Summary` + `SuggestedAction`. If an author puts the real claim only in `Evidence`, it won't count; this is intended (evidence is not a claim).

## 6. Testing

Table-driven over synthetic `providers.Investigation` values (no live cluster, no LLM):

- **`TestEntityRecallAllNamedPasses`** — all `RootCauseEntities` in claim text, no distractors → `Pass==true`, no `Missing`/`OverClaimed`.
- **`TestEntityMissingFails`** — an expected entity absent from claim text → it appears in `Missing`, `Pass==false`.
- **`TestOverClaimDistractorBlamedFails`** (headline) — a `Distractors` entity appears in claim text *while all expected entities are also present* → `OverClaimed` lists it, `Pass==false`. Proves the over-claim penalty independent of recall.
- **`TestDistractorInEvidenceNotPenalized`** — a distractor appears only in a hypothesis's `Evidence` (and/or `Unresolved`), never in `Summary`/`SuggestedAction`/`Title` → not penalized, `Pass==true`. Proves claim-text scoping.
- **`TestNoEntitiesBackwardCompatible`** — a case with only `MustContain` (no `RootCauseEntities`) scores identically to today (pass on keyword+confidence, ignore entities).
- `go build ./... && go test ./... && go vet ./...` green.

## 7. Out of scope (later slices)

- **#7** — wiring Track A (this gate) into CI with a fail-on-threshold.
- Graded (fractional) entity precision/recall instead of binary pass.
- Word-boundary / structured-ref entity matching.
- Porting the deterministic entity gate into Track B's live runner (the brainstorm's rejected "Track B too" option).
- Structured-entity precision from `inv.Resource`/`Changes` (the rejected D2 alternative).

This slice closes the deep analysis's eval-validity gap: Track A can finally *see* over-claiming — the dominant RCA failure mode — and gates on it deterministically, instead of rewarding any investigation that mentions the right keyword among many wrong ones.
