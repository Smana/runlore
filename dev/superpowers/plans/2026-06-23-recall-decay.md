# Outcome-Driven Recall Decay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bias instant-recall confidence by each catalog entry's resolution track record and reject (re-investigate) entries that recall-but-never-resolve — the "learns, not accumulates" edge.

**Architecture:** A nil-safe `OutcomeStats` interface (satisfied by `*outcome.Ledger`) is wired onto `Recall` in the serve path. In `lookup`, after the structural gate, an optimistic Beta factor `(resolved+k)/(recalls+k)` multiplies the derived confidence; below a configurable floor the recall is rejected (fail-safe fall-through to a full investigation). Per-decision compute; A1 recording unchanged.

**Tech Stack:** Go 1.26, stdlib `testing` (no testify). Tests follow `internal/investigate/recall_test.go` (fakes, `telemetry.Setup` + Prometheus scrape) and `internal/config/config_investigation_test.go` (`applyDefaults`).

**Spec:** `dev/superpowers/specs/2026-06-23-recall-decay-design.md`

**Branch:** `feat/recall-decay` (already checked out; the spec is already committed there).

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/config/config.go` | config schema | Add `OutcomePrior`/`OutcomeFloor` to `InstantRecall` |
| `internal/config/load.go` | defaults | Default them (2.0 / 0.5) in `applyDefaults` |
| `internal/config/config_investigation_test.go` | config tests | Assert defaults + explicit-respect |
| `internal/investigate/recall.go` | recall gate | `outcomeFactor`; `OutcomeStats` interface; `Outcome`/`OutcomePrior`/`OutcomeFloor` fields; decay/gate block in `lookup`; import `outcome` |
| `internal/investigate/recall_test.go` | recall tests | `outcomeFactor` unit + `lookup` decay scenarios + rejection metric |
| `cmd/lore/main.go` | wiring | Set decay config on the built `Recall`; set `recall.Outcome = ledger` |

Order: T1 (config) and T2 (`outcomeFactor`) are independent. T3 (recall fields + lookup) needs T2. T4 (wiring) needs T1 + T3. T5 verifies.

---

### Task 1: Config knobs `outcome_prior` / `outcome_floor`

**Files:**
- Modify: `internal/config/config.go` (`InstantRecall` struct, after `RequireWorkloadMatch` at `:127`)
- Modify: `internal/config/load.go` (`applyDefaults`, inside the `if c.Catalog.InstantRecall.Enabled` block)
- Test: `internal/config/config_investigation_test.go`

- [ ] **Step 1: Write the failing tests**

In `internal/config/config_investigation_test.go`, replace `TestApplyDefaultsInstantRecall` (currently asserts only MinScore/MarginGap/SoloFloor) with the version below, and add the explicit-respect test after it:

```go
func TestApplyDefaultsInstantRecall(t *testing.T) {
	// enabled with no tuning → margin/solo gates and decay knobs default to active values.
	var c Config
	c.Catalog.InstantRecall.Enabled = true
	applyDefaults(&c)
	ir := c.Catalog.InstantRecall
	if ir.MinScore != 1.0 || ir.MarginGap != 1.0 || ir.SoloFloor != 4.0 {
		t.Fatalf("instant-recall defaults not applied: %+v", ir)
	}
	if ir.OutcomePrior != 2.0 || ir.OutcomeFloor != 0.5 {
		t.Fatalf("recall-decay defaults not applied: %+v", ir)
	}
}

