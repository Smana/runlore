# Investigation data sources

RunLore correlates signals from **pluggable** backends. Every source is an interface
(`internal/providers/providers.go`), so the investigation loop and the knowledge catalog are
written against engine-agnostic types — never against a specific vendor. You **choose** which
sources to wire; an unset source simply disables its tool (no source is required, none is
assumed).

| Signal | Tool(s) | Interface | Providers (v1) | Config key |
|---|---|---|---|---|
| GitOps "what changed" | `what_changed`, `gitops_*` | `GitOpsProvider` | Flux, ArgoCD | `gitops.engine` |
| Metrics | `query_metrics`, `query_metrics_range` | `MetricsProvider` | VictoriaMetrics, Prometheus (PromQL) | `metrics.url` |
| Logs | `query_logs` | `LogsProvider` | VictoriaLogs (LogsQL) | `logs.url` |
| **Network flows** | `network_drops` | `NetworkProvider` | **Cilium Hubble · AWS VPC Flow Logs · GCP Firewall Logs** | `network.provider` |
| Cloud control plane | `cloud_*` | `CloudProvider` | AWS (CloudTrail + EC2/ASG/EKS) | `cloud.provider` |
| Kubernetes | `pod_status`, `kube_events`, `controller_logs`, `pod_logs` | `KubeReader`/`LogReader` | client-go | (in-cluster) |
| Knowledge | `kb_search` | catalog index | bleve BM25 | `catalog.*` |
| Source repos | `source_diff` | built-in (go-git) | any git host over HTTPS (GitHub App auth) | `source_repos.allow` |

## Network flows are CNI-agnostic

The `network_drops` tool surfaces **denied / dropped** flows (NetworkPolicy denials,
security-group/NACL rejects, firewall denials) — a strong "is this a connectivity block?" signal.
It is **pluggable and does not assume any particular CNI**. Pick the provider that matches your
environment via `network.provider`; no provider is enabled by default.

### `hubble` — Cilium Hubble
eBPF flow visibility with rich drop reasons. Requires the **Cilium** CNI + Hubble Relay.
```yaml
network:
  provider: hubble
  hubble: { url: hubble-relay.kube-system:80 }   # Relay gRPC host:port
```

### `aws-vpc-flow-logs` — AWS VPC Flow Logs
Works on **any AWS VPC**, including EKS clusters running the AWS VPC CNI (where Cilium is absent).
Reads `REJECT` records from the CloudWatch Logs group that receives your VPC Flow Logs. Read-only;
auth is in-cluster identity (EKS Pod Identity / IRSA) — no static keys. Requires
`logs:DescribeLogGroups`/`logs:FilterLogEvents` on the target log group.
```yaml
network:
  provider: aws-vpc-flow-logs
  aws: { region: eu-west-3, log_group: /aws/vpc/flowlogs }
```
> Note: VPC Flow Logs are IP-based, so v1 returns recent VPC-wide `REJECT`s rather than
> pod-scoped flows (the namespace/pod selector is not mapped to IPs).

### `gcp-firewall-logs` — GCP Firewall Rules Logging
Works on **any GCP VPC**, including GKE. Reads `DENIED` connections from Cloud Logging
(`compute.googleapis.com/firewall`). Requires **Firewall Rules Logging** enabled on the relevant
rules. Read-only; auth is Workload Identity / ADC. Needs `logging.logEntries.list` (e.g. the
`roles/logging.viewer` role).
```yaml
network:
  provider: gcp-firewall-logs
  gcp: { project: my-gcp-project }
```
> Same IP-based caveat as AWS: v1 returns recent subnet/VPC-wide `DENIED` connections.

### Adding another provider
Implement `providers.NetworkProvider` (a single `Drops(ctx, sel, window)` method), then add a
`case` in `buildModelAndTools` keyed off `network.provider`. Candidates: Azure NSG Flow Logs, or
CNI-agnostic eBPF (Microsoft **Retina** exposes a Hubble-compatible flow API, so it can reuse the
`hubble` provider directly).

