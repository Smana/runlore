---
type: Playbook
title: Argo CD Application health Degraded after a sync
description: An Argo CD Application reports health Degraded (often with Synced status) because a managed resource is unhealthy; alerts such as ArgoCDAppUnhealthy or ArgoAppUnhealthy fire off argocd_app_info.
tags: [argocd, argo-cd, application, gitops, degraded, sync, ArgoCDAppUnhealthy, ArgoAppUnhealthy, ArgoCdAppUnhealthy, ArgoCDAppSyncFailed, ComparisonError]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

An Argo CD `Application` shows `Health: Degraded`. Note the common and confusing
combination: **`Sync: Synced` + `Health: Degraded`** — Argo applied exactly what Git asked
for, and the result is unhealthy. That is a workload problem wearing a GitOps costume.

Argo CD ships no canonical upstream Prometheus rule bundle, so the alert name varies by
platform. The widely used community names all derive from `argocd_app_info{health_status=...}`:
`ArgoCDAppUnhealthy`, `ArgoAppUnhealthy`, `ArgoCdAppUnhealthy`, `ArgoCDAppSyncFailed`.
Check your own rules for the exact name.

# Investigate

1. `argocd app get <app>` (or the UI resource tree) — Argo names the **specific child
   resource** that is Degraded. Start there; the Application's own health is just an
   aggregate.
2. `argocd app history <app>` — did a sync land just before the degradation? Note the
   revision to compare against.
3. `argocd app diff <app> --revision <previous>` — the actual manifest delta that landed,
   not the commit message's claim about it.
4. Go to the named child resource and diagnose it as an ordinary Kubernetes object:
   `kubectl -n <ns> describe <kind>/<name>`, its events, its pods' logs.
5. `Health: Degraded` on a custom resource means Argo used a **custom health check** (a Lua
   script in the `argocd-cm` ConfigMap, or a built-in for known CRDs). Read that check
   before trusting the verdict — a stale health script reports Degraded on a healthy object.

# Common causes

- A Deployment/StatefulSet whose new pods never become Ready — crash loop, failing
  readiness probe, OOMKill, or an image that will not pull.
- A Job or hook (`PreSync`, `PostSync`, `SyncFail`) that failed; Argo marks the Application
  Degraded and, for `PreSync`, blocks the rest of the sync.
- An Ingress or Service whose backing endpoints are empty, so Argo's built-in health check
  for that kind reports Degraded.
- A custom-resource health check (cert-manager `Certificate`, an operator CR) reporting a
  genuine problem in the operator's own domain.
- A resource that was healthy but degraded later with no sync at all — an infrastructure
  event, not a deployment.

# Resolution

- **If a sync caused it**, roll back to the previous history revision
  (`argocd app rollback <app> <history-id>`) — reversible, and it isolates whether the
  change is at fault. Then revert the commit in Git so self-heal or the next sync does not
  reintroduce it.
- **If nothing synced recently**, treat it as a workload incident and fix the child
  resource. A rollback will not help and wastes the window.
- Re-run a failed hook Job only after understanding why it failed; hooks that mutate data
  are frequently not idempotent.

# Not covered

- **OutOfSync that never converges** — a sync-state problem, not a health problem; separate
  playbook.
- **Diagnosing the child workload.** This entry gets you to the resource at fault; the
  crash-loop, OOM, probe, and image-pull playbooks take it from there.
- **Flux.** Flux has no Application object and no equivalent aggregated health status.
- **ApplicationSet generator problems**, app-of-apps ordering, and sync waves.
- **Writing or fixing custom Lua health checks** — only recognising that one is in play.
- The exact alerting rule to deploy; the names above are observed conventions, not an
  upstream standard.
