# Durability: opt-in RWX volume + audit fsync

Date: 2026-06-23
Slice: #14 durability
Status: accepted

## Problem

The outcome ledger (`outcome.ledger_path`, the learning signal) and the
hash-chained audit log (`actions.audit_log_path`, the tamper-evident accountability
record) both live under the catalog data path (`/var/lib/runlore/catalog`, the
`catalog` volume `mountPath`). In the shipped Helm chart that volume is an
`emptyDir` (rendered only on the `catalog.gitSync` branch), so both files are
**destroyed on every pod restart, upgrade, or leader failover** — even though the
audit package docstring promises a "durable, ordered, complete record … [that]
survives process restarts."

Two independent gaps:

1. **Storage gap (Helm):** an `emptyDir` cannot survive pod replacement, and the
   `Deployment` uses `Recreate` + leader election, so an upgrade tears the pod
   down entirely. There is no persistent backing.
2. **Write gap (Go):** `audit.Logger.Log` writes via an `io.Writer` with no
   `fsync`, so even on a durable volume an unclean crash can lose the tail of the
   hash chain (the most recent, most relevant records).

## Scope

- Opt-in persistent volume for the writable catalog/ledger/audit data path (Helm).
- `fsync` after each audit write when the Logger is backed by a real file (Go).
- Fix the misleading `values.yaml` comment that points `ledger_path` at a
  non-existent "git-sync mirror PV".

Out of scope (deferred): see below.

## Decision

**Opt-in ReadWriteMany shared volume, default off.**

- Add a `persistence` block to `values.yaml`, **disabled by default**. When
  enabled it provisions (or reuses) a PVC and mounts it at the existing
  `catalog.mountPath`, so the ledger and audit log survive restarts *and* leader
  failover. When disabled, behavior is **byte-identical to today** (`emptyDir`).
- **ReadWriteMany** (e.g. EFS / Filestore) is the access mode because the
  `Deployment` runs 2 replicas with leader election: the standby must be able to
  mount the same data so it can read the seeded hash chain and the ledger when it
  takes leadership. A single-writer RWO volume would block the standby's mount.
- A new `templates/pvc.yaml` renders only when `persistence.enabled` **and**
  `persistence.existingClaim` is empty (standard Helm guard). When
  `existingClaim` is set, the Deployment references it directly and no PVC is
  created.
- In `deployment.yaml` the `catalog` volume becomes a `persistentVolumeClaim`
  when `persistence.enabled`, else falls back to `emptyDir: {}`. The mount path
  is unchanged, so nothing downstream of the path changes.

**fsync audit writes.** `audit.Logger` gains an optional `sync func() error`:
`Open(path)` sets it to the `*os.File`'s `Sync`; `NewWriter(io.Writer)` leaves
it nil (an arbitrary writer can't fsync). `Log` calls `sync` after a successful
write, skipping gracefully when nil. The record / hash-chain format is unchanged.

## Why opt-in (the fork)

Forcing a PVC would (a) require RWX storage every cluster may not have, and (b)
change the default render for non-HA / ephemeral users who are fine with
`emptyDir`. Opt-in keeps the zero-config path working and lets durability-needing
operators turn it on with one flag.

## Deferred

- **StatefulSet** with per-replica RWO PVCs (would avoid the RWX requirement but
  needs per-pod ledger reconciliation and a different leader-handoff story).
- **Automatic ledger-path derivation** from the persistence mount (today the
  operator still sets `outcome.ledger_path` / `actions.audit_log_path` by hand;
  the fixed comment documents pointing them at the persistent mount).

## Verification

- `go build ./... && go test ./... && go vet ./...` green.
- `helm template deploy/helm/runlore` (default) renders `emptyDir`, no PVC.
- `helm template deploy/helm/runlore --set persistence.enabled=true` renders a
  PVC and a `persistentVolumeClaim` volume.
- `helm lint deploy/helm/runlore` passes.
