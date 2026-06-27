# Recall Trustworthiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make instant recall short-circuit a full investigation only when it's *trustworthy* — a BM25 *margin* over the runner-up + *structural agreement* on the resource — with a *derived* confidence (no more hardcoded `0.8`), and never re-curate a cache hit.

**Architecture:** All recall-side logic lives in `internal/investigate/recall.go` (the `lookup` gate + a derived-confidence formula). A new `Investigation.Recalled` flag lets the curator skip a recalled finding; a new `Investigation.Resource` carries the originating workload so curated entries store it (enabling structural agreement on future recalls). Reuses the existing `catalog.Entry.Resource` and `providers.KBEntry.Resource` fields (both already exist, just unused).

**Tech Stack:** Go, bleve (BM25), OpenTelemetry metrics, plain `testing`. Module `github.com/Smana/runlore`.

**Design spec:** `docs/superpowers/specs/2026-06-22-recall-trustworthiness-design.md`

**Conventions:** TDD red→green per task; before each commit `cd /home/smana/Sources/runlore && gofmt -w <files> && go vet ./... && go build ./... && golangci-lint run && <pkg tests>`. Plain `testing`/`t.Fatalf` (no testify). Conventional commits; **NO `Co-Authored-By` / attribution trailers**. Depends on PR #67's recall metrics (this branch is based on it).

---

## Task 1: Config knobs + `recall_rejections_total` metric

**Files:**
- Modify: `internal/config/config.go` (`InstantRecall` struct)
- Modify: `internal/telemetry/metrics.go`
- Test: `internal/config/config_test.go` (or the existing config test file)

- [ ] **Step 1: Write the failing test** (append to the config test file)

```go
func TestInstantRecallTrustConfig(t *testing.T) {
	const y = `