> Compatibility: the legacy `network: { url: ... }` shape (Hubble-only) is still accepted and
> mapped to `provider: hubble` with a deprecation warning. Prefer the explicit `provider` form.

## Custom webhooks — any vendor, no code

The `custom` source maps ANY vendor's alert JSON to investigations with dot-path
field extraction — config only. Each named instance gets its own endpoint
`POST /webhook/custom/<instance>` and its own optional bearer token
(`token_env`, falling back to `server.webhook_token_env`). Field paths are
dot-separated with optional `[n]` indexes (`alerts[0].labels.alertname`); a
missing path falls back to `defaults`. `severity_map` normalizes vendor
severities to yours. A payload with `items` set is a batch (path to the event
array); without it the whole body is one event. Events whose `resolved` path
equals `resolved_value` (default `"resolved"`) record a resolution for the
outcome ledger instead of triggering an investigation (requires `fingerprint`).
The per-delivery request cap and 1MiB body cap apply as for every webhook
source. A typo'd instance key, an unparseable path, or a missing `fields.title`
aborts startup — mappings never fail silently at ingest.

### Grafana Alerting

```yaml
sources:
  custom:
    instances:
      grafana:
        token_env: GRAFANA_WEBHOOK_TOKEN
        items: alerts
        fields:
          title: labels.alertname
          message: annotations.summary
          severity: labels.severity
          namespace: labels.namespace
          workload_name: labels.pod
          fingerprint: fingerprint
          resolved: status
        labels: labels
        defaults: { environment: prod }
```

Point a Grafana webhook contact point at
`https://<runlore>/webhook/custom/grafana` with an `Authorization: Bearer …`
custom header.

### Datadog (custom webhook payload)

Datadog webhooks POST a single flat JSON you define with template variables:

```json
{"title": "$EVENT_TITLE", "text": "$TEXT_ONLY_MSG", "alert_type": "$ALERT_TYPE",
 "alert_status": "$ALERT_TRANSITION", "aggreg_key": "$AGGREG_KEY"}
```

```yaml
sources:
  custom:
    instances:
      datadog:
        token_env: DATADOG_WEBHOOK_TOKEN
        fields:
          title: title
          message: text
          severity: alert_type
          fingerprint: aggreg_key
          resolved: alert_status
        resolved_value: Recovered
        severity_map: { error: critical }
```

Requests without a Kubernetes workload recall only resource-less entries (the
scopeless tier) — same as PagerDuty.

## Source repos

`what_changed` surfaces manifest-layer changes ("image `v1.2.2 → v1.2.3`"). Registering
source repos deepens that to the actual code behind such a bump — commit subjects, a
per-file diffstat, and the largest changed hunks — turning a correlation into a cause.
This powers the `source_diff` tool; **unset (default) ⇒ the tool is not registered**.

```yaml
source_repos:
  allow:
    - github.com/acme/*              # every repo directly under the org
    - gitlab.com/acme/infra-modules  # or exact host/org/repo
```

**Auth:** private **GitHub** repos reuse the forge GitHub App installation token — install
the App on those repos with `contents: read`. Public repos need nothing. Private non-GitHub
hosts are not supported yet.

RunLore performs **read-only clones with no working-tree checkout** — bare mirrors when the
mirror cache is enabled (the default), or a no-checkout clone per call when the cache is
disabled. Either way, no writes are made to the source repo. Mirrors reuse the
`gitops.mirror` directory (a `source/` subdir of the same root), so warm mirrors cost
nothing after the first clone.

Allowlist matching is enforced **server-side before any network call**: the model can only
make RunLore clone repos you explicitly listed. A wrong repo guess fails at ref resolution
(the tag won't exist) and the error response lists nearby tags so the model can self-correct.

Full key reference and token-cost details: [configuration.md → `source_repos`](configuration.md#source_repos--source-repo-allowlist-for-source_diff).
