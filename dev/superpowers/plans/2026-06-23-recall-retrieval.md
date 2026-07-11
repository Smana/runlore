# Recall Retrieval — Wider Candidates + Structural Pre-filter — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make instant recall find the structurally-correct catalog entry even when it isn't the top lexical hit, by fetching a wider candidate set, pre-filtering by structural agreement, and applying the margin gate among the agreeing candidates.

**Architecture:** One atomic restructure of `Recall.lookup` in `internal/investigate/recall.go`: `SearchScored(query, recallCandidateK)` → structural pre-filter (preserving lexical order) → margin gate among the agreeing subset → derive + decay (unchanged) on the winner. Go-side filter; no bleve index change.

**Tech Stack:** Go 1.26, stdlib `testing` (no testify). Tests use the existing `recallWith`/`okReq`/`fakeScored` helpers in `internal/investigate/` (`fakeScored.SearchScored` ignores `k` and returns all hits, so multi-candidate tests work directly).

**Spec:** `dev/superpowers/specs/2026-06-23-recall-retrieval-design.md`

**Branch:** `feat/recall-retrieval` (already checked out; the spec is already committed there).

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/investigate/recall.go` | recall gate | `recallCandidateK` constant; restructure `lookup` (wider fetch → structural pre-filter → margin among agreeing) |
| `internal/investigate/recall_test.go` | recall tests | add 3 tests (non-top-lexical winner, margin-among-agreeing clear winner, no-agreement); update `TestLookupMarginNearTieFallsThrough` |

The change is atomic (you cannot half-restructure `lookup` and keep tests green), so it is one implementation task (T1) plus whole-package verification (T2).

---

### Task 1: Restructure `lookup` — wider candidates + structural pre-filter

**Files:**
- Modify: `internal/investigate/recall.go` (`lookup`, currently `:48-108`; add `recallCandidateK` const)
- Test: `internal/investigate/recall_test.go`

- [ ] **Step 1: Write the failing headline test**

Add to `internal/investigate/recall_test.go`:

```go
func TestLookupStructuralWinnerBelowTopLexical(t *testing.T) {
	// The two highest lexical hits are DIFFERENT workloads; the structurally-correct
	// entry (apps/web) is only the 3rd lexical hit. Pre-filtering must surface it —
	// the old k=2 / top-hit-only logic could not.
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OtherA", Path: "a.md", Resource: "apps/api"}, Score: 9.0},
		{Entry: catalog.Entry{Title: "OtherB", Path: "b.md", Resource: "apps/worker"}, Score: 8.0},
		{Entry: catalog.Entry{Title: "Web OOM", Path: "web.md", Resource: "apps/web"}, Score: 5.0},
	})
	e, _ := r.lookup(context.Background(), okReq()) // okReq workload = apps/web
	if e == nil || e.Path != "web.md" {
		t.Fatalf("expected the structurally-correct apps/web entry (web.md) to be recalled, got %+v", e)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestLookupStructuralWinnerBelowTopLexical`
Expected: FAIL — under current logic `hits[0]` is `a.md` (`apps/api`), Gate 2 rejects (`no_resource_match`), so `lookup` returns `nil` and the test's `e == nil` branch fires.

- [ ] **Step 3: Restructure `lookup`**

In `internal/investigate/recall.go`, add the constant just above `lookup`:

```go
// recallCandidateK is the internal lexical candidate window. Recall fetches this
// many hits, then structurally pre-filters them, so an entry matching the alert's
// workload is reachable even when other workloads' entries score higher on symptom
// text alone.
const recallCandidateK = 20
```

Replace the entire body of `lookup` (from the `SearchScored` line through `return &e, conf`) with:

```go
	// Query the symptom (title + message); severity/reason is noise for matching.
	hits, err := r.Catalog.SearchScored(strings.TrimSpace(req.Title+" "+req.Message), recallCandidateK)
	if err != nil || len(hits) == 0 {
		return nil, 0
	}

	// Structural pre-filter: keep candidates whose stored resource agrees with the
	// alert's workload, preserving lexical order. Pre-filtering (rather than checking
	// only the top hit) lets a structurally-correct entry win even when wrong-workload
	// entries score higher on symptom tokens.
	var agreeing []catalog.ScoredEntry
	for _, h := range hits {
		if resourceAgrees(req.Workload, h.Entry.Resource, r.RequireWorkloadMatch) != matchNone {
			agreeing = append(agreeing, h)
		}
	}
	if len(agreeing) == 0 {
		if r.Metrics != nil {
			r.Metrics.RecallScore.Record(ctx, hits[0].Score) // best lexical score, for miss visibility
		}
		r.reject(ctx, "no_resource_match")
		return nil, 0
	}

	winner := agreeing[0]
	score := winner.Score
	if r.Metrics != nil {
		r.Metrics.RecallScore.Record(ctx, score)
	}

	// Gate — margin among the structurally-agreeing candidates: a clear winner for
	// this workload, not merely the top lexical hit. A lone agreeing hit must clear
	// both the solo floor and the min score.
	margin := score
	confident := score >= r.SoloFloor && score >= r.MinScore
	if len(agreeing) > 1 {
		margin = score - agreeing[1].Score
		confident = score >= r.MinScore && margin >= r.MarginGap
	}
	if !confident {
		r.reject(ctx, "low_margin")
		return nil, 0
	}

	e := winner.Entry
	strength := resourceAgrees(req.Workload, e.Resource, r.RequireWorkloadMatch)
	conf := deriveRecallConfidence(score, margin, strength)
	// Outcome decay: bias confidence by the entry's resolution track record, and
	// reject (re-investigate) an entry that recalls-but-never-resolves. Fail-safe —
	// a rejected recall just falls through to a full investigation.
	if r.Outcome != nil {
		if counts, err := r.Outcome.OpenCounts(); err == nil {
			if agg, ok := counts[e.Path]; ok { // only entries with recall history
				f := outcomeFactor(agg.Recalls, agg.Resolved, r.OutcomePrior)
				if f < r.OutcomeFloor {
					r.reject(ctx, "low_outcome")
					return nil, 0
				}
				conf = clampF(conf*f, 0, 0.90)
			}
		} else if r.Log != nil {
			r.Log.Warn("recall: outcome stats unavailable; skipping decay", "err", err)
		}
	}
	if r.Log != nil {
		r.Log.Info("instant recall decision",
			"alert", req.Title, "entry_id", e.Path, "score", score, "margin", margin, "confidence", conf)
	}
	return &e, conf
```

(The `if r == nil || r.Catalog == nil { return nil, 0 }` guard at the top of `lookup` stays; only the body below it is replaced. `resourceAgrees`, `deriveRecallConfidence`, `outcomeFactor`, `clampF`, `reject` are unchanged.)

- [ ] **Step 4: Run the headline test to verify it passes**

Run: `go test ./internal/investigate/ -run TestLookupStructuralWinnerBelowTopLexical`
Expected: PASS — `agreeing = [web.md]` (the two higher hits are different workloads), solo, `5.0 >= SoloFloor 4.0` → recalls `web.md`.

- [ ] **Step 5: Update the near-tie test for the new margin semantics**

In `internal/investigate/recall_test.go`, replace `TestLookupMarginNearTieFallsThrough` with the version below (the runner-up now shares the winner's resource, so it is a genuine *same-workload* near-tie post-filter):

```go
func TestLookupMarginNearTieFallsThrough(t *testing.T) {
	// Two entries for the SAME workload with a near-tie (gap 0.5 < MarginGap 1.0) →
	// genuinely ambiguous → falls through. (Post-structural-filter the margin compares
	// same-workload candidates, not arbitrary lexical neighbours.)
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "BadImage", Path: "b.md", Resource: "apps/web"}, Score: 5.5},
	})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("near-tie among same-workload entries (gap 0.5 < 1.0) must fall through")
	}
}
```

- [ ] **Step 6: Add the remaining new tests**

Add to `internal/investigate/recall_test.go`:

```go
func TestLookupMarginAmongAgreeingClearWinner(t *testing.T) {
	// Two same-workload entries with a clear margin (gap 4.0 >= 1.0) → recall the winner.
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Web recent", Path: "web1.md", Resource: "apps/web"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "Web old", Path: "web2.md", Resource: "apps/web"}, Score: 2.0},
	})
	e, _ := r.lookup(context.Background(), okReq())
	if e == nil || e.Path != "web1.md" {
		t.Fatalf("clear same-workload winner should recall web1.md, got %+v", e)
	}
}

