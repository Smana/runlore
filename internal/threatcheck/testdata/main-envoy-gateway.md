---
type: Incident
title: 'envoy-gateway HelmRelease terminal-failed: chart v1.8.2 RBAC gap blocks InferencePool watch → healthz fails →…'
description: 'envoy-gateway pod''s health/readiness endpoints never come up because controller-runtime''s healthz check fails — its cache reflector cannot watch the InferencePool CRD (inference.networking.k8s.io/v1) due to an RBAC denial: ''inferencepools.inference.networking.k8s.io is forbidden: User system:serviceaccount:envoy-gateway-system:envoy-gateway cannot list resource inferencepools in API group inference.networking.k8s.io at the cluster scope''. The failing healthz check blocks the /healthz and /readyz endpoints (connection refused / 500), so the liveness probe kills the pod repeatedly (7 restarts), the Deployment never becomes Ready, and Helm''s --wait times out on every install attempt. envoy-gateway v1.8.2 added an InferencePool EventSource/controller (the log shows ''Starting EventSource ... *unstructured.Unstructured[inference.networking.k8s.io/v1 InferencePool]''), but the chart''s ClusterRole (envoy-gateway-gateway-helm-envoy-gateway-role) was not updated to grant list/watch on inferencepools — a chart/RBAC gap in v1.8.2.'
resource: envoy-gateway-system/envoy-gateway
tags:
    - runlore
    - incident
    - helmrelease
    - envoy-gateway-system
timestamp: "2026-07-13T13:19:05Z"
fingerprint: c98cf34be4cc2cfac6e62d70dbf63e31dab7f0607589db88cc9bb02998c4e6c6
confidence: 0.82
---

## Decision

- **why keep:** envoy-gateway pod's health/readiness endpoints never come up because controller-runtime's healthz check fails — its cache reflector cannot watch the InferencePool CRD (inference.networking.k8s.io/v1) due to an RBAC denial: 'inferencepools.inference.networking.k8s.io is forbidden: User system:serviceaccount:envoy-gateway-system:envoy-gateway cannot list resource inferencepools in API group inference.networking.k8s.io at the cluster scope'. The failing healthz check blocks the /healthz and /readyz endpoints (connection refused / 500), so the liveness probe kills the pod repeatedly (7 restarts), the Deployment never becomes Ready, and Helm's --wait times out on every install attempt. envoy-gateway v1.8.2 added an InferencePool EventSource/controller (the log shows 'Starting EventSource ... *unstructured.Unstructured[inference.networking.k8s.io/v1 InferencePool]'), but the chart's ClusterRole (envoy-gateway-gateway-helm-envoy-gateway-role) was not updated to grant list/watch on inferencepools — a chart/RBAC gap in v1.8.2.
- **confidence:** 82%

## Symptom

envoy-gateway HelmRelease terminal-failed: chart v1.8.2 RBAC gap blocks InferencePool watch → healthz fails → pod crash-loop → install timeout

Affected resource: HelmRelease envoy-gateway-system/envoy-gateway

## Investigate

