# RunLore Eval Statistics — k-of-n Gate + Flaky-fail + N — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-23 |
| **Scope** | Make a live-eval "pass" a statistically meaningful measurement: raise the default N, replace the bare-median gate with a k-of-n pass rule, fail "flaky" scenarios whose root-cause variance is high, and state N in the report. Confined to `internal/eval/live.go`, `internal/eval/report.go`, and the `-n` default in `cmd/lore/main.go`. |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | Deep-analysis report (`docs/analysis/2026-06-23-deep-analysis.md`) roadmap #5 / "N=3 bare-median gate is statistically meaningless"; the eval harness (`internal/eval`); the separate #4 (entity-level precision / raise the per-run bar) and #7 (wire eval into CI) slices that build on this |

---

## 1. Why this exists

The live-eval pass gate is `median(root_cause) >= 2 AND median(coverage) == 1.0 AND no confident-wrong run`, over a default of **N=3** (`live.go:159`, `main.go:465`). A median over three integer scores spanning 0–3 is a coin flip, not a measurement — the committed OOM baseline scored root_cause `{0,2,1}` (median 1), and flipping any single run flips the verdict. So no eval claim about RunLore (or about whether the five just-shipped learning-loop slices help) can be trusted, and there is nothing to gate CI on.

This slice makes the gate honest: enough runs (N≥10), a **k-of-n** pass rule (a clear majority of runs must reach the root cause), and a **flaky-fail** for scenarios whose runs wildly disagree — plus the report stating its own N. It is purely the *statistics* layer; the per-run quality bar (`root_cause ≥ 2`) is unchanged here (raising it to `≥3` / entity-level precision is the separate #4 slice).

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **k-of-n pass rule** replaces the median for the gate | "≥70% of runs reached the root cause" is honest; a median over an integer 0–3 dimension is coarse (5/10 zeros can still median to 2). |
| D2 | **Flaky-fails:** high root-cause variance ⇒ not a pass | Catches "mostly passes but with alarming occasional total misses" that k-of-n alone would bless (8×score-2 + 2×score-0 → rate 0.8 passes, variance 0.64 flaky). An unreliable measurement is not a pass. |
| D3 | **Defaults as internal constants:** N=10, min-pass-rate 0.7, max root-cause variance 0.5 | Sensible starting values, tunable from real run data; YAGNI on config flags (a `--min-pass-rate` flag pairs naturally with the #7 CI slice). |
| D4 | **`ConfidentWrong` stays any-run-fails** (not rate-based) | A confidently-wrong root cause even once is a safety red flag worth failing on. (A future rate-based relaxation is noted, not done.) |
| D5 | **Coverage stays median == 1.0** | The deterministic coverage track is stable; no need to k-of-n it. |
| D6 | **Bootstrap CI deferred** | The heaviest part; k-of-n + variance + N is the meaningful, shippable core. |

## 3. Design

### 3.1 Constants (`live.go`)

```go
const (
	evalRootCauseBar         = 2   // a run "reaches the root cause" at score >= 2
	evalMinPassRate          = 0.7 // fraction of runs that must reach it
	evalMaxRootCauseVariance = 0.5 // above this, the scenario is flaky → not a pass
)
```

### 3.2 Aggregation + gate (`live.go`, `aggregate`)

Keep computing `DimMedian`/`DimVariance`/`CoverageRatio`/`ConfidentWrong`/`ToolErrors` as today (they still inform the report). Add:

- `rootCausePassRate` = (count of runs with `Verdict.Scores["root_cause"] >= evalRootCauseBar`) / `len(Runs)`.
- `Flaky bool` on `LiveResult` = `DimVariance["root_cause"] > evalMaxRootCauseVariance`.
- New gate:
  ```go
  res.Pass = rootCausePassRate >= evalMinPassRate &&
  	res.CoverageRatio == 1.0 &&
  	!res.ConfidentWrong &&
  	!res.Flaky
  ```

### 3.3 N default (`cmd/lore/main.go`)

`fs.Int("n", 10, "runs per scenario (live mode)")` (was `3`). The harness fallback in `RunScenario` (`n := lr.N; if n <= 0 { n = 1 }`) is unchanged — single-run is fine for fast local iteration; N≥10 is for reported/CI runs.

### 3.4 Report (`report.go`)

- Add `N int` to `LiveReport`; `NewLiveReport(at string, n int, results []LiveResult) LiveReport` sets it; the `cmd/lore` caller passes the run's `n`.
- Markdown header states the statistical power, e.g.:
  `**Passed 7/9** ran (3 skipped) · N=10 runs/scenario, pass-rate ≥70%.`
- Per-scenario `result` column gains a **`FLAKY`** status: when a scenario is `!Pass` *because* `Flaky` is true, render `FLAKY` instead of `FAIL`, so a flaky failure (unreliable measurement) is distinguishable from a genuine RCA failure. (`PASS`/`FAIL`/`FLAKY`/`SKIP`.)

### 3.5 What is unchanged

The per-run rubric/judge, coverage scoring, `DimMedian`/`DimVariance` computation, `ConfidentWrong` semantics, the JSON shape (additive `N`/`Flaky` fields only), and the replay/record paths.

## 4. Components / seams

| Change | Location |
|---|---|
| Constants; `rootCausePassRate`; `Flaky` field; k-of-n + variance gate | `internal/eval/live.go` (`LiveResult`, `aggregate`) |
| `N` field; `NewLiveReport(at, n, results)`; header N + pass-rate; `FLAKY` status | `internal/eval/report.go` |
| `-n` default 10; pass `n` into `NewLiveReport` | `cmd/lore/main.go` |
| Tests | `internal/eval/live_test.go`, `internal/eval/report_test.go` (if present) |

## 5. Trade-offs accepted in v1

- **N=10, not larger.** Enough to make k-of-n and variance meaningful while keeping a live campaign affordable; bootstrap CI (which would quantify the residual uncertainty) is deferred.
- **Fixed thresholds.** `evalMinPassRate`/`evalMaxRootCauseVariance` are constants tuned by judgement, not learned; surfaced via the report so they can be revised. A `--min-pass-rate` flag is a natural #7 (CI) addition.
- **Variance threshold is heuristic.** `0.5` flags runs that span the scale (e.g. a 0 among 2s) while allowing a 2-vs-3 cluster. It may need tuning once real N=10 data exists.
- **`ConfidentWrong` any-run-fails is strict** — one transient confidently-wrong run fails the scenario. Deliberate (safety); a rate-based relaxation is a possible follow-up.
- **Per-run bar unchanged at `root_cause ≥ 2`** — "correct but shallow" still passes a run; raising the bar is #4, kept separate so this slice is purely statistical.

## 6. Testing

Unit-test `aggregate` by constructing a `LiveResult` with synthetic `Runs` (each a `RunOutcome{Verdict: Verdict{Scores: map[string]int{...}}, Coverage: Coverage{Ratio: 1.0}}`) and calling `(&LiveRunner{}).aggregate(&res)`:

- **k-of-n boundary (isolated from variance):** use adjacent scores so variance stays low — 7×`root_cause=2` + 3×`root_cause=1` → `rootCausePassRate=0.7 >= 0.7`, variance ≈0.21 (not flaky) → `Pass=true`; 6×2 + 4×1 → rate 0.6 < 0.7 → `Pass=false`. (Adjacent scores keep variance below the flaky threshold so the rate gate is what's being tested.)
- **Flaky-fail:** a set that clears the rate but has `root_cause` variance `> 0.5` (e.g. 8×2 + 2×0 → rate 0.8, variance 0.64) → `Flaky=true` → `Pass=false`; assert the report renders it `FLAKY`.
- **Clean pass:** 9/10 at `root_cause=3`, one at 2 (variance ≤ 0.25), coverage 1.0, no confident-wrong → `Pass=true`, `Flaky=false`.
- **Coverage gate:** rate passes but `CoverageRatio < 1.0` → `Pass=false`.
- **Confident-wrong:** one run `ConfidentWrong` → `Pass=false` regardless of rate.
- **Report:** `NewLiveReport(at, 10, results).Markdown()` header contains `N=10` and the pass-rate; a `Flaky` result shows `FLAKY`.

## 7. Out of scope (later slices)

- **Bootstrap confidence intervals** on the per-dimension mean / RCA-rate.
- **Raising the per-run bar** to `root_cause ≥ 3` (true root, not shallow) and **entity-level precision** scoring (#4).
- **Wiring eval into CI** with k-of-n repeat gating (#7) — this slice makes the gate trustworthy; #7 runs it on merges.
- Making `min-pass-rate` / N / variance threshold **config flags**.

This slice turns the eval gate from a coin flip into a measurement: a scenario passes only when a clear majority of enough runs reach the root cause, consistently — and the report says how many runs that majority was over.
