# Design — GCP CloudProvider (Cloud Audit Logs · GKE/MIG health · Workload Identity)

- **Date:** 2026-08-24
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Implements:** the `cloud/{aws,gcp,azure}/` slot reserved in `website/content/docs/concepts/design.md:485`
- **Follow-up spec (not this one):** Cloud Logging as a `LogProvider`

## Problem

`providers.CloudProvider` has exactly one implementation — `internal/providers/cloud/aws/`. A team
running GKE gets the cluster, GitOps, metrics, logs and network lenses, and then hits a wall at the
cloud control plane: no `cloud_what_changed`, no `cloud_resource_health`. The two questions the
cloud lens exists to answer — *who changed this outside Git?* and *is the node layer actually
healthy?* — are unanswerable on GCP today.

RunLore is already partly GCP-aware, which sharpens the gap rather than closing it:

- `internal/network/gcpfirewall/` reads Cloud Logging over ADC and is proven in production shape.
- `internal/model/gemini/` is a first-class model provider.
- `google.golang.org/api` is already a direct dependency (`go.mod:23`).

So the missing piece is not GCP literacy. It is the cloud lens, plus the identity story that makes
it work on GKE without configuration.

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Scope | `CloudProvider` + Workload Identity **only** | Cloud Logging as a `LogProvider` is a second subsystem; it gets its own spec |
| Data sources | **Cloud Logging for changes, native APIs for state** | Event-derived health tells you an upgrade *started*; it cannot tell you a pool is `DEGRADED` **now**, which is what the health lens is for |
| Audit log streams | `activity` + `system_event`; **not** `data_access` | Both chosen streams are always-on, free, and mutating by definition. Data Access is off by default, dominated by reads, and would need `logging.privateLogViewer` |
| Workload Identity flavor | **Direct principal binding only** | No GSA to create or rotate, no KSA annotation, no impersonation code. Modern GA path |
| Consequence of the above | RunLore always runs **on** GKE for this provider | Which is what makes metadata-server autodetect a legitimate default rather than a guess |
| GKE mode | **Standard first, degrade explicitly on Autopilot** | Stockout and quota errors live in MIG `listErrors`; dropping them would remove the highest-value output |
| `compute/v1` dependency | **Use it** — measured, not assumed | 15M of generated source links to **+88 KB**. See [Binary size](#binary-size-measured) |
| Tool descriptions | New optional `CloudDescriber` capability | Shipping GCP behind CloudTrail-worded tool text would mislead the model. Absent describer = today's AWS text verbatim, so AWS does not churn |
| Config shape | Nested `cloud.gcp` block | Matches the `Network` precedent in the same file. AWS's flat fields are untouched |
| Verification | Fakes now; **live GKE run before it is called verified** | Owner will have a cluster. The runbook is written so that run is executable, not improvised |

## Non-goals

Stated so the spec cannot quietly grow:

- **Cloud Logging as a `LogProvider`.** Separate spec. It is the bigger GKE win — a GKE cluster with
  no Loki has no log source at all — but it is an independent subsystem.
- **GSA impersonation, the `iam.gke.io/gcp-service-account` annotation, external WIF (RunLore off
  GCP), cross-project impersonation.** Direct principal binding only.
- **Data Access audit logs.**
- **Cloud Monitoring.** Google Managed Prometheus is Prometheus-compatible: point the existing
  metrics provider at the GMP frontend. Document it; do not build it.
- **Folder-level, org-level or multi-project audit reads.** Single project.
- **Azure.**

## Architecture

```
internal/providers/cloud/gcp/
  gcp.go             Client, New(), ADC, preflight, narrow API interfaces, ptr/deref
  identity.go        project / cluster / location resolution (3 tiers)
  auditlog.go        CloudChanges  — entries.list over activity + system_event
  resourcehealth.go  ResourceHealth — GKE cluster, MIG listErrors, instance status
  *_test.go
```

File-for-file this mirrors `cloud/aws/` (`aws.go` / `cloudtrail.go` / `resourcehealth.go`), with
`identity.go` added because GCP has an autodetect story AWS does not.

### Changes to `internal/providers/providers.go`

Three small ones. No new `Workload` fields, no identity-key change.

1. `EngineGCP Engine = "gcp"` alongside `EngineAWS` (`providers.go:36`).
2. `ChangeCloudAPI`'s comment says `(CloudTrail)` (`providers.go:48`) — widen to name both engines.
3. `Workload.Region` / `.Account` are documented as AWS-only (`providers.go:99-100`). GCP location
   and project id map onto them exactly. **The fields stay; only the doc comments widen.** `Ref()`,
   the recall index and the dedup fingerprint are untouched, so a GCP resource keys exactly as an
   AWS one does.

   **One place this is not automatic: the notification card.** `notKubernetesShaped`
   (`internal/notify/resource_scope.go:145`) recognises a cloud resource by testing
   `strings.Contains(kind, ":")` — true for `AWS::EC2::Instance`, false for `gke_nodepool`
   and `gce_instance`. A GCP resource therefore falls through to `Workload.Ref()`, which
   returns `""` when `Namespace` is empty, so the resource identity is dropped from the
   card entirely where the AWS equivalent renders its name. The kind test has to learn
   the GCP shape, or GCP kinds have to join the existing non-Kubernetes kind list.

### New optional capability: `CloudDescriber`

`internal/investigate/cloud_tools.go` hardcodes AWS vocabulary into the model-facing tool text —
`cloud_what_changed` announces "MUTATING **AWS** control-plane events (**CloudTrail**)", and
`instance_id` is described as "optional EC2 instance id (i-…)". Wiring GCP behind that text tells
the model it is querying CloudTrail while it queries Cloud Audit Logs.

That matters more here than it would elsewhere: `cloud_tools.go:60-80` documents a real incident
where an imprecise mental model of the *query semantics* dead-ended an investigation. Shipping
knowingly-wrong nouns would undo that work.

```go
// CloudDescriber is an OPTIONAL CloudProvider extension naming the cloud's own
// vocabulary, so the cloud tools describe the API the model actually queries.
// Absent, the tools keep their AWS wording — which is correct for AWS, and is why
// wiring GCP does not churn the AWS text.
type CloudDescriber interface{ CloudVocabulary() CloudVocabulary }

type CloudVocabulary struct {
	Cloud          string // "AWS" | "GCP"
	ChangeLog      string // "CloudTrail" | "Cloud Audit Logs"
	ChangeExamples string // the kinds of change this log carries
	ScopeGuidance  string // how the `resource` arg matches, and when to omit it
	WidenedBanner  string // format string with ONE %q, shown when a scoped lookup is widened
	LagNote        string // ingestion lag, e.g. "CloudTrail lags ~15m"
	HealthSurface  string // "EKS nodegroup, ASG activities, EC2 status"
	InstanceArg    string // "EC2 instance id (i-…)" | "Compute Engine instance name"
}
```

The fields are fragments rather than two finished strings so the rendering skeleton stays shared, and the two clouds cannot drift into describing structurally different contracts for the same tool. `WidenedBanner` is the field that most needs to vary: the AWS banner makes a claim about *match semantics* that is false on GCP (see [the exact-match trap](#the-exact-match-trap-does-not-reproduce--and-the-banner-must-say-so) below).

Same shape as every other optional capability in this codebase — `OwnerWalker`, `EventWindower`,
`GitOpsEngineReporter`, `ProgressNotifier`, `ThreadNotifier`: type-asserted, gracefully absent. Only
the GCP client implements it in this spec.

### Configuration

Follows the `Network` precedent, which already nests per-provider blocks in this same file.

```yaml
cloud:
  provider: gcp        # "" (disabled) | aws | gcp
  gcp:                 # every field optional — autodetected on GKE
    project: ""
    location: ""       # cluster region or zone
    cluster_name: ""
```

AWS's existing flat `cloud.region` / `cloud.cluster_name` are **untouched** — no breaking change.
Validation warns when `cloud.region` is set while `provider: gcp`, because it is otherwise silently
ignored. Mirroring `cloud.aws.*` for symmetry is deliberately **not** in scope.

`internal/app/investigate.go:283` becomes a switch on `cfg.Cloud.Provider` rather than
`if == "aws"`.

### Identity resolution — `identity.go`

Three tiers, first hit wins. The resolved triple **and the tier that produced it** are logged once
at startup; the tier is what makes tier 3's fate decidable on a real cluster.

1. **Explicit config** — `cloud.gcp.{project,location,cluster_name}`. The escape hatch.
2. **GKE metadata server** — `project/project-id`, `instance/attributes/cluster-name`,
   `instance/attributes/cluster-location`, via `cloud.google.com/go/compute/metadata` (already
   indirect at `go.mod:36`; promoted to direct).
3. **Kubernetes node object** — `.spec.providerID` is `gce://PROJECT/ZONE/INSTANCE`;
   `topology.kubernetes.io/region` carries the region. Read through the existing `KubeReader`.

**Tier 3 is provisional and must be written so it deletes cleanly.** It exists because it is not
established that the GKE metadata server proxies `instance/attributes/cluster-location` to Pods
across every GKE version and mode, and the zero-configuration promise should not rest on an untested
assumption. Step 3 of the live runbook settles it. If tier 2 proves reliable, **tier 3 is removed**
rather than kept as dead weight.

### Workload Identity

With direct principal binding, ADC resolves credentials with **no RunLore code** — and the Helm
chart renders **nothing**, because there is no annotation. What is actually missing today is
diagnosis: a wrong binding surfaces as a bare 403 in the middle of an investigation.

So `New()` runs one cheap preflight — `entries.list`, one entry, a one-minute window — and on denial
emits a fully-substituted command. `POD_NAMESPACE` already exists in the pod template;
`POD_SERVICE_ACCOUNT` is added via the downward API (`fieldRef: spec.serviceAccountName`, no RBAC
needed); the project **number** comes from `project/numeric-project-id`.

```
gcp cloud provider unavailable: Cloud Logging read denied on project my-proj.
RunLore authenticated as ServiceAccount runlore/runlore, which no GCP role is bound to.

  gcloud projects add-iam-policy-binding my-proj \
    --role=roles/logging.viewer \
    --member="principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-proj.svc.id.goog/subject/ns/runlore/sa/runlore"
```

The project **number** rather than the id in the `principal://` string is a classic footgun, and the
reason this is generated rather than documented.

Preflight failure is **non-fatal**: warn and disable the cloud tools, exactly as
`investigate.go:285` does for AWS. It probes the logging API only; the health APIs degrade
per-sub-query at call time.

**Roles required, in full:** `roles/logging.viewer`, `roles/container.clusterViewer`,
`roles/compute.viewer`. All read-only. Notably **not** `roles/logging.privateLogViewer`, which
Data Access logs would have required.

## Lens 1 — `CloudChanges` (`auditlog.go`)

One `entries.list` call, `resourceNames: ["projects/P"]`, `orderBy: "timestamp desc"`:

```
logName=("projects/P/logs/cloudaudit.googleapis.com%2Factivity" OR
         "projects/P/logs/cloudaudit.googleapis.com%2Fsystem_event")
timestamp>="…" timestamp<="…"
[protoPayload.resourceName:"<sel.Name>"]     # only when scoped; ':' is substring
```

Both streams are `AuditLog` protos, so one decoder serves both.

| `providers.Change` | GCP source | AWS analog |
|---|---|---|
| `When` | `timestamp` | `EventTime` |
| `Engine` | `EngineGCP` | `EngineAWS` |
| `Type` | `ChangeCloudAPI` | same |
| `ManagedBy` | `protoPayload.serviceName` | `EventSource` |
| `ToRev` | `insertId` | `EventId` |
| `Workload.Kind` | `resource.type` (`gke_nodepool`, `gce_instance`) | `Resources[0].ResourceType` |
| `Workload.Name` | `protoPayload.resourceName` | `Resources[0].ResourceName` |
| `Workload.Account` | project id | account id |
| `Workload.Region` | `resource.labels.location` / `zone` | region |
| `Source.Path` | `methodName by principalEmail` | same shape |

**Failed calls are the payoff**, mirroring the `errorCode` handling in `eventToChange`:
`protoPayload.status.code != 0` renders `— FAILED: RESOURCE_EXHAUSTED (…)`, mapping `google.rpc.Code`
numbers to names. `PERMISSION_DENIED` and `RESOURCE_EXHAUSTED` are precisely the two an
investigation wants surfaced.

**`system_event` has no AWS-provider equivalent at all** — it carries the Google-initiated actions:
host error, live migration, preemption.

Cap, sort and the `(truncated)` sentinel behave identically to `cloudtrail.go`: over-collect by one,
sort newest-first, cap, append the sentinel **last** so a zero `When` cannot sort it among real
events.

### The exact-match trap does not reproduce — and the banner must say so

`cloud_tools.go:60-80` documents a real dead-end: CloudTrail's `ResourceName` is an exact match, a
scoped miss is indistinguishable from "nothing changed", and an investigation reported a Secrets
Manager deletion as uncapturable with the answer one unscoped lookup away. Hence the widening
fallback.

GCP's filter language has `:` (substring) alongside `=` (exact), so `protoPayload.resourceName:"foo"`
matches the way the model expects. **The fallback is kept for parity and safety, but its banner is
rewritten, not copied.** On GCP a scoped miss means the resource genuinely did not appear; telling
the model it probably used the wrong match semantics would be false.

## Lens 2 — `ResourceHealth` (`resourcehealth.go`)

Three best-effort sub-queries. Each contributes an error line rather than failing the call, per the
contract at `resourcehealth.go:25`.

1. **`container.projects.locations.clusters.get`** → cluster `status`, `statusMessage`,
   `conditions[]`; then per node pool: status, conditions, `autoscaling` bounds, and node version vs
   control-plane version (upgrade skew). Each pool carries `instanceGroupUrls`, so sub-query 2 is
   **handed** its MIG names — where AWS must tag-match (`asgInCluster`).
2. **`compute.instanceGroupManagers.listErrors`** on those MIGs → `ZONE_RESOURCE_POOL_EXHAUSTED`,
   `QUOTA_EXCEEDED`, `IP_SPACE_EXHAUSTED`. The `DescribeScalingActivities` analog, and the
   highest-value line this provider emits. Window-scoped by error timestamp, mirroring
   `activityBeforeWindow`. `listManagedInstances` supplies `currentAction` and `instanceHealth` for
   churn detection, capped at the same `defaultMaxEvents` (25) the AWS client uses (`aws.go:52`) so
   a thrashing pool cannot flood the lens.
3. **`compute.instances.get`** when the selector names an instance → `status`
   (`RUNNING`/`TERMINATED`/`REPAIRING`), `statusMessage`, `lastStartTimestamp`.

### Autopilot

Detected from `cluster.autopilot.enabled` in sub-query 1. When true, **sub-queries 2 and 3 are
skipped entirely** and a note tells the model the node layer is Google-managed — rather than running
them and returning empty results the model would reasonably read as "capacity is fine".

## Error handling

Three tiers, each matching an existing contract:

| Tier | Behavior | Precedent |
|---|---|---|
| Startup | Preflight denial → warn, disable cloud tools, print the binding command | `investigate.go:285` |
| Per-call | `CloudChanges` propagates its error to the tool | AWS |
| Per-sub-query | `ResourceHealth` contributes an error line, never a hard failure | `resourcehealth.go:25` |

**One addition beyond the AWS shape: 403 and 404 get different messages.** A 403 on `clusters.get`
is a missing `container.clusterViewer` binding. A 404 is a wrong cluster name or location — an
*autodetect* bug. Collapsing them sends the operator debugging IAM for a metadata problem.

## Binary size (measured)

`google.golang.org/api/compute/v1` is 15M of generated source, which is a legitimate concern for a
project whose pitch is a single Go binary. Measured rather than assumed, `-trimpath -ldflags="-s -w"`:

| Binary | Size | Delta |
|---|---|---|
| `logging/v2` only | 14,512,306 | baseline |
| `+ container/v1` | 14,513,058 | **+752 B** |
| `+ compute/v1` | 14,602,786 | **+88 KB** |

The generated clients are per-method and the linker drops everything uncalled. The concern is
unfounded: use `compute/v1` idiomatically. No hand-rolled REST.

## Testing

Follows `gcpfirewall_test.go` exactly: real generated clients against `httptest`, via
`option.WithHTTPClient` + `WithEndpoint` + `WithoutAuthentication`. Compile-time assertions
`var _ providers.CloudProvider = (*Client)(nil)` and `var _ providers.CloudDescriber = (*Client)(nil)`,
as at `aws.go:75`.

Table tests must cover:

- **auditlog:** full field mapping; `status.code` → `RESOURCE_EXHAUSTED` rendering; `system_event`
  entries; cap plus sentinel **ordering**; scoped hit; scoped miss → widened, with the GCP banner;
  empty window.
- **resourcehealth:** Standard cluster with a degraded pool; version skew; **Autopilot skipping
  sub-queries 2–3 and emitting the note**; MIG stockout; a sub-query failure degrading to a line, not
  a hard failure.
- **identity:** config beats metadata; metadata beats node; all three absent → a clear error naming
  what to set.
- **config:** the new `cloud.gcp` block; the `cloud.region`-with-`provider: gcp` warning.

Fixtures start hand-written from the API reference and are **replaced with real captures** after the
live run — the concrete link between shipping now and verifying later.

## Live-validation runbook

Executable, not improvised. Until step 7 lands, the provider is documented as functional but not
verified on a real cluster, matching the honesty posture of `README.md`'s project-status table.

1. Standard cluster, one node, `--workload-pool=PROJ.svc.id.goog`.
2. The three `add-iam-policy-binding` commands with `principal://…`.
3. Deploy → confirm the startup line names project/cluster/location **and its resolution tier**.
   **This decides whether tier 3 survives.**
4. `cloud_what_changed` returns the cluster-creation audit events.
5. Request a rare machine type in a MIG → confirm `listErrors` surfaces the stockout.
6. **Negative test:** unbind a role; confirm the printed command works when pasted.
7. Capture real API responses → replace the hand-written fixtures.

## Documentation and Helm

| File | Change |
|---|---|
| `website/content/docs/integrations/data-sources/gcp-cloud.md` | New; mirrors `aws-cloud.md` |
| `website/content/docs/concepts/data-sources.md` | Cloud row gains GCP |
| `website/content/docs/concepts/design.md:310` | Cloud row: AWS **and GCP**, no longer "GCP … future" |
| `README.md:195` area | Capability table row |
| `deploy/helm/runlore/values.yaml` | Commented `cloud:` GCP example |
| `deploy/helm/runlore/values-full.yaml` | Worked example |
| `deploy/helm/runlore/templates/_helpers.tpl` | Add `POD_SERVICE_ACCOUNT` downward-API env |

**No `serviceAccount.annotations` change** — direct principal binding needs none. The docs must say
this loudly, because essentially every other GKE guide instructs you to annotate, and a leftover
`iam.gke.io/gcp-service-account` annotation would silently redirect RunLore to a GSA holding none of
these roles — presenting as a permissions failure whose printed fix does not resolve it.

## Open question deferred to the live run

Whether the GKE metadata server exposes `instance/attributes/cluster-location` to Pods across GKE
versions and modes. Tier 3 of identity resolution exists solely to cover the case where it does not,
and step 3 of the runbook resolves it. This is the only known unknown in the design, and it is
contained: it changes whether ~40 lines survive, nothing else.
