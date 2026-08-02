---
type: Playbook
title: Container OOMKilled with exit code 137
description: A container is killed by the kernel OOM killer because its working set exceeds its memory limit; KubeContainerOOMKilled or KubePodCrashLooping fires and the pod restarts with exit code 137.
tags: [container, memory, oom, oomkilled, limits, requests, exit-137, KubeContainerOOMKilled, KubePodCrashLooping, KubeDeploymentReplicasMismatch, working-set]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

A container restarts with `Last State: Terminated`, `Reason: OOMKilled`, `Exit Code: 137`.
It typically surfaces as `KubePodCrashLooping` (the upstream kubernetes-mixin rule) plus a
platform-specific OOM rule — `KubeContainerOOMKilled` is the widely used name, built on
`kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}`; it is **not** part
of the upstream mixin, so check your own rules.

Two shapes, and they need different fixes:

- **Immediate OOM at startup** — the limit is simply below what the process needs to boot.
- **OOM after minutes or hours** — usually a leak, an unbounded cache, or a workload whose
  memory scales with request volume.

# Investigate

1. `kubectl -n <ns> describe pod <pod>` — confirm `Reason: OOMKilled` on the terminated
   state and note **which container**. A sidecar can be the one being killed.
2. Compare usage to the limit over time:
   `container_memory_working_set_bytes{namespace="<ns>",pod=~"<pod>.*",container!=""}`
   against
   `kube_pod_container_resource_limits{resource="memory",namespace="<ns>",pod=~"<pod>.*"}`.
   Working set — not RSS, not cache — is what the kernel compares against the limit.
3. Look at the shape of the curve before the kill. A sawtooth that resets on each restart
   means a leak. A step change means a workload or config change.
4. `kubectl -n <ns> get pod <pod> -o jsonpath='{.spec.containers[*].resources}'` — is the
   limit set at all, and how far is it from the request? A limit far above the request means
   the pod can be scheduled somewhere it cannot actually run.
5. Correlate with the last change: an image bump, a new feature flag, a JVM/runtime version
   change, or a heap/GC setting that no longer matches the limit.

# Common causes

- Memory limit set below the real working set — the limit was a guess and was never revised.
- A runtime that does not read cgroup limits and sizes its heap from host memory. Modern
  JVMs and .NET honour cgroups; older ones, and many custom allocators, do not. Explicit
  heap sizing is the fix.
- An unbounded in-process cache or connection pool that grows with traffic.
- A genuine leak introduced by the last image.
- A large batch/report operation that spikes far above steady-state usage; the pod dies only
  when that path is exercised.
- Memory limit lowered by a values change nobody associated with this workload.

# Resolution

- **Short term**: raise the limit to comfortably above the observed peak working set, in
  Git, and reconcile. Raise the **request** with it — a request far below the limit puts the
  pod on nodes that cannot honour the limit and turns one workload's growth into a node-wide
  problem.
- **Then decide whether the limit was wrong or the code is.** A leak fixed by raising the
  limit comes back with a longer fuse. Say which one you concluded and why.
- For runtimes with their own heap setting, size the heap **below** the container limit and
  leave headroom for non-heap memory; a limit raise alone will not help if the runtime never
  grows into it.

# Not covered

- **Node-level memory pressure and eviction.** If pods are being *evicted* with
  `Reason: Evicted` rather than OOMKilled, the node is out of memory, not the container.
  Different playbook, different fix.
- **Exit code 137 without `OOMKilled`** — that is a plain SIGKILL (a failed liveness probe
  can produce it) and is not a memory problem.
- **Choosing a limit value for you.** The right limit depends on the workload's peak, not on
  a rule of thumb.
- **Profiling the application** to find the leak.
- **JVM/Go/Node runtime tuning** beyond the cgroup-awareness point above.
- Cluster-level overcommit policy and whether requests are sized correctly fleet-wide.
