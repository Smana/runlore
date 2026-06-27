# BM25 Scorer Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the catalog's bleve index actually run BM25 (it silently runs legacy TF-IDF), prove it with a test, and add observability for the always-on curator dedup score — deferring all numeric re-tuning.

**Architecture:** A single shared `newIndexMapping()` helper pins `ScoringModel = "bm25"` so both index-construction sites can't drift again. Curator dedup gains a `Novelty.TopHit` accessor and a nil-safe `*telemetry.Metrics`, recording the top-hit score on every `Curate` via a new `curation_dedup_score` histogram (mirroring the existing `recall_score` pattern).

**Tech Stack:** Go 1.26, `github.com/blevesearch/bleve/v2` (v2.6.0), OpenTelemetry metrics (`go.opentelemetry.io/otel/metric`), stdlib `testing` (no testify).

**Spec:** `docs/superpowers/specs/2026-06-23-bm25-scorer-fix-design.md`

**Branch:** `feat/bm25-scorer-fix` (already checked out; the spec is already committed there).

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/catalog/catalog.go` | Index construction + search | Add `newIndexMapping()`; route `NewEmpty` + `buildIndex` through it; import `bleve/v2/mapping` |
| `internal/catalog/catalog_test.go` | Catalog tests | Add `TestNewIndexMappingUsesBM25`, `TestBuildIndexScores` |
| `internal/telemetry/metrics.go` | Instrument set | Add `CurationDedupScore` histogram field + constructor line |
| `internal/telemetry/metrics_test.go` | Instrument safety | Add `TestNewMetricsCurationInstruments` |
| `internal/curator/fingerprint.go` | Novelty/dedup scoring | Add `TopHit`; refactor `IsDuplicate` to use it |
| `internal/curator/fingerprint_test.go` | Novelty tests | Add `TestTopHitReturnsScore`, `TestTopHitNilCatalog` |
| `internal/curator/curator.go` | File-time curation gate | Add nil-safe `Metrics` field; use `TopHit` in `Curate`; record score; enrich dup log |
| `internal/curator/curator_test.go` | Curator tests | Add `TestCurateRecordsDedupScore`, `TestCurateDedupScoreNilMetricsSafe` |
| `cmd/lore/main.go` | Wiring | Thread `metrics` into `buildCurator` + its call site |

Task order: Task 1 (catalog) and Task 2 (telemetry) are independent. Task 3 (fingerprint) is independent. Task 4 depends on 2+3. Task 5 depends on 4.

---

### Task 1: Pin the catalog index to BM25

**Files:**
- Modify: `internal/catalog/catalog.go` (imports; `NewEmpty:34-38`; `buildIndex:67-82`)
- Test: `internal/catalog/catalog_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/catalog/catalog_test.go`:

```go
func TestNewIndexMappingUsesBM25(t *testing.T) {
	// bleve defaults to legacy TF-IDF when ScoringModel is unset. Both index
	// sites are forced through this helper, so asserting it guarantees BM25
	// everywhere. This is the regression guard for the silent-fallback bug.
	if got := newIndexMapping().ScoringModel; got != "bm25" {
		t.Fatalf("ScoringModel = %q, want \"bm25\"", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/catalog/ -run TestNewIndexMappingUsesBM25`
Expected: FAIL — compile error `undefined: newIndexMapping`.

- [ ] **Step 3: Add the helper and route both sites through it**

In `internal/catalog/catalog.go`, add `"github.com/blevesearch/bleve/v2/mapping"` to the import block. Add the helper near the top of the file (after the imports, before `New`):

```go
// newIndexMapping returns the index mapping used by every catalog index. It pins
// the scoring model to BM25 — bleve defaults to legacy TF-IDF when ScoringModel
// is unset, whose unbounded, non-saturating scores are not corpus-portable.
func newIndexMapping() *mapping.IndexMappingImpl {
	im := bleve.NewIndexMapping()
	im.ScoringModel = "bm25" // validated by bleve against SupportedScoringModels
	return im
}
```

In `NewEmpty` (`:36`), replace `bleve.NewMemOnly(bleve.NewIndexMapping())` with `bleve.NewMemOnly(newIndexMapping())`.

In `buildIndex` (`:68`), replace `bleve.NewMemOnly(bleve.NewIndexMapping())` with `bleve.NewMemOnly(newIndexMapping())`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/catalog/ -run TestNewIndexMappingUsesBM25`
Expected: PASS.

- [ ] **Step 5: Add the end-to-end scoring sanity test**

Add to `internal/catalog/catalog_test.go`:

```go
func TestBuildIndexScores(t *testing.T) {
	// Proves the BM25 mapping is accepted (NewMemOnly errors on an unsupported
	// scoring model) and that scoring + ranking work end-to-end. We do NOT assert
	// magnitudes — TF-IDF also length-normalizes, so magnitude-based BM25-vs-TFIDF
	// discrimination is brittle; the helper assertion above is the reliable guard.
	entries := []Entry{
		{Title: "OOMKilled pod", Body: "container exceeded its memory limit"},
		{Title: "Image pull failure", Body: "registry returned forbidden"},
	}
	idx, err := buildIndex(entries)
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	defer func() { _ = idx.Close() }()
	c := &Catalog{index: idx, entries: entries}
	hits, err := c.SearchScored("memory limit", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Score <= 0 {
		t.Fatalf("expected a positive-scored hit, got %+v", hits)
	}
	if hits[0].Entry.Title != "OOMKilled pod" {
		t.Fatalf("expected the OOM entry ranked first, got %q", hits[0].Entry.Title)
	}
}
```

- [ ] **Step 6: Run the catalog tests**

Run: `go test ./internal/catalog/`
Expected: PASS (all, including pre-existing tests).

- [ ] **Step 7: Commit**

```bash
git add internal/catalog/catalog.go internal/catalog/catalog_test.go
git commit -m "fix(catalog): pin bleve index to BM25 (was silently TF-IDF)"
```

---

### Task 2: Add the `CurationDedupScore` instrument

**Files:**
- Modify: `internal/telemetry/metrics.go` (struct `:15-33`; constructor `:50-68`)
- Test: `internal/telemetry/metrics_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/telemetry/metrics_test.go`:

```go
func TestNewMetricsCurationInstruments(_ *testing.T) {
	// With no provider configured the global meter is a no-op; the instrument must
	// still construct and be safe to record.
	m := NewMetrics()
	m.CurationDedupScore.Record(context.Background(), 4.2)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/telemetry/ -run TestNewMetricsCurationInstruments`
Expected: FAIL — compile error `m.CurationDedupScore undefined`.

- [ ] **Step 3: Add the instrument**

In `internal/telemetry/metrics.go`, add a field to the `Metrics` struct (after `RecallScore`, `:28`):

```go
	CurationDedupScore        metric.Float64Histogram // catalog top-hit score at the curation dedup decision
```

And add the constructor line in the returned struct literal (after the `RecallScore:` line, `:63`):

```go
		CurationDedupScore:        histF("curation_dedup_score", "catalog top-hit BM25 score at the curation dedup decision"),
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/telemetry/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/telemetry/metrics.go internal/telemetry/metrics_test.go
git commit -m "feat(telemetry): curation_dedup_score histogram"
```

---

### Task 3: Add `Novelty.TopHit` (score-surfacing accessor)

**Files:**
- Modify: `internal/curator/fingerprint.go` (`IsDuplicate:36-48`)
- Test: `internal/curator/fingerprint_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/curator/fingerprint_test.go`:

```go
func TestTopHitReturnsScore(t *testing.T) {
	// TopHit surfaces the top hit + score regardless of the DupScore threshold,
	// so the caller can both observe the score and apply the threshold itself.
	n := Novelty{Catalog: fakeScored{score: 2.0, title: "Below threshold"}, DupScore: 5.0}
	top, ok, err := n.TopHit(context.Background(), providers.Investigation{Title: "x"})
	if err != nil || !ok {
		t.Fatalf("want a hit, got ok=%v err=%v", ok, err)
	}
	if top.Score != 2.0 || top.Entry.Title != "Below threshold" {
		t.Fatalf("unexpected top hit %+v", top)
	}
}

func TestTopHitNilCatalog(t *testing.T) {
	_, ok, err := Novelty{Catalog: nil}.TopHit(context.Background(), providers.Investigation{Title: "x"})
	if ok || err != nil {
		t.Fatalf("nil catalog: want ok=false err=nil, got ok=%v err=%v", ok, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/curator/ -run TestTopHit`
Expected: FAIL — compile error `n.TopHit undefined`.

- [ ] **Step 3: Add `TopHit` and refactor `IsDuplicate` through it**

In `internal/curator/fingerprint.go`, replace the `IsDuplicate` method (`:34-48`) with:

```go
// TopHit returns the highest-scoring catalog entry for a finding's fingerprint.
// ok is false when no catalog is configured or there are no hits. It surfaces the
// score regardless of any threshold, so callers can both observe it and decide.
func (n Novelty) TopHit(ctx context.Context, inv providers.Investigation) (catalog.ScoredEntry, bool, error) { //nolint:revive // ctx kept for future remote-index symmetry
	if n.Catalog == nil {
		return catalog.ScoredEntry{}, false, nil
	}
	hits, err := n.Catalog.SearchScored(Fingerprint(inv), 1)
	if err != nil {
		return catalog.ScoredEntry{}, false, err
	}
	if len(hits) == 0 {
		return catalog.ScoredEntry{}, false, nil
	}
	return hits[0], true, nil
}

// IsDuplicate returns true + the matching entry when the top hit clears DupScore.
func (n Novelty) IsDuplicate(ctx context.Context, inv providers.Investigation) (bool, catalog.Entry, error) {
	top, ok, err := n.TopHit(ctx, inv)
	if err != nil || !ok {
		return false, catalog.Entry{}, err
	}
	if top.Score >= n.DupScore {
		return true, top.Entry, nil
	}
	return false, catalog.Entry{}, nil
}
```

- [ ] **Step 4: Run the curator tests to verify they pass**

Run: `go test ./internal/curator/`
Expected: PASS — the new `TestTopHit*` tests and all pre-existing `IsDuplicate`/`Novelty` tests (signature unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/curator/fingerprint.go internal/curator/fingerprint_test.go
git commit -m "refactor(curator): add Novelty.TopHit; IsDuplicate via it"
```

---

### Task 4: Curator records the dedup score on every `Curate`

**Files:**
- Modify: `internal/curator/curator.go` (imports `:8-16`; `Curator` struct `:21-27`; `Curate` dedup block `:39-45`)
- Test: `internal/curator/curator_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/curator/curator_test.go` (and add `"net/http"`, `"net/http/httptest"`, `"strings"`, and `"github.com/Smana/runlore/internal/telemetry"` to its import block):

```go
func TestCurateDedupScoreNilMetricsSafe(t *testing.T) {
	// newCurator leaves Metrics nil; recording the dedup score must not panic.
	c := newCurator(&fakeForge{}, fakeScored{score: 2.0, title: "Some entry"})
	if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
		t.Fatal(err)
	}
}

func TestCurateRecordsDedupScore(t *testing.T) {
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	c := newCurator(&fakeForge{}, fakeScored{score: 2.0, title: "Some entry"}) // below DupScore → records, then continues
	c.Metrics = telemetry.NewMetrics()
	if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "runlore_curation_dedup_score") {
		t.Fatalf("runlore_curation_dedup_score not found in metrics output:\n%s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/curator/ -run TestCurate.*DedupScore`
Expected: FAIL — compile error `c.Metrics undefined` (field not yet added).

- [ ] **Step 3: Add the `Metrics` field and record in `Curate`**

In `internal/curator/curator.go`, add `"github.com/Smana/runlore/internal/telemetry"` to the import block. Add a field to the `Curator` struct (after `Catalog`, `:23`):

```go
	Metrics       *telemetry.Metrics     // optional; nil-safe — dedup score unrecorded when unset
```

Replace the catalog-dedup block in `Curate` (`:39-45`, the `if dup, hit, err := (Novelty{...}).IsDuplicate(...)` block) with:

```go
	// 1. dedup — catalog (observe the top-hit score on every check), then open PRs
	n := Novelty{Catalog: c.Catalog, DupScore: c.DupScore}
	if top, ok, err := n.TopHit(ctx, inv); err != nil {
		c.Log.Warn("dedup: catalog search failed", "err", err)
	} else if ok {
		if c.Metrics != nil {
			c.Metrics.CurationDedupScore.Record(ctx, top.Score)
		}
		if top.Score >= c.DupScore {
			c.Log.Info("finding duplicates a catalog entry; not filing", "entry", top.Entry.Title, "score", top.Score)
			return providers.Ref{}, nil
		}
	}
```

- [ ] **Step 4: Run the curator tests to verify they pass**

Run: `go test ./internal/curator/`
Expected: PASS — the two new tests plus all pre-existing curator tests (the catalog-duplicate / catalog-error / drop-silently cases still behave identically).

- [ ] **Step 5: Commit**

```bash
git add internal/curator/curator.go internal/curator/curator_test.go
git commit -m "feat(curator): record dedup top-hit score on every Curate"
```

---

### Task 5: Wire `metrics` into the curator at build time

**Files:**
- Modify: `cmd/lore/main.go` (`buildCurator:698`, struct literal `:721`; call site `:1034`)

- [ ] **Step 1: Thread the metrics handle through `buildCurator`**

In `cmd/lore/main.go`, change the `buildCurator` signature (`:698`) to add a `metrics *telemetry.Metrics` parameter:

```go
func buildCurator(cfg *config.Config, token forgeToken, cat *catalog.Catalog, metrics *telemetry.Metrics, log *slog.Logger) *curator.Curator {
```

Update the struct literal (`:721`) to set it:

```go
	cur := &curator.Curator{Forge: client, DupScore: dup, MinConfidence: minConf, Metrics: metrics, Log: log}
```

Update the call site (`:1034`, inside `buildInvestigator`, where `metrics` is already in scope) to pass it:

```go
	cur := buildCurator(cfg, buildForgeTokenSource(cfg, log), cat, metrics, log)
```

(`telemetry` is already imported in `main.go`.)

- [ ] **Step 2: Build to verify the wiring compiles**

Run: `go build ./...`
Expected: success, no output. (No new unit test — this is pure wiring; the metric recording itself is covered by Task 4's tests.)

- [ ] **Step 3: Commit**

```bash
git add cmd/lore/main.go
git commit -m "feat(curator): pass metrics into the curator at build time"
```

---

### Task 6: Whole-tree verification

- [ ] **Step 1: Build, test, lint**

Run:
```bash
go build ./... && go test ./... && golangci-lint run
```
Expected: build clean; all tests PASS; lint clean.

- [ ] **Step 2: Confirm the scorer is genuinely active (manual sanity)**

Run: `go test ./internal/catalog/ -run 'TestNewIndexMappingUsesBM25|TestBuildIndexScores' -v`
Expected: both PASS — confirms BM25 is set on the shared mapping and scoring works end-to-end.

No commit (verification only).

---

## Notes for the implementer

- **Do not re-tune** `MinScore` / `SoloFloor` / `MarginGap` (`internal/config/load.go:64-71`) or `DupScore` (`cmd/lore/main.go:711-714`, default `5.0`). The scorer flip shifts score magnitudes; re-fitting these floors from the now-correct `recall_score` / `curation_dedup_score` distributions is the **next** slice, deliberately kept separate and reversible (spec §2 D2, §5).
- The `"bm25"` literal is validated by bleve against `index.SupportedScoringModels`; an unsupported value makes `NewMemOnly` error — which is exactly why `TestBuildIndexScores` doubles as a guard that the model is accepted.
- The dedup score is recorded on **every** `Curate` call that finds at least one hit (not only when it crosses `DupScore`), so the follow-up sees the full distribution to tune from.
