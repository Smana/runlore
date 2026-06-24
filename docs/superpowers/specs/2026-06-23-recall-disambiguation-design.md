# RunLore Recall Disambiguation (Gate 2) — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-23 |
| **Scope** | Make instant-recall's structural gate (Gate 2) actually tell apart different workloads in the same namespace: derive the workload name from alert labels (read side), record the investigation's discovered failing resource (write side), and fix `resourceAgrees` so two distinct named workloads no longer match at namespace strength. Confined to `internal/investigate`. |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | Deep-analysis report (retired report) roadmap #2 / "Gate 2 collapses to namespace equality"; recall-trustworthiness (`2026-06-22-recall-trustworthiness-design.md`, which introduced Gate 2 / `resourceAgrees` / the `Resource` field); the recall-decay slice (`2026-06-23-recall-decay-design.md`) that sits on top of these gates |

---

## 1. Why this exists

Gate 2 (structural agreement) is the lever the recall-trustworthiness design named to separate the many-to-one symptom→cause problem (CrashLoopBackOff = OOM | bad-image | missing-secret). In practice it cannot, for two reasons:

1. **The data isn't there.** On the dominant alert path, `FromIncident` builds `Workload{Namespace: inc.Namespace}` with no `Name`, and the investigation's *discovered* failing workload is never recorded — `loop.go` overwrites `inv.Resource` with the namespace-only originating workload, and `parseFindings` has no field for an affected resource. So both the alert's workload and the curated entry's `Resource` are bare namespaces.
2. **The matching over-matches.** Even with names present, `resourceAgrees` treats a named alert `apps/payment-api` as matching a *different* named entry `apps/web` at namespace strength, because `strings.HasPrefix("apps/web", "apps/")` is true. So any two workloads in a namespace recall each other's entries.

This slice fixes all three (read, write, match) so a curated entry recalls only for the workload it is actually about — the disambiguation Gate 2 was meant to provide.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **Fix `resourceAgrees` AND plumb the data** | The data fix alone doesn't disambiguate — the matching still namespace-matches different names. Both are required for the slice to achieve its goal. |
| D2 | **Namespace-strength agreement only when one side is a *bare namespace*** | A genuinely namespace-scoped alert or entry should still recall; two *distinct named* workloads in one namespace must not. Keeps `RequireWorkloadMatch=false` default while making the gate discriminate. |
| D3 | **Read-side label precedence: controller → `workload` → `pod`** | Controller names (Deployment/StatefulSet/…) are stable and match runbooks; pod names carry ephemeral hashes (`payment-api-7d9f-xyz`) that won't. Prefer the most matchable identifier. |
| D4 | **`affected_resource` is structured `{namespace, kind, name}`** | Mirrors the existing `actions[].target` shape in the same schema; consistent and unambiguous. |
| D5 | **Discovered resource wins; fall back to the alert workload only when absent** | The model's identified failing workload is what should be recorded/curated; the originating alert workload is the fallback when the model didn't name one. |
| D6 | **Curator needs no change** | `draftKBEntry` already stores `inv.Resource.Ref()`; once `inv.Resource` carries the discovered `ns/name`, curated entries improve automatically. |

## 3. Design

### 3.1 Read side — workload name from alert labels (`investigate.go`)

Add a helper:

```go
// workloadFromLabels derives the affected workload (kind, name) from Alertmanager
// labels, preferring a stable controller name over an ephemeral pod name.
func workloadFromLabels(labels map[string]string) (kind, name string)
```

Precedence (first present wins):
1. controller labels — `deployment`→`Deployment`, `statefulset`→`StatefulSet`, `daemonset`→`DaemonSet`, `replicaset`→`ReplicaSet`, `cronjob`→`CronJob`, `job`→`Job`;
2. `workload` (kind from `workload_type` if present, else empty);
3. `pod` → `Pod`.

