# Argo CD provider — closing the genuine gaps vs Flux (R24)

Status: in progress
Date: 2026-06-24
Scope: `internal/providers/gitops/argocd/`

## Problem (R24)

The Argo CD `GitOpsProvider` is ~1 tier shallower than the Flux one. Three
specific gaps were flagged. Each is challenged against the CURRENT code below,
with a verdict and `file:line`, before any change.

### Gap 1 — Multi-source apps silently dropped — CONFIRMED

`applicationFromUnstructured` (`dynamic.go:79-81`) reads only the **single-source**
fields: `spec.source.repoURL`, `spec.source.path`, `status.sync.revision`. An
Argo CD Application that uses the multi-source schema (`spec.sources[]` — plural,
mutually exclusive with the singular `spec.source`) therefore yields an
`application` with empty `RepoURL`/`Revision`. `Changes` (`argocd.go:68`) then
hits `if a.RepoURL == "" || a.Revision == "" { continue }` and drops the app —
**with no log line**. So it is a *silent* drop, not graceful degradation.

Verdict: real gap. A multi-source app contributes nothing to the change spine and
leaves no trace that it was skipped.

Fix (pragmatic, defensible): read the **first** source of `spec.sources[]` (its
`repoURL`/`path`) and the **first** synced revision of `status.sync.revisions[]`
when the singular fields are absent. This makes the common multi-source app
(one Git source + one Helm/values source) produce a real, diffable Change instead
of vanishing. When neither schema yields a usable RepoURL+Revision, emit a single
`Debug` log noting the skip (so the blind spot is observable) instead of dropping
silently.

Why "first source" and not "a Change per source": the engine-agnostic `Change`
carries exactly one `SourceRef`. Fanning out one Change per source would change
the spine's cardinality and ripple into dedup/diff/blast-radius semantics far
beyond this provider — out of scope. The first source is, in practice, the Git
source that backs the app's manifests (the values/chart sources are refs); it is
the most defensible single pick and matches what a human reads first. Documented
in code.

### Gap 2 — Failure trigger is health-only — CONFIRMED

`WatchFailures` (`argocd.go:100`) triggers only on `HealthStatus == "Degraded"`.
A **failed sync operation** surfaces in `status.operationState.phase ∈ {Failed,
Error}` (the `OperationPhase` enum is Running/Terminating/Failed/Error/Succeeded).
A sync can fail (e.g. a bad manifest, a hook error) while health is still
`Healthy`/`Progressing` — that failure is currently missed entirely.

Verdict: real gap. Add `operationState.phase ∈ {Failed,Error}` as an additional
trigger. Carry the new field `OperationPhase` on `application`, read it in
`applicationFromUnstructured`, and fire a FailureEvent when health is Degraded
**or** the last sync operation phase is Failed/Error. `Reason` reflects which
condition fired so the downstream investigation has accurate attribution.

Note: the R24 text also mentioned `OutOfSync`. The current code does **not** key
on OutOfSync, and OutOfSync alone is not a failure — it is the steady state of any
app with auto-sync disabled or mid-drift. Triggering on it would flood the React
loop with non-failures. Decision: do **not** add OutOfSync as a failure trigger;
the genuine missing signal is the failed operation phase. Documented here.

### Gap 3 — Events dropped under backpressure — CONFIRMED

`dynamic.go:56-60` `send` does `select { case out<-ev: case <-ctx.Done(): default: }`.
The `default:` makes the send non-blocking: when the buffered (128) channel is
full, the event is **silently dropped**. The informer's periodic resync
(10 min) re-delivers current state eventually, which *masks* the drop as a delayed
blind spot rather than a hard loss — but a failure event that fires and is dropped
won't trigger a timely React investigation.

Verdict: real gap. Replace the `default:` drop with a **bounded** blocking send: a
`time.NewTimer`-gated `select` that blocks up to a small timeout for the consumer
to drain, and only then gives up (logging the drop). This preserves the
"never block the informer indefinitely" invariant (a stuck consumer must not wedge
the shared informer) while eliminating the silent instant-drop on a transient
burst.

## GitOpsInspector — decision: OUT OF SCOPE

Flux implements the optional `providers.GitOpsInspector` (ResourceStatus +
DependencyTree) over its `dependsOn`/`sourceRef` graph. Argo CD's model is
different: an Application is a self-contained sync unit; there is no first-class
`dependsOn` between Applications (sync waves order resources *within* an app, and
App-of-Apps nesting is just another Application syncing child Applications). A
faithful Argo inspector would need a different traversal (app → managed resources
via `status.resources[]`, or app-of-apps child refs), which is a separate design,
not "parity." R24 explicitly says "you don't have to reach full Flux parity, just
close the genuine gaps." Consumers already type-assert for `GitOpsInspector` and
degrade when it is absent, so omitting it is safe. Decision: do not add it now;
recorded as a follow-up.

## Tests (test-first, stdlib `testing`, dynamic fake for the reader)

1. `applicationFromUnstructured` on a multi-source object (`spec.sources[]`,
   `status.sync.revisions[]`) maps the first source's repoURL/path and the first
   revision.
2. `Changes` yields a Change for a multi-source app (proves it is no longer
   dropped).
3. `applicationFromUnstructured` reads `operationState.phase`.
4. `WatchFailures` fires for an app whose `operationState.phase == "Failed"` (or
   `"Error"`) even when health is not Degraded; the Reason reflects the sync
   failure. Health-Degraded still fires (unchanged).
5. A bounded send does not silently drop: a fast consumer receives a burst of
   events through `WatchApplications` without loss (and the `-race` run stays
   clean).

## Gate (before each commit)

`go build/vet/test ./...` · `gofmt -l .` · `golangci-lint run ./...` (0 issues,
gosec on) · `go test -race ./internal/providers/gitops/argocd/`.

## Non-goals / deviations

- No `Change`-per-source fan-out (cardinality change — out of scope).
- No OutOfSync failure trigger (non-failure steady state).
- No GitOpsInspector for Argo (different model; follow-up).
- Build on the existing `ctx`-threaded `Diff` signature (R4+R5) — do not revert.
