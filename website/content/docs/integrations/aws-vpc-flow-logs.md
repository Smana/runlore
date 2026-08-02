---
title: AWS VPC Flow Logs
weight: 60
integration: {kind: network, id: aws-vpc-flow-logs}
---

**What it gives you** — the `network_drops` tool on **any AWS VPC**, including EKS clusters running
the AWS VPC CNI where Cilium/Hubble is absent. Reads `REJECT` records from the CloudWatch Logs group
your VPC Flow Logs deliver to. Read-only; auth is in-cluster identity (EKS Pod Identity / IRSA) — no
static keys.

## Minimal config

```yaml
network:
  provider: aws-vpc-flow-logs
  aws: { region: eu-west-3, log_group: /aws/vpc/flowlogs }
```

A custom flow-log field layout (a log group created with a non-default field list):

```yaml
network:
  provider: aws-vpc-flow-logs
  aws:
    region: eu-west-3
    log_group: /aws/vpc/flowlogs
    flow_format: custom
    flow_fields: { srcaddr: 1, dstaddr: 2, srcport: 3, dstport: 4, protocol: 5 }
```

## Verify it locally

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'network provider enabled.*aws-vpc-flow-logs'
```

Fire a test incident that touches a rejected flow and confirm `network_drops` was called:

```bash
kubectl -n runlore logs deploy/runlore | grep 'tool=network_drops'
```

## Notes

- Requires **`logs:FilterLogEvents`** on the target CloudWatch Logs group (Pod Identity / IRSA role
  policy).
- **`flow_format` defaults to the standard v2 layout** (14 fields). Set it to `custom` and provide
  `flow_fields` (a field-name → 0-based column index map; `srcaddr`, `dstaddr`, `srcport`, `dstport`,
  `protocol` are required) when your log group was created with a non-default field list — a
  custom-format group silently returns no results under the `v2` assumption otherwise, because the
  positional field parsing no longer holds.
- **VPC Flow Logs are IP-based.** v1 returns recent VPC-wide `REJECT` records rather than pod-scoped
  flows — the namespace/pod selector on the `network_drops` call is **not** mapped to IPs. Treat the
  result as "what's being rejected in this VPC lately", not "what's being rejected for this specific
  pod".
- Any AWS VPC works, Cilium or not — this is the CNI-agnostic path for clusters on the AWS VPC CNI.
  For a Cilium cluster, [Hubble]({{< relref "hubble.md" >}}) gives pod-scoped flows instead.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `network` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
