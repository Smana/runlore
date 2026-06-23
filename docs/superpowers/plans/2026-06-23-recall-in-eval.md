# Recall-in-Eval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the eval harness exercise the instant-recall short-circuit (today dropped) and prove with unit tests that it fires and that a poisoned entry is caught by the verify pass.

**Architecture:** Add `Recall *investigate.Recall` to `LiveRunner` and thread it into the `LoopInvestigator` that `runOnce` builds; capture the (currently discarded) recall in `runEvalLive`. The recall short-circuit sets `Investigation.Recalled = true` and runs the `Verify` pass, so tests assert on `Recalled` and on a scripted `submit_verdicts` verdict.

**Tech Stack:** Go 1.26, stdlib `testing` (no testify). Reuses the eval package's fake conventions; adds a tiny `catalog.ScoredSearcher` fake + a verify-only model fake.

**Spec:** `docs/superpowers/specs/2026-06-23-recall-in-eval-design.md`

**Branch:** `feat/recall-in-eval` (already checked out, off the #5-merged `main`; the spec is already committed there).

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/eval/live.go` | live runner | `Recall *investigate.Recall` on `LiveRunner`; pass `Recall: lr.Recall` into the `runOnce` `LoopInvestigator` |
| `internal/eval/live_test.go` | tests | recall-fires + poisoned-rejected tests; `fakeCatalog` (ScoredSearcher) + `verifyModel` fakes |
| `cmd/lore/main.go` | eval CLI wiring | capture the recall return in `runEvalLive`; set `Recall` on the `LiveRunner` |

Order: **T1** (live.go field + runOnce thread + tests — the testable core) → **T2** (main.go wiring — build-verified) → **T3** (verify). T1 is self-contained; T2 makes a real live run use it.

---

### Task 1: Thread `Recall` into the eval runner (+ prove it)

**Files:**
- Modify: `internal/eval/live.go` (`LiveRunner` struct; `runOnce`'s `LoopInvestigator`)
- Test: `internal/eval/live_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/eval/live_test.go`. First add imports `"context"`, `"fmt"`, `"github.com/Smana/runlore/internal/catalog"` (keep the existing `io`, `slog`, `testing`, `investigate`, `providers`). Then:

```go
// fakeCatalog returns a single fixed scored entry regardless of query/k — enough
// to drive the recall gates in a unit test.
type fakeCatalog struct {
	entry catalog.Entry
	score float64
}

func (f fakeCatalog) SearchScored(string, int) ([]catalog.ScoredEntry, error) {
	return []catalog.ScoredEntry{{Entry: f.entry, Score: f.score}}, nil
}

// verifyModel answers only the adversarial verify call (submit_verdicts) with a
// fixed verdict for root cause index 0. On the recall short-circuit path this is
// the sole model call (the ReAct loop is skipped).
type verifyModel struct{ verdict string }

func (m verifyModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{ID: "v", Name: "submit_verdicts",
		Args: fmt.Sprintf(`{"verdicts":[{"index":0,"verdict":%q}]}`, m.verdict)}}}, nil
}

