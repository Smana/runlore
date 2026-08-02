---
type: Playbook
title: Pod Running but never Ready because the readiness probe fails
description: KubePodNotReady and KubeDeploymentReplicasMismatch fire while the pod stays Running with zero restarts, because its readiness probe never succeeds and the Service removes it from endpoints.
tags: [pod, readiness, probe, livenessProbe, readinessProbe, startupProbe, endpoints, rollout, KubePodNotReady, KubeDeploymentReplicasMismatch, KubeStatefulSetReplicasMismatch, KubeDeploymentRolloutStuck]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`KubePodNotReady` fires; `kubectl get pods` shows `Running` with `READY 0/1` and — the key
signal — **`RESTARTS 0`**. The container is alive and the process is fine as far as the
kubelet is concerned; it simply never reports ready, so the Service drops it from endpoints
and the rollout stalls with `KubeDeploymentReplicasMismatch`.

Zero restarts is what separates this from a crash loop. If restarts are climbing, a
*liveness* probe is failing too, and that is a different (usually worse) situation.

# Investigate

1. `kubectl -n <ns> describe pod <pod>` — events show
   `Readiness probe failed: <reason>`. The reason is the diagnosis: connection refused,
   timeout, HTTP 503, or a non-zero exec exit.
2. Read the probe definition:
   `kubectl -n <ns> get pod <pod> -o jsonpath='{.spec.containers[*].readinessProbe}'`.
   Check path, port, scheme, `initialDelaySeconds`, `periodSeconds`, `timeoutSeconds`,
   `failureThreshold` — and whether a `startupProbe` exists.
3. Test the endpoint from inside:
   `kubectl -n <ns> exec <pod> -- wget -qO- localhost:<port><path>` (or `curl`). If it
   answers from inside but the kubelet's probe fails, the mismatch is port/scheme/host,
   not the application.
4. Container logs — the application often logs exactly why it is not ready (waiting on a
   migration, on a leader election, on a dependency).
5. If this started after a change, diff the probe config and the image against the previous
   ReplicaSet. Probe regressions are frequently a copy-paste of a port number.

# Common causes

- Application startup takes longer than `initialDelaySeconds` allows, so the probe fails
  before the app is up. With no `startupProbe` and a liveness probe present, the pod is
  killed mid-boot and never recovers.
- Probe points at the wrong port, path, or scheme (`HTTP` against an HTTPS-only listener,
  or the container port renamed).
- The readiness endpoint legitimately reports not-ready: a dependency (database, cache,
  upstream API) is unreachable, or a leader election has not resolved.
- `timeoutSeconds: 1` (the default) against an endpoint that does real work — a readiness
  handler that queries a database will exceed it under load, taking healthy pods out of
  rotation exactly when they are needed.
- The application binds `127.0.0.1` instead of `0.0.0.0`, so nothing outside the process
  namespace can reach it.
- A NetworkPolicy or mesh sidecar that starts after the app blocks the kubelet's probe path.

# Resolution

- Fix the mismatch the probe events named — port, path, scheme, or timeout — in Git and
  reconcile.
- For slow starts, add a `startupProbe` rather than inflating `initialDelaySeconds`. The
  startup probe suspends liveness until the app is up and stops the boot-kill loop.
- If readiness is honestly reporting a broken dependency, **do not relax the probe**. The
  probe is doing its job; go fix the dependency. Loosening it puts a non-functional pod into
  rotation and converts a visible outage into silent errors.
- Raise `timeoutSeconds` when the readiness handler does real work, and consider making the
  handler cheap instead.

# Not covered

- **Crash loops.** Restarts climbing means the container is exiting — different playbook.
- **Liveness-probe kill loops** where the container is restarted repeatedly by the kubelet;
  the mitigation there is a `startupProbe` or a longer `failureThreshold`, and the risk
  profile is different.
- **Pods stuck `Pending`** — never scheduled, so no probe has run yet.
- **`Init:` states.** An init container that never finishes means the main container has not
  started at all.
- **Diagnosing the dependency** a readiness endpoint is waiting on.
- Service mesh probe rewriting and sidecar startup ordering specifics.
