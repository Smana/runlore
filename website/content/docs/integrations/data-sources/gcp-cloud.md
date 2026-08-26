---
title: GCP cloud control plane
weight: 311
integration: {kind: cloud, id: gcp}
---

> **Status: not yet wired into the binary.** `internal/providers/cloud/gcp` exists on `main` as a
> client skeleton and its model-facing tool vocabulary — both unit-tested. The two lenses,
> `CloudChanges` and `ResourceHealth`, are stubs: they unconditionally return an "unimplemented
> lens" error (`TestTheUnimplementedLensesFailLoudlyRatherThanReportingCalm` pins exactly that).
> And `internal/app/investigate.go`'s cloud-provider switch only matches `cfg.Cloud.Provider ==
> "aws"` — there is no `gcp` case and no fallback warning. **Setting `cloud.provider: gcp` today is
> a silent no-op:** the YAML parses, but no tool registers, nothing is logged, nothing errors. This
> is a different claim from "implemented but not verified on a live cluster" (which is where the
> AWS provider and most of this project's integrations sit) — GCP genuinely does not run yet. The
> rest of this page describes the design it is being built to, so it's ready to use the moment the
> remaining tasks land; every forward-looking claim below is marked.

## What it gives you (by design)

`cloud_*` tools: Cloud Audit Logs (the `activity` and `system_event` streams — a "what changed
outside GitOps" lens covering GKE/Compute/IAM/network changes, manual actions, and
Google-initiated host events like preemption and live migration) plus GKE/MIG/Compute Engine
resource health. Read-only; auth is in-cluster identity — a GKE Workload Identity **direct
principal binding** — via Application Default Credentials. No service-account key, no Google
service account, no ServiceAccount annotation.

## Minimal config

```yaml
cloud:
  provider: gcp
```

This block parses today (`internal/config`'s decoder and validation both accept it — see
`TestCloudGCPBlockIsFullyOptional`), but **has no effect yet**: nothing in `investigate.go` reads
`cfg.Cloud.Provider == "gcp"`. Once wiring lands, nothing else will be needed on GKE: project,
location and cluster name are all resolved from the metadata server. Set
`cloud.gcp.{project,location,cluster_name}` only to override autodetection — off-cluster, or to
scope reads to a project other than the one GKE runs in:

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
granted the IAM roles itself, with no intermediate Google service account. This is true of the
design regardless of wiring status, and there's no harm in setting the binding up ahead of time.

**Do not set `serviceAccount.annotations` to `iam.gke.io/gcp-service-account`.** That annotation
is the setup step for the *other*, GSA-impersonation flavor of Workload Identity — almost every
GKE guide you'll find teaches that one. Setting it here anyway will, once the provider is wired,
silently redirect every GCP call RunLore makes to that GSA instead of the roles bound to the KSA
below. If the GSA holds none of those roles — the common case, since there usually is no GSA at
all in a direct-binding setup — the result is a bare 403 that looks exactly like a missing
binding, except the fix RunLore is designed to print assumes direct binding and would not resolve
it. Leave `serviceAccount.annotations` empty.

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
*other* field in that command takes the project id). By design, RunLore will generate this exact
command from a denied preflight read rather than only documenting it — **that preflight does not
exist yet**: `New()` in `internal/providers/cloud/gcp/gcp.go` makes no calls at construction time
today. Get the number wrong before wiring lands and you won't hear about it until the lenses ship
and you hit a bare 403 with nothing naming the principal that was presented.

**Roles required, in full — all read-only:**

- `roles/logging.viewer` — Cloud Audit Logs reads for `cloud_what_changed`
- `roles/container.clusterViewer` — GKE cluster and node-pool status for `cloud_resource_health`
- `roles/compute.viewer` — managed-instance-group errors and instance status, same tool

Notably **not** `roles/logging.privateLogViewer` — that role is for Data Access audit logs, which
this provider deliberately does not read (see Limitations below). This role list is a design
decision, not something any code currently checks or enforces.

## Verify it locally

**Not yet possible.** There is nothing to grep for today. `cloud.provider: gcp` registers no
tools and `investigate.go` never reaches a branch that would log for it, so the natural instinct —
tail the pod's logs after setting the config — finds nothing, and there's no error to explain why.
Don't chase this until the wiring task lands.

Once it does, expect the same shape AWS uses today:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'cloud provider enabled.*gcp'
```

Per the design, that startup line will also carry an `identity_source` field naming which of the
three identity-resolution tiers (explicit config, the GKE metadata server, the node's
`providerID`) won — but neither the log field nor the resolver that would produce it
(`internal/providers/cloud/gcp/identity.go`) exists on this branch yet. Treat both commands above
as a preview of the intended interface, not something to run today.

## Autopilot (design)

`ResourceHealth` is currently a two-line stub that unconditionally returns an unimplemented-lens
error — there are no sub-queries yet, so there's nothing to skip. Per the design, once
implemented: on an Autopilot cluster, the node-layer sub-queries (managed-instance-group errors,
instance status) will be skipped entirely, because the node layer is Google-managed and there is
no MIG to query. The tool is designed to say so explicitly, rather than running those sub-queries
anyway and returning an empty result a model could read as "capacity is fine."

## Limitations (by design, once wired)

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
