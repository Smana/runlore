# CronJob outcome-ledger visibility — design (item R12)

| | |
|---|---|
| **Date** | 2026-06-24 |
| **Item** | R12 — the curate CronJob may silently no-op the Queue + Recurrence passes |
| **Status** | accepted |
| **Effort** | S |

## The reported finding

> The curate CronJob (`deploy/helm/runlore/templates/cronjob.yaml`) mounts only the
> `catalog` volume; the scheduled `lore curate` reads the outcome ledger for its
> Queue + Recurrence Phase-2 passes, and if `config.outcome.ledger_path` isn't under
> `catalog.mountPath` it finds an empty ledger and silently skips them.

## Challenge against the current code — verdict: PARTIAL (visibility gap, not a missing mount)

**Against "missing mount":**

- The CronJob does NOT mount "only the catalog volume." It mounts `config`, `tmp`,
  AND `catalog` (`cronjob.yaml:45-58`) under the **identical** condition as the
  serve Deployment (`deployment.yaml:83-90`): `catalog.configMap` → read-only
  ConfigMap; else `catalog.gitSync || persistence.enabled` → the `catalog` volume
  at `catalog.mountPath` (PVC when `persistence.enabled`, else `emptyDir`).
- The documented durable ledger path is `/var/lib/runlore/catalog/outcomes.jsonl`
  (`values.yaml:93`) — **under** `catalog.mountPath` (`/var/lib/runlore/catalog`).
- `helm template … --set persistence.enabled=true --set curate.cronjob.enabled=true`
  renders the CronJob with `persistentVolumeClaim: claimName: <fullname>-data`
  mounted at `/var/lib/runlore/catalog`. So in the **documented** durable config the
  serve pod and the CronJob share the same PVC and the same ledger. **The mount is
  already wired correctly — the "missing mount" claim is overstated.**

**Against "silent skip" (the real gap):**

- `outcome.New` succeeds and returns an **empty** ledger when the file is absent:
  `readEvents` returns `nil, nil` on `fs.ErrNotExist` (`internal/outcome/ledger.go:71-72`).
- `runCurate` therefore opens the ledger with no error and logs the **reassuring**
  `"curate: Queue + Recurrence enabled"` (`cmd/lore/main.go:860`, pre-change) even
  when the file the CronJob can see does not exist. The passes then run against zero
  episodes and produce nothing — indistinguishable, in the logs, from "no work to do."
- Real configs that hit this silent no-op:
  - `persistence.enabled=false` with `outcome.ledger_path` set → serve and CronJob
    get **separate** ephemeral `catalog` volumes; the CronJob's ledger is always
    absent/empty.
  - `catalog.gitSync=true` + `persistence.enabled=false` → the CronJob's `catalog`
    is a **fresh `emptyDir` per Job run** → always empty.
  - `outcome.ledger_path` pointed somewhere **not** under a mounted volume.

**Verdict:** PARTIAL. The mount wiring is correct in the documented config; the
genuine defect is that a misconfigured/absent ledger is **silent**. The smallest
real fix is startup visibility, plus chart docs/NOTES that flag the classic
misconfiguration — **not** a new PVC mount (that would be redundant with the existing
`catalog` mount).

## Design

### Go — make the no-op loud (the substantive fix)

1. `outcome.Ledger.Status() outcome.Status` — a cheap, cache-free snapshot of what a
   fresh process actually sees on disk:
   ```go
   type Status struct {
       Path       string // configured ledger path ("" when disabled)
       Configured bool   // a non-empty ledger_path was set
       Present    bool   // the file exists (os.Stat ok; true even when empty)
       Events     int    // replayable events (0 for absent/empty)
   }
   ```
   It re-reads the file (it does not trust the in-memory open-index) so it reflects
   the CronJob pod's reality. No behavior change to the serve path.

2. `logLedgerStartup(log *slog.Logger, s outcome.Status)` in `cmd/lore/main.go` — a
   pure level-decision helper, unit-testable by capturing slog JSON:
   - `!Configured` → `Info` ("not configured; passes skipped") — unchanged for the off case.
   - `!Present` → **`Warn`**: configured but the file is absent here; check the ledger is
     on a volume the CronJob mounts (enable persistence, keep `ledger_path` under
     `catalog.mountPath`).
   - `Present && Events == 0` → **`Warn`**: present but empty; verify serve + curate
     share the same persistent volume, not separate emptyDirs.
   - else → `Info` "Queue + Recurrence enabled" with `events` count.

   `runCurate` calls it instead of the old unconditional info log. The passes still
   run (they are harmless on an empty ledger); only the visibility changes.

### Helm — visibility, no redundant mount

The `catalog` mount already covers the documented path, so **no volume change.** Add:

- **`NOTES.txt`** post-install WARNING gated on
  `curate.cronjob.enabled && dig "outcome" "ledger_path" "" .Values.config && not persistence.enabled`
  — the exact silent-no-op config — telling the operator to enable persistence and
  keep `ledger_path` under `catalog.mountPath`.
- **`values.yaml`** — tighten the `outcome.ledger_path` comment: both the serve
  Deployment (writer) and the curate CronJob (reader) must mount the SAME persistent
  volume; with persistence off they get separate ephemeral volumes and the passes
  read an empty ledger (and the command now warns at startup).

## Why this design (the fork)

A redundant PVC mount was rejected: the CronJob already mounts `catalog` on the same
condition as the Deployment, so an extra mount would duplicate it and drift from the
serve path. The defensible fix for "silent skip" is to **make the failure
observable** — a startup `Warn` the operator sees in `kubectl logs job/…-curate`,
plus an install-time NOTES warning for the misconfiguration that produces it. This is
the smallest change that genuinely closes the gap the item describes.

## Verification

- `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` (`0 issues`).
- `internal/outcome`: `Status()` distinguishes disabled / configured-but-absent /
  present-empty / present-with-events.
- `cmd/lore`: `logLedgerStartup` emits `WARN` for absent & empty, `INFO` for present
  & disabled (asserted via captured slog JSON).
- `helm lint deploy/helm/runlore` passes; `helm template … --set persistence.enabled=true
  --set curate.cronjob.enabled=true` shows the CronJob mounting the PVC at
  `catalog.mountPath`; the NOTES gate renders WARN only in the misconfig case
  (validated via a `--show-only` debug template since `helm template` omits NOTES.txt).

## Files touched

- `internal/outcome/ledger.go` — `Status` type + `Ledger.Status()`.
- `internal/outcome/ledger_test.go` — `Status` cases.
- `cmd/lore/main.go` — `logLedgerStartup`; call it from `runCurate`.
- `cmd/lore/curate_test.go` — `logLedgerStartup` level assertions.
- `deploy/helm/runlore/templates/NOTES.txt` — misconfig WARNING.
- `deploy/helm/runlore/values.yaml` — clarified `outcome.ledger_path` comment.

## Out of scope

- Automatic derivation of `ledger_path` from the persistence mount (operator still
  sets it; the comment + NOTES document the constraint).
- Failing `lore curate` hard on an empty ledger — the passes are harmless no-ops, so a
  warning (not an error) is the right severity; a present-but-empty ledger is also the
  legitimate first-run state.
