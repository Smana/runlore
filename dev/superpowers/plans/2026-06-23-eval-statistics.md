# Eval Statistics — k-of-n Gate + Flaky-fail + N — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a live-eval "pass" a statistically meaningful measurement: N≥10, a k-of-n pass rule, a flaky-variance fail, and N stated in the report.

**Architecture:** `internal/eval/live.go` gains constants + a `Flaky` field and a rewritten gate (k-of-n root-cause pass rate + variance flag); `internal/eval/report.go` carries `N` and renders a `FLAKY` status; `cmd/lore/main.go` raises the `-n` default and threads `n` into the report. Pure statistics layer — the per-run bar stays `root_cause ≥ 2`.

**Tech Stack:** Go 1.26, stdlib `testing` (no testify). `aggregate` is unit-testable directly on a `LiveResult` with synthetic `Runs`.

**Spec:** `docs/superpowers/specs/2026-06-23-eval-statistics-design.md`

**Branch:** `feat/eval-statistics` (already checked out; the spec is already committed there).

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/eval/live.go` | scenario runs + aggregation/gate | constants; `Flaky` field on `LiveResult`; k-of-n + variance gate in `aggregate` |
| `internal/eval/report.go` | report rendering | `N` field on `LiveReport`; `NewLiveReport(at, n, results)`; header N + pass-rate; `FLAKY` status |
| `cmd/lore/main.go` | eval CLI | `-n` default 3→10; pass `n` to `NewLiveReport` |
| `internal/eval/live_test.go`, `internal/eval/report_test.go` | tests | gate unit tests; report N/FLAKY tests; update existing `NewLiveReport` calls for the new signature |

Order: **T1** (gate logic, self-contained) → **T2** (report/N plumbing — its `FLAKY` status needs T1's `Flaky` field, and its `NewLiveReport` signature change ripples to `main.go` + `report_test.go`) → **T3** (verify).

---

### Task 1: k-of-n + flaky gate in `aggregate`

**Files:**
- Modify: `internal/eval/live.go` (constants; `LiveResult` struct; `aggregate`)
- Test: `internal/eval/live_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/eval/live_test.go`:

```go
// rcRuns builds N runs with the given root_cause scores and full coverage.
func rcRuns(scores ...int) []RunOutcome {
	rs := make([]RunOutcome, len(scores))
	for i, s := range scores {
		rs[i] = RunOutcome{
			Coverage: Coverage{Ratio: 1.0},
			Verdict:  Verdict{Scores: map[string]int{"root_cause": s}},
		}
	}
	return rs
}

func aggregated(runs []RunOutcome) LiveResult {
	res := LiveResult{DimMedian: map[string]int{}, DimVariance: map[string]float64{}, Runs: runs}
	(&LiveRunner{}).aggregate(&res)
	return res
}

func TestAggregateKOfNPass(t *testing.T) {
	res := aggregated(rcRuns(2, 2, 2, 2, 2, 2, 2, 1, 1, 1)) // 7/10 >= 2, variance ~0.21
	if !res.Pass || res.Flaky {
		t.Fatalf("7/10 at root_cause>=2 should pass cleanly: pass=%v flaky=%v var=%v", res.Pass, res.Flaky, res.DimVariance["root_cause"])
	}
}

func TestAggregateKOfNFailsBelowRate(t *testing.T) {
	res := aggregated(rcRuns(2, 2, 2, 2, 2, 2, 1, 1, 1, 1)) // 6/10
	if res.Pass {
		t.Fatal("6/10 at root_cause>=2 should fail the k-of-n gate")
	}
}

func TestAggregateFlakyFails(t *testing.T) {
	res := aggregated(rcRuns(2, 2, 2, 2, 2, 2, 2, 2, 0, 0)) // rate 0.8 but variance ~0.64
	if !res.Flaky || res.Pass {
		t.Fatalf("high-variance run must be flaky and not pass: flaky=%v pass=%v var=%v", res.Flaky, res.Pass, res.DimVariance["root_cause"])
	}
}

func TestAggregateCleanPass(t *testing.T) {
	res := aggregated(rcRuns(3, 3, 3, 3, 3, 3, 3, 3, 3, 2)) // variance ~0.09
	if !res.Pass || res.Flaky {
		t.Fatalf("consistent high scores should pass cleanly: pass=%v flaky=%v", res.Pass, res.Flaky)
	}
}

