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
