---
type: Incident
title: harbor-core readiness fails after a chart bump — stuck database migration
description: A Harbor chart upgrade runs a schema migration that deadlocks; harbor-core never passes readiness and the gateway returns 5xx until the migration lock is cleared.
resource: apps/harbor-core
tags: [harbor, helmrelease, migration, readiness, database, gitops]
timestamp: 2026-06-28T09:14:00Z
last_validated: 2026-07-19T08:02:00Z
---

# Symptom

`HarborProbeFailure` fires for `apps/harbor-core`: readiness probes fail shortly after a
HelmRelease chart bump. The pod stays Running — it never crash-loops — so pod status alone
looks healthy while every request through the gateway returns 5xx.

# Cause

The new chart version ships a schema migration that acquires an exclusive lock and does not
release it. `harbor-core` blocks on the migration at startup, so `/readyz` never succeeds.
The pod is up, the container is fine, and the fault is entirely in the database session.

# Resolution

1. Confirm the migration is the blocker — `harbor-core` logs stop at the migration step with no
   subsequent progress line.
2. Clear the stale migration lock in the Harbor database.
3. If the lock cannot be cleared safely, roll the HelmRelease back to the previous chart
   revision; readiness recovers within a minute and the migration can be re-run off-peak.

Rolling back is the reversible option and the one to prefer while the incident is live.

# Notes

Recurs on every chart bump that carries a migration. The tell is a **Running but never Ready**
pod combined with a HelmRelease revision change in the same window — restarts stay at zero
throughout, which is what distinguishes this from an ordinary crash-loop.
