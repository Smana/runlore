---
okf_version: "0.1"
type: Index
title: RunLore Knowledge Commons
description: Generic, vendor-neutral OKF playbooks for common Kubernetes and GitOps failure classes, so a new RunLore deployment has something to ground an investigation on from day one.
---

# RunLore Knowledge Commons

An [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog) bundle of **generic**
playbooks covering failure *classes* that every Kubernetes platform hits: GitOps
reconciliation failures, workload crash/OOM/probe problems, storage, networking,
certificates, and node health.

Point a deployment at it (`catalog.dir`) — on its own, or alongside your own catalog — so
`kb_search` returns something useful before you have written a single entry of your own.

## What these entries are, and what they are not

Every entry here is `type: Playbook` and carries **no `resource:` field**. That is
deliberate, and it has one concrete consequence you should understand before relying on
this bundle:

- **They do not fire instant recall.** Recall applies a structural workload filter before
  any scoring: a resource-less entry can only agree with a request that itself carries no
  workload. Real Kubernetes alerts carry a namespace and a workload, so they will never
  match an entry here. See the `resource` row in the OKF format reference.
- **They do ground investigations.** `kb_search` applies no structural filter. The model can
  find and cite any of these mid-loop, which is where their value is: they tell the
  investigation which command to run next and which readings distinguish one cause from
  another.

If you want instant recall, you need entries scoped to **your** workloads
(`resource: <namespace>/<name>`). Those are the entries RunLore drafts from your own
incidents. This bundle is the floor, not the goal.

## Scope discipline

Each entry ends with a **`# Not covered`** section, and it is not decoration. A confidently
wrong cached answer is worse than no answer at all, so every entry states the adjacent
failures it must not be applied to — crash-loop vs OOMKill vs readiness failure, PV filling
up vs node disk pressure, unbound PVC vs volume node-affinity conflict. Read it before
acting on an entry.

Entries name alert conditions in their `tags` and `description` so a query built from an
alert can find them. Where an alert name is not part of an upstream rule bundle (Argo CD has
none; `KubeContainerOOMKilled` is a common convention, not upstream), the entry says so
rather than implying a standard exists.

## Playbooks

### GitOps

- [Flux HelmRelease stuck Ready=False after an upgrade](playbooks/flux-helmrelease-upgrade-failed-20260802.md)
- [Flux HelmRelease terminal state with retries exhausted or Stalled](playbooks/flux-helmrelease-retries-exhausted-20260802.md)
- [Flux Kustomization Ready=False on build or apply failure](playbooks/flux-kustomization-build-failed-20260802.md)
- [Argo CD Application health Degraded after a sync](playbooks/argocd-application-degraded-20260802.md)
- [Argo CD Application stuck OutOfSync and never converging](playbooks/argocd-application-outofsync-stuck-20260802.md)

### Workloads

- [Pod in CrashLoopBackOff shortly after a deploy](playbooks/pod-crashloopbackoff-after-deploy-20260802.md)
- [Container OOMKilled with exit code 137](playbooks/container-oomkilled-20260802.md)
- [Pod Running but never Ready because the readiness probe fails](playbooks/readiness-probe-failure-20260802.md)
- [Pod stuck in ImagePullBackOff or ErrImagePull](playbooks/imagepullbackoff-20260802.md)
- [Pod blocked in Init state because an init container fails](playbooks/init-container-failure-20260802.md)

### Storage

- [PersistentVolumeClaim stuck Pending and never bound](playbooks/pvc-pending-unbound-20260802.md)
- [PersistentVolume filling up and about to run out of space](playbooks/persistentvolume-filling-up-20260802.md)
- [Pod unschedulable with volume node affinity conflict](playbooks/volume-node-affinity-conflict-20260802.md)

### Networking

- [In-cluster DNS resolution failures via CoreDNS](playbooks/dns-resolution-failure-20260802.md)
- [Traffic silently dropped by a NetworkPolicy](playbooks/networkpolicy-denial-20260802.md)
- [Service has no endpoints so traffic to it fails](playbooks/service-no-endpoints-20260802.md)

### Certificates

- [cert-manager Certificate not ready with a stuck ACME order or challenge](playbooks/certmanager-challenge-stuck-20260802.md)
- [TLS certificate approaching expiry without renewing](playbooks/certificate-expiring-soon-20260802.md)

### Nodes and scheduling

- [Node NotReady or unreachable and its pods stop being rescheduled promptly](playbooks/node-notready-20260802.md)
- [Node under memory pressure evicting pods](playbooks/node-memory-pressure-eviction-20260802.md)
- [Node under disk pressure evicting pods and refusing image pulls](playbooks/node-disk-pressure-eviction-20260802.md)
- [Pod stuck Pending because the scheduler finds no suitable node](playbooks/pod-unschedulable-20260802.md)

## Contributing

`index.md` and `log.md` are reserved bundle files and are never indexed. Everything else
under this directory is an entry.

Validate before opening a PR — CI runs exactly this:

```bash
go run ./cmd/lore validate-kb examples/kb-commons
```

New entries here must stay generic: no cluster names, namespaces, organisation names, IP
addresses, or vendor-specific paths, and no `resource:` field. An entry tied to one
platform's workloads belongs in that platform's own catalog, not in the commons.
