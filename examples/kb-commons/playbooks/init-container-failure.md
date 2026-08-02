---
type: Playbook
title: Pod blocked in Init state because an init container fails
description: KubePodNotReady and KubeContainerWaiting fire while a pod sits in Init or Init:CrashLoopBackOff; an init container never exits 0, so the main containers never start and the rollout stalls.
tags: [pod, initcontainer, init, PodInitializing, Init-CrashLoopBackOff, migration, sidecar, KubePodNotReady, KubeContainerWaiting, KubeDeploymentReplicasMismatch, KubeStatefulSetReplicasMismatch]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`kubectl get pods` shows a status of `Init:0/2`, `Init:Error`, `Init:CrashLoopBackOff`, or a
long `PodInitializing`. `KubePodNotReady` fires. Init containers run **sequentially and to
completion before any app container starts**, so a single failing init container blocks the
whole pod indefinitely — and the main container's logs are empty because it never ran.

The `Init:N/M` counter tells you exactly which one is stuck: the failing container is index
`N` (zero-based) in `.spec.initContainers`.

# Investigate

1. Name the init containers in order:
   `kubectl -n <ns> get pod <pod> -o jsonpath='{.spec.initContainers[*].name}'`.
2. Read the failing one's logs — **you must pass `-c`**, and `--previous` if it is looping:
   `kubectl -n <ns> logs <pod> -c <init-container> --previous`.
   Without `-c`, `kubectl logs` targets an app container that never started and tells you
   nothing.
3. `kubectl -n <ns> describe pod <pod>` — the `Init Containers` block shows each one's state,
   exit code, and reason. Events cover mount and image failures that produce no logs.
4. If it is waiting rather than failing (`PodInitializing` for minutes), ask what it is
   waiting *for*: a dependency check loop, a lock, a leader election, a volume.
5. Check whether the init container images pull at all — `Init:ImagePullBackOff` is a
   registry problem, not an init-logic problem.

# Common causes

- A "wait for dependency" init container polling a service that is down, unreachable, or
  behind a NetworkPolicy that denies it. It loops forever by design.
- A database schema migration that fails or blocks on a lock held by another pod. With
  multiple replicas rolling at once, migrations can deadlock against each other.
- A config-fetch init container (secrets manager, vault agent, config service) failing on
  auth, or on a token whose ServiceAccount lost its binding.
- A permission error writing to a shared `emptyDir` after `runAsNonRoot`/`fsGroup` changed —
  the init container writes as one UID and the app container reads as another.
- A missing ConfigMap/Secret referenced by the init container; the pod never leaves
  `PodInitializing` and only events say why.
- Init container image tag wrong or not pushed.

# Resolution

- Fix what the init container is waiting on. When it is a dependency check, the init
  container is reporting a real outage — resolve that, and the pod proceeds without any
  change to this workload.
- For a stuck migration, clear the lock or let the in-flight migration finish, then let the
  init container retry. If replicas are deadlocking against each other, scale to one replica
  for the migration or move the migration to a Job/Helm hook that runs once.
- For auth/config fetch failures, restore the credential or binding — the init container is
  a messenger.
- **Do not delete the init container to unblock the rollout.** It exists to guarantee a
  precondition; removing it converts a blocked rollout into an app that starts against a
  broken precondition.

# Not covered

- **Native sidecar containers** (init containers with `restartPolicy: Always`). Those are
  meant to keep running and do not "complete"; a pod waiting on one behaves differently.
- **Main-container crash loops** once init has completed — different playbook, and `logs`
  works normally there.
- **`Init:ImagePullBackOff`**, which is the image-pull playbook applied to an init container.
- **Pods stuck `Pending`**, which have not been scheduled and have not started init at all.
- **Writing or fixing the migration itself**, or choosing a migration strategy for a
  multi-replica rollout.
- Vault/secrets-operator agent configuration specifics.
