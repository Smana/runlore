---
type: Playbook
title: PersistentVolumeClaim stuck Pending and never bound
description: A PVC stays Pending so its pod cannot be scheduled - KubePodNotReady and KubePersistentVolumeErrors fire - because no StorageClass provisioner satisfies it, the class is missing, or capacity or topology constraints cannot be met.
tags: [storage, pvc, persistentvolumeclaim, pending, unbound, storageclass, provisioner, csi, WaitForFirstConsumer, KubePersistentVolumeErrors, KubePodNotReady, KubeStatefulSetReplicasMismatch, FailedScheduling]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`kubectl -n <ns> get pvc` shows `STATUS Pending` with no volume bound. The dependent pod
sits in `Pending` with a `FailedScheduling` event mentioning
`pod has unbound immediate PersistentVolumeClaims`, and `KubePodNotReady` /
`KubeStatefulSetReplicasMismatch` fire. `KubePersistentVolumeErrors` fires when provisioning
actively failed rather than merely waiting.

**One benign case to rule out first**: with `volumeBindingMode: WaitForFirstConsumer`, a PVC
is *supposed* to stay Pending until a pod that uses it is scheduled. Pending PVC + no pod
referencing it = normal, not an incident.

# Investigate

1. `kubectl -n <ns> describe pvc <pvc>` — provisioning events state the reason directly
   (`storageclass.storage.k8s.io "x" not found`, quota exceeded, zone mismatch, provisioner
   error).
2. `kubectl get storageclass` — does the class the PVC names exist? Is there a default class
   (`storageclass.kubernetes.io/is-default-class: "true"`) for a PVC that names none? A
   cluster with no default class leaves every class-less PVC Pending forever.
3. `kubectl -n <ns> get pvc <pvc> -o yaml` — read `spec.storageClassName`,
   `spec.accessModes`, `spec.resources.requests.storage`, and any `selector`. An
   `accessModes: [ReadWriteMany]` request against a block-storage provisioner that only
   offers `ReadWriteOnce` never binds.
4. Check the CSI controller is alive: its pods in the storage namespace, and their logs. A
   dead or crash-looping provisioner leaves every new PVC Pending with no error on the PVC.
5. For static provisioning, list available PVs (`kubectl get pv`) and compare class, size,
   access modes, and `claimRef`. A PV bound to a deleted claim (`Released`) is not reusable
   until reclaimed.
6. Topology: if the class is zonal and the pod has node affinity elsewhere, no node can
   satisfy both. The scheduler event names the conflict.

# Common causes

- The named StorageClass does not exist, or the cluster has no default class and the PVC
  names none.
- The CSI driver / provisioner is not installed, not running, or lacks the cloud IAM
  permission to create volumes.
- Requested `accessModes` unsupported by the provisioner (`ReadWriteMany` on a block driver
  is the classic).
- Cloud quota or per-zone capacity exhausted for that volume type.
- Topology mismatch: zonal volume versus the zone the pod can actually be scheduled in.
- Static PV expected but none matches the class/size/access modes, or the candidate PV is
  `Released` and not yet recycled.
- A `ResourceQuota` on the namespace caps `requests.storage` or `persistentvolumeclaims`.

# Resolution

- Install or name an existing StorageClass, or set a cluster default — in Git, so it is not
  a one-off `kubectl` fix that vanishes at the next reconcile.
- Restore the CSI provisioner (or its cloud credentials) if it is the blocker; every PVC in
  the cluster is affected, so this is a platform incident.
- Correct `accessModes` or pick a class that supports them.
- Raise the quota, or provision in a zone/type with capacity.
- A PVC's `storageClassName`, size (downward), and access modes are **immutable once
  created**. Fixing them means deleting and recreating the PVC — which for a StatefulSet
  means deleting the claim the controller generated. Confirm the data is disposable or
  backed up before you do that.

# Not covered

- **Bound PVCs that fill up.** A volume running out of space is
  `KubePersistentVolumeFillingUp` and a different problem entirely.
- **Volume node-affinity conflicts on an already-bound PVC**, where the volume exists but no
  node can attach it — its own playbook.
- **Mount and attach failures** (`FailedMount`, `FailedAttachVolume`) after successful
  binding, including multi-attach errors.
- **Volume expansion** of an existing PVC, and whether the class allows it.
- **Backup, snapshot, and restore** procedures.
- Cloud-provider-specific volume type selection and IOPS sizing.
