# RunLore NetworkPolicy Egress Scoping — Design

| | |
|---|---|
| **Status** | **Implemented** (2026-06-24) — *lighter-hardening* variant chosen (DNS always scoped + opt-in `strict` deny-by-default), keeping a working out-of-box default. Validated via `helm lint` + `helm template`. |
| **Date** | 2026-06-24 |
| **Scope** | Scope DNS egress to the cluster DNS service in all modes, and add an opt-in `networkPolicy.strict` deny-by-default mode with a structured egress allowlist — without breaking the permissive out-of-box default. Touches `deploy/helm/runlore/templates/networkpolicy.yaml` + `values.yaml`. |
| **Author** | Smana (drafted with Claude) |
| **Related** | [`docs/roadmap.md`](../../roadmap.md) **R3** (P0); coordinate with **R9** (server hardening also edits this file: ingress `from:` selector) |

---

## 1. Why this exists

The chart's NetworkPolicy egress rules specify **ports without a `to:` selector**
(`deploy/helm/runlore/templates/networkpolicy.yaml:20-32` — DNS:53, then `443`/`6443` with no peer).
Kubernetes NetworkPolicy semantics then allow those ports to **any destination**. The header comment
frames this as an intentional baseline ("tighten egress to specific CIDRs via `networkPolicy.extraEgress`"),
but the *default* posture is still 443-to-anywhere — and the pod holds a **GitHub App private key**,
**model API keys**, and **Slack/Matrix tokens**, while ingesting untrusted alert/log/catalog text into an
LLM. That makes the default egress a live data-exfiltration / prompt-injection beacon channel.

**Implemented as the lighter-hardening variant** (chosen 2026-06-24 over a hard default-deny, which would
break every install until operators enumerate their deployment-specific model/forge/observability
endpoints — many would just set `0.0.0.0/0` and gain nothing): DNS egress is **always** scoped to the
cluster DNS service (a free win), and an opt-in `networkPolicy.strict=true` turns the 443/6443 rules into
a deny-by-default allowlist; the default stays permissive so installs keep working. The existing
`CiliumNetworkPolicy` for the EKS Pod Identity link-local endpoint is correct and stays (it renders in
both modes).

> **Reviewer note (the dispute):** one reviewer reads the current state as an acceptable documented
> baseline; the Kubernetes fact (ports-without-`to:` = any destination on that port) is not in dispute.
> This slice resolves it by making destination-scoping the **default**, with knobs to widen it — worth
> doing regardless of how the current default is characterised.

## 2. Decisions (resolved 2026-06-24)

| # | Decision | Notes |
|---|---|---|
| D1 (revised) | **Default stays permissive; `strict` is opt-in.** A hard default-deny was rejected — it breaks every install until operators enumerate deployment-specific model/forge/observability endpoints (many would just set `0.0.0.0/0`, gaining nothing). Instead: DNS always scoped + opt-in `strict`. | Product call (user, 2026-06-24). |
| D2 | DNS scoped to the cluster DNS service (`namespaceSelector: kube-system` + `podSelector: k8s-app: kube-dns`), configurable; `toClusterDNS: false` falls back to open `:53`. | The free, always-on hardening. |
| D3 (resolved) | **Allowlist schema:** `networkPolicy.strict` (bool) + `networkPolicy.egress.{dns, allowedCIDRs (ipBlock → 443), namespaceSelectors (→ 443), apiServerCIDRs (ipBlock → 6443)}` + the existing `extraEgress` (appended in both modes). Empty strict allowlist ⇒ DNS-only (genuine deny). | Documented in `values.yaml` comments. |
| D4 (resolved) | **Validation:** rendered + asserted inline via `helm lint` + `helm template` across default / strict / DNS-disabled / empty-strict / awsPodIdentity. A *CI-wired* render assertion is deferred to roadmap **R13** (CI hardening). | No `go test` coverage for charts; kept the change focused. |

## 3. Design

### 3.1 Template (sketch — finalize against D3)