- Pod logs (raw query_logs): 'error provider.controller-runtime.cache.UnhandledError ... Failed to watch ... type: inference.networking.k8s.io/v1, Kind=InferencePool, error: failed to list inference.networking.k8s.io/v1, Kind=InferencePool: inferencepools.inference.networking.k8s.io is forbidden: User system:serviceaccount:envoy-gateway-system:envoy-gateway cannot list resource inferencepools in API group inference.networking.k8s.io at the cluster scope' (x52, 12:56:32Z→13:15:38Z)
- Pod logs: 'info provider.controller-runtime.healthz healthz/healthz.go:128 healthz check failed {runner: provider, statuses: [{}]}' (x80, 12:56:44Z→13:15:45Z) — the healthz check fails because the InferencePool watch error is an unhandled cache error
- Pod logs: 'Starting EventSource ... source: kind source: *unstructured.Unstructured[inference.networking.k8s.io/v1 InferencePool]' — envoy-gateway v1.8.2 registers an InferencePool controller that needs RBAC the chart doesn't grant
- kube_events: 'Unhealthy ... Liveness probe failed: Get http://100.64.45.207:8081/healthz: dial tcp 100.64.45.207:8081: connect: connection refused' and 'Readiness probe failed: ... statuscode: 500' — the healthz endpoint is down because the health check fails
- pod_status: envoy-gateway-6d7667f4bf-f4fdg Running ready=0/1 restarts=7 — the pod is in a crash/restart loop driven by liveness probe failures
- HelmRelease status: 'Helm install failed ... timeout waiting for: [Deployment/envoy-gateway-system/envoy-gateway status: InProgress]' — Helm --wait times out because the Deployment never becomes Ready
- Kustomization flux-system/envoy-gateway sourceRef is ExternalArtifact/flux-system/infra-artifact (Ready=True); dependsOn flux-system/crds (Ready=True) — the source and dependency chain are healthy, so the failure is in the chart's rendered manifests, not the GitOps source
- helm-controller logs: '2026-07-13T12:58:32.138Z Reconciler error ... error: terminal error: exceeded maximum retries: cannot remediate failed release'
- helm-controller logs show the install→timeout→uninstall→reinstall cycle repeating 4 times (12:37, 12:42, 12:48, 12:53) before terminal failure at 12:58
- HelmRelease status Ready=False (InstallFailed) with stale message 'Helm install failed ... timeout waiting for: [Deployment/envoy-gateway-system/envoy-gateway status: InProgress]'
- KB runbook 'helmrelease-terminal-failed-exhausted-retries' confirms: once retries are exhausted Flux stops retrying even after root cause is fixed; `flux reconcile helmrelease --reset` clears the counters

## Cause

1. **envoy-gateway pod's health/readiness endpoints never come up because controller-runtime's healthz check fails — its cache reflector cannot watch the InferencePool CRD (inference.networking.k8s.io/v1) due to an RBAC denial: 'inferencepools.inference.networking.k8s.io is forbidden: User system:serviceaccount:envoy-gateway-system:envoy-gateway cannot list resource inferencepools in API group inference.networking.k8s.io at the cluster scope'. The failing healthz check blocks the /healthz and /readyz endpoints (connection refused / 500), so the liveness probe kills the pod repeatedly (7 restarts), the Deployment never becomes Ready, and Helm's --wait times out on every install attempt. envoy-gateway v1.8.2 added an InferencePool EventSource/controller (the log shows 'Starting EventSource ... *unstructured.Unstructured[inference.networking.k8s.io/v1 InferencePool]'), but the chart's ClusterRole (envoy-gateway-gateway-helm-envoy-gateway-role) was not updated to grant list/watch on inferencepools — a chart/RBAC gap in v1.8.2.** (82%)
2. **The Flux HelmRelease envoy-gateway-system/envoy-gateway has entered a terminal failed state ('terminal error: exceeded maximum retries: cannot remediate failed release' at 12:58:32Z). Even after the RBAC gap is fixed, Flux will NOT retry the install on its own — the install failure counters are exhausted and persist in status. A plain reconcile or suspend/resume will not clear it; `flux reconcile helmrelease --reset` is required to reset the counters and trigger a fresh install attempt.** (45%)

## Resolution

- Fix the envoy-gateway ClusterRole to grant list/watch/get on inferencepools.inference.networking.k8s.io (add the rule to the chart's ClusterRole or via a values override), then reset the HelmRelease failure counters with `flux -n envoy-gateway-system reconcile helmrelease envoy-gateway --reset` so Flux performs a fresh install. (reversible=true)
- After fixing the RBAC gap, run `flux -n envoy-gateway-system reconcile helmrelease envoy-gateway --reset` to clear the exhausted failure counters and let Flux perform a fresh install. If the helm release storage is wedged (helm history shows only failed revisions), `helm -n envoy-gateway-system uninstall envoy-gateway` first, then the --reset reconcile. (reversible=true)

## Unresolved

- Whether the InferencePool CRD itself is installed in the cluster — the RBAC error is about listing the resource (permission denied), which would occur whether or not the CRD exists; however the pod log shows the EventSource starting for InferencePool, suggesting the CRD may be registered. A human should confirm whether the InferencePool CRD is expected to be present and whether envoy-gateway v1.8.2 is the intended chart version for this cluster.

