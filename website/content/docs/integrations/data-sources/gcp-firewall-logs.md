---
title: GCP Firewall Logs
weight: 309
integration: {kind: network, id: gcp-firewall-logs}
---

**What it gives you** — the `network_drops` tool on **any GCP VPC**, including GKE. Reads `DENIED`
connections from Cloud Logging (`compute.googleapis.com/firewall`). Read-only; auth is Workload
Identity / Application Default Credentials.

## Minimal config

```yaml
network:
  provider: gcp-firewall-logs
  gcp: { project: my-gcp-project }
```

## Verify it locally

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'network provider enabled.*gcp-firewall-logs'
```

Fire a test incident that touches a denied connection and confirm `network_drops` was called:

```bash
kubectl -n runlore logs deploy/runlore | grep 'tool=network_drops'
```

## Notes

- **Firewall Rules Logging must be enabled on the relevant rules** — this provider reads a log
  stream that only exists once logging is turned on per-rule; it does not enable it for you.
- Requires **`logging.logEntries.list`** (e.g. the `roles/logging.viewer` role) on the Workload
  Identity / ADC principal.
- **Same IP-based caveat as AWS VPC Flow Logs**: v1 returns recent subnet/VPC-wide `DENIED`
  connections rather than pod-scoped flows — the namespace/pod selector is not mapped to IPs. Read it
  as "what's being denied in this VPC lately", not a pod-scoped signal.
- `project` defaults to what ADC / the GCE metadata server resolves; set it explicitly to be sure
  which project's logs are queried.
- Works on any GCP VPC, GKE or not — this is the CNI-agnostic path. See
  [AWS VPC Flow Logs]({{< relref "aws-vpc-flow-logs.md" >}}) for the AWS equivalent, or
  [Hubble]({{< relref "hubble.md" >}}) for pod-scoped flows on a Cilium cluster.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `network` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
