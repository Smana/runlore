# Scope `pods/log` RBAC — design + plan (R10)

**Branch:** `fix/scope-pods-log-rbac` · **Date:** 2026-06-24 · **Roadmap:** [R10](../../roadmap.md#r10)

## 1. Reported problem

`deploy/helm/runlore/templates/rbac.yaml:28-30` grants a ClusterRole `get,list` on
`pods` **and** `pods/log` across **all** namespaces. Pod logs routinely contain
secrets/tokens/PII, and investigation context (tool output) is shipped to an external
LLM. The reported fix: move `pods`/`pods/log` into a namespaced Role bound in
`flux-system`.

## 2. CHALLENGE — does the reported fix survive a code trace?

The reported fix as literally stated (**move BOTH `pods` and `pods/log` to a
`flux-system`-only Role**) **does NOT survive** — it would break core functionality.
But a *narrower* version of it **does** survive and is genuinely net-positive. The
roadmap entry already hints at this ("The intended use is only the Flux controllers in
`flux-system`") but conflates two resources with different requirements.

Tracing the actual consumers:

| Tool | Reader call | Namespace | RBAC needed |
|------|-------------|-----------|-------------|
| `controller_logs` | `PodLogs(ctx, "flux-system", "app="+controller, …)` — `Pods(ns).List` then `Pods(ns).GetLogs` | **hardcoded `flux-system`** (`controllerlogs_tool.go:13,58`) | `pods get/list` + **`pods/log get`**, `flux-system` only |
| `pod_status` | `PodStatuses(ctx, in.Namespace, …)` → `Pods(ns).List` | **LLM-supplied incident namespace** — `apps`, `payments`, … (`kube_tools.go:41`, `cluster.go:76`) | `pods get/list`, **cluster-wide** |
| `kube_events` | `Events(ctx, in.Namespace, …)` → `Events(ns).List` | **LLM-supplied incident namespace** (`kube_tools.go:94`, `cluster.go:144`) | `events get/list`, **cluster-wide** (already granted at `rbac.yaml:24-26`) |

Key facts established by `grep` over `internal/`:

- **`pods/log` (`GetLogs`) has exactly ONE consumer**: `controller_logs`, via the
  `LogReader.PodLogs` interface, and it is **hardcoded** to `flux-system`
  (`const fluxControllerNamespace = "flux-system"`, `controllerlogs_tool.go:13`).
  `query_logs` uses VictoriaLogs (LogsQL), not the Kubernetes API, so it needs no RBAC.
- **`pods` get/list has TWO consumers**: `controller_logs` (lists pods in `flux-system`
  before fetching logs) **and** `pod_status` (lists pods in the *incident's* namespace,
  which is arbitrary). So `pods get/list` **must remain cluster-wide** or `pod_status`
  — described in its own tool prompt as "the FIRST tool to reach for when a workload
  won't start" — breaks for every non-`flux-system` workload (`apps`, `payments`, …),
  i.e. exactly the pod-level failures the design says to read first.

**Verdict.**

- ✗ Moving **`pods` get/list** to `flux-system` only → **BREAKS** `pod_status`. Reject.
- ✓ Splitting **`pods/log`** out of the ClusterRole into a namespaced Role in
  `flux-system` → **safe and net-positive**. `pods/log`'s sole consumer is hardcoded to
  `flux-system`, so a cluster-wide `pods/log` grant is strictly more than needed. This
  removes the cluster-wide raw-log read — the precise concern in R10 — while leaving
  `pods get/list` (status only, no log bodies) and `events get/list` cluster-wide so
  `pod_status` / `kube_events` keep working.

This also matches the R10 acceptance criteria verbatim: "Default `helm template` shows
no cluster-wide `pods/log`; the controller-logs tool still works against `flux-system`".

**On RBAC vs redaction.** RBAC scoping cannot fix "logs contain secrets" in general —
`pod_status`/`kube_events` still surface messages (e.g. a Secret key name in a
`CreateContainerConfigError`) from arbitrary namespaces into the LLM. The *raw log
bodies* are the high-risk surface, and after this change they are read only from
`flux-system`. The residual exposure (status/event messages, and the flux-system log
bodies themselves) is the **redaction** problem, tracked by **R19**. This change does
the genuine RBAC-side mitigation; it does not over-claim to solve secret leakage.

## 3. Decision

1. **Split `pods/log` into a namespaced Role**, one per controller-log namespace,
   bound to the agent ServiceAccount. Default: `["flux-system"]`.
2. **Keep `pods` get/list cluster-wide** (status reads, no log bodies) in the
   ClusterRole, with a comment explaining `pod_status` needs arbitrary namespaces.
3. **Make the controller-log namespace set a value**: `rbac.controllerLogNamespaces`
   (default `["flux-system"]`), so operators who relocate Flux controllers, or who
   genuinely want `controller_logs` to read additional controller namespaces, can opt
   in explicitly — and the surface stays auditable in `helm template`.
4. Document the trust boundary + the R19 redaction follow-up in `rbac.yaml` comments,
   `values.yaml`, and `docs/design.md`.

This is the smallest change that removes the cluster-wide raw-log grant without
breaking `pod_status`/`kube_events`, and keeps the controller-log namespace explicit
and overridable.

## 4. Plan

1. `values.yaml`: add `rbac.controllerLogNamespaces: ["flux-system"]` with a comment.
2. `rbac.yaml`:
   - Remove `pods/log` from the cluster-wide rule; keep `pods` get/list (comment: needed
     cluster-wide for `pod_status`/`kube_events`; `pods/log` is scoped below).
   - Add a `range .Values.rbac.controllerLogNamespaces` block emitting a namespaced Role
     (`pods get/list`, `pods/log get`) + RoleBinding per namespace, named
     `<fullname>-controller-logs`.
3. Validate: `helm lint`; `helm template` default (assert no cluster-wide `pods/log`,
   assert a `flux-system` Role with `pods/log`); `helm template` with a custom
   `controllerLogNamespaces` example (assert a Role per namespace); empty-list edge.
4. Docs: update `docs/design.md` §9 and `docs/roadmap.md` R10 status.
5. Gate: `go build ./...` + `go test ./...` (no Go change, but the gate must be green),
   `helm lint`. Commit incrementally.

No Go code changes — the namespace is already hardcoded to `flux-system`; this is a
chart-only change. The `controllerLogNamespaces` value documents/bounds the RBAC grant;
it does not need to flow into Go (the tool already targets `flux-system`).
