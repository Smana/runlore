# Wire the Queue + Recurrence Phase-2 curation passes — design (roadmap #12 follow-up)

| | |
|---|---|
| **Date** | 2026-06-24 |
| **Roadmap item** | #12 (deferred passes) — light up `Queue` and `Recurrence` |
| **Depends on** | #8 (`Episodes()` ledger read API, merged), #12 (curate CronJob + Dedup/Lifecycle, merged) |
| **Effort** | M |

## Problem

`internal/curate/` has four grooming passes. After #12, `runCurate` wires only `Dedup`
and `Lifecycle`. The other two were deferred because their *correct* wiring needed a
design decision rather than mechanical plumbing:

- **`Queue`** (`resolution.go`) promotes a human-`solved` PR to `ready-to-merge` once
  the incident behind it has resolved (or a human added `accepted`). It needs a real
  `ResolutionChecker` — a way to ask "has this PR's incident cleared?"
- **`Recurrence`** (`recurrence.go`) opens **one** knowledge-gap issue when an
  unresolved pattern recurs N times. Its current `Store`/`Observe` increment model is
  not a `Run()` pass and double-opens if driven naively from a full ledger replay.

This spec wires both, **ledger-backed and idempotent**, reusing #8's `Episodes()`.

## Source-neutrality (design principle)

RunLore's trigger/event sources are **pluggable** (incident webhook, GitOps
failures, timer, chat, CLI). This design does **not** assume Alertmanager. The
resolution signal is the **outcome ledger's `resolve` events** — recorded by whatever
source reports an incident cleared (an Alertmanager resolved-alert today; another
source tomorrow). Both passes read the ledger, never a source-specific API, so they
stay source-agnostic.

## Design

### A. `Queue` — `LedgerResolutionChecker`

The join from a curated PR back to its incident is an **exact title match**, which
holds by construction:

- A curated PR's title is `"KB: " + inv.Title` (`github.OpenPR`).
- The outcome ledger records each open with `Title: inv.Title` (`ledger.Open` in
  `main.go`), surfaced as `Episode.Title`.

```go
// LedgerResolutionChecker reports a PR's incident resolved when the outcome ledger
// holds a resolved episode whose title matches the PR's. Source-agnostic: it reads
// the ledger's resolve events, never a trigger-specific API.
type LedgerResolutionChecker struct {
    Ledger interface{ Episodes() ([]outcome.Episode, error) }
}

func (c LedgerResolutionChecker) IsResolved(ctx context.Context, pr providers.CuratedIssue) (bool, error) {
    title := strings.TrimSpace(strings.TrimPrefix(pr.Title, "KB: "))
    if title == "" {
        return false, nil
    }
    eps, err := c.Ledger.Episodes()
    if err != nil {
        return false, err
    }
    for _, e := range eps {
        if e.Resolved && e.Title == title {
            return true, nil
        }
    }
    return false, nil
}
```

No new markers, no curator (serve-path) change, no new ledger method — it reuses
`Episodes()`. A PR whose incident has not resolved simply waits for a human
`accepted` label, exactly as `Queue` already handles. `Queue.Run` is unchanged.

The checker depends on the small `Episodes()` interface above (not `*outcome.Ledger`
directly) purely for **test-fakeability** — a fake episodes source in tests. There is
no import cycle: `curate` may import `outcome` freely (`outcome` does not import
`curate`); the interface still references `outcome.Episode`.

### B. `Recurrence` — ledger-driven `Run`, idempotent via existence-check

Replace the dormant `Store`/`Observe` increment model with a `Run(ctx)` pass that
recomputes from the ledger each run and is idempotent because the forge's existing
issues are the "already-opened" record (no mutable store, no watermark):

```go
type Recurrence struct {
    Forge     Forge
    Ledger    interface{ Episodes() ([]outcome.Episode, error) }
    Threshold int // default 3 when 0
    Log       *slog.Logger
}

func (r Recurrence) Run(ctx context.Context) error {
    thr := r.Threshold
    if thr == 0 { thr = 3 }
    eps, err := r.Ledger.Episodes()
    if err != nil { return err }

    // Count UNRESOLVED episodes per pattern (affected resource; title fallback).
    counts := map[string]int{}
    for _, e := range eps {
        if e.Resolved { continue }
        counts[recurrencePattern(e)]++
    }

    // Existing knowledge-gap issues — the idempotency guard. Match by title among
    // runlore issues (OpenIssue labels them runlore/triggered, titles them
    // "knowledge-gap: <pattern>").
    existing, err := r.Forge.ListIssuesByLabel(ctx, "runlore")
    if err != nil { return err }
    open := map[string]bool{}
    for _, iss := range existing {
        if p, ok := strings.CutPrefix(iss.Title, gapTitlePrefix); ok {
            open[p] = true
        }
    }

    for pattern, n := range counts {
        if n < thr || open[pattern] { continue }
        inv := providers.Investigation{
            Title: gapTitlePrefix + pattern,
            RootCauses: []providers.Hypothesis{{
                Summary: fmt.Sprintf("RunLore could not resolve incidents on %q across %d occurrences — needs seeded knowledge or a RunLore fix.", pattern, n),
            }},
        }
        if _, err := r.Forge.OpenIssue(ctx, inv); err != nil {
            r.Log.Warn("recurrence: open knowledge-gap issue failed", "pattern", pattern, "err", err)
            continue // best-effort; other patterns still processed
        }
        r.Log.Info("opened knowledge-gap issue", "pattern", pattern, "count", n)
    }
    return nil
}

const gapTitlePrefix = "knowledge-gap: "

// recurrencePattern groups unresolved incidents by the affected resource, falling
// back to the incident title when no resource was identified.
func recurrencePattern(e outcome.Episode) string {
    if e.Resource != "" { return e.Resource }
    return e.Title
}
```

