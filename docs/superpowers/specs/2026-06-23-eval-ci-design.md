# Eval into CI — design (roadmap #7)

| | |
|---|---|
| **Date** | 2026-06-23 |
| **Roadmap item** | #7 — wire the eval harness into CI with a k-of-n + fail-on-threshold gate |
| **Depends on** | #4 (deterministic entity precision, merged PR #80), #5 (k-of-n + variance, merged PR #78) |
| **Effort** | S |

## Problem

The replay eval (`lore eval`, Track A) produces the project's only trustworthy
RCA-quality signal: it replays recorded tool evidence through the real
investigation loop and scores the result with **deterministic** entity-precision
logic (`internal/eval/score.go` — entity recall over `root_cause_entities`, an
over-claim penalty over `distractors`, a confidence floor). Today nothing runs it
automatically:

- `runEval` (`cmd/lore/main.go:465`) runs each case **once** and **always exits 0**
  regardless of pass/fail — it cannot gate anything.
- `.github/workflows/ci.yaml` runs `go build/vet/test` + lint only. `grep eval
  .github/` is empty: eval never gates merges and no scheduled run exists, so RCA
  regressions are invisible until someone runs the harness by hand.

The k-of-n + variance machinery from #5 lives only in the **live** path
(`internal/eval/live.go`), which needs a real cluster. The replay path — the
cluster-free one — has no statistical gate.

## Constraint that shapes the design

Replay mode is **cluster-free but not LLM-free**: `staticTool` replays recorded
tool outputs, but `LoopInvestigator` runs the live model over them
(`internal/eval/eval.go:48-70`). Only the scoring layer is deterministic; the agent
run is not. Therefore a CI eval needs an **LLM API key secret**, costs tokens per
run, is **non-deterministic** per run, and **cannot run on fork PRs** (GitHub does
not expose secrets to forked-PR workflows).

Decision: **do not** make eval a per-PR blocking gate. Run it on a **schedule
(nightly) + manual dispatch**, fail-loud on regression, upload the report. The
existing `go test ./...` already protects the gate/scoring *logic* deterministically
on every PR via unit tests; the nightly protects the *agent's RCA quality*.

## Design

### Part A — a gate in replay mode (`runEval`, `internal/eval`)

**New flags** on the `lore eval` replay path (`cmd/lore/main.go:466-479`):

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `-n` | int | **1** | repeats per case. Default 1 preserves today's one-shot local behavior. |
| `-fail-under` | float | **0** | minimum overall case pass-rate; below it the command returns a non-zero exit. Default 0 = no gate, so local `lore eval` still exits 0. |

`-report-dir` (already defined, unused in replay) gains a use: replay writes a JSON
report there for the CI artifact.

**Aggregation.** The replay `Runner` (`internal/eval/eval.go`) gains repeat support:
each case runs `n` times. A new aggregate type holds, per case:

- `PassRate float64` — fraction of the `n` repeats whose `Result.Pass` is true.
- `Reached bool` — `PassRate >= evalMinPassRate` (k-of-n; reuse the **0.7** live
  constant). This is the per-case verdict.
- `Flaky bool` — `variance(passIndicators) > evalMaxRootCauseVariance` (reuse the
  live **0.5** bound over the 0/1 pass indicators). A flaky case is surfaced and
  does **not** count as reached.
- `Confidence` (median over repeats), `Missing`, `OverClaimed` (union over repeats,
  for the report).

**Overall gate.** `reachedCases / totalCases` is the campaign pass-rate. `runEval`
returns a non-zero error when it is `< fail-under` (and `fail-under > 0`), with a
message naming the cases that did not reach RCA and any flaky cases. At/above the
threshold, or when `fail-under == 0`, it returns nil.

**DRY.** The constants `evalRootCauseBar`, `evalMinPassRate`,
`evalMaxRootCauseVariance` and the helpers `variance`, `medianFloat` are currently
unexported in `live.go`. Lift them into a new `internal/eval/stats.go` (same package,
no API change) so replay and live share one definition. Pure move + the existing
live tests keep passing.

**Output.** Print a per-case line (`REACHED/MISSED  name  pass-rate=4/5
flaky=false`) and a campaign summary (`reached N/M cases (X%)  threshold=70%`). Write
`<report-dir>/<stamp>-replay.json` containing the per-case aggregates and the
campaign pass-rate.

### Part B — CI workflow (`.github/workflows/eval.yaml`)

A **new** workflow file (kept separate from `ci.yaml` so the expensive job never
blocks PRs):

```yaml
name: eval
on:
  schedule:
    - cron: "0 6 * * *"        # nightly 06:00 UTC
  workflow_dispatch:            # manual run
permissions:
  contents: read
concurrency:
  group: eval-${{ github.ref }}
  cancel-in-progress: true
jobs:
  replay-eval:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.26" }
      - run: go build -o lore ./cmd/lore
      - name: replay eval
        env:
          RUNLORE_EVAL_API_KEY: ${{ secrets.RUNLORE_EVAL_API_KEY }}
        run: ./lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7 -report-dir eval/reports
      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: eval-report
          path: eval/reports/
```

**Committed CI eval config** `eval/ci.runlore.yaml` — a minimal config carrying only
a `model:` block, whose `api_key_env: RUNLORE_EVAL_API_KEY` matches the secret the
workflow injects. It defaults to the provider the repo already uses. The user adds
exactly one repo secret (`RUNLORE_EVAL_API_KEY`) to enable the nightly. The config is
documented inline.

If the secret is unset, the configured model fails fast and the job goes red — the
intended fail-loud behavior for the maintainer's own repo. (Forks never run this job:
`schedule`/`workflow_dispatch` only fire on the upstream repo.)

## Testing

Unit tests (run on every PR via `go test ./...`, fully deterministic — they stub the
model):

- `TestReplayKOfNRepeats` — a stub model passing 4 of 5 repeats → case `Reached`
  (≥0.7); passing 2 of 5 → not reached.
- `TestReplayFlakyCaseNotReached` — alternating pass/fail repeats push variance over
  0.5 → `Flaky`, not counted as reached.
- `TestFailUnderExitNonZero` — campaign pass-rate below `-fail-under` makes `runEval`
  return an error; at/above returns nil.
- `TestReplayDefaultsPreserveBehavior` — `-n` and `-fail-under` unset → one run per
  case and nil error regardless of scores (back-compat).
- existing `live_test.go` continues to pass after the constants/helpers move
  (`stats.go`).

Workflow file validated structurally (valid YAML, pinned action majors, least-priv
permissions).

## Out of scope

- Live-fire k3d in CI (needs a cluster; effort L; a separate item).
- Baseline regression auto-commit / trend tracking.
- Per-PR blocking eval (rejected above: secret + cost + fork limitation).

## Files touched

- `internal/eval/stats.go` — **new**: lifted constants + `variance`/`medianFloat`.
- `internal/eval/live.go` — drop the moved constants/helpers.
- `internal/eval/eval.go` — repeat support + per-case aggregate type + campaign
  pass-rate.
- `internal/eval/score.go` — unchanged (scoring already deterministic).
- `cmd/lore/main.go` — `-n` / `-fail-under` flags, aggregate wiring, JSON report,
  non-zero exit.
- `internal/eval/eval_test.go` — new tests above.
- `.github/workflows/eval.yaml` — **new** workflow.
- `eval/ci.runlore.yaml` — **new** minimal CI eval config.
