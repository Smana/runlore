---
title: Cilium Hubble
weight: 307
integration: {kind: network, id: hubble}
---

**What it gives you** — the `network_drops` tool backed by eBPF flow visibility with rich drop
reasons (NetworkPolicy denials in particular). Requires the **Cilium** CNI + Hubble Relay.

## Minimal config

```yaml
network:
  provider: hubble
  hubble: { url: hubble-relay.kube-system:80 }   # Relay gRPC host:port
```

With TLS to the Relay endpoint:

```yaml
network:
  provider: hubble
  hubble: { url: hubble-relay.kube-system:443, tls: true }
```

## Verify it locally

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'network provider enabled.*hubble'
```

Fire a test incident that touches a NetworkPolicy-denied flow and confirm `network_drops` appears
among the tools called:

```bash
kubectl -n runlore logs deploy/runlore | grep 'tool=network_drops'
```

## Notes

- **`network.provider: hubble` plus `network.hubble.url` both matter** — the provider selects the
  implementation, the URL is the Relay gRPC address.
- `tls` defaults to `false` (plaintext) — set `true` for an encrypted Relay endpoint.
- The `network_drops` tool is **pluggable and CNI-agnostic**: `network.provider` also accepts
  `aws-vpc-flow-logs` and `gcp-firewall-logs` for non-Cilium clusters. See
  [AWS VPC Flow Logs]({{< relref "aws-vpc-flow-logs.md" >}}) and
  [GCP Firewall Logs]({{< relref "gcp-firewall-logs.md" >}}).
- **Legacy shape still accepted**: a bare `network: {url: …}` (no `provider`) is treated as Hubble
  with a startup deprecation warning — prefer the explicit `provider: hubble` form above.
- No provider is enabled by default; leaving `network` unset simply means `network_drops` is never
  registered.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `network` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
