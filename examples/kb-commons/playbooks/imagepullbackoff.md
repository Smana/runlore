---
type: Playbook
title: Pod stuck in ImagePullBackOff or ErrImagePull
description: KubeContainerWaiting and KubePodNotReady fire because the kubelet cannot pull the container image - wrong tag, missing registry credentials, rate limit, or an unreachable registry - so the container never starts.
tags: [pod, container, image, registry, imagepullbackoff, ErrImagePull, imagePullSecrets, rate-limit, KubeContainerWaiting, KubePodNotReady, KubeDeploymentReplicasMismatch, KubeDeploymentRolloutStuck]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`KubeContainerWaiting` fires with `reason=ImagePullBackOff` (or `ErrImagePull` on the first
attempts). The pod never runs, so there are **no container logs at all** — `kubectl logs`
returns nothing useful and `--previous` has nothing to show. All the evidence is in events.

New rollouts stall behind it; existing pods keep serving, which is why this often stays
unnoticed until the old ReplicaSet is scaled down.

# Investigate

1. `kubectl -n <ns> describe pod <pod>` — the `Failed to pull image` event carries the
   registry's own error verbatim. Read it before theorising; it distinguishes every cause
   below:
   - `manifest unknown` / `not found` → the tag or digest does not exist.
   - `unauthorized` / `authentication required` → credentials missing or wrong.
   - `denied` → credentials valid, but not for this repository.
   - `toomanyrequests` → registry rate limit.
   - `dial tcp ... i/o timeout` / `no such host` → network or DNS to the registry.
2. Print the exact reference the kubelet is using:
   `kubectl -n <ns> get pod <pod> -o jsonpath='{.spec.containers[*].image}'`. Compare it
   character by character with what exists in the registry — a typo in the registry host or
   a missing project path is common and invisible at a glance.
3. Check the pull secret is actually attached:
   `kubectl -n <ns> get pod <pod> -o jsonpath='{.spec.imagePullSecrets}'` and confirm the
   named Secret exists **in the same namespace** (`imagePullSecrets` never cross namespaces),
   or that the pod's ServiceAccount carries it.
4. Is it one node or all of them? `kubectl get pods -o wide` — a single failing node points
   at that node's network, proxy, or containerd config, not at the image.
5. If the tag is mutable (`latest`, `main`), verify it still resolves; a retagged or garbage
   collected image breaks pods that were fine an hour ago.

# Common causes

- Image tag that was never pushed — CI failed after the manifest was updated, so Git points
  at an image that does not exist. Very common in GitOps flows where the tag bump and the
  build are separate pipelines.
- `imagePullSecrets` missing in the workload's namespace (a new namespace rarely inherits
  it) or referencing a Secret name that does not exist.
- Expired registry credentials, or a token-based credential (cloud registry) whose refresh
  mechanism broke.
- Public registry rate limiting for anonymous pulls from the cluster's egress IP.
- Registry unreachable — DNS failure, egress firewall, proxy misconfiguration, or a private
  registry that is genuinely down.
- `imagePullPolicy: Always` on a node with no route to the registry, where a cached image
  would otherwise have worked.

# Resolution

- Wrong tag → correct it in Git (or re-run the build that should have pushed it) and
  reconcile. Prefer digests or immutable tags so this cannot recur silently.
- Missing/expired credentials → create or refresh the pull Secret in the workload's
  namespace and attach it to the ServiceAccount so future workloads inherit it.
- Rate limit → authenticate pulls, or mirror the image into a registry you control.
  Unauthenticated pulls of public images are a cluster-wide fragility, not a per-app one.
- Registry unreachable → this is an infrastructure incident; the workload is a symptom.
- Existing pods are still serving. **Do not delete them** to "retry the pull" — that trades
  a stalled rollout for an outage.

# Not covered

- **Image pulled successfully but the container then crashes** — that is a crash loop, and
  `logs --previous` will have content.
- **Init-container image pull failures**, which show as `Init:ImagePullBackOff` and block
  before the main container is even considered.
- **Image signature / admission policy rejections** (Cosign, Kyverno, Connaisseur). Those
  are rejected at admission with a webhook message, not by the kubelet, and the pod usually
  never gets created.
- **Building or publishing the image**, or fixing the CI pipeline that should have.
- Registry-specific credential-helper configuration for each cloud provider.
- Node-level container runtime and proxy configuration beyond identifying a single-node
  pattern.
