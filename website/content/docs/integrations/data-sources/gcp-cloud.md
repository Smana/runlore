---
title: GCP cloud control plane
weight: 311
integration: {kind: cloud, id: gcp}
---

> **Status: starts and authenticates on a live GKE cluster; the two lenses' responses are
> still unobserved.** A first run on GKE Standard 1.35.6 with self-managed Cilium
> ([#562](https://github.com/Smana/runlore/issues/562)) confirmed startup end to end —
> the metadata server resolves all three scope fields, a Workload Identity direct
> principal binding is accepted, and the preflight passes, registering both tools.
>
> What is still *not* observed is a real Cloud Logging or Container API payload. The unit
> tests run against `httptest` fixtures hand-written from the API reference, so the
> response shapes remain plausible rather than captured — see
> [Live validation](#live-validation) for which questions that leaves open. This is still
> a weaker claim than the AWS provider's.

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

That is the whole configuration on GKE: `wireCloudProvider` in `internal/app/investigate.go`
builds the provider and registers both cloud tools, and project, location and cluster name are all
resolved from the metadata server. Set
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
*other* field in that command takes the project id). You should not have to get this right from
the documentation: RunLore's startup preflight makes one `entries.list` call and, on a denial,
writes this exact command to stderr with the number already substituted. Get the number wrong and
the pod tells you, at startup, instead of a bare 403 arriving mid-investigation with nothing
naming the principal that was presented.

The command goes to **stderr**, not the structured log. Under the chart's default JSON logging a
multi-line value embedded in a log message arrives as one escaped string, backslash-continuations
and all, which is not pastable — and being pastable is the only reason to generate it.

**Roles required, in full — all read-only:**

- `roles/logging.viewer` — Cloud Audit Logs reads for `cloud_what_changed`
- `roles/container.clusterViewer` — GKE cluster and node-pool status for `cloud_resource_health`
- `roles/compute.viewer` — managed-instance-group errors and instance status, same tool

Notably **not** `roles/logging.privateLogViewer` — that role is for Data Access audit logs, which
this provider deliberately does not read (see Limitations below).

Only the first of the three is checked at startup: `Preflight` makes one `entries.list` call and,
on a 403/401, disables the cloud lens and writes a pastable `gcloud add-iam-policy-binding`
command to stderr with the project **number** already substituted. The other two roles degrade
per-sub-query at call time with a role-specific message in place, so a deployment granted one
binding and not the other still gets the half that answered.

## Verify it locally

Two lines at startup, in the same shape AWS uses:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'cloud provider enabled.*gcp'
kubectl -n runlore logs deploy/runlore | grep 'resolved cloud identity'
```

The second carries a `source` field naming which of the three identity-resolution tiers won:
`config`, `metadata-server`, or `node-provider-id`. That field is the point of the line — the
resolved triple alone cannot be checked, because autodetection landing on a same-named cluster in
a neighbouring region answers confidently about the wrong cluster and is indistinguishable from a
quiet one.

It also settles whether tier 3 is needed on your cluster. `source=metadata-server` means the
metadata server answered and the node fallback contributed nothing; `source=node-provider-id`
means the fallback earned its place. On GKE Standard 1.35.6 it reported `metadata-server`
([#562](https://github.com/Smana/runlore/issues/562)). **The reading still wanted is an
Autopilot one** — that is the mode most likely to expose a different metadata surface, and it
decides whether tier 3 and its cluster-wide `nodes` RBAC grant can be dropped. If you run this
on Autopilot, that one field is the most useful thing you can report back.

If autodetection fails outright — `source="unresolved"` and a `project is required` error — on a
Cilium cluster, read [Cilium clusters need one extra toggle](#cilium-clusters-need-one-extra-toggle)
before setting `cloud.gcp.*`. A blocked metadata server presents as a config problem, and
hardcoding the values hides it without fixing the token path.

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

ADC fetches its Workload Identity token from the GKE metadata server at
`169.254.169.254:80`, and on Cilium that fetch is dropped unless you allow it:

<!-- docsguard:ignore Helm chart values, not a runlore.yaml — networkPolicy is a chart key, outside .Values.config -->

```yaml
networkPolicy:
  gcpWorkloadIdentity: true   # Cilium only
```

**This toggle is not interchangeable with `awsPodIdentity`, and the reason is worth
knowing if you write your own policy.** The two once shared a single
`toEntities: [host]` rule, on the reasoning that both clouds' credential endpoints are
link-local addresses served by the node. That is false on GKE: Cilium does **not**
classify `169.254.169.254` as the `host` entity, so the shared rule matched nothing.
A/B tested on GKE Standard 1.35.6 with self-managed Cilium, from a pod carrying
RunLore's own labels ([#562](https://github.com/Smana/runlore/issues/562)):

| egress rule | `curl http://169.254.169.254/computeMetadata/v1/project/project-id` |
|---|---|
| `toEntities: [host]` | **timed out** |
| `toCIDR: [169.254.169.254/32]` | returned the project id |

So `gcpWorkloadIdentity` renders `toCIDR`, and `awsPodIdentity` keeps `toEntities:
[host]`, which is correct there — the EKS Pod Identity agent really does run on the node
host network.

Worth setting deliberately rather than discovering, because the failure does not present
as a network failure at any point. With no token, autodetection finds nothing and RunLore
reports:

```
gcp: could not resolve project, location or cluster … source="unresolved"
cloud provider unavailable; cloud tools disabled  err="gcp: project is required (autodetection found none; set cloud.gcp.project)"
```

That reads as a configuration problem. Follow it, hardcode all three values, and the
symptom goes away while ADC still cannot mint a token for the API calls that come later —
which then fail as a *credentials* error, which the preflight answers with an IAM binding
command, sending you to fix a binding that was already correct. Nothing anywhere in that
chain mentions egress. **If autodetection fails on a Cilium cluster, check this toggle
before you touch `cloud.gcp.*`.**

Non-Cilium CNIs match the link-local address with an ordinary `ipBlock`, which the default
`strict: false` already permits. In strict mode you must additionally allow 443 to
`logging.googleapis.com`, `container.googleapis.com` and `compute.googleapis.com` — the
toggle above covers only the token fetch, not the API calls it authenticates.

## Live validation

First run: GKE Standard 1.35.6-gke.1710000, zonal `europe-west4-a`, self-managed Cilium
([#562](https://github.com/Smana/runlore/issues/562)). Two of the six questions are
settled; the rest need an investigation that actually calls the lenses.

**1. Which tier resolves the scope — settled: `metadata-server`.** All three fields came
from tier 2, so the Kubernetes-node fallback contributed nothing on that cluster. It also
settles the sub-question this page flagged as unestablished: the GKE metadata server
*does* proxy `instance/attributes/cluster-location` to Pods.

That is one cluster, and one mode. Tier 3 stays for now rather than being deleted on it —
the bar stated here was "across every GKE version and mode", and **Autopilot in
particular is still untested**, which is exactly the mode most likely to differ in what
its metadata server exposes. If an Autopilot run also reports `source=metadata-server`,
tier 3 and the `nodes` RBAC rule it needs should go together.

**6. That the printed IAM binding works when pasted — settled for the positive half.**
Three roles bound to the direct principal using the project **number** produced a passing
preflight, so `cloud provider enabled` is itself evidence that a binding in that exact
shape is accepted. The negative path was not exercised: no role was unbound to make the
preflight *print* its command, so the generated text remains unverified.

**2, 3, 5 — still open.** They need real `cloud_what_changed` / `cloud_resource_health`
calls. On that run the tools registered (`tools: 13`, `incident_timeline cloud:true`) and
an investigation was accepted, but no evidence was emitted before teardown. Nothing was
observed either way about `protoPayload.status.code!=0` over an absent field, whether a
stockout surfaces through `listErrors`, or which `StatusCondition` field GKE populates.

**4. Whether Autopilot answers the node-layer lookups — not applicable.** That cluster was
Standard.

Until 2, 3 and 5 are answered, the fixtures stay hand-written from the API reference and
should be replaced with captured responses once a real payload is seen.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `cloud` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
