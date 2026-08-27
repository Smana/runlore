---
title: AWS cloud control plane
weight: 310
integration: {kind: cloud, id: aws}
---

**What it gives you** — `cloud_*` tools: CloudTrail `LookupEvents` (a "what changed outside GitOps"
lens — who/what mutated infrastructure directly) plus EC2/ASG/EKS resource health. Read-only; auth is
in-cluster identity (EKS Pod Identity / IRSA) via the AWS SDK's default credential chain — no static
keys.

## Minimal config

```yaml
cloud:
  provider: aws
  region: eu-west-3
  cluster_name: my-eks-cluster   # scopes nodegroup/ASG queries
```

## Verify it locally

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'cloud provider enabled.*aws'
```

Fire a test incident where the root cause is outside GitOps (a manual console change, a scaling
event) and confirm a cloud tool was called:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'tool=cloud_'
```

## Notes

- **Opt-in** — `cloud.provider` is empty (disabled) by default; set it to `aws` to enable the cloud
  tools. `aws` and `gcp` are the supported values; see [GCP Cloud](gcp-cloud.md) for the latter.
- `region` defaults to `AWS_REGION` / IMDS when unset. `cluster_name` scopes nodegroup/ASG queries to
  your EKS cluster — set it, or those queries have nothing to filter on.
- **Read-only IAM policy** — the Pod Identity / IRSA role needs: `cloudtrail:LookupEvents`,
  `ec2:DescribeInstances`, `ec2:DescribeInstanceStatus`, `autoscaling:DescribeAutoScalingGroups`,
  `autoscaling:DescribeScalingActivities`, `eks:DescribeNodegroup`, `eks:ListNodegroups`. No mutating
  action is ever called.
- CloudTrail results are capped (25 events by default) so a noisy account doesn't flood the model's
  context.
- If the AWS client fails to build (bad region, no reachable credential chain), RunLore logs a warning
  and disables the cloud tools rather than failing startup — the investigation loop still runs without
  them.
- **Cilium clusters only — a known NetworkPolicy trap.** The EKS Pod Identity credential endpoint runs
  on the node **host network** (`169.254.170.23:80`), which Cilium classifies as the `host` entity. A
  plain Kubernetes `NetworkPolicy` **cannot** match that entity, so the SDK's credential fetch is
  silently dropped and the cloud tools just hang. Set `networkPolicy.awsPodIdentity: true` (chart
  value) to render a `CiliumNetworkPolicy` that allows it:
  <!-- docsguard:ignore Helm chart values, not a runlore.yaml -->
  ```yaml
  networkPolicy:
    enabled: true
    awsPodIdentity: true   # CiliumNetworkPolicy: egress to host:80 for the Pod Identity endpoint
  ```
  Confirm with Hubble if calls hang: `hubble observe --pod runlore/<pod> --verdict DROPPED` showing
  `169.254.170.23:80 (host) … DROPPED` is this exact issue.
- **Memory** — a thorough run (a "pro" model over the full step budget with the cloud tools enabled) is
  the memory peak; the chart default limit is `1.5Gi`. Lower it only if you use a smaller model / fewer
  tools.
- Complements, doesn't replace, [Source repos]({{< relref "source-repos.md" >}}) and GitOps
  `what_changed`: this is the layer for changes that happened **outside** your GitOps pipeline
  entirely.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `cloud` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
