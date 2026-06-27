# NetworkPolicy Egress Scoping Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Implemented (2026-06-24) as the lighter-hardening variant** — see the design spec for the resolved decisions. The shipped change keeps a permissive default + opt-in `networkPolicy.strict`, with DNS always scoped to the cluster DNS service; validated via `helm lint` + `helm template` (default / strict / DNS-disabled / empty-strict / awsPodIdentity). The full-default-deny task list below is superseded.

**Goal:** Replace the chart's port-only egress rules (443/6443 to any destination) with destination-scoped, deny-by-default + allowlist egress — the P0 that most reduces exfiltration risk given the in-pod GitHub App key, model keys, and chat tokens.

**Architecture:** Each egress rule gains a `to:` selector (DNS scoped to kube-dns; API server; operator-supplied off-cluster CIDRs and in-cluster namespaceSelectors; plus `extraEgress`). An empty allowlist = DNS-only egress. The EKS Pod Identity `CiliumNetworkPolicy` is unchanged.

**Tech Stack:** Helm (chart in `deploy/helm/runlore`), `helm template`/`helm lint`, `yq`/`grep` (or the `helm-unittest` plugin per D4) for render assertions; `hack/e2e-k3d.sh` for the live check.

**Spec:** `docs/superpowers/specs/2026-06-24-networkpolicy-egress-scope-design.md`

**Branch:** `fix/networkpolicy-egress-scope`

**Coordinate-with:** **R9** also edits `networkpolicy.yaml` (ingress `from:` selector) — sequence or land together to avoid a conflict.

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `deploy/helm/runlore/values.yaml` | egress knobs | `networkPolicy.egress.{apiServer,allowedCIDRs,namespaceSelectors}` + docs |
| `deploy/helm/runlore/templates/networkpolicy.yaml` | rendered policy | destination-scoped egress; remove the port-only 443/6443 rule |
| `hack/` script or CI (D4) | render assertion | assert every egress rule has a `to:`; default = DNS+API only |
| `docs/getting-started.md` | operator guidance | how to declare model/forge/observability egress destinations |

Order: T1 sets up the render assertion (the "failing test"). T2 implements values + template. T3 runs the e2e. T4 docs + verify.

---

### Task 1: Render assertion (the failing "test")

**Files:** `hack/` (new small script, e.g. `hack/chart-assert.sh`) or a CI step — per D4.

- [ ] **Step 1:** Write an assertion that renders the chart with default values and **fails** if any
  `spec.egress[*]` entry lacks a `to:` selector (e.g. `helm template deploy/helm/runlore | yq …`), plus
  cases for `allowedCIDRs`/`namespaceSelectors`/`extraEgress` rendering and the `awsPodIdentity` Cilium
  policy. Decide (D4) whether this lives in CI, a Make target, or `helm-unittest`.
- [ ] **Step 2: Run to verify it fails** against the current template (today's egress rules have no
  `to:`) → assertion FAILS.

---

### Task 2: Destination-scoped egress

**Files:** `deploy/helm/runlore/values.yaml`, `deploy/helm/runlore/templates/networkpolicy.yaml`

- [ ] **Step 1:** Add the `networkPolicy.egress.*` allowlist schema to `values.yaml` (per spec §3.2; finalize against D3) with documented, safe defaults (DNS + API server only).
- [ ] **Step 2:** Rewrite the `egress:` block in `networkpolicy.yaml` so every rule carries a `to:` (DNS scoped to kube-dns; API server; `range` over `allowedCIDRs`/`namespaceSelectors`; keep `extraEgress`). Remove the blanket `443`/`6443` port-only rule. Leave the `CiliumNetworkPolicy` block untouched.
- [ ] **Step 3: Run the assertion + `helm lint`/`helm template`** → assertion PASSES; chart lints; rendered YAML is valid.
- [ ] **Step 4: Commit.** `git commit -m "fix(chart): destination-scoped, deny-by-default NetworkPolicy egress"`

---

### Task 3: e2e — connectivity still works

- [ ] **Step 1:** Run `hack/e2e-k3d.sh` (the agent must still reach the mock backends). If the default-deny
  blocks the mock, add the mock host/CIDR to the e2e values (`allowedCIDRs`/`namespaceSelectors`) and
  re-run. This proves the allowlist model is usable, not just secure.
- [ ] **Step 2: Commit** any e2e values change. `git commit -m "test(e2e): allow mock backend egress under scoped NetworkPolicy"`

---

### Task 4: Docs + whole-tree verification

- [ ] **Step 1:** Document the egress allowlist in `docs/getting-started.md` (operators must declare their
  model/forge/observability destinations) and update the template header comment.
- [ ] **Step 2:** `helm lint deploy/helm/runlore` clean; the render assertion green in CI.
- [ ] **Step 3:** Flip **R3** Status in `docs/roadmap.md` to the PR number.

---

## Notes for the implementer

- **Coordinate with R9** before editing `networkpolicy.yaml` (it adds the ingress `from:` selector) — a
  single combined PR for both ingress + egress scoping may be cleanest.
- The shipped default intentionally can **not** reach off-cluster model/forge endpoints until the operator
  declares them — this is the safe default, but it must be *loudly* documented so it isn't a silent
  connectivity failure on first install.
- Keep the EKS Pod Identity `CiliumNetworkPolicy` exactly as-is; it solves a different (host-entity) problem.
- Don't regress the DNS rule into open `:53` — scope it to kube-dns.