func TestAggregateCoverageGate(t *testing.T) {
	runs := rcRuns(3, 3, 3, 3, 3, 3, 3, 3, 3, 3) // rate 1.0, not flaky
	for i := range runs {
		runs[i].Coverage.Ratio = 0.5 // median 0.5 != 1.0
	}
	if aggregated(runs).Pass {
		t.Fatal("coverage median < 1.0 must fail regardless of root_cause rate")
	}
}

func TestAggregateConfidentWrongFails(t *testing.T) {
	runs := rcRuns(3, 3, 3, 3, 3, 3, 3, 3, 3, 3)
	runs[0].Verdict.ConfidentWrong = true
	if aggregated(runs).Pass {
		t.Fatal("any confident-wrong run must fail")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/eval/ -run TestAggregate`
Expected: FAIL — compile error `res.Flaky undefined` (the field doesn't exist yet).

- [ ] **Step 3: Add constants, the `Flaky` field, and the new gate**

In `internal/eval/live.go`, add the constants after the imports (above the `StepRunner` interface):

```go
const (
	evalRootCauseBar         = 2   // a run "reaches the root cause" at score >= 2
	evalMinPassRate          = 0.7 // fraction of runs that must reach it
	evalMaxRootCauseVariance = 0.5 // above this, the scenario is flaky → not a pass
)
```

In the `LiveResult` struct, add a field after `ConfidentWrong`:

```go
	Flaky          bool     // root_cause scores vary too much across runs to trust
```

In `aggregate`, replace the final gate line (`res.Pass = res.DimMedian["root_cause"] >= 2 && res.CoverageRatio == 1.0 && !res.ConfidentWrong`) with:

```go
	// k-of-n: a clear majority of runs must reach the root cause. A median over an
	// integer 0–3 dimension is too coarse to trust at small N.
	rootCausePasses := 0
	for _, r := range res.Runs {
		if r.Verdict.Scores["root_cause"] >= evalRootCauseBar {
			rootCausePasses++
		}
	}
	rootCausePassRate := float64(rootCausePasses) / float64(len(res.Runs))
	// Flaky: runs disagree too much on root_cause to call this a reliable pass.
	res.Flaky = res.DimVariance["root_cause"] > evalMaxRootCauseVariance
	res.Pass = rootCausePassRate >= evalMinPassRate &&
		res.CoverageRatio == 1.0 &&
		!res.ConfidentWrong &&
		!res.Flaky
```

(`res.DimVariance["root_cause"]` is populated by the `Rubric` loop just above this line, so it's available. `len(res.Runs) > 0` is guaranteed by the early `if len(res.Runs) == 0 { return }` at the top of `aggregate`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/eval/`
Expected: PASS — the six new `TestAggregate*` tests AND all pre-existing eval tests. In particular `TestRunScenarioPassesAndTearsDown` (fixed judge `root_cause:3` for every run → rate 1.0, variance 0, not flaky → passes) and `TestRunScenarioFailsGateOnLowRootCause` (`root_cause:1` every run → rate 0 → fails) still behave correctly.

- [ ] **Step 5: Commit**

```bash
git add internal/eval/live.go internal/eval/live_test.go
git commit -m "feat(eval): k-of-n pass rule + flaky-variance fail (was bare median)"
```

---

### Task 2: Report carries N; FLAKY status; thread N through

**Files:**
- Modify: `internal/eval/report.go` (`LiveReport` struct; `NewLiveReport`; `Markdown` header + status)
- Modify: `cmd/lore/main.go` (`-n` default; `NewLiveReport` call)
- Test: `internal/eval/report_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/eval/report_test.go`:

```go
func TestReportHeaderStatesN(t *testing.T) {
	rep := NewLiveReport("t", 10, []LiveResult{{Scenario: "a", Pass: true, DimMedian: map[string]int{"root_cause": 3}}})
	if md := rep.Markdown(); !strings.Contains(md, "N=10") {
		t.Fatalf("header must state N:\n%s", md)
	}
}

func TestReportFlakyStatus(t *testing.T) {
	rep := NewLiveReport("t", 10, []LiveResult{{Scenario: "a", Pass: false, Flaky: true, DimMedian: map[string]int{"root_cause": 2}}})
	if md := rep.Markdown(); !strings.Contains(md, "FLAKY") {
		t.Fatalf("a flaky scenario must render FLAKY:\n%s", md)
	}
}
```

(`strings` is already imported in `report_test.go` — if not, add it.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/eval/ -run 'TestReportHeaderStatesN|TestReportFlakyStatus'`
Expected: FAIL — compile error: `NewLiveReport` takes 2 args, not 3 (signature not changed yet).

- [ ] **Step 3: Add `N` + update `NewLiveReport` + header + status**

In `internal/eval/report.go`:

(a) Add `N` to the `LiveReport` struct (after `At`):

```go
	N       int          `json:"n"` // runs per scenario (live mode); 0 in replay mode
```

(b) Change `NewLiveReport` to take and set `n`:

```go
func NewLiveReport(at string, n int, results []LiveResult) LiveReport {
	rep := LiveReport{At: at, N: n, Results: results}
	for _, r := range results {
		if r.Skipped {
			rep.Skipped++
			continue
		}
		rep.Ran++
		if r.Pass {
			rep.Passed++
		}
	}
	return rep
}
```

(c) In `Markdown`, change the summary line to state N + the pass-rate (references the `evalMinPassRate` constant — same package):

```go
	fmt.Fprintf(&b, "**Passed %d/%d** ran (%d skipped) · N=%d runs/scenario, pass-rate ≥%.0f%%.\n\n",
		rep.Passed, rep.Ran, rep.Skipped, rep.N, evalMinPassRate*100)
```

(d) In the per-scenario table loop, give a flaky failure its own status:

```go
		status := "FAIL"
		if r.Pass {
			status = "PASS"
		} else if r.Flaky {
			status = "FLAKY"
		}
```

(replacing the existing `status := "FAIL"; if r.Pass { status = "PASS" }`).

- [ ] **Step 4: Update the `NewLiveReport` callers (compile fix)**

In `cmd/lore/main.go`:
- Line ~465: change `n := fs.Int("n", 3, "runs per scenario (live mode)")` → `n := fs.Int("n", 10, "runs per scenario (live mode)")`.
- Line ~576 (inside `runEvalLive`, where `n` is an `int` parameter): change `rep := eval.NewLiveReport(stamp, results)` → `rep := eval.NewLiveReport(stamp, n, results)`.

In `internal/eval/report_test.go`, update the pre-existing `NewLiveReport` calls to pass an N argument:
- `NewLiveReport("2026-06-21T20:00:00Z", sampleResults())` → `NewLiveReport("2026-06-21T20:00:00Z", 10, sampleResults())`
- `NewLiveReport("t0", []LiveResult{...})` → `NewLiveReport("t0", 10, []LiveResult{...})`
- `NewLiveReport("t1", []LiveResult{...})` → `NewLiveReport("t1", 10, []LiveResult{...})`

(Search `report_test.go` for every `NewLiveReport(` and add the N arg; there are three.)

If the pre-existing `TestReportJSONAndMarkdown` asserts on the report body, the additive `"n": <N>` JSON field and the new header line (`· N=… runs/scenario, pass-rate ≥…%`) are expected — adjust its expectations only if it does exact/golden matching (substring checks need no change). Run it in Step 5 to confirm.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/eval/`
Expected: build clean; PASS — the two new report tests + all pre-existing eval tests (with the updated `NewLiveReport` calls).

- [ ] **Step 6: Commit**

```bash
git add internal/eval/report.go internal/eval/report_test.go cmd/lore/main.go
git commit -m "feat(eval): report states N + pass-rate; FLAKY status; default N=10"
```

---

### Task 3: Whole-tree verification

- [ ] **Step 1: Build, test, vet**

Run:
```bash
go build ./... && go test ./... && go vet ./...
```
Expected: build clean; all tests PASS; vet clean.

No commit (verification only).

---

## Notes for the implementer

- `aggregate` is a method on `*LiveRunner` but reads only `res`, so `(&LiveRunner{}).aggregate(&res)` unit-tests the gate directly — no Model/Steps/Judge needed.
- Variances for the test fixtures: `7×2+3×1 → ~0.21` (pass, not flaky); `8×2+2×0 → ~0.64` (flaky); `9×3+1×2 → ~0.09` (clean pass). Adjacent scores keep variance below `0.5` so the k-of-n rate is what's tested; the `0/2` spread is what trips the flaky threshold.
- The per-run bar stays `root_cause ≥ 2` (`evalRootCauseBar`); do NOT raise it to 3 or add entity-precision — that's the separate #4 slice. Do NOT add config flags, bootstrap CI, or wire CI (#7).
- `report.go` and `live.go` are the same package (`eval`), so the header can reference `evalMinPassRate` directly.
- `NewLiveReport`'s signature change is the only ripple: exactly one production caller (`cmd/lore/main.go:576`) and three test calls (`report_test.go`) — update all four in Task 2 so the build stays green.
