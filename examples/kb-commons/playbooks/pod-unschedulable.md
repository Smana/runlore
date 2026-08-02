---
type: Playbook
title: Pod stuck Pending because the scheduler finds no suitable node
description: A pod stays Pending with a FailedScheduling event listing per-node reasons such as Insufficient cpu, Insufficient memory, or node(s) had untolerated taint, so the rollout stalls and replicas never reach desired count.
tags: [scheduling, pod, pending, unschedulable, FailedScheduling, taint, toleration, nodeSelector, affinity, topologySpreadConstraints, resourcequota, autoscaler, KubePodNotReady, KubeDeploymentReplicasMismatch, KubeStatefulSetReplicasMismatch, KubeDaemonSetNotScheduled]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`kubectl -n <ns> get pods` shows `Pending` with age but no node assigned.
`KubeDeploymentReplicasMismatch` or `KubeStatefulSetReplicasMismatch` fires because the
desired replica count is never reached; for DaemonSets it is `KubeDaemonSetNotScheduled`.

`kubectl -n <ns> describe pod <pod>` carries the whole answer in one `FailedScheduling`
event, which tallies **why each node was rejected**, for example:

```
0/12 nodes are available: 5 Insufficient cpu, 4 node(s) had untolerated taint {dedicated: gpu},
3 node(s) didn't match Pod's node affinity/selector.
```

Read that tally before anything else — the counts tell you whether this is capacity, policy,
or placement, and they add up to the cluster size.

# Investigate

1. Read the `FailedScheduling` tally and classify it:
   - `Insufficient cpu` / `Insufficient memory` / `Insufficient <extended-resource>` →
     capacity versus **requests**.
   - `had untolerated taint {…}` → the taint is named; the pod lacks the toleration.
   - `didn't match Pod's node affinity/selector` → `nodeSelector`/`affinity` matches no node.
   - `had volume node affinity conflict` → a storage topology problem, not a capacity one.
   - `didn't match pod topology spread constraints` → spread rules cannot be satisfied.
   - `node(s) were unschedulable` → nodes are cordoned.
2. Check the pod's requests against real node headroom:
   `kubectl -n <ns> get pod <pod> -o jsonpath='{.spec.containers[*].resources.requests}'` and
   `kubectl describe node <node>` (the `Allocated resources` block). A single request larger
   than any node's allocatable can never schedule, no matter how many nodes exist.
3. Verify the selector actually matches something:
   `kubectl get nodes -l <selector>` — an empty result is the whole story.
4. If a cluster autoscaler or provisioner is in play, read its logs/events. It records why it
   did **not** add a node (no matching node group, quota, instance type unavailable), which
   is otherwise invisible.
5. Check namespace limits: `kubectl -n <ns> describe resourcequota` and `limitrange`. A quota
   rejection usually blocks pod *creation* rather than scheduling, but a `LimitRange` default
   can inflate requests beyond what any node has.

# Common causes

- Requests raised (often via a chart default) beyond what the current node sizes can hold.
- The cluster is genuinely at capacity and the autoscaler cannot add nodes — quota exhausted,
  instance type unavailable in the zone, or no node group matches the pod's constraints.
- A `nodeSelector` or `nodeAffinity` referencing a label that was renamed or a node pool that
  was removed.
- Taints added to a node pool without adding the matching tolerations to the workloads that
  belong there.
- `topologySpreadConstraints` with `whenUnsatisfiable: DoNotSchedule` and fewer eligible
  zones/nodes than replicas — the last replicas stay Pending forever, by design.
- Pod anti-affinity requiring one replica per node with more replicas than nodes.
- Nodes cordoned during maintenance and never uncordoned.

# Resolution

- Match the fix to the tally, not to a guess:
  - Capacity → lower requests to a realistic value, or add nodes/raise the autoscaler bound.
  - Taint → add the toleration in Git (or remove the taint if it was unintended).
  - Selector/affinity → correct the label or restore the node pool.
  - Spread constraints → relax to `ScheduleAnyway`, or add eligible topology domains.
  - Cordoned nodes → uncordon once maintenance is done.
- Prefer changing the workload's requests over adding capacity when the request was never
  measured — oversized requests waste cluster capacity permanently and silently.
- Verify by re-reading the event after the change; the tally shifts to show what still
  blocks it.

# Not covered

- **`volume node affinity conflict`** and other storage topology constraints, which appear
  in the same event but need the storage playbook.
- **Unbound PVCs**, which also hold a pod Pending but for a different reason.
- **Pods that scheduled and then failed** — crash loops, image pulls, probes.
- **Cluster autoscaler and Karpenter configuration**, node group design, and instance-type
  selection.
- **Right-sizing requests for a specific workload** — that needs the workload's real usage,
  not a rule of thumb.
- **Priority and preemption behaviour**, which changes what "unschedulable" means.
