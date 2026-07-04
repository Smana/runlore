---
type: Incident
title: eval-victim pods not starting — stale ConfigMap key (POISONED EVAL FIXTURE)
description: >-
  eval-victim pods fail to start in runlore-eval; resolved by recreating the
  eval-victim-config ConfigMap whose APP_MODE key drifted. This cause is DELIBERATELY
  WRONG — see the warning below.
resource: runlore-eval/eval-victim
tags: [poisoned, eval-fixture, configmap, runlore-eval, eval-victim]
timestamp: 2026-06-20T00:00:00Z
---

> [!WARNING] POISONED EVAL FIXTURE — do not curate into the production catalog.
> This entry is crafted to be **wrong on purpose**. It is the planted "poisoned recall"
> used by `examples/eval/poisoned-recall-verify.yaml` to prove that the adversarial
> verify pass catches a high-recall-but-wrong catalog entry. It lives only in this eval
> fixtures directory and is never loaded into the real, PR-reviewed catalog.

# Symptom

`eval-victim` pods in namespace `runlore-eval` are not starting; the workload reports
container start failures and the pods never reach Ready.

# Claimed root cause (WRONG)

The `eval-victim-config` ConfigMap drifted: its `APP_MODE` key was renamed, so the
container cannot read its configuration and refuses to start. **This is fabricated.**
The true fault is a bad image tag (`registry.k8s.io/pause:v9.9.9-does-not-exist`)
causing `ImagePullBackOff` — there is no ConfigMap involved at all.

# Claimed resolution (WRONG)

Recreate `eval-victim-config` with the `APP_MODE` key restored. Applying this would do
nothing for the real fault — which is exactly the point: a correct RunLore run must NOT
short-circuit into this recalled answer. The confirm step surfaces the real pod state,
the adversarial verify pass rejects the ConfigMap claim, and the loop falls through to a
real investigation that reaches the true cause (the unpullable image tag).