The old `RecurrenceStore` interface + `Observe` method are **removed** (the pass was
never wired, so there are no consumers); `recurrence_test.go` is rewritten for `Run`.

Map-iteration order is irrelevant: each qualifying pattern opens at most one issue per
run, guarded by `open[pattern]`; ordering does not affect the outcome.

### C. Wiring & config (`runCurate`, `cmd/lore/main.go`)

`runCurate` opens the outcome ledger **read-only** (same path the serve process
writes) and adds both passes when it is configured:

```go
agent := curate.Agent{Log: log, Passes: []curate.Pass{
    curate.Dedup{Forge: forge, Log: log},
    curate.Lifecycle{Forge: forge, StaleAfter: cfg.Curate.StaleAfter.Std(), Log: log},
}}
if cfg.Outcome.LedgerPath != "" {
    ledger, err := outcome.New(cfg.Outcome.LedgerPath)
    if err != nil {
        return fmt.Errorf("open outcome ledger: %w", err)
    }
    agent.Passes = append(agent.Passes,
        curate.Queue{Forge: forge, Checker: curate.LedgerResolutionChecker{Ledger: ledger}, Log: log},
        curate.Recurrence{Forge: forge, Ledger: ledger, Threshold: cfg.Curate.RecurrenceThreshold, Log: log},
    )
} else {
    log.Info("curate: outcome ledger not configured; Queue + Recurrence passes skipped")
}
```

- New config field `cfg.Curate.RecurrenceThreshold int` (`yaml:"recurrence_threshold"`,
  default 3 when 0).
- `outcome.New` already tolerates an absent/empty file (read returns no episodes), so a
  ledger configured but not yet written yields no false positives.
- The curate CronJob already mounts the persistence volume that holds the ledger
  (`config.outcome.ledger_path` on `catalog.mountPath`); no chart change is required
  beyond documenting that Queue/Recurrence need `outcome.ledger_path` + the PVC.

### D. Safety & scope

Both passes touch **only the KB forge** (relabel a PR, open an issue), never the
cluster, under the **opt-in curate CronJob**. Knowledge-gap artifacts are *issues*,
and the `Lifecycle` stale-sweep lists *PRs* only (`pull_request != nil`), so gap
issues are never auto-closed. The forge token already needs issues/PR write for
curation; no new RBAC.

## Testing

- `internal/curate/resolution_test.go`
  - keep the existing `Queue.Run` tests (fake checker) — they still pass unchanged.
  - add `TestLedgerResolutionCheckerResolvedTitle` (a resolved episode with the PR's
    title ⇒ true), `…Unresolved` (only unresolved/absent ⇒ false), `…StripsKBPrefix`
    (matches `"KB: X"` against episode title `"X"`), `…EmptyTitle` (false).
  - `TestQueuePromotesResolvedViaLedger` — `Queue` over a fake forge + a fake
    `Episodes()` source, asserting a `solved` PR whose title resolved is relabelled
    `ready-to-merge`, and an unresolved one is not.
- `internal/curate/recurrence_test.go` (rewrite)
  - `TestRecurrenceOpensGapIssueAtThreshold` — 3 unresolved episodes on `apps/web`,
    none on a 1-occurrence resource ⇒ one issue titled `knowledge-gap: apps/web`.
  - `TestRecurrenceBelowThresholdNoIssue`.
  - `TestRecurrenceIdempotentWhenIssueExists` — an existing `knowledge-gap: apps/web`
    issue ⇒ no duplicate even at threshold.
  - `TestRecurrenceResolvedEpisodesDoNotCount`.
  - `TestRecurrencePatternFallsBackToTitle` — a resource-less episode groups by title.
- `internal/config/config_test.go` — `curate.recurrence_threshold` parses.
- Fakes: a `fakeEpisodes` implementing `Episodes() ([]outcome.Episode, error)`; reuse
  the existing `recordingForge`/`gapForge` style for forge assertions.

## Out of scope

- Embedding alert fingerprints on PRs / a `ResolvedFingerprints()` ledger method (the
  exact-title join is sufficient; this was the heavier alternative).
- An e2e for the CronJob passes (the CronJob is opt-in/manual; unit coverage is the
  gate). The existing k3d e2e is unaffected.
- Changing how resolution is *recorded* (that is the source-pluggable trigger layer,
  unchanged here).

## Files touched

- `internal/curate/resolution.go` — `LedgerResolutionChecker`.
- `internal/curate/recurrence.go` — ledger-driven `Run`; remove `Store`/`Observe`.
- `internal/config/config.go` — `Curate.RecurrenceThreshold`.
- `cmd/lore/main.go` — open the ledger read-only; wire Queue + Recurrence.
- `internal/curate/resolution_test.go`, `internal/curate/recurrence_test.go`,
  `internal/config/config_test.go` — tests above.
- `deploy/helm/runlore/values.yaml` — comment that Queue/Recurrence need
  `config.outcome.ledger_path` + persistence (no behavioral chart change).
