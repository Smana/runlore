---
type: Playbook
title: Pod unschedulable with volume node affinity conflict
description: A pod with a bound PVC cannot be scheduled - FailedScheduling reports "volume node affinity conflict" or "had volume node affinity conflict" - because the zonal or node-local volume lives where no eligible node is.
tags: [storage, pvc, persistentvolume, scheduling, topology, zone, node-affinity, local-volume, FailedScheduling, KubePodNotReady, KubeStatefulSetReplicasMismatch, WaitForFirstConsumer, nodeAffinity]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

A pod stays `Pending`. `kubectl -n <ns> describe pod <pod>` shows a `FailedScheduling` event
containing `volume node affinity conflict` (often alongside counts like
`3 node(s) had volume node affinity conflict`). `KubePodNotReady` and
`KubeStatefulSetReplicasMismatch` follow.

The distinguishing feature versus an unbound PVC: **the PVC is `Bound`**. The volume exists;
it just exists somewhere the pod cannot be placed. This typically appears after a node was
replaced, a zone lost capacity, or a StatefulSet pod was rescheduled.

# Investigate

1. Confirm the PVC is bound: `kubectl -n <ns> get pvc` → `STATUS Bound`. If it is Pending,
   you are in the unbound-PVC playbook instead.
2. Read the PV's topology constraint — this is the actual constraint being violated:
   `kubectl get pv <pv> -o jsonpath='{.spec.nodeAffinity}'`.
   It will pin a zone (`topology.kubernetes.io/zone`), a region, or a single node
   (`kubernetes.io/hostname`) for local volumes.
3. List candidate nodes and their labels:
   `kubectl get nodes -L topology.kubernetes.io/zone`.
   Is there any Ready, schedulable node in the required zone at all?
4. Read the pod's other placement constraints — `nodeSelector`, `affinity`,
   `tolerations`, `topologySpreadConstraints`. The conflict is often between the volume's
   zone and a *newly added* affinity rule, not a storage change.
5. For a single-node PV (`kubernetes.io/hostname`), check that node's status. A local volume
   whose node is gone is unrecoverable by scheduling alone.
6. Check taints on nodes in the required zone — a fully tainted zone looks identical to an
   empty one from the pod's perspective.

# Common causes

- The node holding a `local` or host-path-backed volume was drained, replaced, or removed.
  Cluster autoscaling and node auto-upgrade do this routinely.
- The volume's zone has no schedulable capacity — autoscaler cannot add a node there, or the
  instance type is unavailable in that zone.
- Pod affinity/nodeSelector added later that conflicts with the volume's zone (for example
  pinning to a node pool that exists in only one zone, while the volume is in another).
- All nodes in the required zone are cordoned, tainted, or NotReady.
- A `WaitForFirstConsumer` class was changed to `Immediate` later, so volumes were
  provisioned in a zone chosen before the pod's constraints were known.
- Multi-zone StatefulSet where each replica's volume anchors it to a different zone, and one
  zone is unavailable.

# Resolution

- **Restore a node in the volume's zone** — uncordon, remove the taint, or let the
  autoscaler bring capacity back. Reversible, non-destructive, and it preserves the data.
- Remove or widen the conflicting pod-level constraint if that is what changed; the volume
  did not move, the policy did.
- For a genuinely lost local volume, the data is gone with the node. Delete the PVC and let
  the workload reprovision **only after confirming the data is disposable or restorable from
  backup** — this is destructive and irreversible.
- Longer term, use `volumeBindingMode: WaitForFirstConsumer` on zonal classes so volumes are
  provisioned where the pod can actually run, and avoid `local` volumes for workloads that
  must survive node replacement.

# Not covered

- **Unbound / Pending PVCs.** Different failure, different event text.
- **Mount and attach failures** on a schedulable node (`FailedAttachVolume`,
  `Multi-Attach error for volume`) — the pod is placed but the volume will not attach.
- **Generic unschedulable pods** (insufficient CPU/memory, taints, no capacity) with no
  volume involved.
- **Data recovery** from a lost node or a deleted volume.
- **Choosing a zonal vs regional storage class** for a given workload — an architecture
  decision.
- Cluster-autoscaler and Karpenter zone-selection configuration.
