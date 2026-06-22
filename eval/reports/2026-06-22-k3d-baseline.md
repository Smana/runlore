# RunLore eval — first baseline (local k3d)

**Date:** 2026-06-22 · **Cluster:** local k3d (`runlore-eval`) — mycluster-0 was destroyed, so the
baseline ran on a throwaway k3d. **Investigation model:** `gemini-2.5-flash` (RunLore, via the
gemini provider). **Judge:** `gemini-2.5-pro` (stronger, blind). **N=3** per scenario.

> This run **validated the harness end-to-end**: induce fault → real LLM investigation (real k8s
> tools against the k3d API) → deterministic coverage grade + blind LLM-judge → always teardown →
> dated report. It is the first repeatable "deeply test RunLore" run.

## Scope caveat (k3d vs mycluster-0)

On a vanilla k3d the only *real* investigation signal is the **kubernetes API** (`pod_status`,
`kube_events`, `controller_logs`). There is no Flux/Cilium/cert-manager/VictoriaMetrics/VictoriaLogs/
Hubble and no EKS, so `gitops`/`metrics`/`logs`/`network`/`aws` coverage **cannot** be exercised here.
The 12-scenario EKS catalog (`eval/scenarios/`) targets those; this baseline used the k8s-native
subset (`eval/scenarios-k3d/`).

## Results

| scenario | result | coverage (k8s) | root_cause | evidence | solution | calibration | notes |
|---|---|---|---|---|---|---|---|
| k3d-bad-image-tag | **PASS** | 100% | 2/3 | 3 | 3 | 2 | correct (ImagePullBackOff) but didn't fully nail "tag deliberately changed to a non-existent one" |
| k3d-pvc-unbound | **PASS** | 100% | 3/3 | — | — | — | nailed the unbound-PVC / bad-StorageClass root cause cleanly |
| k3d-oom-saturation | **FAIL** | 100% | 1/3 | 1 | 0 | 2 | **confident-wrong**, high variance (var_rc=0.67) — symptom-only, no/Wrong fix |

**2/3 pass.** Coverage was 100% on every scenario (kubernetes touched; the deterministic track works).

## Top gaps (the workstream-B / RunLore-improvement input)

1. **OOM-from-limit reasoning is weak and flaky.** `k3d-oom-saturation` got root_cause **1/3**,
   solution **0**, and was **confident-wrong** on ≥1 of 3 runs (variance 0.67). RunLore+flash sees the
   restarts but doesn't reliably tie `OOMKilled` / exit 137 to *the 64Mi limit vs the 256M workload*.
   - Likely fixes: have `pod_status` surface container **resource limits + lastState.terminated
     (reason=OOMKilled, exitCode=137)** explicitly so the model has the limit-vs-usage signal; and/or
     test a stronger investigation model. This is the kind of finding the eval exists to produce.
2. **`what_changed` errors on a non-Flux cluster** (flagged as a tool error on all 3 scenarios). On
   k3d there is no GitOps engine, yet `gitOpsFromKube` still builds a provider and the model calls
   `what_changed`, which errors. Harmless to coverage (kubernetes is what's graded) but noisy —
   consider not registering the gitops tools when no GitOps CRDs are present.
3. **Shallow root-cause on the image-tag case** (2/3): the agent stops at "ImagePullBackOff" without
   stating the change that caused it. A small nudge in the loop/prompt toward "name the change" could
   lift this.

## Authoring bug caught (now fixed)

`k3d-pvc-unbound` originally SKIPped with `setup failed` — its manifest referenced namespace
`runlore-eval` but didn't create it (the sibling scenarios create it via `eval-victim-app.yaml`).
Fixed by adding a `kubectl create namespace` step. The graceful-skip behaviour worked as designed (a
setup failure → SKIP, never a false pass).

## Reproduce

```bash
k3d cluster create runlore-eval --no-lb --k3s-arg "--disable=traefik@server:0"
GEMINI_API_KEY=… lore eval --live \
  --config <gemini-config.yaml> \
  --scenarios eval/scenarios-k3d \
  --judge-provider gemini --judge-model gemini-2.5-pro --judge-api-key-env GEMINI_API_KEY \
  --n 3
```

Raw machine-readable reports: `eval/reports/2026-06-22T05-0*.json`.
