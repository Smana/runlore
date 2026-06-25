# whatchanged Hardening — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation |
| **Date** | 2026-06-24 |
| **Scope** | **R5 (must):** make go-git clone/patch cancellable (thread `ctx` through the Differ → `PlainCloneContext` + `PatchContext`) and bound every investigation with a configurable per-investigation deadline. **R4 (scoped down):** correct the overstated "two deployed revisions" doc/comment to match what the Flux path actually diffs (the latest applied commit vs its git parent); defer the full deploy-span fix (needs prior-`lastAppliedRevision` state the stateless differ does not have). |
| **Author** | Smana (challenged + designed with Claude) |
| **Related** | `internal/whatchanged/differ.go`; `internal/providers/gitops/flux/flux.go`; `internal/providers/gitops/argocd/argocd.go`; `internal/providers/providers.go`; `internal/investigate/loop.go` + `budget.go`; `internal/config/config.go` + `load.go`; `internal/telemetry/metrics.go`; `cmd/lore/main.go`. go-git v5.19.1 (`PlainCloneContext` repository.go:479, `Commit.PatchContext` commit.go:154). |

---

## 1. Challenge — relevance verdicts (current code, both sides)

### R5a — go-git clone/patch are uncancellable — **Confirmed**
- *For:* `differ.go cloneToDisk` calls `git.PlainClone` with no ctx (`differ.go:133`); `diffCommits` calls `from.Patch(to)` with no ctx (`differ.go:57`). `flux.Diff(_ context.Context, …)` (`flux.go:151`) and `argocd.Diff(_ context.Context, …)` (`argocd.go:77`) discard ctx. `whatchanged_tool.go:50` hands ctx to `Diff`, but it dies there. A hung HTTPS remote on a large monorepo blocks the calling queue worker until TCP timeout (minutes), or indefinitely on a stalled-but-alive connection.
- *Against:* `cloneToDisk` already bounds heap; the loop has `MaxSteps`/token budgets. But neither bounds wall-clock on a single blocking syscall inside one tool call — `MaxSteps` only counts model turns, and the model never gets a turn while `Patch`/`Clone` blocks.
- *Verdict:* **Confirmed.** go-git v5.19.1 exposes `PlainCloneContext` and `Commit.PatchContext`; thread ctx end-to-end.

### R5b — no per-investigation deadline — **Confirmed**
- *For:* `LoopInvestigator.Investigate` (`loop.go:109`) never wraps its body in `context.WithTimeout`; only `MaxSteps` + the token budget bound it. A slow model or a slow tool (see R5a) can stall a worker far past any reasonable SLA.
- *Against:* the queue retries failed investigations; a stuck one will eventually be cancelled by the parent ctx on shutdown. But that is process-shutdown, not per-investigation — one stuck investigation still starves the single-worker queue for its whole lifetime.
- *Verdict:* **Confirmed.** Add a configurable per-investigation deadline (default disabled; 0 = off).

