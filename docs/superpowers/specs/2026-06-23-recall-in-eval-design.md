# RunLore Recall-in-Eval — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-23 |
| **Scope** | Make the live-eval harness actually exercise the instant-recall short-circuit (today it's dropped, so none of the recall work is eval-tested), and prove with unit tests that it fires AND that a poisoned catalog entry is caught by the verify pass. Confined to `internal/eval/live.go`, `cmd/lore/main.go` (`runEvalLive`), and `internal/eval/live_test.go`. |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | Deep-analysis report (`docs/analysis/2026-06-23-deep-analysis.md`) roadmap #6 / "the recall closed-loop scenario tests a code path that is structurally bypassed in eval"; the recall slices (BM25/decay/disambiguation/retrieval); eval-statistics (#5, the gate this runs under); next: #4 entity-precision, #7 CI |

---

## 1. Why this exists

The recall short-circuit (`loop.go`, gated on `li.Recall != nil`) is the cache that skips the ReAct loop on a trustworthy catalog hit — five merged slices now shape it (BM25 scoring, outcome decay, workload disambiguation, wider-candidate retrieval). But the eval harness **never wires it**: `runEvalLive` discards the recall (`model, tools, _, _ := buildModelAndTools(...)`) and `runOnce` builds a `LoopInvestigator` with no `Recall` field. So the short-circuit is dead in eval — `known-pattern-recall.yaml` can only "pass" via the agent organically calling `kb_search`, a different mechanism. None of the recall work is exercised by eval, and there's no regression guard that a poisoned entry is rejected.

This slice wires recall into the eval runner (so a live run takes the real production path) and adds unit tests proving the short-circuit fires and that the verify pass catches a poisoned recall.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **Thread `Recall` into the eval `LoopInvestigator`** | The one structural fix: capture the dropped recall, carry it on `LiveRunner`, pass it into `runOnce`'s `LoopInvestigator`. Makes eval take the production short-circuit path. |
| D2 | **"Recall fired" signal = `Investigation.Recalled`** | `recalledInvestigation` already sets `Recalled: true`; no new field/instrument. Tests also assert zero non-`kb` tool calls (proof the loop was skipped). |
| D3 | **Wiring + unit tests only; no live-harness fixture seeding** | Unit tests (fakes) deterministically prove the loop closes; the live `known-pattern-recall.yaml` works against a real seeded KB on a cluster. Harness catalog-seeding is deferred (bigger plumbing). |
| D4 | **Include a poisoned-entry test** | The safety proof: a high-recall but wrong entry must be rejected/downgraded by the verify pass, not short-circuited into a confident wrong answer. Cheap once the recall-fires test harness exists. |

## 3. Design

### 3.1 Wiring (`main.go` + `live.go`)

- `cmd/lore/main.go` `runEvalLive`: capture the recall — `model, tools, recall, _ := buildModelAndTools(ctx, cfg, gitOpsFromKube(cfg, log), log)`.
- `internal/eval/live.go`: add `Recall *investigate.Recall` to the `LiveRunner` struct; `runEvalLive` sets it on the runner; `runOnce` passes `Recall: lr.Recall` into the `LoopInvestigator` it builds (alongside `Model`/`Tools`/`Log`/`Verify`/`OnComplete`).

A live eval run now short-circuits whenever the configured catalog has a trustworthy matching entry (`instant_recall` enabled) — the same path production takes. When `lr.Recall` is nil (recall disabled / not configured), behavior is unchanged (the gate is `li.Recall != nil`).

### 3.2 The recall-fired signal

The short-circuit delivers a `recalledInvestigation` whose `Recalled` field is `true` (and records no tool call). So `RunOutcome.Investigation.Recalled` is the signal; the recorder's calls show whether any non-`kb` tool ran. Tests assert both. (Surfacing a "recalled" column in the report is deferred — minor.)

### 3.3 Tests (`live_test.go`)

Using the existing fake-model harness (the `scriptModel`/script style already in the package's tests) plus a small eval-local `catalog.ScoredSearcher` fake returning a fixed scored entry:

- **`TestRunOnceRecallShortCircuits`**: a `LiveRunner` with `Recall` configured over a catalog whose top entry matches the scenario's symptom + workload (resource agrees), thresholds set so the gate passes, and a model scripted only to satisfy the adversarial `Verify` call (keep the finding). `runOnce` on a non-invasive scenario → assert `out.Investigation.Recalled == true` AND no non-`kb` tool call was recorded (the ReAct loop was skipped).
- **`TestRunOnceRecallPoisonedRejected`**: same setup but the entry is "poisoned" (its recalled finding is wrong) and the model is scripted to **reject** it on the `Verify` call → assert the result is NOT a confident recalled answer (root causes withdrawn / `Recalled` not delivered as the answer), i.e. the verify pass caught it rather than short-circuiting into the wrong root cause.

Both reuse the package's existing fake `StepRunner`/model conventions; `Judge` is left nil (grading is irrelevant to these assertions). The recall is a real `*investigate.Recall` over the fake `ScoredSearcher`, exercising the true `lookup` gates.

### 3.4 What is unchanged

The eval gate (k-of-n from #5), coverage scoring, the judge, the report, and the production/serve recall wiring. This slice only stops eval from *dropping* recall, and tests the path.

## 4. Components / seams

| Change | Location |
|---|---|
| Capture the recall return | `cmd/lore/main.go` (`runEvalLive`) |
| `Recall *investigate.Recall` on `LiveRunner`; set it; pass into `runOnce`'s `LoopInvestigator` | `internal/eval/live.go` |
| Recall-fires + poisoned-rejected unit tests (+ eval-local `ScoredSearcher` fake) | `internal/eval/live_test.go` |

## 5. Trade-offs accepted in v1

- **Unit-tested, not live-fixture-tested.** The wiring is proven by fakes; the live offline scenario (`known-pattern-recall.yaml` running without a cluster/KB) would need the live harness to seed a catalog — deferred. A real live run on a cluster with a seeded KB exercises it end-to-end via the new wiring.
- **No new "recalled" report column.** `Investigation.Recalled` carries the signal; surfacing it per-scenario in the report is a minor follow-up.
- **Poisoned-case verify is scripted.** The test scripts the verify model's verdict; it proves the *wiring* (verify runs on the recalled finding and can reject), not the judgement quality of a real model.

## 6. Testing

- `TestRunOnceRecallShortCircuits` (§3.3): recall fires → `Recalled == true`, no non-`kb` tool recorded.
- `TestRunOnceRecallPoisonedRejected` (§3.3): poisoned entry → verify rejects → not delivered as a confident recall.
- A nil-`Recall` `LiveRunner` still runs `runOnce` normally (no short-circuit) — unchanged behavior (covered by the existing `RunScenario` tests, which set no `Recall`).
- `go build ./... && go test ./... && go vet ./...` green.

## 7. Out of scope (later eval-cluster slices)

- **#4** — raise the per-run bar to `root_cause ≥ 3` / `root_cause_entities` entity-level precision in Track A.
- **#7** — wire eval into CI with k-of-n repeat gating.
- Live-harness catalog fixture seeding so the recall scenario runs fully offline.
- A "recalled" column in the report.

This slice closes the gap the deep analysis flagged: the recall short-circuit — the cache five slices have been hardening — is finally exercised (and its verify-backstop proven) by the eval harness instead of being silently bypassed.
