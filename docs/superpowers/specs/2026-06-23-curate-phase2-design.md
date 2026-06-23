# Phase-2 curation: scheduler + lifecycle sweep — design (roadmap #12)

| | |
|---|---|
| **Date** | 2026-06-23 |
| **Roadmap item** | #12 — `lore curate` CronJob + light up the dormant Phase-2 passes |
| **Depends on** | #8 (`Episodes`/`OpenCounts`, merged), #14 (ledger PVC, merged) |
| **Effort** | M |

## Problem

The Phase-2 grooming passes are built and tested in `internal/curate/`
(`dedup`, `lifecycle`, `recurrence`, `resolution`), but `runCurate`
(`cmd/lore/main.go:785`) wires **only `Dedup`**, and there is **no scheduler** — the
chart has no CronJob, so the only groom is a manual one-shot CLI. The result
(analysis §3.5): knowledge compounds only as fast as humans merge (~14% baseline),
and duplicate/stale KB PRs accumulate.

## Scope decision (and why)

Of the three dormant passes, exactly one — **Lifecycle** — is cleanly and safely
wireable now: `CuratedIssue` simply needs the `updated_at` GitHub already returns,
and a staleness window. The other two require genuine design decisions that should
not be shipped autonomously:

- **Queue (`ResolutionChecker`)** needs to map a curated PR back to its incident's
  *current* resolution state. The PR body carries the #11 **dup**-fingerprint
  (resource+cause), while the outcome ledger keys on the **alert** fingerprint — they
  don't join, and a cluster-state checker (re-query alerts/resources per PR) is a
  product decision with real failure modes.
- **Recurrence** needs idempotent "open one gap-issue when a pattern recurs N times."
  Its existing `Observe`/`RecurrenceStore` is an increment-once-per-occurrence model;
  driving it from `ledger.Episodes()` (which returns *all* episodes every run)
  requires either a processed-watermark or an existence-check rework — also a design
  call.

**This spec therefore delivers the scheduler + the Lifecycle pass** (moving curation
from "manual, dedup-only" to "scheduled, dedup + stale-sweep") and **defers Queue and
Recurrence** as a documented follow-up, updating `runCurate`'s admitting comment to
state precisely what each still needs. This is the safe, high-value subset; the
deferred passes are a focused follow-up, not abandoned.

## Design

### 1. `updated_at` on the curated view

- Add `UpdatedAt time.Time` to `providers.CuratedIssue`.
- In `internal/forge/github/github.go`, add `UpdatedAt time.Time \`json:"updated_at"\``
  to `rawIssue` (GitHub populates it on the issues list) and propagate it in
  `curated()`. Existing callers are unaffected (new field, zero value when absent —
  e.g. in tests/`ListIssuesByLabel` fakes).

### 2. Lifecycle uses `UpdatedAt` directly

Now that the timestamp is on `CuratedIssue`, replace `Lifecycle`'s placeholder
`Stale func(number int) bool` callback with intrinsic staleness:

```go
type Lifecycle struct {
	Forge     Forge
	StaleAfter time.Duration // 0 disables the sweep
	Now       func() time.Time // injectable clock; nil ⇒ time.Now
	Log       *slog.Logger
}
```

`Run` skips protected PRs (unchanged `protectedLabels`/`isProtected`), and closes a
PR only when `StaleAfter > 0` and `now.Sub(pr.UpdatedAt) > StaleAfter` **and**
`pr.UpdatedAt` is non-zero (never close an artifact whose age is unknown). The
comment-before-close safety (preserve the "why") is unchanged. This removes the
awkward by-number callback and the double-fetch it implied.

### 3. Wire Dedup + Lifecycle in `runCurate`