```yaml
egress:
  # DNS — scoped to kube-dns, not open :53 to anywhere.
  - to:
      - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kube-system } }
        podSelector: { matchLabels: { k8s-app: kube-dns } }
    ports: [{ protocol: UDP, port: 53 }, { protocol: TCP, port: 53 }]
  # Kubernetes API server.
  {{- with .Values.networkPolicy.egress.apiServer }}
  - to: {{ toYaml .to | nindent 8 }}
    ports: [{ protocol: TCP, port: {{ .port | default 443 }} }]
  {{- end }}
  # Off-cluster model / git forge / observability (operator-supplied).
  {{- range .Values.networkPolicy.egress.allowedCIDRs }}
  - to: [{ ipBlock: { cidr: {{ . | quote }} } }]
    ports: [{ protocol: TCP, port: 443 }]
  {{- end }}
  # In-cluster backends by namespace.
  {{- range .Values.networkPolicy.egress.namespaceSelectors }}
  - to: [{ namespaceSelector: {{ toYaml . | nindent 12 }} }]
    ports: [{ protocol: TCP, port: 443 }]
  {{- end }}
  {{- with .Values.networkPolicy.extraEgress }}
  {{- toYaml . | nindent 4 }}
  {{- end }}
```

Every rule now carries a `to:`. An empty allowlist yields DNS-only egress (deny everything else) — the
genuine default-deny posture.

### 3.2 `values.yaml` (sketch — finalize against D3)

```yaml
networkPolicy:
  enabled: true
  egress:
    apiServer:
      to: [{ ipBlock: { cidr: 0.0.0.0/0 } }]   # REPLACE with your API server endpoint/CIDR
      port: 443
    allowedCIDRs: []          # model / git forge / observability endpoints, e.g. ["1.2.3.4/32"]
    namespaceSelectors: []    # in-cluster backends, e.g. [{ matchLabels: { name: monitoring } }]
  extraEgress: []
  awsPodIdentity: false
```

## 4. Components / seams

| Change | Location |
|---|---|
| Destination-scoped egress rules | `deploy/helm/runlore/templates/networkpolicy.yaml` |
| `egress.*` allowlist values + docs | `deploy/helm/runlore/values.yaml` |
| Chart-render assertion (D4) | `hack/` script or CI step (+ optionally `helm-unittest`) |
| Getting-started egress guidance | `docs/getting-started.md` |

## 5. Trade-offs accepted in v1

- The shipped default likely can't reach the model/forge/observability out of the box (those endpoints
  are deployment-specific) — operators must declare them. This is the correct safe default for a pod
  holding secrets; document it prominently so it isn't a silent connectivity failure.
- A coarse `allowedCIDRs: ["0.0.0.0/0"]` escape hatch remains available for operators who can't enumerate
  destinations — but it is opt-in, not the default.

## 6. Testing

- **Chart render (D4 harness):** `helm template` assertions —
  - every `spec.egress[*]` entry has a non-empty `to:` (no port-only rules);
  - default values → DNS-scoped + API-server rules only (no `0.0.0.0/0` on 443 unless explicitly set);
  - `allowedCIDRs` / `namespaceSelectors` / `extraEgress` each render their rules;
  - `awsPodIdentity: true` still renders the `CiliumNetworkPolicy`.
- **e2e:** `hack/e2e-k3d.sh` still installs and the agent still reaches its (mock) backends — confirm the
  default-deny doesn't break the e2e wiring (add the mock's host/CIDR to the test values).

## 7. Out of scope (later slices)

- Ingress `from:` scoping to Alertmanager — owned by **R9** (same file; coordinate the edit).
- A `CiliumNetworkPolicy` variant of the full egress allowlist (FQDN-based egress) — a possible Cilium-only
  enhancement.
- Auto-deriving destinations from configured provider URLs.

This slice makes the agent's default network posture deny-by-default with an explicit allowlist — the
single highest-leverage reduction of exfiltration risk given the secrets it holds.