`FromIncident` uses it: `kind, name := workloadFromLabels(inc.Labels); Workload{Namespace: inc.Namespace, Kind: kind, Name: name}`. No matching label → namespace-only (today's behavior).

### 3.2 Write side — discovered resource (`tools.go`, `loop.go`)

Add a top-level `affected_resource` to the `submit_findings` schema:

```json
"affected_resource":{"type":"object",
  "description":"the workload your investigation identified as the failing/affected resource",
  "properties":{"kind":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"}}}
```

Add the matching field to the `findings` struct and, in `parseFindings`, set `inv.Resource = providers.Workload{Kind, Name, Namespace}` from it.

In `loop.go`, replace the unconditional `inv.Resource = req.Workload` with:

```go
// Prefer the workload the investigation identified; default a missing namespace to
// the alert's, and fall back to the originating workload only when none was named.
if inv.Resource.Name != "" && inv.Resource.Namespace == "" {
	inv.Resource.Namespace = req.Workload.Namespace
}
if inv.Resource.Ref() == "" {
	inv.Resource = req.Workload
}
```

(`recalledInvestigation` keeps `Resource: req.Workload`, now name-bearing from §3.1 — consistent.)

### 3.3 Matching fix — `resourceAgrees` (`recall.go`)

```go
func resourceAgrees(reqW providers.Workload, entryResource string, requireWorkload bool) matchStrength {
	if entryResource == "" || reqW.Namespace == "" {
		return matchNone
	}
	if reqW.Ref() == entryResource {
		return matchExact
	}
	if requireWorkload {
		return matchNone
	}
	// Namespace-level agreement only when one side is a bare namespace — never two
	// distinct named workloads (that would defeat disambiguation).
	if entryResource == reqW.Namespace { // entry is a bare namespace; reqW is in it
		return matchNamespace
	}
	if reqW.Name == "" && strings.HasPrefix(entryResource, reqW.Namespace+"/") { // reqW bare ns; entry named in it
		return matchNamespace
	}
	return matchNone
}
```

Resulting matrix (with `requireWorkload=false`):

| reqW | entry | result |
|---|---|---|
| `apps/payment-api` | `apps/payment-api` | `matchExact` |
| `apps/payment-api` | `apps/web` | **`matchNone`** (was `matchNamespace`) |
| `apps/payment-api` | `apps` (bare ns) | `matchNamespace` |
| `apps` (bare ns) | `apps/web` | `matchNamespace` |
| `apps` (bare ns) | `apps` | `matchExact` |
| `apps/payment-api` | `other/web` | `matchNone` |

## 4. Components / seams

| Change | Location |
|---|---|
| `workloadFromLabels` + use in `FromIncident` | `internal/investigate/investigate.go` |
| `resourceAgrees` namespace-strength fix | `internal/investigate/recall.go` |
| `affected_resource` schema + `findings` field + `parseFindings` | `internal/investigate/tools.go` |
| Prefer discovered resource over `req.Workload` | `internal/investigate/loop.go` |
| Tests | `investigate_test.go`, `recall_test.go`, `tools_test.go`, `loop_test.go` |

## 5. Trade-offs accepted in v1

- **Label heuristic, not ground truth** — `workloadFromLabels` derives the workload from whatever labels the alert carries; a missing/odd label set falls back to namespace-only (today's behavior, safe). The controller-over-pod precedence is a best-effort to match curated entries.
- **Read/write convergence is best-effort** — the read side derives from labels, the write side from the model's discovery; they may pick different identifiers for the same incident. A mismatch degrades to `matchNamespace` (if one is bare-ns) or `matchNone` (a missed recall → a correct full investigation). Fail-safe either way.
- **`RequireWorkloadMatch` default stays `false`** — the namespace fallback remains for genuinely namespace-scoped alerts/entries; only the over-matching of distinct names is removed.
- **Matching-semantics change** — existing recall tests that relied on the old prefix behavior are updated as part of this slice.

## 6. Testing

- **`workloadFromLabels`**: `{deployment:"payment-api"}`→`(Deployment, payment-api)`; `{pod:"x-abc"}`→`(Pod, x-abc)`; both present → controller wins; `{workload:"w","workload_type":"Rollout"}`→`(Rollout, w)`; none → `("","")`.
- **`resourceAgrees`** (the matrix in §3.3), including the key `apps/payment-api` vs `apps/web` → `matchNone`, and both bare-namespace fallbacks → `matchNamespace`.
- **`FromIncident`**: an incident with a `deployment` (or `pod`) label yields a `Workload` carrying that name + kind.
- **`parseFindings`**: `affected_resource` populates `inv.Resource`.
- **`loop`**: the model's discovered resource is preferred; a name without a namespace inherits the alert's namespace; absent → falls back to `req.Workload`.
- **Disambiguation (success metric)**: two catalog entries `apps/payment-api` and `apps/web`; an alert labeled `deployment=payment-api` (ns `apps`) recalls only the `payment-api` entry; a `web` alert does not match it.
- Existing recall/loop tests stay green (updating any that asserted the old prefix-match semantics).

## 7. Out of scope (later slices)

- Flipping the `RequireWorkloadMatch` default to `true`.
- Curator / KB-frontmatter changes (the curator benefits automatically via `inv.Resource`).
- Indexing `resource` as a filterable bleve field / widening internal `k` / cause-text indexing (roadmap #3 — the lexical-retrieval half).
- Any decay or eval change.

This slice turns Gate 2 from a namespace check into a real workload check — so a curated incident entry recalls for the workload it describes, not for every workload that happens to share its namespace.
