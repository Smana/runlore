---
type: Playbook
title: Service has no endpoints so traffic to it fails
description: A Service resolves but has an empty endpoint list, so callers get connection refused and Prometheus reports TargetDown; the selector matches no ready pods or the ports do not line up.
tags: [network, service, endpoints, endpointslice, selector, targetPort, ingress, connection-refused, TargetDown, KubePodNotReady, KubeDeploymentReplicasMismatch, KubeStatefulSetReplicasMismatch]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`kubectl -n <ns> get endpoints <svc>` shows `<none>`, or
`kubectl -n <ns> get endpointslice -l kubernetes.io/service-name=<svc>` shows no addresses.
Callers get `connection refused` (kube-proxy has no backend to forward to), the Ingress
returns 502/503, and `TargetDown` fires for scrape targets behind that Service.

The DNS name resolves normally — that is what makes this confusing. Resolution and
reachability are separate layers, and only the second one is broken.

# Investigate

1. `kubectl -n <ns> describe service <svc>` — note the `Selector` and the
   `Port`/`TargetPort` pair.
2. Do any pods match the selector?
   `kubectl -n <ns> get pods -l <selector-from-above>`.
   No pods at all → a labelling or scale problem. Pods but no endpoints → readiness.
3. Are the matching pods **Ready**? Only ready pods become endpoints. `READY 0/1` explains
   an empty endpoint list completely, and the real problem is the readiness probe.
4. Compare ports precisely: the Service's `targetPort` must match a `containerPort` **name
   or number** on the pod. A named `targetPort` referencing a port name the container does
   not declare yields no endpoints and no error anywhere.
5. Check for a `publishNotReadyAddresses` expectation (headless Services used for peer
   discovery often rely on it) — without it, a StatefulSet's peers are invisible to each
   other until ready, which can deadlock bootstrap.
6. For an `ExternalName` or selector-less Service, there is no selector at all; endpoints
   are managed manually or by another controller. Look at who is supposed to write them.

# Common causes

- Every backing pod is not Ready — a failing readiness probe, a crash loop, or a rollout in
  progress. **This is the most common cause by a wide margin**, and the Service is only
  reporting it.
- The Deployment was scaled to zero, or its pods are all Pending/unschedulable.
- Selector and pod labels drifted apart — a label renamed on one side during a refactor, or
  a Helm chart change to `commonLabels`.
- `targetPort` names a container port that was renamed or removed.
- The Service was created in a different namespace than the pods; selectors never cross
  namespaces.
- A headless Service (`clusterIP: None`) behaving as designed — clients get pod A records,
  and "no endpoints" reflects unready pods rather than a Service bug.

# Resolution

- If the backing pods are unready, **fix the pods** — the Service needs no change at all.
  Editing the Service here would be treating the symptom.
- Realign selector and labels in Git, then reconcile. Change one side deliberately, not
  both.
- Correct `targetPort` to match the container's declared port name or number.
- For a Service with no selector by design, restore whatever controller writes its
  EndpointSlices.
- Scale the workload back up if replicas are zero — and find out who scaled it, because a
  surprise zero-replica Deployment is usually a symptom of something else (an HPA at
  minimum zero, a failed rollout, a pruning event).

# Not covered

- **DNS resolution failures.** If the name does not resolve, endpoints are not the problem.
- **Diagnosing why the backing pods are unready** — readiness-probe, crash-loop, and
  scheduling playbooks cover that.
- **Ingress controller configuration**, TLS termination, and path routing above the Service.
- **kube-proxy / eBPF datapath failures**, where endpoints exist but traffic still does not
  flow.
- **NetworkPolicy denial**, which drops packets rather than refusing connections.
- **Service mesh routing**, where the mesh may bypass kube-proxy entirely and the endpoint
  list is not the whole story.
