---
type: Playbook
title: Pod in CrashLoopBackOff shortly after a deploy
description: KubePodCrashLooping fires because a container exits repeatedly right after a new image or config landed; the kubelet backs off restarts and the Deployment never completes its rollout.
tags: [pod, container, crashloopbackoff, restart, deploy, rollout, KubePodCrashLooping, KubeContainerWaiting, KubeDeploymentReplicasMismatch, KubeDeploymentRolloutStuck, exit-code]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`KubePodCrashLooping` fires. `kubectl get pods` shows `CrashLoopBackOff` with a climbing
restart count, and `KubeDeploymentReplicasMismatch` or `KubeDeploymentRolloutStuck` follows
because the new ReplicaSet never reaches its desired count. A change — image tag, ConfigMap,
Secret, command args — usually landed in the preceding minutes.

# Investigate

1. **Read the previous container's logs, not the current one's**:
   `kubectl -n <ns> logs <pod> --previous`. The running container is in backoff; the crash
   evidence is in the terminated instance. This single flag is the most common reason an
   investigation stalls.
2. `kubectl -n <ns> describe pod <pod>` — `Last State: Terminated` gives the **exit code**,
   which narrows the cause sharply:
   - `1` / `2` — application error; the logs have it.
   - `137` — SIGKILL. Check `Reason: OOMKilled` before assuming a crash.
   - `139` — SIGSEGV, native crash.
   - `143` — SIGTERM; something asked it to stop.
   - `0` with restarts — the process exits cleanly; a long-running container that returns 0
     still crash-loops. Usually a wrong `command`/`args` or a one-shot process in a
     Deployment.
3. Confirm what changed: compare the pod's image and its ConfigMap/Secret checksum
   annotation against the previous ReplicaSet
   (`kubectl -n <ns> get rs -l app=<label> --sort-by=.metadata.creationTimestamp`).
4. `kubectl -n <ns> get events --sort-by=.lastTimestamp` — mount failures, missing keys, and
   admission rejections show up here and never in the container logs.

# Common causes

- Application startup error introduced by the new image (bad migration, missing dependency,
  failed self-check).
- A required environment variable, ConfigMap key, or Secret key is missing or renamed. The
  pod fails to start at all and the event says so.
- Wrong `command`/`args` after a base-image change (entrypoint moved, shell absent in a
  distroless image).
- The container cannot reach a dependency at startup and exits rather than retrying —
  database, message broker, config service.
- Memory limit below actual startup usage — check `OOMKilled` before treating this as an
  application bug.
- A file-system permission problem after `runAsNonRoot` or `fsGroup` changed.

# Resolution

- **Roll back first** when a deploy caused it: revert the image/config commit in Git and let
  the GitOps controller reconcile. `kubectl rollout undo` is faster but will be reverted by
  the next reconcile — use it only to buy time.
- Fix forward from the exit code and the `--previous` logs: restore the missing key, correct
  the entrypoint, raise the limit, or fix the application bug.
- If the container exits `0`, check whether the workload belongs in a Job/CronJob rather
  than a Deployment.

# Not covered

- **OOMKilled specifically.** Exit 137 with `Reason: OOMKilled` is a memory-sizing problem
  with its own playbook; the remedy there is a limit change, not a rollback.
- **Pods that stay `Running` but never become Ready** — that is a readiness-probe problem,
  not a crash loop. Restart count stays at zero, which is the distinguishing signal.
- **`ImagePullBackOff` / `ErrImagePull`.** The container never started, so there are no
  previous logs to read.
- **Init-container failures**, which show `Init:CrashLoopBackOff` and need
  `logs -c <init-container>`.
- **Node-level causes** (kubelet restarting containers, disk pressure eviction) — those hit
  many unrelated pods at once, which is how you tell them apart.
- Application-specific debugging beyond identifying which change introduced the crash.