### R4 — "what changed" diffs only the last commit, not the deploy span — **Partial**
- *For:* For a healthy Kustomization, `revisionRange` returns `("", lastApplied)` (`flux.go:134-135`); `ForChange` then takes `RemoteFromParent` (`differ.go:158`), which diffs the applied rev against its single git **parent**. When several commits land between Flux reconciliations, only the newest commit's delta is shown. `providers.go:72` claims "a Git diff between two deployed revisions" — overstated for the Flux path.
- *Against:* fixing it properly needs the *prior* `lastAppliedRevision` per Kustomization, i.e. cross-reconciliation state the stateless `Changes`/differ does not hold. Flux status exposes only the *current* `lastAppliedRevision` — no deploy history (unlike Argo CD's `status.history`, which the argocd provider already uses for `PrevRevision`). The failing-Kustomization branch (`revisionRange`, `flux.go:137-145`) already spans `lastApplied..HEAD` when health-check pinning hid the breaking commit — the common "broke on reconcile" case is covered. The remaining gap (healthy, multi-commit span) needs new persistence machinery, which is out of proportion for this slice.
- *Verdict:* **Partial → option (b).** Correct the doc/comment to reflect reality; record R4-full (persist prior `lastAppliedRevision`) as deferred. `Change.When` is **not** set: the Flux `kustomization` struct carries no commit time, and reading it would cost an extra git lookup per change — not "cheap", so deferred with R4-full.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Thread `context.Context` as the first param through `Differ.Local/Remote/RemoteFromParent/ForChange` and `cloneToDisk`.** | The contract already passes ctx into `GitOpsProvider.Diff`; the Differ is the only layer dropping it. Threading it is the minimal honest fix. |
| D2 | **Use `git.PlainCloneContext(ctx,…)` and `from.PatchContext(ctx,to)`.** | The two blocking operations. `Local`'s `PlainOpen` is a local FS op (not networked) but its subsequent patch is still routed through `PatchContext` for uniform cancellability. |
| D3 | **`flux.Diff` / `argocd.Diff` pass their real `ctx` to `ForChange`.** | Removes the `_ context.Context` drop at both providers; the tool→provider→differ chain becomes fully cancellable. |
| D4 | **Per-investigation deadline: `LoopInvestigator.Timeout time.Duration`; wrap the whole `Investigate` body in `context.WithTimeout` when `>0`.** | Bounds recall + every model/tool call (incl. a hung clone) in one place. `0` ⇒ disabled, preserving current behaviour. Wrapping the *whole* body (not per-step) is simplest and matches "per-investigation". |
| D5 | **On deadline: deliver a synthetic timeout result, `result="timeout"`, increment `InvestigationsDropped`.** | Mirrors the token hard-kill (`budgetKillResult`): honest, graceful, observable. Reuses the existing dropped counter (no new instrument for one slice) and the existing `result` label on `investigation_duration_seconds`/`investigations_completed_total`. |
| D6 | **Config: `investigation.max_duration` (`Duration`); default **disabled** (0).** | A default of 10m would change behaviour for every existing deployment silently; opt-in is safer for a hardening slice. Documented in helm values with a recommended `10m`. |
| D7 | **R4: doc/comment fix only; defer R4-full + `Change.When`.** | See §1 R4. Honest doc beats a half-built deploy-span feature. |
| D8 | **No per-repo clone cache in this slice.** | The `RemoteFromParent` perf note stays; a cache keyed by repo URL touches lifecycle (cleanup, concurrency) — out of proportion here. Deferred. |

## 3. Design

### 3.1 Differ ctx threading (`internal/whatchanged/differ.go`)

```go
func (d *Differ) Local(ctx context.Context, path, fromRev, toRev, scope string) (providers.Diff, error)
func (d *Differ) Remote(ctx context.Context, url, fromRev, toRev, scope string) (providers.Diff, error)
func (d *Differ) RemoteFromParent(ctx context.Context, url, rev, scope string) (providers.Diff, error)
func (d *Differ) ForChange(ctx context.Context, c providers.Change) (providers.Diff, error)
func (d *Differ) cloneToDisk(ctx context.Context, url string) (*git.Repository, func(), error)
func diffRevisions(ctx context.Context, repo *git.Repository, fromRev, toRev, scope string) (providers.Diff, error)
func diffCommits(ctx context.Context, from, to *object.Commit, scope string) (providers.Diff, error)
```

- `cloneToDisk` → `git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: url, Auth: d.auth()})`.
- `diffCommits` → `from.PatchContext(ctx, to)`.
- Clone-error wrap stays `clone %s: %w`; a cancelled clone surfaces `context.Canceled`/`DeadlineExceeded` via `%w`, so callers can `errors.Is`.

### 3.2 Provider `Diff` (flux.go / argocd.go)

`func (p *Provider) Diff(ctx context.Context, c providers.Change) (providers.Diff, error)` → `return p.differ.ForChange(ctx, c)` (flux keeps its empty-URL short-circuit).

### 3.3 Per-investigation deadline (`internal/investigate/loop.go`)

Add field `Timeout time.Duration` to `LoopInvestigator`. At the top of `Investigate`, after the duration/metrics `defer` is set up:

```go
if li.Timeout > 0 {
    var cancel context.CancelFunc
    ctx, cancel = context.WithTimeout(ctx, li.Timeout)
    defer cancel()
}
```

When a model/tool call returns a ctx error, the loop already returns `fmt.Errorf("model: %w", err)` for model errors; for the deadline we additionally detect `ctx.Err()` after the model call (or a tool returning a ctx error) and deliver a timeout result rather than bubbling a bare error that the queue would just retry. Concretely: after `li.Model.Complete` returns `err`, if `errors.Is(ctx.Err(), context.DeadlineExceeded)` → log, `InvestigationsDropped++`, `result="timeout"`, `deliver(req, timeoutResult(req))`, `return nil`.

`timeoutResult(req)` mirrors `budgetKillResult` (in `budget.go`): an unresolved investigation noting the deadline.

### 3.4 Config (`config.go` / `load.go`)

`Investigation.MaxDuration Duration `yaml:"max_duration"`` // 0 ⇒ disabled. No default applied (opt-in). Wired in `cmd/lore/main.go` at all three `LoopInvestigator{}` construction sites (`Timeout: cfg.Investigation.MaxDuration.Std()`).

### 3.5 R4 doc/comment corrections

- `providers.go:72` Change doc: "revision history + a Git diff for the change a revision introduced (the latest applied commit against its parent)" — drop "between two deployed revisions".
- `flux.go Changes` doc + `revisionRange` doc: clarify the healthy path diffs the latest applied commit against its git parent, not a prior deployed revision; note R4-full (prior-revision span) is deferred for lack of cross-reconciliation state.

## 4. Invariants
- `Timeout == 0` ⇒ no `WithTimeout` ⇒ behaviour unchanged.
- A cancelled/timed-out clone or patch returns a wrapped ctx error (`errors.Is` works).
- Deadline delivers via the normal `deliver` path (`OnComplete` fires once), result labelled `timeout`.
- R4 doc change is comment-only; no behaviour change.

## 5. Tests
- **differ_test:** `TestRemoteCancelledCtx` — cancel ctx before `Remote`; assert error `errors.Is(context.Canceled)` and empty diff. `TestForChangeCancelledCtx` — same via `ForChange` (empty FromRev → `RemoteFromParent`). Existing tests updated to pass `context.Background()`.
- **flux_test / argocd_test:** existing `Diff` callers updated to pass ctx; (flux empty-URL short-circuit test still returns empty diff).
- **loop_test:** `TestInvestigateDeadline` — a model whose `Complete` blocks on `<-ctx.Done()` then returns `ctx.Err()`; `Timeout` small; assert `Investigate` returns nil, `OnComplete` fired once with a timeout `Unresolved` entry, bounded well under any `MaxSteps`. `TestInvestigateNoDeadlineWhenZero` — `Timeout==0` leaves a normal scripted run unaffected.
- Race: `go test -race ./internal/whatchanged/ ./internal/providers/gitops/flux/ ./internal/investigate/`.

## 6. Out of scope (deferred)
- **R4-full:** persist prior `lastAppliedRevision` per Kustomization to diff the true deploy span; set `Change.When` from commit time. Needs new state + a git lookup.
- **Per-repo clone cache** in the Differ (the `RemoteFromParent` perf note).
- **A dedicated `investigations_timed_out_total` counter** (reuse `InvestigationsDropped` for now).
