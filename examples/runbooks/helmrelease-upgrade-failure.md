---
type: Playbook
title: HelmRelease upgrade/install failure
description: Diagnose a Helm release stuck after an upgrade (Flux HelmRelease or ArgoCD Application running a chart).
resource: helmrelease://*
tags: [flux, argocd, helmrelease, helm, upgrade, gitops]
timestamp: 2026-06-20T00:00:00Z
---

# Symptom

`Ready=False` / `Released=False` on a Flux HelmRelease, or `Degraded`/`OutOfSync` on an ArgoCD
Application, shortly after a chart or values change. Often surfaces downstream as probe failures or
gateway 5xx.

# Investigate

1. `what_changed_near(target, alert_time)` — identify the chart/values bump (from → to revision).
2. `diff_revisions(...)` — read the **actual** values/manifest delta that landed.
3. Pull the managed resources from the release inventory; check the failing one's status, events, logs.
4. Correlate around the change time: metrics (e.g. `*_up`, restarts, saturation) + log error patterns.

# Common causes

- Chart rendering error: invalid or missing values after the bump.
- A migration/job introduced or changed by the new chart version stalls.
- A CRD required by the new version is not installed.
- A `valuesFrom` ConfigMap/Secret is missing or a key was renamed.

# Resolution

- **Reversible first:** roll back to the previous revision (Flux: `flux rollback hr/<name>`;
  ArgoCD: roll back to the previous history revision).
- **Then fix forward:** correct the values / install the CRD / release the stuck migration lock.

# Citations

- Flux HelmRelease failure patterns (upstream troubleshooting docs).
