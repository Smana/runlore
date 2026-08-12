---
title: Upgrade & Uninstall
weight: 30
---

How to upgrade RunLore in place, what survives a restart, and how to remove it cleanly.

## Upgrading

RunLore is a Helm release — upgrade like any other chart (the chart is an OCI artifact on GHCR;
from a clone of this repo, use the `deploy/helm/runlore` path instead):

```bash
helm upgrade runlore oci://ghcr.io/smana/charts/runlore -n runlore -f values.yaml
```

Your `values.yaml` is the source of truth; the entire agent config under `values.config` is rendered
verbatim into the ConfigMap. Re-apply the same file (with your changes) on every upgrade.

> [!WARNING]
> **Expect ~20s of downtime during the agent's own upgrade (default strategy)**
>
> The Deployment ships with **`strategy.type: Recreate`**: old pods are terminated **before** new
> ones start, so the agent is briefly unavailable while the new version boots and wins the leader
> lease. Set `updateStrategy: RollingUpdate` for near-zero-downtime upgrades (see below).

### `Recreate` vs `RollingUpdate`

Historically `Recreate` was **forced** by a readiness deadlock: `/readyz` was gated on leadership, a
new pod couldn't go `Ready` while the old leader held the `Lease`, and the rolling update stalled
(it also meant standbys were never `Ready`, so `helm upgrade --wait` / Flux kstatus timed out with
`replicaCount > 1`).

That deadlock is gone: readiness now reflects **catalog warmth only** — every warm replica is
`Ready`, and a non-leader replica proxies incoming work to the leader. `RollingUpdate` therefore
works: the new pod warms up, goes `Ready`, the old leader terminates (draining, then releasing the
`Lease`), and a surviving pod acquires it.

`Recreate` remains the shipped default for one reason: it never lets two agent **versions** overlap
mid-rollout (briefly co-serving webhooks and contending for the same lease across versions). The
cost is a short gap **only during the upgrade itself** — crash-failover HA is unaffected: if the
leader dies unexpectedly, the hot standby takes over within the lease window (15s lease / 10s renew
/ 2s retry), no upgrade involved. Prefer zero-downtime upgrades? Set `updateStrategy: RollingUpdate`
after validating it in your environment.

`terminationGracePeriodSeconds: 40` gives the draining leader time to finish (the internal drain is
~25s): on shutdown it logs `msg="shutdown: stopping intake; draining in-flight investigation"`,
stops accepting new work, and lets the in-flight investigation complete.

## What persists across upgrades, restarts, and failover

> [!IMPORTANT]
> **State is ephemeral by default**
>
> `persistence.enabled` defaults to **`false`**, which backs the data directory with an `emptyDir` —
> wiped on every pod restart, upgrade, and failover. For anything you want to survive, enable the PVC.

When `persistence.enabled: true`, the chart creates one PVC, `<release>-data`, mounted at
**`/var/lib/runlore/catalog`** (`catalog.mountPath`). Because both replicas (leader + standby) must
mount the same data, the PVC defaults to `accessModes: [ReadWriteMany]` — back it with an RWX class
(EFS on EKS, Filestore on GKE, etc.), default size `1Gi`.

Three things live on that volume; point their config keys inside the mount path:

| What | Config key | Notes |
|---|---|---|
| **Catalog git-sync mirror** | `catalog.dir` (= the mount path) | the local clone of your KB repo, re-synced on an interval |
| **Outcome ledger** | `outcome.ledger_path` (e.g. `/var/lib/runlore/catalog/outcomes.jsonl`) | append-only JSONL written by `serve`, **read by the curate CronJob** — both must share the volume |
| **Audit log** | `actions.audit_log_path` | hash-chained, required for both `actions.mode=approve` and `actions.mode=auto` (all executing rungs must be audited) |

The **bleve search index is *not* persisted** — it is built in memory (`NewMemOnly`) and rebuilt from
the catalog mirror at startup, so a restart simply re-indexes. Instant-recall quality is unaffected by
losing the index; it depends on the catalog content, which lives in your Git repo.

> [!NOTE]
> **If you run with `persistence.enabled: false`**
>
> The outcome ledger and audit log are lost on every restart, and the catalog re-clones from scratch
> on boot. That's fine for a quick trial, but for the **learning loop** to compound (outcome-weighted
> recall decay) the outcome ledger must persist — enable the PVC for any real deployment.

