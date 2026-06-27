# Design — Pluggable network-flow data sources (de-couple from Cilium)

Date: 2026-06-24 · Status: approved (autonomous) · Worktree: `feat/pluggable-network-sources`

## Problem

The `network_drops` investigation tool is the only "network" signal RunLore has, and its
sole implementation is **Cilium Hubble** (`internal/network/hubble`). The config schema bakes
that assumption in — `Network Endpoint` is documented as *"Hubble Relay gRPC address"* — so the
network signal is effectively *Cilium-only*. Cilium is an **optional** CNI absent from many
clusters (EKS with the AWS VPC CNI, GKE with the default CNI, etc.). The network signal must be
**pluggable** and must **not assume any particular CNI**, with no provider enabled by default.

Note: the tool is already opt-in (empty config disables it). This work is about removing the
*architectural* Cilium coupling and adding non-Cilium providers, not about flipping a default.

## What stays the same

`providers.NetworkProvider` is already backend-neutral:

```go
type NetworkProvider interface {
    Drops(ctx context.Context, sel Selector, w TimeWindow) (LogResult, error)
}
```

We keep this interface verbatim. "Drops" generalizes cleanly to *denied/rejected flows*:
Hubble DROPPED verdicts, AWS VPC Flow Logs `REJECT` records, GCP Firewall Logs `DENIED`
dispositions. The `Selector` (namespace/pod) is honored where the backend supports it (Hubble);
IP-based cloud flow logs cannot map k8s pods to IPs in v1, so they ignore the selector and return
recent VPC/subnet-wide denials (documented in the tool result + docs).

## Approach (chosen)

Mirror the existing **`Cloud` provider discriminator** pattern (`cloud.provider: "" | aws`).
Replace the single-URL `Network` config with a provider-discriminated block. Wire the selected
provider in `buildModelAndTools`. Make the tool description vendor-neutral.

### Config (internal/config/config.go)

```go
// Network configures the network-flow data source backing the network_drops tool.
// RunLore's network signal is PLUGGABLE and assumes no particular CNI. Empty Provider
// disables it (default). Pick the provider matching your environment.
type Network struct {
    Provider string     `yaml:"provider"` // "" (disabled) | hubble | aws-vpc-flow-logs | gcp-firewall-logs
    Hubble   HubbleCfg  `yaml:"hubble"`
    AWS      AWSFlowCfg `yaml:"aws"`
    GCP      GCPFlowCfg `yaml:"gcp"`

    // Deprecated back-compat: a bare `network: {url: ...}` (old Hubble-only shape) is
    // still accepted and treated as provider=hubble. Custom UnmarshalYAML maps it.
    URL string `yaml:"url"`
}
type HubbleCfg struct { URL string `yaml:"url"` } // hubble-relay.kube-system:80
type AWSFlowCfg struct {
    Region   string `yaml:"region"`    // default: AWS_REGION / IMDS
    LogGroup string `yaml:"log_group"` // CloudWatch Logs group receiving VPC Flow Logs (required)
}
type GCPFlowCfg struct {
    Project string `yaml:"project"` // GCP project ID (default: ADC / metadata server)
}
```

`UnmarshalYAML` on `Network`: if `provider` is empty but a top-level/Hubble `url` is set, set
`Provider=hubble`, `Hubble.URL=url` and let the wiring log a deprecation warning.

### Wiring (cmd/lore/main.go, buildModelAndTools)

Replace the `if cfg.Network.URL != ""` block with a switch on `cfg.Network.Provider`
(after applying the back-compat mapping). Each branch constructs its provider; failures
log a warning and disable the tool (never crash). Mirrors the AWS cloud block.

### Tool description (internal/investigate/query_tools.go)

Vendor-neutral:
> "List recently denied/dropped network flows for a namespace (optionally a pod) — surfaces
> NetworkPolicy denials, firewall/security-group rejects, and connectivity failures, from the
> configured network-flow source."

## Providers shipped in v1

1. **hubble** (existing, `internal/network/hubble`) — kept; no longer the implicit default.
2. **aws-vpc-flow-logs** (`internal/network/awsvpc`, NEW) — CloudWatch Logs `FilterLogEvents`
   over the configured VPC-Flow-Logs log group, `REJECT` records only, parsed from the default
   v2 flow-log format. CNI-agnostic: works on any AWS VPC (incl. EKS + AWS VPC CNI). Auth via
   the default credential chain (EKS Pod Identity / IRSA) like `internal/providers/cloud/aws`.
3. **gcp-firewall-logs** (`internal/network/gcpfirewall`, NEW) — Cloud Logging `entries.list`
   filtered to `compute.googleapis.com%2Ffirewall` entries with `jsonPayload.disposition=DENIED`.
   CNI-agnostic: works on any GCP VPC with Firewall Rules Logging enabled (incl. GKE). Auth via
   ADC / Workload Identity.

### Future providers (documented, not implemented)
Azure NSG Flow Logs; CNI-agnostic eBPF (Microsoft Retina exposes a Hubble-compatible API, so it
can reuse the hubble provider); Calico flow logs. The discriminator makes these drop-in.

## Testing
- Each provider has a unit test with an injected fake backend (narrow API interface for AWS;
  `option.WithHTTPClient`+`option.WithEndpoint` httptest for GCP). No credentials needed.
- Config test: provider selection + the deprecated `url` back-compat mapping.
- `go build ./... && go vet ./... && gofmt -l . && go test ./... && golangci-lint run ./...` clean.

## Docs / Helm
- `docs/getting-started.md`: network section becomes provider-keyed; Cilium is one option.
- `docs/design.md`: NetworkProvider section lists the three providers, CNI-agnostic.
- A short `docs/data-sources.md` (or section) enumerating pluggable sources.
- `deploy/helm/runlore/values.yaml`: commented `config.network` example per provider.
