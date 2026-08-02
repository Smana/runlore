---
type: Playbook
title: Flux HelmRelease stuck Ready=False after an upgrade
description: FluxReconciliationFailure fires and a Flux HelmRelease reports Ready=False with reason UpgradeFailed or InstallFailed shortly after a chart version or values change.
tags: [flux, helmrelease, helm, helm-controller, gitops, upgrade, FluxReconciliationFailure, UpgradeFailed, InstallFailed, ReconciliationFailed]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`FluxReconciliationFailure` fires for a `HelmRelease`. `kubectl get helmrelease -A` shows
`READY=False` with a message like `Helm upgrade failed for release <ns>/<name>`. The change
that triggered it is almost always a chart version bump or a values edit that landed in Git
minutes earlier. Downstream it surfaces as probe failures, gateway 5xx, or
`KubeDeploymentReplicasMismatch` on the workloads the chart owns.

# Investigate

1. `kubectl -n <ns> get helmrelease <name> -o yaml` — read `.status.conditions[]`. Which
   condition is False (`Ready`, `Released`, `Remediated`) and what is its `reason`
   (`UpgradeFailed`, `InstallFailed`, `TestFailed`, `ArtifactFailed`)?
2. `kubectl -n <ns> describe helmrelease <name>` — events carry the Helm error string
   verbatim. **That string is the diagnosis**; the condition reason is only the category.
3. `helm -n <ns> history <release>` — did a new revision land and get rolled back, or did
   none land at all? A missing new revision means the failure was pre-apply (rendering,
   values, CRD), not post-apply.
4. Compare `.status.lastAttemptedRevision` with the last entry in `.status.history` to name
   the from→to chart version, then read the Git commit that moved it.
5. `kubectl -n <ns> get events --sort-by=.lastTimestamp` — find the concrete object that
   failed: a hook Job that never completed, pods that crash, an admission rejection.

# Common causes

- Values contract change in the new chart version — a key renamed, moved under a new
  parent, or newly required. The Helm error names the path.
- A `valuesFrom` ConfigMap or Secret is missing, or one of its keys was renamed;
  helm-controller fails before rendering anything.
- A CRD the new chart version depends on is not installed. Helm does not upgrade CRDs
  shipped in a chart's `crds/` directory, so a chart bump that adds one needs a manual step.
- A chart hook Job (schema migration, pre-upgrade check) never completes, so Helm blocks
  until `.spec.timeout` and reports the release failed.
- An immutable field changed (Deployment `.spec.selector`, Job `.spec.template`, a PVC's
  size on a storage class that forbids expansion). Helm cannot patch these in place.
- An admission webhook — policy engine, mesh injector, image verifier — rejects a rendered
  manifest. The Helm error contains the webhook name.

# Resolution

- **Revert in Git first** — that is the reversible option and the GitOps-correct one:
  revert the commit that bumped the chart or values, then
  `flux -n <ns> reconcile helmrelease <name> --with-source` to pull it immediately instead
  of waiting for the next interval.
- Need the cluster healthy before the revert propagates?
  `helm -n <ns> rollback <release> <previous-revision>` restores the old manifests, but
  Flux re-applies desired state on the next reconcile — treat it as a stopgap that buys
  minutes, never as the fix.
- **Then fix forward**: correct the values path, install the missing CRD, unblock or delete
  the stuck hook Job, or delete-and-recreate the object whose immutable field changed
  (only when the resource tolerates recreation).

# Not covered

- **Terminal / exhausted states.** `RetriesExhausted`, `Stalled=True`, or
  `install retries exhausted` means helm-controller has stopped retrying; reconciling again
  does nothing. That needs the remediation reset, covered by its own playbook.
- **Argo CD-managed Helm charts.** Argo renders with `helm template` and applies the
  output — there is no Helm release history and nothing to `helm rollback`.
- **Chart source failures.** An unreachable or unauthenticated `HelmRepository` /
  `OCIRepository` sets the condition on the *source* object, not the HelmRelease. Check the
  source first if `ArtifactFailed` is the reason.
- **Why the workload itself is unhealthy** once Helm reports success. That is a workload
  problem (crash loop, probe, OOM), not a release problem.
- Helm releases not managed by Flux at all (manual `helm upgrade`, Helmfile, Terraform).