## Uninstalling

```bash
helm uninstall runlore -n runlore
```

This removes the Deployment, Service, ConfigMap, ServiceAccount, RBAC (ClusterRole/Roles + bindings),
and PodDisruptionBudget. It does **not** remove three things, which you must clean up by hand:

1. **The PVC `<release>-data`** (and its underlying PV / EFS / Filestore volume). Helm never deletes
   PVCs it templated, so your catalog mirror, outcome ledger, and audit log survive the uninstall:
   ```bash
   kubectl delete pvc <release>-data -n runlore
   # then delete the backing PV / cloud volume if your StorageClass doesn't reclaim it
   ```
2. **The credentials `Secret`.** The chart never creates it — it only references an existing Secret
   via `envFrom`/`env`. Delete whatever Secret you created for the GitHub App key, LLM API key, and
   notifier tokens:
   ```bash
   kubectl delete secret <your-runlore-secret> -n runlore
   ```
3. **The GitHub App.** It lives in GitHub, not the cluster — RunLore only holds its installation token
   at runtime. If you're done, delete (or uninstall) the App from your GitHub org/account settings so
   its installation and private key are revoked.

The knowledge-catalog **Git repo** is yours and is untouched by any of the above — that's the point:
your accumulated knowledge is portable and outlives the deployment.

## StatefulSet upgrades from chart ≤ 0.7.0: one-time recreate

Releases installed in `workloadKind: StatefulSet` mode by chart **0.7.0 or older**
stamped the full label set (including `helm.sh/chart` and `app.kubernetes.io/version`)
into the `volumeClaimTemplates` — an **immutable** StatefulSet field. Any upgrade that
bumps the chart or app version therefore fails server-side apply
(`Forbidden: updates to statefulset spec…`) and, under Flux, loops into rollback.

Fixed in the chart (the templates now stamp version-less selector labels), but an
existing StatefulSet keeps its old immutable template. **One-time migration** before
the next upgrade — pods and PVCs are untouched (`--cascade=orphan` deletes only the
controller object; the recreated StatefulSet adopts both):

```bash
kubectl delete statefulset <release-name> -n <namespace> --cascade=orphan
# then let Flux reconcile (or: helm upgrade …) — the StatefulSet is recreated
# with the stable labels and adopts the running pods + volumes.
```

Deployments are unaffected (no immutable template fields beyond the selector, which
was always version-less).

## Upgrading to 0.14.0 on the `standard` profile: new outbound traffic

From **0.14.0**, an install using `values-standard.yaml` mounts the
[knowledge commons]({{< relref "/docs/concepts/knowledge-commons.md" >}}) as a second, read-only
catalog root. In practice that means the agent clones
[`Smana/runlore-kb-commons`](https://github.com/Smana/runlore-kb-commons) at startup and re-syncs
it every 24h.

It is a second git fetch rather than a new capability — the profile already syncs your own KB repo
over the network — but it is a repo **you do not control**, tracking `main`. `minimal` and the
chart defaults are unaffected; `full` already enabled it.

**It cannot break the upgrade.** The commons syncer runs in the background, off the startup path:
if the clone fails, it logs a warning and the agent serves normally from your own catalog. Two
situations are worth recognising *before* you go looking for a bug, because both present the same
way — recurring sync warnings and no other symptom:

- **`networkPolicy.strict: true`** denies egress by default, so the clone is blocked unless the
  host is in `egress.allowedCIDRs`. Already covered if your own KB repo is on the same forge; a
  separate entry if it is not.
- **Restricted or air-gapped egress** behaves identically — warnings, no commons, nothing else
  affected.

One more thing worth knowing if the first clone fails: the retry waits a full `interval` (24h by
default), so a transient failure at boot means no commons until the next tick or a pod restart.

### If you would rather not

Three supported ways out, none of which degrade anything else:

- **Opt out** — delete the `catalog.commons` block from your values.
- **Review before it grounds anything** — replace `branch: main` with `ref:` (a tag or a full
  40-character commit id; the two are mutually exclusive and the agent refuses both).
- **Serve it yourself** — mirror the corpus into a repo you control and point `url` at that.

Note what this does *not* change: commons entries are read-only, are never curated into, and
**can never fire an instant recall** — they ground `kb_search` during a full investigation. See
[Knowledge commons]({{< relref "/docs/concepts/knowledge-commons.md" >}}).
