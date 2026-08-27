---
title: GCP cloud control plane
weight: 311
integration: {kind: cloud, id: gcp}
---

> **Status: implemented, not yet verified on a live GKE cluster.** Both lenses,
> identity resolution and the Workload Identity preflight are implemented and unit-tested
> against `httptest` fixtures hand-written from the API reference. `cloud.provider: gcp`
> registers the cloud tools and logs the resolved project, location, cluster and the tier
> autodetection resolved them from.
>
> What has *not* happened is a run against a real cluster. The fixtures were written from
> documentation rather than captured from live responses, so the shapes are plausible
> rather than observed — see [Live validation](#live-validation) for what that leaves open.
> This is the same "functional but less exercised" posture the project uses elsewhere, and
> it is a weaker claim than the AWS provider's.

## What it gives you

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
(`internal/providers/cloud/gcp/identity.go`) resolves it. Treat both commands above
as the interface it is today.

## Autopilot

On an Autopilot cluster the node-layer sub-queries **still run**, and an empty answer is
caveated rather than reported as healthy:

```
gke node groups: no instance-group errors or instance churn reported — this is an Autopilot
cluster, so the node layer is Google-managed and this may reflect limited visibility rather
than an absence of problems
```

The design originally said to skip them on Autopilot, on the reasoning that the node layer
is Google's. That was overridden during implementation, because it is wrong in the way that
matters: Autopilot node VMs and their instance groups live in **your** project and read with
the same `roles/compute.viewer`, and a zonal stockout strands Autopilot Pods as Pending
exactly as it strands Standard ones. Skipping discarded the highest-value line this provider
emits, precisely where you have the least visibility of your own node layer.

What the original reasoning got right is kept: an empty node-layer answer must not read as
"capacity is fine". So the answer is hedged rather than not sought — and a real stockout is
reported plainly, with no hedge at all.

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

## Cilium clusters need one extra toggle

ADC fetches its Workload Identity token from the GKE metadata server on the node host
network (`169.254.169.254:80`). Cilium classifies that as the `host` entity, which a
Kubernetes NetworkPolicy `ipBlock` cannot match — so on Cilium the token fetch is
silently dropped:

<!-- docsguard:ignore Helm chart values, not a runlore.yaml — networkPolicy is a chart key, outside .Values.config -->

```yaml
networkPolicy:
  gcpWorkloadIdentity: true   # Cilium only
```

Worth setting deliberately rather than discovering, because the failure does not look
like a network failure. With no token the Google client reports a *credentials* error,
and RunLore's preflight answers a credentials error with an IAM binding command — so you
add a binding that was already correct, the symptom does not move, and nothing in the
chain mentions egress.

Other CNIs match the link-local address with an ordinary `ipBlock`, which the default
`strict: false` already permits. In strict mode you must additionally allow 443 to
`logging.googleapis.com`, `container.googleapis.com` and `compute.googleapis.com` — the
toggle above covers only the token fetch, not the API calls it authenticates.

## Live validation

Not yet run. The unit tests use `httptest` fixtures hand-written from the API reference,
so every shape below is plausible rather than observed — this section records what a run
against a real cluster is expected to settle.

1. **Which tier resolves the scope.** The startup line reports `identity_source`. Seeing
   `metadata-server` there is proof the Kubernetes-node fallback contributed nothing — and
   that fallback is provisional precisely because it is not established that the GKE
   metadata server exposes `instance/attributes/cluster-location` to Pods across every GKE
   version and mode. If tier 2 proves reliable, the node tier is deleted rather than kept
   as dead weight.
2. **Whether `protoPayload.status.code!=0` matches an absent field.** A successful audit
   entry omits `status` entirely. If Cloud Logging's `!=` over-matches, `failed_only`
   becomes correct-but-slow — it pages through successes — rather than wrong, because a
   local re-check drops any entry that arrives with a zero status.
3. **Whether a stockout actually surfaces.** Request a rare machine type in a node pool and
   confirm `listErrors` reports it. This is the highest-value line the provider emits.
4. **Whether Autopilot answers the node-layer lookups at all.** They are attempted rather
   than skipped, and an empty answer is caveated — the live run says which of those two
   paths a real Autopilot cluster takes.
5. **Which `StatusCondition` field GKE populates** on a degraded pool: `canonicalCode`,
   the deprecated `code`, or both. All three are handled; only a live response says which
   is observed.
6. **That the printed IAM binding works when pasted.** Unbind a role, trigger the preflight,
   and paste the command it prints. The project *number* in the `principal://` string is the
   part most often wrong, and gcloud accepts the id without ever matching.

Once run, the fixtures should be replaced with captured responses.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `cloud` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
