# whatchanged Hardening — Implementation Plan

> **For agentic workers:** execute task-by-task with TDD (test → fail → impl →
> pass → commit). Spec: `dev/superpowers/specs/2026-06-24-whatchanged-hardening-design.md`.

**Goal (R5, must):** make go-git clone/patch cancellable (thread `ctx` through the
Differ; `PlainCloneContext` + `PatchContext`) and bound every investigation with a
configurable per-investigation deadline. **R4 (scoped down):** correct the overstated
"two deployed revisions" doc/comment; defer R4-full (deploy-span) for lack of state.

**Tech Stack:** Go (stdlib `testing`, no testify; table-driven; `%w`; `ctx` first
param). go-git v5.19.1. `golangci-lint` clean; every task ends green.

## Global Constraints
- `GitOpsProvider.Diff(ctx, c)` already carries ctx — only the Differ drops it.
- `Timeout == 0` / `max_duration == 0` ⇒ feature disabled (behaviour unchanged).
- Timeout delivers a synthetic result (mirror `budgetKillResult`), labels
  `result="timeout"`, increments the existing `InvestigationsDropped` counter.
- No new config default for `max_duration` (opt-in); helm values document a 10m rec.

---

### Task 1: Differ ctx threading + cancellable clone/patch
**Files:** `internal/whatchanged/differ.go`, `internal/whatchanged/differ_test.go`
- [ ] Test first: `TestRemoteCancelledCtx` (cancel ctx, then `Remote(ctx, dir, …)` →
  `errors.Is(err, context.Canceled)`, empty diff); `TestForChangeCancelledCtx`
  (empty FromRev path via cancelled ctx). Update all existing differ tests to pass
  `context.Background()` as the new first arg.
- [ ] Impl: add `ctx context.Context` first param to `Local/Remote/RemoteFromParent/
  ForChange/cloneToDisk/diffRevisions/diffCommits`; `git.PlainCloneContext(ctx,…)`;
  `from.PatchContext(ctx, to)`. Run `go test ./internal/whatchanged/`; commit.

### Task 2: Provider Diff passes ctx (flux + argocd)
**Files:** `internal/providers/gitops/flux/flux.go`, `.../argocd/argocd.go`,
`flux_test.go`, `argocd_test.go` (only if a test calls `Diff`/`ForChange`).
- [ ] Test: existing flux empty-URL `Diff` test still returns empty diff with a real
  ctx; any test calling `ForChange`/`Diff` updated to pass ctx.
- [ ] Impl: `Diff(ctx context.Context, c)` → `p.differ.ForChange(ctx, c)` (flux keeps
  the empty-RepoURL short-circuit). Run `go test ./internal/providers/...`; commit.

### Task 3: Per-investigation deadline (loop)
**Files:** `internal/investigate/loop.go`, `internal/investigate/budget.go`,
`internal/investigate/loop_test.go`
- [ ] Test: `TestInvestigateDeadline` — model `Complete` blocks on `<-ctx.Done()`,
  returns `ctx.Err()`; `LoopInvestigator{Timeout: 20*time.Millisecond}`; assert
  `Investigate` returns nil, `OnComplete` fires once, result `Unresolved` contains
  "deadline"/"timed out", and it returns well before `MaxSteps`.
  `TestInvestigateNoDeadlineWhenZero` — `Timeout: 0` + a normal scripted run is
  unaffected (model called, findings delivered).
- [ ] Impl: add `Timeout time.Duration` field; at the top of `Investigate`, wrap
  ctx in `context.WithTimeout` when `>0` (defer cancel). After `li.Model.Complete`
  returns `err`, if `errors.Is(ctx.Err(), context.DeadlineExceeded)` → log warn,
  `InvestigationsDropped++` (nil-safe), `result="timeout"`, `deliver(req,
  timeoutResult(req))`, return nil. Add `timeoutResult(req)` to `budget.go`
  (mirrors `budgetKillResult`). Run `go test ./internal/investigate/`; commit.

### Task 4: Config + wiring + helm docs
**Files:** `internal/config/config.go`, `internal/config/config_investigation_test.go`,
`cmd/lore/main.go`, `deploy/helm/runlore/values.yaml`
- [ ] Test: extend `TestInvestigationConfigParse` to parse `max_duration: 10m` and
  assert `cfg.Investigation.MaxDuration.Std() == 10*time.Minute`.
- [ ] Impl: add `MaxDuration Duration `yaml:"max_duration"`` to `Investigation`
  (comment: `0 ⇒ disabled`); wire `Timeout: cfg.Investigation.MaxDuration.Std()` at
  all three `LoopInvestigator{}` sites in `cmd/lore/main.go`; document
  `# max_duration: 10m` under `investigation:` in helm values. No `applyDefaults`
  entry (opt-in). Run `go test ./internal/config/ ./cmd/...`; commit.

### Task 5: R4 doc/comment correction
**Files:** `internal/providers/providers.go`, `internal/providers/gitops/flux/flux.go`
- [ ] No test (comment-only). Correct `providers.go` `Change` doc (drop "between two
  deployed revisions" → "the change a revision introduced: the latest applied commit
  against its git parent"); clarify the flux `Changes`/`revisionRange` docs and note
  R4-full (prior-revision deploy-span) deferred for lack of cross-reconciliation
  state. Run the full gate; commit.

## Success criteria
SC-1 a cancelled clone/patch returns a wrapped ctx error (`errors.Is`) · SC-2 a
slow investigation is bounded by `max_duration` and delivers a `timeout` result ·
SC-3 `Timeout==0`/`max_duration==0` preserves current behaviour · SC-4 the R4
doc no longer claims "two deployed revisions" · SC-5 full gate green incl.
`-race` on whatchanged/flux/investigate.