// recallRunner builds a LiveRunner whose catalog has one entry matching the
// recallScenario's namespace (so the structural gate agrees), with low recall
// floors so a single strong hit short-circuits.
func recallRunner(model providers.ModelProvider) *LiveRunner {
	entry := catalog.Entry{Title: "HarborRegistryDown", Description: "IAM quota exceeded", Path: "harbor.md", Resource: "tooling"}
	return &LiveRunner{
		Model:  model,
		Recall: &investigate.Recall{Catalog: fakeCatalog{entry: entry, score: 10}, MinScore: 1, SoloFloor: 1, MarginGap: 1},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func recallScenario() Scenario {
	return Scenario{ID: "known-pattern-recall", Trigger: Trigger{Symptom: "harbor registry down", Namespace: "tooling"}}
}

func TestRunOnceRecallShortCircuits(t *testing.T) {
	out := recallRunner(verifyModel{verdict: "keep"}).runOnce(context.Background(), recallScenario())
	if !out.Investigation.Recalled {
		t.Fatal("recall should have short-circuited the loop (Investigation.Recalled=true)")
	}
	if len(out.Investigation.RootCauses) != 1 {
		t.Fatalf("a kept recall should retain its single root cause, got %d", len(out.Investigation.RootCauses))
	}
	if len(out.Coverage.Touched) != 0 {
		t.Fatalf("recall short-circuit must run no investigation tools, got %v", out.Coverage.Touched)
	}
}

func TestRunOnceRecallPoisonedRejected(t *testing.T) {
	// A poisoned entry: the verify pass rejects it → the wrong root cause is withdrawn,
	// not short-circuited into. (Recalled stays true; the safety is an empty RootCauses.)
	out := recallRunner(verifyModel{verdict: "reject"}).runOnce(context.Background(), recallScenario())
	if len(out.Investigation.RootCauses) != 0 {
		t.Fatalf("a rejected poisoned recall must withdraw its root cause, got %d", len(out.Investigation.RootCauses))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/eval/ -run TestRunOnceRecall`
Expected: FAIL — compile error `unknown field Recall in struct literal of type LiveRunner`.

- [ ] **Step 3: Add the field and thread it**

In `internal/eval/live.go`, add to the `LiveRunner` struct (after `N int`):

```go
	Recall    *investigate.Recall    // optional; when set, runOnce takes the instant-recall short-circuit (production path). nil ⇒ no recall.
```

In `runOnce`, add `Recall: lr.Recall` to the `LoopInvestigator` literal:

```go
	li := &investigate.LoopInvestigator{
		Model: lr.Model, Tools: wrap(lr.BaseTools, rec), Log: lr.Log, Verify: true, Recall: lr.Recall,
		OnComplete: func(got providers.Investigation) { inv = got },
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/eval/`
Expected: PASS — the two new tests plus all pre-existing eval tests (the existing `RunScenario` tests set no `Recall`, so `li.Recall == nil` and behavior is unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/eval/live.go internal/eval/live_test.go
git commit -m "feat(eval): exercise the instant-recall short-circuit + poisoned-entry proof"
```

---

### Task 2: Capture and wire the recall in `runEvalLive`

**Files:**
- Modify: `cmd/lore/main.go` (`runEvalLive`)

- [ ] **Step 1: Capture the recall return**

In `cmd/lore/main.go`, `runEvalLive`, change the line that drops recall:
```go
	model, tools, _, _ := buildModelAndTools(ctx, cfg, gitOpsFromKube(cfg, log), log)
```
to capture it:
```go
	model, tools, recall, _ := buildModelAndTools(ctx, cfg, gitOpsFromKube(cfg, log), log)
```

- [ ] **Step 2: Set it on the LiveRunner**

In the same function, add `Recall: recall,` to the `eval.LiveRunner{...}` literal (alongside `Model`, `BaseTools`, `Judge`, `Steps`, `Log`, `N`, `OnRecord`):

```go
	runner := &eval.LiveRunner{
		Model: model, BaseTools: tools, Judge: judge, Steps: shellStepRunner{}, Log: log, N: n, Recall: recall,
		OnRecord: func(scn eval.Scenario, calls []eval.Call) {
			if err := eval.WriteCase(recordDir, eval.RecordedCase(scn, calls)); err != nil {
				log.Warn("record case failed", "id", scn.ID, "err", err)
			}
		},
	}
```

- [ ] **Step 3: Build to verify the wiring compiles**

Run: `go build ./...`
Expected: success, no output. (No new unit test — the recall behavior is covered by Task 1's `runOnce` tests; this is the production wiring so a live run uses recall when `instant_recall` is enabled and the catalog matches.)

- [ ] **Step 4: Commit**

```bash
git add cmd/lore/main.go
git commit -m "feat(eval): wire recall into the live-eval runner (was dropped)"
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

- `runOnce` is unexported but the test is in `package eval`, so it's directly callable; it does NOT run precheck/setup/teardown (those are in `RunScenario`), so the tests need no `Steps`.
- The recall short-circuit is the *only* model call on the happy path (the ReAct loop is skipped), and it is the **verify** call — that's why `verifyModel` returns a `submit_verdicts` tool call. If recall failed to fire, the main loop would call the model and get an unrecognized `submit_verdicts` response, and `Investigation.Recalled` would be false — so the `Recalled` assertion is the definitive "short-circuit fired" proof.
- After a `reject` verdict, `applyVerdicts` moves the root cause to `Unresolved` and empties `RootCauses` (and `Recalled` remains `true` — it *was* a recall, just withdrawn). The poisoned test asserts `len(RootCauses) == 0`, the safety property.
- The eval `Request` carries a namespace-only `Workload` (`Workload{Namespace: scn.Trigger.Namespace}`), so the fixture entry's `Resource: "tooling"` agrees at the namespace level via `resourceAgrees` (reqW has no Name) — the recall passes Gate 2.
- Do NOT add live-harness catalog fixture seeding, a report "recalled" column, entity precision (#4), or CI wiring (#7) — all deferred.
- The existing eval tests set no `Recall`, so they're unaffected; keep them green.
