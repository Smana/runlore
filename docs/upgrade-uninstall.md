# Upgrade & Uninstall

How to upgrade RunLore in place, what survives a restart, and how to remove it cleanly.

## Upgrading

RunLore is a Helm release — upgrade like any other chart:

```bash
helm upgrade runlore deploy/helm/runlore -n runlore -f values.yaml
```

Your `values.yaml` is the source of truth; the entire agent config under `values.config` is rendered
verbatim into the ConfigMap. Re-apply the same file (with your changes) on every upgrade.

> [!warning] Expect ~20s of downtime during the agent's own upgrade
> The Deployment uses **`strategy.type: Recreate`**, not a rolling update. Old pods are terminated
> **before** new ones start, so the agent is briefly unavailable while the new version boots and wins
> the leader lease.

### Why `Recreate` (not a rolling update)

It's a deliberate interaction with leader election. Under a rolling update with `replicaCount: 2`:

1. A new pod can't become `Ready` until it becomes the **leader** (`/readyz` is gated on leadership).
2. The old leader still holds the `Lease`, so the new pod can't lead yet.
3. With `maxUnavailable: 0` the old pod won't terminate until the new one is `Ready` — **deadlock**.

`Recreate` sidesteps this by terminating the old leader first, releasing the lease so the new pod can
acquire it. The cost is a short gap **only during the upgrade itself**. Crash-failover HA is
unaffected: if the leader dies unexpectedly, the hot standby takes over within the lease window
(15s lease / 10s renew / 2s retry), no upgrade involved.

`terminationGracePeriodSeconds: 40` gives the draining leader time to finish (the internal drain is
~25s): on shutdown it logs `msg="shutdown: stopping intake; draining in-flight investigation"`,
stops accepting new work, and lets the in-flight investigation complete.

## What persists across upgrades, restarts, and failover

> [!important] State is **ephemeral by default**
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
| **Audit log** | `actions.audit_log_path` | hash-chained, only required when `actions.mode=auto` |

The **bleve search index is *not* persisted** — it is built in memory (`NewMemOnly`) and rebuilt from
the catalog mirror at startup, so a restart simply re-indexes. Instant-recall quality is unaffected by
losing the index; it depends on the catalog content, which lives in your Git repo.

> [!note] If you run with `persistence.enabled: false`
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