catalog:
  instant_recall:
    enabled: true
    min_score: 1.5
    margin_gap: 1.0
    solo_floor: 4.0
    require_workload_match: false
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ir := c.Catalog.InstantRecall
	if !ir.Enabled || ir.MinScore != 1.5 || ir.MarginGap != 1.0 || ir.SoloFloor != 4.0 || ir.RequireWorkloadMatch {
		t.Fatalf("instant_recall not parsed: %+v", ir)
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (`MarginGap`/`SoloFloor`/`RequireWorkloadMatch` undefined)

Run: `go test ./internal/config/ -run TestInstantRecallTrustConfig`

- [ ] **Step 3: Extend `InstantRecall`** in `internal/config/config.go`

```go
type InstantRecall struct {
	Enabled              bool    `yaml:"enabled"`
	MinScore             float64 `yaml:"min_score"`             // similarity floor for the top hit
	MarginGap            float64 `yaml:"margin_gap"`            // top hit must beat the runner-up by at least this
	SoloFloor            float64 `yaml:"solo_floor"`            // confident bar when there is only one hit (higher than MinScore)
	RequireWorkloadMatch bool    `yaml:"require_workload_match"` // true = exact namespace+workload; false = namespace-level agreement is enough
}
```

- [ ] **Step 4: Add the metric** in `internal/telemetry/metrics.go` — add the field to `Metrics` (after `RecallTokensSaved`):

```go
	RecallRejections metric.Int64Counter // recalls rejected before short-circuit (label: reason)
```

and in `NewMetrics`'s returned struct literal (after the `RecallTokensSaved` line):

```go
		RecallRejections:         ctr("recall_rejections_total", "recalls rejected before short-circuit (label: reason)"),
```

- [ ] **Step 5: Run + commit**

Run: `go test ./internal/config/ ./internal/telemetry/ && go build ./...`
```bash
gofmt -w internal/config/config.go internal/telemetry/metrics.go
git add internal/config/config.go internal/telemetry/metrics.go internal/config/*_test.go
git commit -m "feat(recall): config knobs (margin/solo_floor/structural) + recall_rejections metric"
```

---

## Task 2: Margin gate in `lookup`

Today `lookup` searches `k=1` and accepts any hit ≥ `MinScore`. Make it search `k=2` and only accept an *unambiguous* winner.

**Files:**
- Modify: `internal/investigate/recall.go` (`Recall` struct + `lookup`)
- Test: `internal/investigate/recall_test.go`

- [ ] **Step 1: Write failing tests**

```go
func recallWith(hits []catalog.ScoredEntry) *Recall {
	return &Recall{Catalog: fakeScored{hits: hits}, MinScore: 1.5, MarginGap: 1.0, SoloFloor: 4.0}
}

func TestLookupMarginClearWinner(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "BadImage", Path: "b.md"}, Score: 2.0},
	})
	if e, _ := r.lookup(context.Background(), Request{Title: "pod crashloop"}); e == nil {
		t.Fatal("clear winner (gap 4.0 ≥ 1.0) should recall")
	}
}

func TestLookupMarginNearTieFallsThrough(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "BadImage", Path: "b.md"}, Score: 5.5},
	})
	if e, _ := r.lookup(context.Background(), Request{Title: "pod crashloop"}); e != nil {
		t.Fatal("near-tie (gap 0.5 < 1.0) must fall through")
	}
}

func TestLookupLoneWeakHitFallsThrough(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md"}, Score: 3.0}, // below SoloFloor 4.0
	})
	if e, _ := r.lookup(context.Background(), Request{Title: "pod crashloop"}); e != nil {
		t.Fatal("a lone hit below solo_floor must fall through")
	}
}
```

> These tests set no `Resource` on the entries and leave `Request.Workload` zero; **Task 3 adds the structural gate** which would otherwise reject them — so write Task 2's impl to *pass the margin gate and (for now) return the entry without a structural check*. Task 3's tests will then drive the structural gate, and these three tests get a `Resource`+`Workload` added in Task 3 Step 3 so they keep passing. (If you prefer, give these entries `Resource: "ns"` and `Request{Workload: providers.Workload{Namespace: "ns"}}` now to be forward-compatible.)

- [ ] **Step 2: Run — expect FAIL** (`MarginGap`/`SoloFloor` fields undefined on `Recall`)

Run: `go test ./internal/investigate/ -run TestLookupMargin`

- [ ] **Step 3: Add fields + margin logic** in `recall.go`

Add to the `Recall` struct (after `MinScore float64`):
```go
	MarginGap            float64 // top hit must beat the runner-up by at least this
	SoloFloor            float64 // confident bar when there is only one hit
	RequireWorkloadMatch bool    // structural strictness (used in Task 3)
```

Rewrite the tail of `lookup` (replace the `if score < r.MinScore { return nil, 0 }` block onward). Bump the search to `k=2`:
```go
	hits, err := r.Catalog.SearchScored(strings.TrimSpace(req.Title+" "+req.Message), 2)
	if err != nil || len(hits) == 0 {
		return nil, 0
	}
	score := hits[0].Score
	if r.Metrics != nil {
		r.Metrics.RecallScore.Record(ctx, score)
	}
	margin := score
	confident := score >= r.SoloFloor
	if len(hits) > 1 {
		margin = score - hits[1].Score
		confident = score >= r.MinScore && margin >= r.MarginGap
	}
	if !confident {
		r.reject(ctx, "low_margin")
		return nil, 0
	}
	e := hits[0].Entry
	return &e, score
```

Add a nil-safe reject helper:
```go
func (r *Recall) reject(ctx context.Context, reason string) {
	if r.Metrics != nil {
		r.Metrics.RecallRejections.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}
```
(Imports: `go.opentelemetry.io/otel/metric`, `go.opentelemetry.io/otel/attribute` — already used in `loop.go`.)

- [ ] **Step 4: Run — expect PASS**; **Step 5: Commit**

```bash
gofmt -w internal/investigate/recall.go internal/investigate/recall_test.go
go vet ./... && go build ./... && golangci-lint run ./internal/investigate/...
git add internal/investigate/recall.go internal/investigate/recall_test.go
git commit -m "feat(recall): require a BM25 margin over the runner-up (corpus-portable, not absolute MinScore)"
```

---

## Task 3: Structural agreement gate

A confident recall also requires the entry's stored `Resource` to agree with the incoming alert's workload.

**Files:**
- Modify: `internal/investigate/recall.go`
- Test: `internal/investigate/recall_test.go`

- [ ] **Step 1: Write failing tests**

```go
func structuralRecall(entryResource string, requireWorkload bool) *Recall {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: entryResource}, Score: 6.0},
		{Entry: catalog.Entry{Title: "X", Path: "b.md"}, Score: 2.0},
	})
	r.RequireWorkloadMatch = requireWorkload
	return r
}

func TestLookupStructuralMatch(t *testing.T) {
	r := structuralRecall("apps/web", false)
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	if e, _ := r.lookup(context.Background(), req); e == nil {
		t.Fatal("exact resource match should recall")
	}
}

func TestLookupStructuralNamespaceOnly(t *testing.T) {
	r := structuralRecall("apps/web", false)
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "apps"}} // name unknown from the alert
	if e, _ := r.lookup(context.Background(), req); e == nil {
		t.Fatal("namespace agreement should recall when require_workload_match=false")
	}
}

func TestLookupStructuralMismatchFallsThrough(t *testing.T) {
	r := structuralRecall("apps/web", false)
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "kube-system"}}
	if e, _ := r.lookup(context.Background(), req); e != nil {
		t.Fatal("different namespace must fall through")
	}
}

func TestLookupNoStoredResourceFallsThrough(t *testing.T) {
	r := structuralRecall("", false) // entry predates the write-side change
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "apps"}}
	if e, _ := r.lookup(context.Background(), req); e != nil {
		t.Fatal("entry with no stored resource must fall through (fail-safe)")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (the lookup doesn't yet check resource)

Run: `go test ./internal/investigate/ -run TestLookupStructural`

- [ ] **Step 3: Implement the gate + helpers** in `recall.go`

```go
type matchStrength int

const (
	matchNone matchStrength = iota
	matchNamespace
	matchExact
)

// canonicalResource renders a workload as "namespace/name", or just "namespace"
// when the name is unknown (common for alert-triggered investigations).
func canonicalResource(w providers.Workload) string {
	if w.Namespace == "" {
		return ""
	}
	if w.Name == "" {
		return w.Namespace
	}
	return w.Namespace + "/" + w.Name
}

// resourceAgrees reports how strongly the alert's workload agrees with an entry's
// stored resource. requireWorkload demands an exact namespace+name match.
func resourceAgrees(reqW providers.Workload, entryResource string, requireWorkload bool) matchStrength {
	if entryResource == "" || reqW.Namespace == "" {
		return matchNone
	}
	if canonicalResource(reqW) == entryResource {
		return matchExact
	}
	if requireWorkload {
		return matchNone
	}
	if entryResource == reqW.Namespace || strings.HasPrefix(entryResource, reqW.Namespace+"/") {
		return matchNamespace
	}
	return matchNone
}
```

In `lookup`, after `e := hits[0].Entry` and before returning, add:
```go
	if resourceAgrees(req.Workload, e.Resource, r.RequireWorkloadMatch) == matchNone {
		r.reject(ctx, "no_resource_match")
		return nil, 0
	}
	return &e, score
```

Also update the Task-2 tests' entries to set `Resource: "ns"` and `Request{Workload: providers.Workload{Namespace: "ns"}}` so they still pass (or confirm you did that in Task 2 Step 1).

- [ ] **Step 4: Run — expect PASS** (`go test ./internal/investigate/ -run TestLookup`); **Step 5: Commit**

```bash
gofmt -w internal/investigate/recall.go internal/investigate/recall_test.go
go vet ./... && go build ./... && golangci-lint run ./internal/investigate/...
git add internal/investigate/recall.go internal/investigate/recall_test.go
git commit -m "feat(recall): structural-agreement gate (alert workload must match the entry's resource)"
```

---

## Task 4: Derived recall confidence (replace the hardcoded `0.8`)

**Files:**
- Modify: `internal/investigate/recall.go` (`lookup` returns derived confidence; `recalledInvestigation` takes it)
- Modify: `internal/investigate/loop.go` (pass the confidence through)
- Test: `internal/investigate/recall_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestDeriveRecallConfidence(t *testing.T) {
	// decisive winner, exact match → high but capped < 1
	hi := deriveRecallConfidence(8.0, 6.0, matchExact)
	if hi <= 0.8 || hi > 0.9 {
		t.Fatalf("decisive+exact should be (0.8, 0.9], got %v", hi)
	}
	// marginal winner, namespace-only → lower
	lo := deriveRecallConfidence(2.0, 0.4, matchNamespace)
	if lo >= hi || lo < 0.5 {
		t.Fatalf("marginal+namespace should be lower and ≥ 0.5, got %v (hi=%v)", lo, hi)
	}
}

func TestRecalledInvestigationUsesDerivedConfidence(t *testing.T) {
	inv := recalledInvestigation(Request{Title: "x"}, catalog.Entry{Title: "T", Description: "D", Path: "p.md"}, 0.72)
	if inv.Confidence != 0.72 || inv.RootCauses[0].Confidence != 0.72 {
		t.Fatalf("recalledInvestigation must use the derived confidence, got %+v", inv)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`deriveRecallConfidence` undefined; `recalledInvestigation` arity)

Run: `go test ./internal/investigate/ -run 'TestDeriveRecallConfidence|TestRecalledInvestigationUses'`

- [ ] **Step 3: Implement** in `recall.go`

```go
func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// deriveRecallConfidence turns the match signals into an explainable confidence,
// capped below 1.0 — a cache hit never asserts certainty. (Constants are the
// shape; tune via recall_score / recall_rejections.)
func deriveRecallConfidence(score, margin float64, strength matchStrength) float64 {
	base := 0.55
	if score > 0 {
		base = 0.55 + 0.30*clampF(margin/score, 0, 1) // decisive winner → up to 0.85
	}
	if strength == matchExact {
		base += 0.05
	}
	return clampF(base, 0.50, 0.90)
}
```

Change `recalledInvestigation` signature + the two `0.8` literals:
```go
func recalledInvestigation(req Request, e catalog.Entry, confidence float64) providers.Investigation {
	rc := providers.Hypothesis{
		Summary:    e.Title + " — " + e.Description,
		Confidence: confidence,
		Evidence:   []string{fmt.Sprintf("instant recall: matched knowledge-base entry %q", e.Path)},
	}
	return providers.Investigation{
		Title:      req.Title,
		Confidence: confidence,
		RootCauses: []providers.Hypothesis{rc},
		Unresolved: []string{"recalled from the catalog without a fresh investigation — confirm it still applies"},
	}
}
```

In `lookup`, return the derived confidence instead of the raw score. Capture `strength` from the structural check and compute confidence at the end:
```go
	strength := resourceAgrees(req.Workload, e.Resource, r.RequireWorkloadMatch)
	if strength == matchNone {
		r.reject(ctx, "no_resource_match")
		return nil, 0
	}
	return &e, deriveRecallConfidence(score, margin, strength)
```

In `loop.go`, the recall short-circuit currently does `rec := recalledInvestigation(req, *entry)` after `entry, score := li.Recall.lookup(...)`. Update both: rename `score` → `conf` and pass it:
```go
	if entry, conf := li.Recall.lookup(ctx, req); entry != nil {
		li.Log.Info("instant recall (catalog hit; skipping the loop)",
			"title", req.Title, "entry", entry.Path, "confidence", fmt.Sprintf("%.2f", conf))
		rec := recalledInvestigation(req, *entry, conf)
		...
```

- [ ] **Step 4: Run — expect PASS** (`go test ./internal/investigate/`); **Step 5: Commit**

```bash
gofmt -w internal/investigate/recall.go internal/investigate/loop.go internal/investigate/recall_test.go
go vet ./... && go build ./... && golangci-lint run ./internal/investigate/...
git add internal/investigate/recall.go internal/investigate/loop.go internal/investigate/recall_test.go
git commit -m "feat(recall): derive recall confidence from match signals (replaces hardcoded 0.8)"
```

---

## Task 5: Don't curate recalled findings

A recall matched an existing entry → not novel. Flag the investigation and have the curator skip it (chat delivery is unaffected).

**Files:**
- Modify: `internal/providers/providers.go` (`Investigation.Recalled`)
- Modify: `internal/investigate/recall.go` (`recalledInvestigation` sets it)
- Modify: `internal/curator/curator.go` (`Curate` early-returns when `Recalled`)
- Test: `internal/curator/curator_test.go`

- [ ] **Step 1: Write the failing test** (reuse the package's existing curator test harness for building a `Curator` + a fake forge — grep `func TestCurate` in `internal/curator/curator_test.go`)

```go
func TestCurateSkipsRecalled(t *testing.T) {
	c := newTestCurator(t) // existing helper; fake forge that records drafted PRs
	inv := providers.Investigation{
		Title: "Known incident", Confidence: 0.8, Recalled: true,
		RootCauses: []providers.Hypothesis{{Summary: "x", Confidence: 0.8}},
	}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if c.forge.drafted != 0 { // adjust to the harness's recorder
		t.Fatalf("a recalled finding must not be curated, drafted=%d", c.forge.drafted)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`Recalled` undefined)

Run: `go test ./internal/curator/ -run TestCurateSkipsRecalled`

- [ ] **Step 3: Implement**

In `internal/providers/providers.go`, add to `Investigation` (after `Confidence float64`):
```go
	Recalled bool // true when produced by instant recall (a KB cache hit); the curator skips re-curating it
```

In `internal/investigate/recall.go`, set it in `recalledInvestigation`'s returned `Investigation`:
```go
		Recalled:   true,
```

In `internal/curator/curator.go`, at the top of `Curate` (after the existing early guards):
```go
	if inv.Recalled {
		c.Log.Info("skipping curation of a recalled finding (cache hit, not novel)", "title", inv.Title)
		return providers.Ref{}, nil
	}
```

- [ ] **Step 4: Run — expect PASS**; **Step 5: Commit**

```bash
gofmt -w internal/providers/providers.go internal/investigate/recall.go internal/curator/curator.go internal/curator/curator_test.go
go vet ./... && go build ./... && golangci-lint run
git add internal/providers/providers.go internal/investigate/recall.go internal/curator/curator.go internal/curator/curator_test.go
git commit -m "feat(curator): skip curation of recalled findings (a cache hit is not novel)"
```

---

## Task 6: Write-side — entries store their originating resource

So future recalls have a `Resource` to agree with. Thread the workload onto the `Investigation`, set it at investigation build time, and write it into the curated entry's frontmatter.

**Files:**
- Modify: `internal/providers/providers.go` (`Investigation.Resource`)
- Modify: `internal/investigate/loop.go` (set `inv.Resource` from `req.Workload`)
- Modify: `internal/curator/draft.go` (`draftKBEntry` sets `KBEntry.Resource`)
- Test: `internal/curator/draft_test.go` (create or extend)

- [ ] **Step 1: Write the failing test**

```go
func TestDraftKBEntrySetsResource(t *testing.T) {
	inv := providers.Investigation{
		Title: "Harbor down", Confidence: 0.9,
		Resource:   providers.Workload{Namespace: "tooling", Name: "harbor-core"},
		RootCauses: []providers.Hypothesis{{Summary: "valkey down", Confidence: 0.9}},
	}
	e := draftKBEntry(inv)
	if e.Resource != "tooling/harbor-core" {
		t.Fatalf("KBEntry.Resource = %q, want tooling/harbor-core", e.Resource)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`Investigation.Resource` undefined; `KBEntry.Resource` empty)

Run: `go test ./internal/curator/ -run TestDraftKBEntrySetsResource`

- [ ] **Step 3: Implement**

In `providers.go`, add to `Investigation` (after `Recalled bool`):
```go
	Resource Workload // the originating workload, stored on curated entries for structural recall matching
```

In `internal/curator/draft.go`, set `Resource` in the returned `KBEntry` (it already has the field). Reuse the canonical form — add a small exported helper to `providers` so both curator and recall share it, OR inline `namespace/name` here:
```go
		Resource: resourceString(inv.Resource),
```
and add to `draft.go`:
```go
func resourceString(w providers.Workload) string {
	if w.Namespace == "" {
		return ""
	}
	if w.Name == "" {
		return w.Namespace
	}
	return w.Namespace + "/" + w.Name
}
```
(This mirrors `canonicalResource` in `recall.go`; if you prefer DRY, move one copy to `providers` as `Workload.Resource()` and call it from both — a clean small refactor, but two private copies is acceptable since they're in different packages.)

In `internal/investigate/loop.go`, set the resource on the investigation the model produces (the `submit_findings` success path, near `inv.Title = req.Title`):
```go
	inv.Resource = req.Workload
```

- [ ] **Step 4: Frontmatter serialization** — confirm the KBEntry → markdown writer emits `resource:`. Grep for where `KBEntry` becomes file content (`grep -rn "Type:" internal/curator internal/forge` or the function that writes the PR file body). The `catalog.Entry` parser already reads `resource:` frontmatter, so the writer must emit it:
```yaml
resource: {{ .Resource }}
```
Add the `resource:` line to that template/writer if absent (omit the line when `Resource == ""`). Add/extend a round-trip test if the writer has one.

- [ ] **Step 5: Run + commit**

Run: `go test ./internal/curator/ ./internal/catalog/ && go build ./...`
```bash
gofmt -w internal/providers/providers.go internal/curator/draft.go internal/investigate/loop.go internal/curator/draft_test.go
go vet ./... && golangci-lint run
git add internal/providers/providers.go internal/curator/draft.go internal/investigate/loop.go internal/curator/draft_test.go
git commit -m "feat(curator): store the originating resource on curated entries (enables structural recall)"
```

---

## Task 7: Wire the new config into the Recall constructor + skip-curate verification

**Files:**
- Modify: `cmd/lore/main.go` (where `&Recall{...}` / `Recall` is built from config)
- Test: `internal/investigate/loop_test.go` (recall path doesn't curate)

- [ ] **Step 1: Wire config** — locate where `Recall` is constructed in `cmd/lore/main.go` (grep `Recall{` or `MinScore`) and pass the new knobs:
```go
	&investigate.Recall{
		Catalog:              cat,
		MinScore:             cfg.Catalog.InstantRecall.MinScore,
		MarginGap:            cfg.Catalog.InstantRecall.MarginGap,
		SoloFloor:            cfg.Catalog.InstantRecall.SoloFloor,
		RequireWorkloadMatch: cfg.Catalog.InstantRecall.RequireWorkloadMatch,
		Metrics:              metrics,
		Log:                  log,
	}
```

- [ ] **Step 2: Loop-level test** — assert the recall short-circuit produces a `Recalled` investigation (so the curator skips it). Reuse the `fakeScored` + a stub model from `loop_test.go`:

```go
func TestRecallShortCircuitMarksRecalled(t *testing.T) {
	var delivered providers.Investigation
	li := &LoopInvestigator{
		Recall: &Recall{
			Catalog:  fakeScored{hits: []catalog.ScoredEntry{{Entry: catalog.Entry{Title: "Known", Path: "k.md", Resource: "apps/web"}, Score: 9.0}}},
			MinScore: 1.5, MarginGap: 1.0, SoloFloor: 4.0,
		},
		OnComplete: func(inv providers.Investigation) { delivered = inv },
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := li.Investigate(context.Background(), Request{Title: "alert", Workload: providers.Workload{Namespace: "apps", Name: "web"}})
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if !delivered.Recalled {
		t.Fatal("a recalled investigation must be flagged Recalled so the curator skips it")
	}
}
```

- [ ] **Step 3: Run — expect PASS** (`go test ./internal/investigate/ -run TestRecallShortCircuit`)

- [ ] **Step 4: Commit**

```bash
gofmt -w cmd/lore/main.go internal/investigate/loop_test.go
go vet ./... && go build ./... && golangci-lint run
git add cmd/lore/main.go internal/investigate/loop_test.go
git commit -m "feat(recall): wire margin/structural config; verify recall flags Recalled"
```

---

## Task 8: Helm values + docs

**Files:**
- Modify: `deploy/helm/runlore/values.yaml`

- [ ] **Step 1: Expose the knobs** under the existing `config.catalog.instant_recall` block:
```yaml
    instant_recall:
      enabled: true
      min_score: 1.5
      margin_gap: 1.0
      solo_floor: 4.0
      require_workload_match: false
```

- [ ] **Step 2: Render-check + commit**

Run: `helm template deploy/helm/runlore | grep -A6 instant_recall && helm lint deploy/helm/runlore`
```bash
git add deploy/helm/runlore/values.yaml
git commit -m "feat(helm): expose instant-recall trust knobs"
```

---

## Final verification

- [ ] `gofmt -l ./internal/... ./cmd/...` → no output
- [ ] `go vet ./...` → clean
- [ ] `go build ./...` → clean
- [ ] `golangci-lint run` → 0 issues
- [ ] `go test ./...` → all green
- [ ] Manual sanity: a near-tie or namespace-mismatch alert falls through to a full investigation; an exact, decisive match recalls with a derived (not 0.8) confidence and does **not** open a KB PR.

---

## Notes for the implementer

- **`canonicalResource` (recall.go) and `resourceString` (draft.go) must agree** — same `namespace/name` format — or structural matching silently breaks. If you DRY them into one `providers.Workload.Resource()` method, call it from both.
- **`Request.Workload` is often namespace-only** for alert-triggered investigations (`FromIncident` sets only `Namespace`). That's why `require_workload_match` defaults `false` and namespace-level agreement is accepted — exact matches mostly come from workload labels when present.
- **Pre-existing entries have empty `Resource`** → they fall through (fail-safe) until re-curated with Task 6's write-side. This is intended.