func TestLookupNoAgreeingCandidateFallsThrough(t *testing.T) {
	// Every candidate is a different workload → none agree → fall through.
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "A", Path: "a.md", Resource: "apps/api"}, Score: 9.0},
		{Entry: catalog.Entry{Title: "B", Path: "b.md", Resource: "other/web"}, Score: 8.0},
	})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("no structurally-agreeing candidate must fall through")
	}
}
```

- [ ] **Step 7: Run the full package test**

Run: `go test ./internal/investigate/`
Expected: PASS — the new/updated tests plus all pre-existing recall tests. Notably still green:
- `TestLookupMarginClearWinner` (runner-up has empty `Resource` → filtered out → winner is solo, `6.0 >= 4.0` → recalls);
- `TestLookupStructuralMatch` / `TestLookupStructuralNamespaceOnly` / `TestLookupStructuralMismatchFallsThrough` / `TestLookupNoStoredResourceFallsThrough`;
- the decay tests (`soloRecall` has a single hit → solo path) and the success-metric disambiguation tests.

- [ ] **Step 8: Commit**

```bash
git add internal/investigate/recall.go internal/investigate/recall_test.go
git commit -m "feat(recall): wider candidate set + structural pre-filter so the right entry is findable"
```

---

### Task 2: Whole-tree verification

- [ ] **Step 1: Build, test, vet**

Run:
```bash
go build ./... && go test ./... && go vet ./...
```
Expected: build clean; all tests PASS; vet clean.

No commit (verification only).

---

## Notes for the implementer

- `fakeScored.SearchScored(string, int)` ignores `k` and returns all configured `hits`, so the multi-candidate tests exercise the pre-filter directly even though production fetches `recallCandidateK`.
- The margin now compares the top two **structurally-agreeing** candidates (or applies the solo floor when only one agrees). This is the intended semantics change; the only existing test it affects is `TestLookupMarginNearTieFallsThrough`, updated in Step 5.
- `RecallScore` is recorded for the winner on the agreeing path, and for the top lexical hit on a no-agreement miss — so the histogram stays populated for threshold tuning either way.
- Do not add a bleve resource field, field boosting, or a config knob for `recallCandidateK` — all deferred (spec §7). `kb_search` (k=3) is a separate path; leave it.
- `resourceAgrees` is computed per-candidate in the filter and once more for the winner (to get its `strength` for `deriveRecallConfidence`); it is cheap and pure, so the recompute is fine.
