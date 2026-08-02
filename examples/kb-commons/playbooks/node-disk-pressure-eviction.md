---
type: Playbook
title: Node under disk pressure evicting pods and refusing image pulls
description: A node reports DiskPressure so the kubelet evicts pods and garbage-collects images; NodeFilesystemAlmostOutOfSpace or NodeFilesystemFilesFillingUp fires and new pods cannot start on that node.
tags: [node, disk, DiskPressure, eviction, Evicted, ephemeral-storage, emptyDir, image-gc, containerd, inodes, logs, NodeFilesystemAlmostOutOfSpace, NodeFilesystemSpaceFillingUp, NodeFilesystemFilesFillingUp, NodeFilesystemAlmostOutOfFiles, KubeNodeNotReady, KubePodNotReady]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`kubectl describe node <node>` shows `DiskPressure=True`. Pods on that node are `Evicted`
with `The node was low on resource: ephemeral-storage` (or `: inodes`). The node-exporter
mixin fires `NodeFilesystemAlmostOutOfSpace` / `NodeFilesystemSpaceFillingUp`, and
`NodeFilesystemAlmostOutOfFiles` / `NodeFilesystemFilesFillingUp` for the inode variant.

Secondary effects that look unrelated: image pulls start failing on that node only, and the
kubelet aggressively garbage-collects images, so pods that used to start instantly now pull
every time. If pressure keeps rising the node goes `NotReady`.

**Inode exhaustion is the version people miss** — `df -h` shows free space while every write
fails.

# Investigate

1. `kubectl describe node <node>` — confirm `DiskPressure` and read the eviction events for
   which resource (`ephemeral-storage` vs `inodes`) triggered it.
2. Identify the filesystem under pressure from the node-exporter series:
   `node_filesystem_avail_bytes{instance="<node>"} / node_filesystem_size_bytes` and
   `node_filesystem_files_free / node_filesystem_files`, broken down by `mountpoint`. The
   container runtime root and `/var/log` are the usual ones, and they are often separate
   filesystems.
3. Rank ephemeral-storage consumers:
   `kubectl top pods -A` does **not** show ephemeral storage. Use
   `kubelet_volume_stats_*` for volumes, and the node's own `du` on the runtime and log
   directories, to find the consumer.
4. Check whether pods declare `ephemeral-storage` requests/limits at all
   (`kubectl get pod <pod> -o jsonpath='{.spec.containers[*].resources}'`). Almost none do,
   which is why one pod can consume a whole node's disk without the scheduler noticing.
5. Look for the usual space sinks on the node: container logs under `/var/log/pods`, unused
   images in the runtime store, `emptyDir` volumes, and terminated pods that were never
   cleaned up.
6. Is it one node or the pool? A pool-wide pattern means the node disk is undersized or the
   image/log growth is systemic, not a single misbehaving pod.

# Common causes

- A container writing high-volume logs to stdout with no rotation limit; the node's log
  directory grows until the filesystem is full.
- An `emptyDir` used as scratch space with no `sizeLimit`, filling the node's disk.
- Image accumulation: many image versions pulled over time, with the runtime's garbage
  collection thresholds never reached until pressure hits.
- A workload writing to the container filesystem instead of a PVC.
- Inode exhaustion from a cache or spool directory with millions of small files.
- Node root disk sized for the base image, not for the workload's real ephemeral usage.

# Resolution

- **Reclaim space first** — reversible and immediate: prune unused images on the node, clear
  rotated logs, delete completed/failed pods that still hold storage.
- Fix the producer: cap the offending container's log volume, set a `sizeLimit` on the
  `emptyDir`, or move scratch data to a PVC.
- Set `ephemeral-storage` requests and limits on workloads that use real scratch space, so
  the scheduler and the kubelet can both act on it instead of discovering it at eviction
  time.
- Configure container log rotation (kubelet `containerLogMaxSize`/`containerLogMaxFiles`) and
  the runtime's image GC thresholds at the node-image level, so every node inherits them.
- Grow the node disk, or replace the node, if usage is legitimate and pool-wide.
- Cordon the node while working on it so new pods are not scheduled into the problem.

# Not covered

- **PersistentVolume capacity.** `KubePersistentVolumeFillingUp` is a workload's own volume
  filling up — completely different resource, different fix.
- **Memory-pressure eviction**, which shares the eviction mechanism and nothing else.
- **Nodes going NotReady** for reasons other than disk.
- **Node OS image and disk sizing decisions**, and how to change them for a managed node
  pool.
- **Log pipeline architecture** — whether logs should be on the node at all.
- **Container runtime internals** and its garbage-collection implementation.
