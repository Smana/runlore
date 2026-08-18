---
title: Upgrade & Uninstall
weight: 30
---

How to upgrade RunLore in place, what survives a restart, and how to remove it cleanly.

## Breaking changes in 0.15.0

RunLore is **pre-1.0**, and a breaking change bumps the **minor** version — so a `0.x` upgrade can
still require migration. Read this section before upgrading onto `0.15.0`.

### `investigation.max_tokens_per_investigation` is now a whole-run budget, and its default is raised

The key used to bound the estimated size of the **next request**. It is now a **cumulative** ceiling
on one investigation's model tokens (provider-reported input + output, loop **and** verify), so
twenty steps of 99k each — which previously passed cleanly against a `100000` "budget" — now stop
after the fourth. Read as a run budget that old value funds only four or five steps, so **the default
is raised from `100000` to `400000`** to suit the new meaning. It is *not* unchanged.

A quarter of it (**100 000** at the default) additionally bounds the estimated size of any single
request, and mid-loop compaction triggers at 70 % of that quarter (**70 000**). Those are exactly the
values in force before the ceiling became cumulative, so what one request may cost changes for nobody.

**What you have to do**

- **If you never set the key** — nothing. The raised default is applied for you, and investigations
  that a `100000` run budget would have cut short with `result="budget_exceeded"` now run to
  completion.
- **If you set it explicitly** — your value is **left untouched** and is now read as a whole-run
  budget: a config pinned to `100000` still says `100000`, and now funds roughly four steps rather
  than twenty. Set it to the total you are willing to pay per incident (summed across all turns, not
  the size of one), or `-1` for the previous unbounded behaviour.
- **`0` does not disable it.** `0` applies the bounded default and `-1` is the opt-out, so a config
  written to remove the cap with `0` gets the default instead — the opposite of what it asked for.

Budget for the ceiling **plus one request**: the check compares the *projected* total — already spent
plus the request about to be sent — so a run stops on the first request that would cross, but that
request is still sent: the nudge exists to give the model one turn to conclude. That conceded
request is itself capped at the quarter, so budget **≈1.25×** the number you set. Measured at the
shipped defaults: **≈467 000 tokens delivered against a 400 000 ceiling** (≈1.17×).

Runs stopped by the running total report `reason="tokens_total"`; runs stopped by the per-request
bound report `reason="tokens_request"`, and are fixed by lowering `max_tool_output_bytes` or enabling
`compaction`, not by raising the run budget. Watch
`runlore_investigation_budget_trips_total{reason,stage}` after upgrading. Full reference:
[Configuration → `investigation`]({{< relref "/docs/configuration/configuration.md#investigation--loop-bounds--noise-control" >}}).

### GitHub Enterprise with subdomain isolation must set `forge.git_host`

A GitHub Enterprise install whose `forge.github_api_url` is an **`api.` subdomain** (subdomain
isolation: the API on `api.HOSTNAME`, git and web on `HOSTNAME`) must now set **`forge.git_host`** to
the bare hostname serving its git remotes. `serve` **refuses to start** until it does, rather than
guessing and silently withholding the forge credential from every GitOps repository.

```yaml
forge:
  github_api_url: https://api.ghe.example.com
  git_host: ghe.example.com   # the host your git remotes use — no scheme, path or port
```

The credential is confined to that host because a GitOps `spec.source.repoURL` is cluster state:
whoever can create an Argo CD `Application` or a Flux `GitRepository` chooses where an unconfined
token would be sent. Guessing wrong fails either loudly (the token goes to a host it is not valid
for) or **silently** — withholding it from your own GitOps repo empties `what_changed` on every
investigation, surfacing only as a data-gaps line at the foot of a finding. Loud once at startup
beats quiet forever.

**Every other configuration is untouched and needs no new key**: `github.com`, GitHub Enterprise
serving its API at `HOSTNAME/api/v3`, and GitLab (whose `base_url` is already the instance root that
git, web and API share).

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