func TestApplyDefaultsRecallDecayExplicit(t *testing.T) {
	// Explicit decay knobs must not be overwritten.
	var c Config
	c.Catalog.InstantRecall.Enabled = true
	c.Catalog.InstantRecall.OutcomePrior = 5.0
	c.Catalog.InstantRecall.OutcomeFloor = 0.3
	applyDefaults(&c)
	ir := c.Catalog.InstantRecall
	if ir.OutcomePrior != 5.0 || ir.OutcomeFloor != 0.3 {
		t.Fatalf("explicit recall-decay values overwritten: %+v", ir)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestApplyDefaultsInstantRecall|TestApplyDefaultsRecallDecayExplicit'`
Expected: FAIL — compile error `ir.OutcomePrior undefined` / `ir.OutcomeFloor undefined`.

- [ ] **Step 3: Add the fields and defaults**

In `internal/config/config.go`, add to the `InstantRecall` struct immediately after the `RequireWorkloadMatch` field (`:127`):

```go
	OutcomePrior         float64 `yaml:"outcome_prior"`          // k — Beta prior strength for outcome decay (default 2.0)
	OutcomeFloor         float64 `yaml:"outcome_floor"`          // reject a recall when the outcome factor drops below this (default 0.5)
```

In `internal/config/load.go`, inside the existing `if c.Catalog.InstantRecall.Enabled { ir := &c.Catalog.InstantRecall; ... }` block, after the `SoloFloor` default, add:

```go
		if ir.OutcomePrior == 0 {
			ir.OutcomePrior = 2.0
		}
		if ir.OutcomeFloor == 0 {
			ir.OutcomeFloor = 0.5
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/`
Expected: PASS (the two tests above + all pre-existing config tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/load.go internal/config/config_investigation_test.go
git commit -m "feat(config): instant_recall outcome_prior/outcome_floor knobs"
```

---

### Task 2: `outcomeFactor` (the decay function)

**Files:**
- Modify: `internal/investigate/recall.go` (add the function near `deriveRecallConfidence`)
- Test: `internal/investigate/recall_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/investigate/recall_test.go`, add `"math"` to the import block, and add:

```go
func TestOutcomeFactor(t *testing.T) {
	const k = 2.0
	cases := []struct {
		recalls, resolved int
		want              float64
	}{
		{0, 0, 1.0},  // no history → no penalty
		{5, 5, 1.0},  // always resolves → no penalty
		{3, 0, 0.4},  // (0+2)/(3+2)
		{6, 0, 0.25}, // (0+2)/(6+2)
		{4, 2, 0.6},  // (2+2)/(4+2)
	}
	for _, c := range cases {
		got := outcomeFactor(c.recalls, c.resolved, k)
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("outcomeFactor(%d,%d,%v) = %v, want %v", c.recalls, c.resolved, k, got, c.want)
		}
		if got > 1.0 {
			t.Fatalf("factor must be <= 1.0, got %v", got)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestOutcomeFactor`
Expected: FAIL — compile error `undefined: outcomeFactor`.

- [ ] **Step 3: Implement `outcomeFactor`**

In `internal/investigate/recall.go`, add immediately before `deriveRecallConfidence`:

```go
// outcomeFactor decays a recall's confidence by its track record using an
// optimistic Beta prior: an entry with no history (or that always resolves)
// scores 1.0; one that recalls-but-never-resolves decays toward 0. k is the
// prior strength. Always in (0, 1] since resolved ≤ recalls.
func outcomeFactor(recalls, resolved int, k float64) float64 {
	return (float64(resolved) + k) / (float64(recalls) + k)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/investigate/ -run TestOutcomeFactor`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/recall.go internal/investigate/recall_test.go
git commit -m "feat(recall): outcomeFactor optimistic decay function"
```

---

### Task 3: Wire decay into `lookup` (`OutcomeStats` + fields + gate)

**Files:**
- Modify: `internal/investigate/recall.go` (imports; `Recall` struct `:23-32`; `lookup` tail `:74-79`)
- Test: `internal/investigate/recall_test.go`

- [ ] **Step 1: Write the failing tests**

In `internal/investigate/recall_test.go`, add `"errors"` and `"github.com/Smana/runlore/internal/outcome"` to the import block, plus (for the metric test) `"go.opentelemetry.io/otel"` and `"go.opentelemetry.io/otel/metric/noop"`. Then add:

```go
type fakeOutcome struct {
	counts map[string]outcome.Aggregate
	err    error
}

func (f fakeOutcome) OpenCounts() (map[string]outcome.Aggregate, error) { return f.counts, f.err }

// soloRecall builds a Recall over a single strong hit that clears the margin +
// solo gates for an apps/web workload, with decay configured (k=2, floor=0.5).
func soloRecall(oc OutcomeStats) *Recall {
	return &Recall{
		Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Path: "x.md", Resource: "apps/web"}, Score: 6.0},
		}},
		MinScore: 1.5, SoloFloor: 4.0, MarginGap: 1.0,
		Outcome: oc, OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
}

func TestLookupDecayHealthyEntryRecalls(t *testing.T) {
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 5, Resolved: 5}}})
	e, conf := r.lookup(context.Background(), okReq())
	if e == nil {
		t.Fatal("healthy entry (factor 1.0) should recall")
	}
	if conf < 0.80 {
		t.Fatalf("healthy entry confidence should be ~undecayed, got %v", conf)
	}
}

func TestLookupDecayStaleEntryRejected(t *testing.T) {
	// recalls=4 resolved=0 → factor (0+2)/(4+2)=0.333 < floor 0.5 → reject, fall through.
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 4, Resolved: 0}}})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("stale never-resolving entry must be rejected (fall through to investigation)")
	}
}

func TestLookupDecayNoHistoryRecalls(t *testing.T) {
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{}}) // entry absent from counts
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("an entry with no outcome history must not be penalized")
	}
}

func TestLookupDecayNilOutcomeRecalls(t *testing.T) {
	r := soloRecall(nil) // no outcome stats wired
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("nil Outcome must behave as before (no decay)")
	}
}

func TestLookupDecayStatsErrorRecalls(t *testing.T) {
	r := soloRecall(fakeOutcome{err: errors.New("ledger unavailable")})
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("an outcome-stats error must degrade to a normal recall (skip decay)")
	}
}

func TestLookupDecayRejectionMetric(t *testing.T) {
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 4, Resolved: 0}}})
	r.Metrics = telemetry.NewMetrics()
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("stale entry should be rejected")
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `reason="low_outcome"`) {
		t.Fatalf("expected recall_rejections_total{reason=\"low_outcome\"} in metrics:\n%s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/investigate/ -run TestLookupDecay`
Expected: FAIL — compile error (`Recall` has no `Outcome`/`OutcomePrior`/`OutcomeFloor` field; `OutcomeStats` undefined).

- [ ] **Step 3: Add the interface, fields, and decay block**

In `internal/investigate/recall.go`:

(a) Add `"github.com/Smana/runlore/internal/outcome"` to the import block.

(b) Add the interface just above the `Recall` struct:

```go
// OutcomeStats reports per-entry recall outcomes for confidence decay.
// *outcome.Ledger satisfies it.
type OutcomeStats interface {
	OpenCounts() (map[string]outcome.Aggregate, error)
}
```

(c) Add three fields to the `Recall` struct, immediately after `RequireWorkloadMatch`:

```go
	Outcome      OutcomeStats // optional; nil ⇒ no outcome decay
	OutcomePrior float64      // k — Beta prior strength for decay (e.g. 2.0)
	OutcomeFloor float64      // reject the recall when the outcome factor drops below this (e.g. 0.5)
```

(d) In `lookup`, replace the tail (the `conf := deriveRecallConfidence(...)` block through `return &e, conf`) with:

```go
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/investigate/`
Expected: PASS — the new decay tests plus all pre-existing recall/loop tests (decay is additive; existing tests leave `Outcome` nil).

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/recall.go internal/investigate/recall_test.go
git commit -m "feat(recall): outcome-driven confidence decay + low_outcome gate"
```

---

### Task 4: Wire decay config + ledger into the serve path

**Files:**
- Modify: `cmd/lore/main.go` (`buildModelAndTools` recall literal `:890-895`; `buildInvestigator` `recall != nil` block `:1027-1030`)

- [ ] **Step 1: Set the decay knobs on the built `Recall`**

In `cmd/lore/main.go`, in `buildModelAndTools`, extend the `recall = &investigate.Recall{...}` literal to pass the two config knobs:

```go
			recall = &investigate.Recall{
				Catalog:              cat,
				MinScore:             cfg.Catalog.InstantRecall.MinScore,
				MarginGap:            cfg.Catalog.InstantRecall.MarginGap,
				SoloFloor:            cfg.Catalog.InstantRecall.SoloFloor,
				RequireWorkloadMatch: cfg.Catalog.InstantRecall.RequireWorkloadMatch,
				OutcomePrior:         cfg.Catalog.InstantRecall.OutcomePrior,
				OutcomeFloor:         cfg.Catalog.InstantRecall.OutcomeFloor,
			}
```

- [ ] **Step 2: Set `recall.Outcome = ledger` in `buildInvestigator`**

In `cmd/lore/main.go`, in `buildInvestigator`, extend the existing `if recall != nil { ... }` block (which already sets `Metrics` and `Log`) to also wire the ledger:

```go
	if recall != nil {
		recall.Metrics = metrics
		recall.Log = log
		recall.Outcome = ledger // outcome-driven decay (serve path); *outcome.Ledger satisfies OutcomeStats
	}
```

(`ledger` is a `*outcome.Ledger` parameter of `buildInvestigator`; it is always non-nil — a disabled ledger yields empty `OpenCounts`, so decay is simply a no-op until the ledger is enabled and has data. The eval/reinvestigate/chat builders do not set `Outcome`, so they keep current behavior.)

- [ ] **Step 3: Build to verify the wiring compiles**

Run: `go build ./...`
Expected: success, no output. (No new unit test — pure wiring; decay behavior is covered by Task 3.)

- [ ] **Step 4: Commit**

```bash
git add cmd/lore/main.go
git commit -m "feat(recall): wire outcome decay (config + ledger) into the serve path"
```

---

### Task 5: Whole-tree verification

- [ ] **Step 1: Build, test, vet**

Run:
```bash
go build ./... && go test ./... && go vet ./...
```
Expected: build clean; all tests PASS; vet clean.

No commit (verification only).

---

## Notes for the implementer

- **The factor only ever decays** (`resolved ≤ recalls` ⇒ factor `≤ 1.0`); it never inflates confidence. The lower clamp is `0` (not the 0.50 match-floor) on purpose — a distrusted entry may be reported well below the match-confidence floor; the *gate* (`f < OutcomeFloor`), not the floor, is the guard that forces re-investigation.
- **Do not** add caching, decay on the reinvestigate/chat paths, frontmatter surfacing, or any change to A1 recording — all deferred (spec §7).
- `reject(ctx, "low_outcome")` reuses the existing nil-safe rejection path; no new metric instrument is needed (`recall_rejections_total{reason}` already exists).
- A disabled ledger (`outcome.ledger_path` empty) makes decay a transparent no-op — there is no behavior change for users who haven't enabled the outcome ledger.
