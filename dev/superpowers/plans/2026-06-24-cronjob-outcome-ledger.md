# CronJob outcome-ledger visibility — implementation plan (item R12)

> Spec: `dev/superpowers/specs/2026-06-24-cronjob-outcome-ledger-design.md`.
> Verdict: PARTIAL — the curate CronJob already mounts `catalog` like the
> Deployment; the real gap is that an absent/empty ledger is a **silent** no-op.
> Fix = startup visibility (Go) + NOTES/values clarity (Helm). No new PVC mount.

**Goal:** `lore curate` warns loudly when the Queue + Recurrence passes run against
a ledger the CronJob pod cannot see (absent/empty), and the chart's NOTES/values
flag the config that causes it.

## Global constraints

- Stdlib `testing`, NO testify; `%w` errors.
- No behavior change to the serve path; `Status()` is read-only.
- Full gate green before commit: `go build ./... && go vet ./... && go test ./... &&
  gofmt -l . && golangci-lint run ./...` (`0 issues`); `helm lint` +
  `helm template` clean.

---

### Task 1: `outcome.Ledger.Status()` (TDD)
**Files:** `internal/outcome/ledger.go`, `internal/outcome/ledger_test.go`.
**Produces:** `Status{Path, Configured, Present, Events}`, `(*Ledger).Status()`.
- [x] Test: disabled (path "") → `Configured=false`; configured-but-absent →
  `Configured=true, Present=false, Events=0`; present-with-events → `Present=true,
  Events=N`; present-but-empty file → `Present=true, Events=0`.
- [x] Impl: `os.Stat` for Present, `readEvents` for Events; cache-free. Test green; commit.

### Task 2: `logLedgerStartup` + wire into `runCurate` (TDD)
**Files:** `cmd/lore/main.go`, `cmd/lore/curate_test.go`.
**Produces:** `logLedgerStartup(*slog.Logger, outcome.Status)`.
- [x] Test (capture slog JSON into a buffer): absent → `WARN`; present-empty → `WARN`;
  present-with-events → `INFO`; disabled → `INFO` (never WARN).
- [x] Impl: switch on `Status`; replace the unconditional `"Queue + Recurrence
  enabled"` info log in `runCurate` with `logLedgerStartup(log, ledger.Status())`
  (and `outcome.Status{}` on the disabled branch). Test green; commit.

### Task 3: Helm visibility — NOTES + values comment
**Files:** `deploy/helm/runlore/templates/NOTES.txt`, `deploy/helm/runlore/values.yaml`.
- [x] NOTES: post-install WARNING gated on `and curate.cronjob.enabled
  (dig "outcome" "ledger_path" "" .Values.config) (not persistence.enabled)`.
- [x] values: clarify `outcome.ledger_path` — serve writer + curate reader must mount
  the SAME persistent volume; persistence-off ⇒ separate ephemeral volumes ⇒ empty
  ledger ⇒ startup warning.
- [x] Validate: `helm lint` clean; `helm template --set persistence.enabled=true
  --set curate.cronjob.enabled=true` shows the CronJob PVC mount; NOTES gate yields
  WARN only in the misconfig case (verified via a throwaway `--show-only` debug
  template, since `helm template` does not render `NOTES.txt`). Commit.

## Success criteria

- SC-1 A misconfigured/absent ledger during `lore curate` emits a clear `WARN`
  (not a silent skip) — `cmd/lore` test asserts the level.
- SC-2 The documented durable config (`persistence.enabled=true`,
  `ledger_path` under `catalog.mountPath`) renders the CronJob mounting the shared
  PVC and emits `INFO` (no warning).
- SC-3 `helm lint` passes; the NOTES warning renders only for cronjob-on +
  ledger-set + persistence-off.
- SC-4 Full Go gate green; serve path unchanged.
