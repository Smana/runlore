---
type: Playbook
title: Node NotReady or unreachable and its pods stop being rescheduled promptly
description: KubeNodeNotReady, KubeNodeUnreachable or KubeletDown fires because a node stopped reporting Ready; its pods keep running invisibly for the eviction timeout before the control plane reschedules them.
tags: [node, kubelet, NotReady, unreachable, taint, eviction, containerd, condition, KubeNodeNotReady, KubeNodeUnreachable, KubeletDown, KubeNodeReadinessFlapping, KubeletTooManyPods, KubePodNotReady]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`KubeNodeNotReady`, `KubeNodeUnreachable`, or `KubeletDown` fires (kubernetes-mixin).
`kubectl get nodes` shows `NotReady` or `Unknown`.

The important timing detail: when a node goes unreachable, its pods are **not** rescheduled
immediately. The node gets a `node.kubernetes.io/unreachable` taint and pods with the default
tolerations wait out `tolerationSeconds` (300s by default) before eviction. During that
window pods show `Running` while being completely unreachable — capacity looks fine and
traffic is being blackholed.

`KubeNodeReadinessFlapping` is a distinct and worse signal: the node oscillates, so workloads
are repeatedly evicted and rescheduled onto an unstable node.

# Investigate

1. `kubectl describe node <node>` — the `Conditions` block names the actual problem:
   `Ready=Unknown` with `NodeStatusUnknown` means the kubelet stopped reporting at all;
   `Ready=False` with a message means the kubelet is alive and refusing to be ready
   (container runtime down, network plugin not initialised, disk pressure).
2. Note `LastHeartbeatTime` — how long ago did it stop? That fixes the incident's start time.
3. `kubectl get nodes` for the whole cluster. **One node is a node incident; several at once
   is an infrastructure or control-plane incident**, and the diagnosis is entirely different.
4. Check for a pattern: same node pool, same zone, same instance type, same AMI/image
   version, all recently created. Patterns point at the cause faster than any single node's
   logs.
5. If the node is reachable, look at the kubelet and container runtime on the host (their
   service status and recent logs). If it is not reachable at all, this is an infrastructure
   problem — the instance, its network, or its provider.
6. `kubectl get pods -A -o wide --field-selector spec.nodeName=<node>` — what is stranded
   there, and does anything on it hold a `local` PV or a singleton role?

# Common causes

- Node resource exhaustion — memory, PID, or disk — starving the kubelet itself. Check the
  other conditions (`MemoryPressure`, `DiskPressure`, `PIDPressure`) before looking further.
- Container runtime crashed or hung; the kubelet cannot report container status and goes
  NotReady while the host is otherwise fine.
- CNI plugin not initialised (`NetworkPluginNotReady`), typically on a freshly joined node
  whose CNI DaemonSet has not landed or is failing.
- The instance is gone or unhealthy at the infrastructure layer — terminated, hardware
  failure, spot/preemptible reclaim.
- Network partition between the node and the API server, or expired kubelet client
  certificates (`KubeletClientCertificateExpiration` often precedes this).
- Planned node replacement — an autoscaler scale-down or a rolling node upgrade that is
  working as designed and simply looks alarming.

# Resolution

- **Establish planned vs unplanned first.** A node being replaced by an autoscaler or an
  upgrade needs no action, and intervening can make it worse.
- For an unplanned single node: drain it (`kubectl drain <node> --ignore-daemonsets`) to move
  workloads deliberately rather than waiting out the eviction timeout, then replace it. On
  managed node pools, replacement is usually more reliable than repair.
- Restore the container runtime or CNI if the node is reachable and the condition names one.
- If several nodes are affected, stop treating nodes: look at the control plane, the CNI
  DaemonSet, the node image version, or the cloud provider's status.
- Reduce the blast radius for next time — `topologySpreadConstraints` and PodDisruptionBudgets
  determine how much a single node's loss actually costs.

# Not covered

- **Node resource pressure specifically.** `MemoryPressure` and `DiskPressure` evict pods
  while the node stays Ready; those have their own playbooks and their own fixes.
- **Pods that cannot be scheduled** because no node has room — a capacity problem, not a
  node-health problem.
- **Host-level operating system debugging**, kernel issues, and hardware diagnostics.
- **Control-plane and etcd health**, even though it produces a similar many-nodes-NotReady
  picture.
- **Cloud provider instance lifecycle**, spot interruption handling, and autoscaler
  configuration.
- **Tuning `tolerationSeconds` or pod-eviction timeouts** for a workload.
