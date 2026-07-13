---
type: Incident
title: Harbor HelmRelease stuck InstallFailed after a cluster capacity shortage
description: Harbor never finished installing; its HelmRelease is terminal-failed (timeout waiting for Deployments/StatefulSets) because the cluster could not schedule the pods in time, and Flux exhausted its install retries.
resource: tooling/harbor
tags:
    - runlore
    - incident
    - flux
    - helmrelease
    - harbor
    - capacity
    - karpenter
---

## Symptom

The `tooling/harbor` HelmRelease is `Ready=False / InstallFailed` and never recovers on its own. This
is an instance of [[helmrelease-terminal-failed-exhausted-retries]], triggered by the cluster-wide
capacity shortage in [[karpenter-ec2nodeclass-ami-not-found]] — not by any application change.

## Cause

Confidence: high (verified against live state).

1. **The Harbor install timed out while the cluster had no schedulable capacity, then Flux exhausted
   its retries and left the HelmRelease terminal-failed.**
   - evidence: `kubectl -n tooling get hr harbor` → `InstallFailed: timeout waiting for: [Deployment/harbor-nginx, harbor-registry, harbor-core, harbor-portal, harbor-jobservice, StatefulSet/harbor-trivy ... 'InProgress']`.
   - evidence: `helm -n tooling history harbor` → a single revision, `status: failed`, timestamped in the cluster's capacity-shortage window (Karpenter could not provision nodes, so the pods sat Pending past the Helm wait).
   - evidence: `harbor-core` shows ~37 restarts — it crash-looped waiting on its dependencies (harbor DB / valkey) which were themselves Pending during the shortage.
   - **Not** caused by the `crds-actions-runner-controller` change an earlier automated pass guessed: that was correlation (a recent cluster change) with no read diff and no causal link to Harbor — rejected on review.

## Resolution

The capacity root is already fixed (Karpenter EC2NodeClass AMI corrected — see
[[karpenter-ec2nodeclass-ami-not-found]]). With nodes available, clear the terminal HelmRelease so
Flux re-installs cleanly:

```
# only the failed release exists (no good revision to roll back to)
helm -n tooling uninstall harbor          # StatefulSet PVCs persist
flux -n tooling reconcile helmrelease harbor --reset   # clears exhausted retry counters → fresh install
```

`--reset` is the key step: without it Flux refuses to retry ("exceeded maximum retries: cannot
install release") even though capacity is now available. Verify with `kubectl -n tooling get hr harbor`
→ `Ready=True` and the harbor Deployments/StatefulSet becoming Ready.

> This is a shared, multi-component production release — run the uninstall/reset deliberately
> (human-approved), not as part of a blind bulk sweep.

## Prevention

- Raise `spec.install.timeout` for large multi-component charts so transient scheduling delays don't
  burn all retries.
- Keep autoscaling healthy: a single capacity root (a broken Karpenter NodeClass) knocks out many
  HelmReleases at once via simultaneous install timeouts.
