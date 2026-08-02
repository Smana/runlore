---
type: Playbook
title: PersistentVolume filling up and about to run out of space
description: KubePersistentVolumeFillingUp or KubePersistentVolumeInodesFillingUp fires because a bound volume is projected to exhaust its space or inodes; writes start failing and the workload degrades before the volume is actually full.
tags: [storage, pvc, persistentvolume, disk, capacity, inodes, expansion, KubePersistentVolumeFillingUp, KubePersistentVolumeInodesFillingUp, KubePersistentVolumeErrors, allowVolumeExpansion, kubelet_volume_stats]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`KubePersistentVolumeFillingUp` fires (upstream kubernetes-mixin, based on
`kubelet_volume_stats_available_bytes / kubelet_volume_stats_capacity_bytes`). The rule has
two forms — a critical one at very low free space and a warning one that **predicts**
exhaustion within a few days from the recent trend, so it can fire while the volume still
looks fine.

`KubePersistentVolumeInodesFillingUp` is the same shape for inodes: a volume with plenty of
free bytes and no free inodes fails writes exactly as hard, and `df -h` will not show it.
Downstream, the workload logs `no space left on device` and may crash-loop.

# Investigate

1. Identify the claim and its consumer:
   `kubectl -n <ns> get pvc` then `kubectl -n <ns> describe pvc <pvc>` (the `Used By` field).
2. Read both metrics, not just bytes:
   - `kubelet_volume_stats_available_bytes / kubelet_volume_stats_capacity_bytes`
   - `kubelet_volume_stats_inodes_free / kubelet_volume_stats_inodes`
   A near-zero inode ratio with healthy byte ratio means millions of small files.
3. Find what is consuming it from inside the pod:
   `kubectl -n <ns> exec <pod> -- df -h <mountpath>` and
   `kubectl -n <ns> exec <pod> -- du -xh --max-depth=1 <mountpath> | sort -h`.
   Add `df -i` for inodes.
4. Look at the growth curve. Steady linear growth is normal data accumulation and needs
   capacity planning; a sudden knee means a specific event — log verbosity raised, retention
   disabled, a job dumping output, a rotation that stopped.
5. Check whether expansion is even possible:
   `kubectl get storageclass <class> -o jsonpath='{.allowVolumeExpansion}'`.

# Common causes

- Log or trace files written to the volume with no rotation, or rotation that broke.
- Retention/compaction disabled or misconfigured in a database, metrics store, or broker.
- A backup or export job writing to the data volume and never cleaning up.
- Deleted-but-open files: the process still holds the descriptor, so space is not returned
  until it is restarted. `du` and `df` disagree, which is the tell.
- Volume sized for launch-day data and never revisited while the workload grew.
- Inode exhaustion from a cache or queue directory holding millions of tiny files.

# Resolution

- **Reclaim space first** — it is reversible and usually enough to stop the bleeding: rotate
  or delete old logs, trigger compaction, clear the cache directory, remove stale backups.
- Restart the holder if deleted-but-open files are the cause; nothing else returns that
  space.
- **Expand the volume** when the growth is legitimate: edit
  `spec.resources.requests.storage` upward in Git and reconcile. This requires
  `allowVolumeExpansion: true` on the StorageClass. Expansion is **one-way** — a PVC cannot
  be shrunk afterwards.
- Some drivers need a pod restart to complete the file-system resize; check the PVC's
  `FileSystemResizePending` condition before declaring it done.
- Fix the retention policy in the same change, otherwise the alert returns at the new size.

# Not covered

- **Node ephemeral disk pressure and eviction.** That is the node's own filesystem, not a
  PV, and pods get evicted rather than failing writes. Separate playbook.
- **PVCs that never bound** — a Pending PVC has no usage to report.
- **Shrinking a volume**, which Kubernetes does not support; the path is create-new,
  migrate, delete-old.
- **Snapshot, backup, and restore** procedures.
- **Choosing retention policy values** for any specific database or log pipeline.
- Storage-class and cloud-provider expansion limits and cooldowns.
