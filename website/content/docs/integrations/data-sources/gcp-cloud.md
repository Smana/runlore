---
title: GCP cloud control plane
weight: 311
integration: {kind: cloud, id: gcp}
---

**What it gives you** — `cloud_*` tools: Cloud Audit Logs (the `activity` and `system_event`
streams — a "what changed outside GitOps" lens covering GKE/Compute/IAM/network changes, manual
actions, and Google-initiated host events like preemption and live migration) plus GKE/MIG/Compute
Engine resource health. Read-only; auth is in-cluster identity — a GKE Workload Identity **direct
principal binding** — via Application Default Credentials. No service-account key, no Google
service account, no ServiceAccount annotation.

> **Not yet verified on a live GKE cluster.** The provider is implemented and unit-tested against
> recorded API fixtures, matching the "functional but less exercised" posture of
> [Project status](https://github.com/Smana/runlore#project-status--stability). The live-validation
> runbook (create a Workload Identity cluster, bind the roles below, confirm a real stockout
> surfaces) is the remaining step before this note comes off.

## Minimal config

```yaml
cloud:
  provider: gcp
```

Nothing else is needed on GKE: project, location and cluster name are all resolved from the
metadata server. Set `cloud.gcp.{project,location,cluster_name}` only to override
autodetection — off-cluster, or to scope reads to a project other than the one GKE runs in:

```yaml
cloud:
  provider: gcp
  gcp:
    project: my-gcp-project
    location: europe-west1
    cluster_name: my-gke-cluster
```

## IAM — bind the ServiceAccount directly. Do NOT annotate it

GCP auth is a Workload Identity **direct principal binding**: the Kubernetes ServiceAccount is
granted the IAM roles itself, with no intermediate Google service account.

**Do not set `serviceAccount.annotations` to `iam.gke.io/gcp-service-account`.** That annotation
is the setup step for the *other*, GSA-impersonation flavor of Workload Identity — almost every
GKE guide you'll find teaches that one. Setting it here anyway silently redirects every GCP call
RunLore makes to that GSA instead of the roles bound to the KSA below. If the GSA holds none of
those roles — the common case, since there usually is no GSA at all in a direct-binding setup —
the result is a bare 403 that looks exactly like a missing binding, except the fix RunLore prints
assumes direct binding and does **not** resolve it. Leave `serviceAccount.annotations` empty.

Bind the roles:

```bash
PN=$(gcloud projects describe MY_PROJECT --format='value(projectNumber)')
for ROLE in roles/logging.viewer roles/container.clusterViewer roles/compute.viewer; do
  gcloud projects add-iam-policy-binding MY_PROJECT --role="$ROLE" \
    --member="principal://iam.googleapis.com/projects/$PN/locations/global/workloadIdentityPools/MY_PROJECT.svc.id.goog/subject/ns/runlore/sa/runlore"
done
```

**Use the project NUMBER, not the id, in the `principal://` string** — the `gcloud projects
describe --format='value(projectNumber)'` call above resolves it. This is a classic footgun (every
*other* field in that command takes the project id) and the reason RunLore generates this exact
command from a denied preflight read rather than only documenting it: get the number wrong and the
binding silently matches nothing, so the next attempt fails exactly the same way.

**Roles required, in full — all read-only:**

- `roles/logging.viewer` — Cloud Audit Logs reads for `cloud_what_changed`
- `roles/container.clusterViewer` — GKE cluster and node-pool status for `cloud_resource_health`
- `roles/compute.viewer` — managed-instance-group errors and instance status, same tool

Notably **not** `roles/logging.privateLogViewer` — that role is for Data Access audit logs, which
this provider deliberately does not read (see Limitations below).

## Verify it locally

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'cloud provider enabled.*gcp'
```

The startup line also reports `identity_source` — which of the three tiers (explicit config, the
GKE metadata server, the node's `providerID`) resolved the project/location/cluster identity. Worth
checking on a project with more than one GKE cluster in the same region: autodetection landing on
the wrong cluster is silent, and every subsequent answer would then be confidently about a
neighbor's cluster.

## Autopilot

On an Autopilot cluster, `cloud_resource_health`'s node-layer sub-queries (managed-instance-group
errors, instance status) are skipped entirely — the node layer is Google-managed and there is no
MIG to query. The tool says so explicitly, rather than running those sub-queries anyway and
returning an empty result a model could read as "capacity is fine."

## Limitations

- **Single project.** No folder-level, org-level or multi-project audit reads.
- **Data Access audit logs are not read.** Only the `activity` and `system_event` streams — both
  always-on, free, and mutating by definition. Data Access logs are off by default outside
  BigQuery, dominated by reads, and would need `roles/logging.privateLogViewer`, which this
  provider does not request.
- **Not a Cloud Monitoring integration.** Google Managed Prometheus is Prometheus-compatible —
  point the existing metrics provider (`metrics.url`) at the GMP frontend instead of expecting this
  provider to surface metrics.
- Complements, doesn't replace, [Source repos]({{< relref "source-repos.md" >}}) and GitOps
  `what_changed`: this is the layer for changes that happened **outside** your GitOps pipeline
  entirely.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `cloud` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
