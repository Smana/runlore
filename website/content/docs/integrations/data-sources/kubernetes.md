---
title: Kubernetes
weight: 306
integration: {kind: kubernetes, id: kubernetes}
---

**What it gives you** — `pod_status`, `kube_events`, `controller_logs`, `pod_logs` and
`resource_spec`: the baseline cluster-introspection tools every investigation can reach for, via
client-go.

## How it's enabled

There is no config key for this one. RunLore builds a read-only clientset automatically —
`rest.InClusterConfig()` first, falling back to the local kubeconfig (`$KUBECONFIG`, then
`~/.kube/config`) — and registers the cluster tools whenever that succeeds. No cluster reachable (a
local run with no kubeconfig) simply leaves them unregistered; nothing else changes.

`controller_logs` is additionally gated on `gitops.engine: flux` — it exists to surface why a Flux
controller failed to reconcile, so it isn't registered under `argocd`.

`resource_spec` needs a **dynamic** client plus **discovery** (the typed clientset cannot read a
CRD), and is left unregistered when either is unavailable — a tool that cannot answer must not
exist, or its unanswerable questions get answered with a misleading negative.

## Verify it locally

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'tool=pod_status|tool=kube_events|tool=controller_logs|tool=pod_logs'
```

If the clientset itself fails to build, RunLore logs it instead of registering the tools silently:

```bash
kubectl -n runlore logs deploy/runlore | grep 'clientset unavailable'
```

## Notes

- **`pod_status` / `kube_events` are cluster-wide** — an investigation's incident namespace is
  arbitrary (e.g. `apps/payments`), so pod status and event reads need cluster scope. RunLore's
  ClusterRole grants `get`/`list` on `pods` and `get`/`list`/`watch` on `events` for exactly this,
  and **nothing else** — no `pods/log` at cluster scope.
- **`pod_logs` is namespace-restricted at two layers**, because raw log bodies can carry secrets/PII
  that would otherwise flow straight to the LLM: a namespaced RBAC Role scoped to
  `rbac.controllerLogNamespaces` (chart default `[flux-system]`), and an app-layer allowlist
  (`config.investigation.pod_log_namespaces`) enforced *before* the cluster is even queried — a
  request for any other namespace is rejected at the app layer, not just denied by RBAC. The chart
  auto-defaults the app-layer allowlist to the RBAC namespace list, so the two stay in sync unless you
  override one.
- **`resource_spec` reads one object's `.spec`/`.status` for any kind the cluster serves**, CRDs
  included, resolving a bare `Kind` through discovery rather than a compiled-in table. Its **six**
  endings are kept apart deliberately — `found`, `absent`, `forbidden`, `kind_unknown`, `refused`
  and `kind_ambiguous`; **only `absent` is evidence that an object does not exist**. A
  kind served by several API groups (`Event` everywhere, `NetworkPolicy` on a Calico cluster) reads
  nothing and names the candidates; pass `group` to pick one.
- **`resource_spec` is bounded by an RBAC allowlist, not by a wildcard.** `rbac.resourceSpecRules`
  (chart values) grants `get` on the spec-bearing kinds it reads. **It ships populated**, so a stock
  `helm install` already grants those kinds cluster-wide; `rbac.resourceSpecRules: []` declines the
  whole grant and costs you this tool alone. `Secret` is refused by the tool
  outright — before and after kind resolution — but the ClusterRole is the real boundary, so
  **never** widen the list to `resources: ["*"]`: that includes `secrets`. A kind you have not
  granted comes back as a *denial*, never as a missing object.
- **A container's env values are masked structurally** on the way out (`name: POSTGRES_PASSWORD` /
  `value: …`), because the secret-shaped-key ruleset cannot see a shape that puts the sensitive word
  in the *value* of `name:`. `valueFrom` references are kept — the model still needs to see which
  Secret a workload consumes.
- Every pod's log fetch is bounded: up to 5 matching pods, 300 tail lines each, and every container is
  read explicitly (a pod with 2+ containers — an istio/linkerd/cloudsql sidecar, say — rejects a log
  request with no container named, so RunLore always names one).
- `pod_logs` can also read a **previously-terminated** container's logs (the crash output of a
  `CrashLoopBackOff`) instead of the running one.
- The ServiceAccount's RBAC grants **no write verbs** by default — see [Security model → Least-privilege
  RBAC]({{< relref "/docs/security/security-model.md#least-privilege-rbac" >}}) for the full
  ClusterRole and the write-verb ladder `actions.mode` climbs.

## Reference

- [Security model → Least-privilege RBAC]({{< relref "/docs/security/security-model.md#least-privilege-rbac" >}})
  for the full RBAC rules and the namespace-scoping model.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
