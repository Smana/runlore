---
type: Playbook
title: Node under memory pressure evicting pods
description: A node reports MemoryPressure and the kubelet evicts pods with reason Evicted; NodeMemoryHighUtilization and KubePodNotReady fire while healthy workloads are killed because a neighbour overcommitted the node.
tags: [node, memory, pressure, eviction, Evicted, overcommit, requests, limits, qos, BestEffort, Burstable, kube-reserved, NodeMemoryHighUtilization, NodeMemoryMajorPagesFaults, KubeNodeNotReady, KubePodNotReady, KubeDeploymentReplicasMismatch]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

Pods on one node show `Status: Evicted` with a message like
`The node was low on resource: memory`. `kubectl describe node <node>` shows
`MemoryPressure=True`. `NodeMemoryHighUtilization` (node-exporter mixin) and
`KubePodNotReady` fire; the evicted pods' Deployments report replica mismatches.

**The evicted pod is usually not the culprit.** The kubelet evicts by QoS class and by how
far a pod exceeds its memory *request* — so a well-behaved `BestEffort` or `Burstable` pod is
killed to make room for whichever neighbour actually consumed the node. Diagnosing the victim
wastes the incident.

# Investigate

1. Confirm the node condition and its timing: `kubectl describe node <node>` →
   `Conditions` and the `Events` block, which records eviction decisions.
2. Rank actual consumption on that node: `kubectl top pods -A --sort-by=memory` filtered to
   the node, or
   `sum by (pod) (container_memory_working_set_bytes{node="<node>"})`.
   The top consumer is the subject of the investigation.
3. Compare consumption with **requests**, not limits:
   `kubectl describe node <node>` prints allocated resources and the request totals. A node
   whose memory requests sum well below its capacity while usage is at capacity is
   overcommitted by design.
4. Identify the QoS classes present
   (`kubectl get pod <pod> -o jsonpath='{.status.qosClass}'`). Pods with no memory request at
   all are `BestEffort` and are evicted first regardless of how little they use.
5. Check whether this node is different from its peers — a larger workload landed on it, a
   DaemonSet is heavier here, or `kube-reserved`/`system-reserved` are unset so the kubelet
   and runtime compete with pods for the same memory.
6. Look for a step change in the top consumer's usage and correlate with a deploy.

# Common causes

- One workload with a memory limit far above its request, scheduled onto a node that could
  never honour the limit. The scheduler only ever looked at the request.
- Pods with no memory request (`BestEffort`) accumulating on a node until it saturates.
- A DaemonSet whose memory usage scales with node size or pod count but whose request is a
  fixed small number.
- Node-level reservations (`kube-reserved`, `system-reserved`, `evictionHard`) unset or too
  small, so the kubelet has no headroom and evicts late and hard.
- A genuine leak in one workload that reaches the node's capacity before its own limit —
  or with no limit at all.
- Cluster autoscaler unable to add nodes (quota, zone capacity), so pressure concentrates.

# Resolution

- **Set or raise memory requests on the real consumer** so the scheduler stops overcommitting
  the node. Requests are what schedule pods; limits only bound them.
- Bring requests and limits closer together for critical workloads — a `Guaranteed` pod
  (request == limit) is evicted last.
- Cordon the node and drain the top consumer if the pressure is live and needs to stop now;
  reversible, and it moves the workload rather than killing arbitrary neighbours.
- Configure `kube-reserved` / `system-reserved` so the kubelet always has headroom. Without
  them, node pressure is a matter of when, not if.
- Add capacity if the aggregate demand is legitimate — this is a sizing outcome, and worth
  saying so explicitly rather than repeatedly evicting.

# Not covered

- **Container-level OOMKills.** A container killed at its own limit (`OOMKilled`, exit 137)
  is a per-container sizing problem; the node is fine. Different playbook, different fix, and
  they are easy to confuse.
- **Disk-pressure eviction**, which has the same eviction mechanism but a completely
  different cause and remedy.
- **Nodes going NotReady** outright.
- **Profiling the consuming application** to find why it uses that much.
- **Cluster capacity planning and instance-type selection.**
- **Tuning kubelet eviction thresholds** beyond noting that unset reservations cause this.
