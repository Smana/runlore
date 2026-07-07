# StatefulSet mode for real HA (per-replica storage)

## Why

`replicaCount: 2` + `persistence.enabled: true` today means N pods all reference the
**same** RWO PVC via one hardcoded `claimName` in `deployment.yaml`. That's fine on a
single-node dev cluster (the e2e-k3d leader-election check passes) but on a real
multi-node cluster a standby scheduled on a different node cannot attach an
already-attached RWO (EBS-class) volume — it would hang. The e2e suite doesn't catch
this because it uses `catalog.configMap` + an ephemeral `/tmp` ledger path, not
`persistence.enabled`.

Add a `workloadKind: Deployment | StatefulSet` toggle. `StatefulSet` mode uses
`volumeClaimTemplates` so every replica gets its own volume — no shared-attach
conflict, and leader election (already topology-agnostic) decides who's active.
`Deployment` stays the default; nothing changes for existing installs.

## Tasks

1. **Render test (fast, no cluster)** — `deploy/helm/runlore/hack/test-workloadkind.sh`:
   asserts default render is unchanged (`kind: Deployment`, single PVC via `pvc.yaml`),
   and `--set workloadKind=StatefulSet --set persistence.enabled=true` renders
   `kind: StatefulSet` with a `volumeClaimTemplates` entry, a headless Service, no
   standalone `pvc.yaml` PVC, and no `persistentVolumeClaim` volume mount (that's what
   `volumeClaimTemplates` replaces). Write it first, watch it fail.
2. **Chart implementation**:
   - `values.yaml` / `values.schema.json`: add `workloadKind` (enum, default
     `Deployment`) and a `headless` service toggle needed for StatefulSet.
   - `_helpers.tpl`: extract the pod `template:` block (metadata + spec) already shared
     verbatim into a named template so Deployment and StatefulSet render identically
     except for the workload-level fields — no drift between the two modes.
   - `templates/statefulset.yaml`: new template, `serviceName`, `volumeClaimTemplates`
     when `persistence.enabled`, reuses the shared pod template.
   - `templates/deployment.yaml`: guard on `workloadKind != "StatefulSet"`.
   - `templates/pvc.yaml`: guard on `workloadKind != "StatefulSet"` (StatefulSet owns its
     PVCs via volumeClaimTemplates instead).
   - `templates/service.yaml`: headless variant (`clusterIP: None`) for StatefulSet mode
     (required by the StatefulSet API even though the webhook Service already exists for
     traffic — Kubernetes needs a governing headless Service for the ordinal DNS).
3. **e2e-k3d extension** — extend `hack/e2e-k3d.sh`'s existing "leader election +
   failover" step: after the plain-Deployment 2-replica check, add a second sub-check
   that upgrades to `workloadKind=StatefulSet` + `persistence.enabled=true` +
   `replicaCount=2` against a real (if small) PVC-backed StorageClass in k3d, and
   asserts 2 independently-bound PVCs + leader election/failover still holds.
4. **Docs** — `values.yaml` comments + README HA section: when to use StatefulSet mode,
   the failover trade-off (new leader starts with an empty local outcome ledger unless
   using a shared filesystem — catalog is unaffected, it re-clones from git).

## Non-goals

Not touching the RWX/EFS path — that's a separate, heavier option for anyone who wants
zero data loss on failover. This plan only makes `replicaCount>1` safe to run at all on
real multi-node clusters.