- Add config `Curate { StaleAfter Duration `yaml:"stale_after"` }` under the top-level
  config (default **720h** = 30 days when unset; `StaleAfter <= 0` disables the
  lifecycle sweep so it's opt-in-by-config).
- `runCurate` builds the passes:
  ```go
  agent := curate.Agent{Log: log, Passes: []curate.Pass{
      curate.Dedup{Forge: forge, Log: log},
      curate.Lifecycle{Forge: forge, StaleAfter: cfg.Curate.StaleAfter.Std(), Log: log},
  }}
  ```
- Update the dormant-passes comment to: Dedup + Lifecycle wired; **Queue** still needs
  a resolution checker (alert/ledger join), **Recurrence** still needs an idempotent
  ledger-backed driver — follow-up.

### 4. CronJob in the chart (opt-in)

New `deploy/helm/runlore/templates/cronjob.yaml`, gated on
`curate.cronjob.enabled` (default **false** — no behavior change for existing
installs). It mirrors the Deployment's image, config mount, env/envFrom (GitHub App
creds), securityContext, and the catalog/persistence volume (so the curate run reads
the same config + ledger), but runs `args: [curate, --config, /etc/runlore/runlore.yaml]`
on `curate.cronjob.schedule` (default `"0 * * * *"` — hourly). `concurrencyPolicy:
Forbid`, `successfulJobsHistoryLimit`/`failedJobsHistoryLimit` set, `restartPolicy:
Never`. `values.yaml` gains a top-level deploy block:

```yaml
curate:
  cronjob:
    enabled: false
    schedule: "0 * * * *"
```

The staleness window is **app config**, not a deploy value: the ConfigMap renders
`.Values.config` verbatim (`toYaml`), so it is set under `config.curate.stale_after`
in `values.yaml` and the CronJob reads it from the same mounted `runlore.yaml`. No
`configmap.yaml` change is needed.

## Testing

- `internal/forge/github/github_test.go`
  - `TestListPRsByLabelParsesUpdatedAt` — a stub `updated_at` in the JSON response
    surfaces as `CuratedIssue.UpdatedAt` (non-zero, parsed).
- `internal/curate/lifecycle_test.go` (update + add)
  - Rework the existing stale test to set `UpdatedAt` on fixtures + `StaleAfter`/`Now`.
  - `TestLifecycleClosesOnlyAgedUnprotected` — a PR older than `StaleAfter` and
    unprotected closes; a fresh one and a protected old one do not.
  - `TestLifecycleZeroStaleAfterDisables` — `StaleAfter == 0` closes nothing.
  - `TestLifecycleUnknownAgeNotClosed` — a zero `UpdatedAt` is never closed.
- `internal/config/config_test.go` (if present) or a focused parse test — `curate.stale_after` parses to a `Duration`.
- Chart: `helm template` renders the CronJob only when `curate.cronjob.enabled=true`,
  with the curate args and the config mount (validated by a `helm template` smoke in
  the task, plus YAML well-formedness).
- Existing curate/forge tests stay green (the `Forge` fakes gain nothing; `CuratedIssue`'s new field defaults to zero).

## Out of scope (documented follow-up)

- **Queue / `ResolutionChecker`** — needs the incident-resolution join (alert
  fingerprint ↔ curated PR) or a cluster-state checker.
- **Recurrence wiring** — needs an idempotent ledger-backed driver (watermark or
  gap-issue existence-check) over `ledger.Episodes()`.
- Both remain implemented + unit-tested in `internal/curate`; only their production
  wiring is deferred.

## Files touched

- `internal/providers/providers.go` — `CuratedIssue.UpdatedAt`.
- `internal/forge/github/github.go` — parse + propagate `updated_at`.
- `internal/curate/lifecycle.go` — intrinsic staleness via `UpdatedAt`/`StaleAfter`/`Now`.
- `internal/config/config.go` — `Curate.StaleAfter`.
- `cmd/lore/main.go` — `runCurate` wires Dedup+Lifecycle; updated comment.
- `deploy/helm/runlore/templates/cronjob.yaml` — **new**, opt-in.
- `deploy/helm/runlore/values.yaml` — `curate.cronjob.*` (deploy) + `config.curate.stale_after` (app config, rendered into the ConfigMap by the existing `toYaml`).
- corresponding `_test.go` files above.
