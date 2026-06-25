# Plan — Argo CD parity gaps (R24)

Spec: `2026-06-24-argocd-parity-design.md`. Test-first, stdlib `testing`, dynamic
fake reader. Full gate (build/vet/test/gofmt/lint + `-race` on the argocd pkg)
green before each commit.

## Step 1 — Gap 1: multi-source sources read (no silent drop)
- Test: `applicationFromUnstructured` on a `spec.sources[]` / `status.sync.revisions[]`
  object → first source + first revision; multi-source history → prev from
  `.revisions[0]`. `Changes` yields a Change for the multi-source app.
- Impl: `sourceRepoPath` / `syncRevision` fall back from singular to first of the
  plural arrays; `prevRevision` handles `.revisions[]`; `Changes` logs the skip at
  Debug instead of dropping silently.

## Step 2 — Gap 2: failed sync operation triggers a failure
- Test: `WatchFailures` fires for `operationState.phase ∈ {Failed,Error}` even when
  health is not Degraded; OutOfSync alone does not fire; health-Degraded unchanged.
  `failureReason` table test.
- Impl: carry `OperationPhase` on `application`, read `operationState.phase`; add
  `failureReason` (Degraded OR Failed/Error), used by `WatchFailures`.

## Step 3 — Gap 3: bounded send (no silent backpressure drop)
- Test: `sendEvent` blocks for a slightly-late consumer (no drop); ctx cancel
  returns promptly.
- Impl: replace the non-blocking `default:` with a `time.NewTimer(sendTimeout)`-
  gated select that blocks up to 5s, then logs and gives up.

## Decisions (documented in spec)
- First-source pick, not Change-per-source (cardinality out of scope).
- No OutOfSync failure trigger (steady state, not a failure).
- No GitOpsInspector for Argo (different dependency model; follow-up).
- Build on the existing ctx-threaded `Diff` (R4+R5) — not reverted.

## Commits
1. spec + plan.
2. Gap 1 (multi-source) + tests.
3. Gap 2 (sync-failure trigger) + tests.
4. Gap 3 (bounded send) + tests.

(Implementation landed cohesively; commits below reflect the actual split.)
