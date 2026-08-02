---
type: Playbook
title: Argo CD Application stuck OutOfSync and never converging
description: An Argo CD Application stays OutOfSync sync after sync - or flaps between Synced and OutOfSync - because something in the cluster keeps mutating the resource Argo manages; alerts such as ArgoCDAppOutOfSync or ArgoAppNotSynced fire.
tags: [argocd, argo-cd, application, gitops, outofsync, drift, selfheal, ArgoCDAppOutOfSync, ArgoAppNotSynced, ArgoCdAppOutOfSync, ArgoCDAppSyncFailed, ignoreDifferences]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

An Argo CD `Application` reports `OutOfSync` and stays there — or oscillates
`Synced → OutOfSync → Synced` on a short cycle even with `selfHeal: true`. Syncs report
success; the diff comes straight back. Community alert names for this state include
`ArgoCDAppOutOfSync`, `ArgoAppNotSynced`, and `ArgoCdAppOutOfSync` (there is no upstream
Argo CD rule bundle — check your platform's rules for the exact name).

The flapping variant is the important tell: it means a **write loop**, two parties writing
the same field, not a one-off drift.

# Investigate

1. `argocd app diff <app>` — read the field-level diff. Ask "who else writes this field?"
   before anything else; the answer is usually in the diff itself.
2. `kubectl -n <ns> get <kind>/<name> -o yaml --show-managed-fields` — `managedFields` names
   the other writer by field manager (an operator, a mutating webhook, the HPA, a mesh
   injector).
3. `argocd app get <app> -o json` and check `status.conditions` for `ComparisonError` — a
   comparison error means Argo could not *generate* the desired manifests, which looks like
   OutOfSync but is a generation failure.
4. `argocd app history <app>` — a growing history with no Git commits behind it means
   self-heal is syncing in a loop.
5. Check `spec.syncPolicy` — `automated.selfHeal`, `automated.prune`, and any
   `ignoreDifferences` already declared.
6. For Helm-sourced apps, render the same values locally (`helm template`) and compare with
   what Argo produced. A chart that injects a timestamp, random value, or checksum
   annotation is never stably synced.

# Common causes

- A **mutating admission webhook** (service mesh sidecar injector, policy engine defaulting
  labels, image-tag mutator) rewrites the object right after Argo applies it.
- An **HPA** manages `spec.replicas` while Git also declares it. Every scale event is drift.
  Remove `replicas` from Git or declare it under `ignoreDifferences`.
- A Kubernetes **defaulted field** Argo did not expect for that kind, so it diffs forever.
- A **chart that renders non-deterministically** — `randAlphaNum`, `now`, a config checksum
  annotation over a Secret that itself changes.
- Two controllers own the object (a Flux Kustomization and an Argo Application both applying
  the same manifests) and fight over it.
- `ComparisonError` from an unreachable Helm/Git source, a missing values file, or a plugin
  that errors — the app is not really OutOfSync at all.

# Resolution

- **Identify the other writer first.** Fixing drift without knowing who writes the field
  produces an `ignoreDifferences` rule that hides a real problem.
- Once identified, pick the honest owner:
  - HPA-managed replicas → remove `replicas` from Git.
  - Injected sidecar / defaulted field → an `ignoreDifferences` entry with a precise
    `jsonPointers`/`jqPathExpressions` path. Never ignore a whole resource.
  - Two controllers → delete one owner. Dual ownership has no stable resolution.
  - Non-deterministic chart output → pin the value, or ignore that one annotation path.
- Disable `selfHeal` only as a temporary measure while investigating, and re-enable it —
  a permanently disabled self-heal quietly ends GitOps enforcement for that app.

# Not covered

- **Degraded health.** An app can be perfectly `Synced` and still `Degraded`; that is the
  Application-degraded playbook.
- **Sync operations that fail outright** (`SyncFailed` with an apply error) rather than
  reverting — that is closer to the manifest/apply failure path.
- **Flux drift correction**, which uses a different mechanism and different fields.
- **ApplicationSet** generators producing or removing Applications.
- **Choosing which controller should own a resource** in a mixed Flux/Argo estate — an
  architecture decision this entry can only flag, not make.
- Argo CD's own component health (repo-server, application-controller).
