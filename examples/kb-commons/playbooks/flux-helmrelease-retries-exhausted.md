---
type: Playbook
title: Flux HelmRelease terminal state with retries exhausted or Stalled
description: A Flux HelmRelease stops retrying entirely - RetriesExhausted, Stalled=True, or "install retries exhausted" - so FluxReconciliationFailure stays firing and further reconcile requests do nothing.
tags: [flux, helmrelease, helm, helm-controller, gitops, stalled, RetriesExhausted, Stalled, FluxReconciliationFailure, remediation]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`FluxReconciliationFailure` has been firing for a `HelmRelease` for far longer than one
reconcile interval, and the status no longer changes. Tell-tale strings in
`.status.conditions[]`:

- `reason: RetriesExhausted` — `upgrade retries exhausted` or `install retries exhausted`
- `Stalled` condition present and `True`
- `.status.installFailures` / `.status.upgradeFailures` at or above
  `.spec.install.remediation.retries` / `.spec.upgrade.remediation.retries`

The distinguishing signal versus an ordinary failure: `flux reconcile` returns success but
the status timestamp does not move. helm-controller has given up on purpose.

# Investigate

1. `kubectl -n <ns> get helmrelease <name> -o yaml` — confirm the terminal marker above.
   `Stalled=True` or `RetriesExhausted` is the whole distinction; without it you are looking
   at a plain upgrade failure that is still retrying.
2. Read `.status.conditions[].message` for the **original** Helm error. The terminal state
   hides it behind a generic exhaustion message, but the first failure is what you must fix.
3. `helm -n <ns> history <release>` — is the release itself wedged in `pending-upgrade` /
   `pending-install` / `pending-rollback`? A pending Helm release is a separate blocker that
   survives any Flux-side reset.
4. `kubectl -n <ns> get helmrelease <name> -o jsonpath='{.spec.upgrade.remediation}'` — is
   remediation configured at all, and does `strategy` say `rollback` or `uninstall`?
   `uninstall` means a failed attempt removed the workloads.
5. Check whether the release was suspended (`.spec.suspend: true`) — suspension also freezes
   status and is trivially confused with stalling.

# Common causes

- The underlying failure was never fixed; retries simply ran out at the configured count.
- A Helm release left in a `pending-*` state by a controller restart or a killed operation.
  Helm refuses the next upgrade because it believes one is in flight.
- `.spec.upgrade.remediation.retries` set low (or left at the default of 0 for install)
  against a slow chart hook, so a transient timeout becomes terminal.
- `remediation.strategy: uninstall` combined with a failing install, which deletes and
  recreates the release each attempt and can destroy PVC-less state.

# Resolution

1. **Fix the original error first.** Resetting the state without fixing the cause just
   re-exhausts the retries and costs another interval.
2. Clear a wedged Helm release if `helm history` showed a `pending-*` status — roll the
   release back to the last deployed revision, or delete the pending release secret in the
   release namespace so Helm stops seeing an in-flight operation.
3. Force one fresh attempt from Flux:
   `flux -n <ns> suspend helmrelease <name> && flux -n <ns> resume helmrelease <name>`.
   The suspend/resume cycle resets the failure counters — plain `flux reconcile` does not.
4. Watch `.status.conditions[]` through the next attempt. If it re-enters the terminal state
   with the same message, the cause is still live; go back to step 1.

# Not covered

- **The first, still-retrying failure.** If `Stalled`/`RetriesExhausted` is absent, this is
  the wrong entry — use the HelmRelease upgrade-failure playbook.
- **Diagnosing the underlying Helm error.** This entry gets the controller retrying again;
  it does not tell you why the chart failed.
- **Argo CD.** Argo has no equivalent terminal remediation state; its retry behaviour lives
  in `spec.syncPolicy.retry` and behaves differently.
- **Suspended releases.** A HelmRelease with `.spec.suspend: true` is intentionally frozen,
  not stalled. Resuming it without knowing who suspended it can re-apply a change someone
  deliberately held back.
- Kustomization stalling — the reconciler and the remediation model are different.
