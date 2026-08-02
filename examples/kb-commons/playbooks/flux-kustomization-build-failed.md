---
type: Playbook
title: Flux Kustomization Ready=False on build or apply failure
description: FluxReconciliationFailure fires for a Kustomization whose kustomize build fails, whose manifests are rejected on apply, or whose health checks never pass, so nothing from that path reaches the cluster.
tags: [flux, kustomization, kustomize, kustomize-controller, gitops, BuildFailed, HealthCheckFailed, FluxReconciliationFailure, ReconciliationFailed, drift]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`FluxReconciliationFailure` fires for a `Kustomization`. `flux get kustomizations -A` (or
`kubectl get kustomization -A`) shows `READY=False` with a reason such as `BuildFailed`,
`ReconciliationFailed`, `HealthCheckFailed`, or `ArtifactFailed`. Because the reconciler
applies the whole overlay atomically, **one broken file freezes every resource in that
path** — the visible effect is usually "my change never showed up", not an error.

# Investigate

1. `kubectl -n <ns> get kustomization <name> -o yaml` — `.status.conditions[]` gives the
   reason and, for build errors, the offending file and line.
2. Reproduce the build locally against the same commit:
   `kustomize build <path>` (or `flux build kustomization <name> --path <path>`). A build
   error reproduces off-cluster in seconds and needs no cluster access.
3. `flux diff kustomization <name> --path <path>` — shows what *would* change. Useful to
   confirm the overlay is what you think it is before blaming the cluster.
4. `ArtifactFailed` means the **source** failed, not the build. Check the `GitRepository` /
   `OCIRepository` this Kustomization references — auth, ref, or reachability.
5. `HealthCheckFailed` means the manifests applied fine but a `.spec.healthChecks` target
   never became Ready. Go look at that object, not at the Kustomization.
6. `kubectl -n <ns> describe kustomization <name>` — apply-time rejections (webhook, RBAC,
   immutable field) appear here with the object name.

# Common causes

- A file listed in `kustomization.yaml` `resources:` was renamed or deleted in the same
  commit — the most common build failure by a wide margin.
- A patch target (`patches[].target`) matches nothing after a rename, so kustomize errors
  out rather than silently skipping.
- YAML indentation or a duplicate key in a new manifest.
- The service account Flux impersonates lacks RBAC for a newly added kind.
- An immutable field changed on an existing object (Deployment selector, Job template,
  Service `clusterIP`); server-side apply rejects the patch.
- An admission webhook rejects the manifest, or the webhook backend is down and the
  failure policy is `Fail`.
- A `dependsOn` Kustomization is itself not Ready, so this one never gets its turn.

# Resolution

- **Revert the commit** that broke the build and let the reconciler pick it up
  (`flux -n <ns> reconcile kustomization <name> --with-source`). Reverting is reversible and
  unblocks every other resource in the same path immediately.
- Fix forward once the path is unblocked: restore the missing file, retarget the patch,
  grant the missing RBAC, or delete-and-recreate the object with the immutable field change
  (only when recreation is safe for that resource).
- If a webhook backend is down, restore the webhook rather than removing it — deleting a
  `ValidatingWebhookConfiguration` to unstick an apply is a security regression that tends
  to become permanent.

# Not covered

- **HelmRelease failures.** A Kustomization that applies a HelmRelease can be perfectly
  Ready while the release inside it fails; that is a different controller and a different
  playbook.
- **Drift correction and pruning semantics** — what `.spec.prune` deletes, and when.
- **Argo CD** manifest generation. Argo's equivalent failure surfaces as
  `ComparisonError` on the Application, with different tooling.
- **Why a health-check target is unhealthy.** `HealthCheckFailed` points at a workload;
  diagnose that workload with the workload playbooks.
- Source-level authentication and Git provider outages beyond identifying that the source,
  not the build, is at fault.
