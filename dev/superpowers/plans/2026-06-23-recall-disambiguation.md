# Recall Gate 2 Disambiguation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make instant-recall's structural gate tell apart different workloads in the same namespace, so a curated entry recalls only for the workload it describes.

**Architecture:** Three coordinated changes in `internal/investigate`: (1) fix `resourceAgrees` so two distinct named workloads no longer match at namespace strength; (2) derive `Workload.Name`/`Kind` from alert labels in `FromIncident` (read side); (3) record the investigation's discovered `affected_resource` via `submit_findings` and prefer it over the originating alert workload (write side).

**Tech Stack:** Go 1.26, stdlib `testing` (no testify). Tests follow `internal/investigate/recall_test.go`, `investigate_test.go`, `tools_test.go`, `loop_test.go`.

**Spec:** `dev/superpowers/specs/2026-06-23-recall-disambiguation-design.md`

**Branch:** `feat/recall-disambiguation` (already checked out; the spec is already committed there).

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/investigate/recall.go` | recall gate | Fix `resourceAgrees` namespace-strength branch |
| `internal/investigate/investigate.go` | trigger→request | `workloadFromLabels` helper; use in `FromIncident` |
| `internal/investigate/tools.go` | submit_findings | `affected_resource` schema + `findings` field + set `inv.Resource` in `parseFindings` |
| `internal/investigate/loop.go` | ReAct loop | `preferDiscoveredResource` helper; replace the unconditional `inv.Resource = req.Workload` |
| `*_test.go` | tests | one per change |

Task order: T1 (match), T2 (read), T3 (write-parse), T4 (loop-prefer), T5 (verify). T1/T2/T3 are independent; T4 reads `inv.Resource` set by T3 but is safe in any order (zero `Resource` ⇒ falls back to the alert workload = today's behavior).

---

### Task 1: Fix `resourceAgrees` so distinct named workloads don't namespace-match

**Files:**
- Modify: `internal/investigate/recall.go` (`resourceAgrees`)
- Test: `internal/investigate/recall_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/investigate/recall_test.go`:

```go
func TestResourceAgrees(t *testing.T) {
	w := func(ns, name string) providers.Workload { return providers.Workload{Namespace: ns, Name: name} }
	cases := []struct {
		name      string
		reqW      providers.Workload
		entry     string
		requireWL bool
		want      matchStrength
	}{
		{"exact ns/name", w("apps", "payment-api"), "apps/payment-api", false, matchExact},
		{"different names same ns -> none", w("apps", "payment-api"), "apps/web", false, matchNone},
		{"named alert vs bare-ns entry -> namespace", w("apps", "payment-api"), "apps", false, matchNamespace},
		{"bare-ns alert vs named entry -> namespace", w("apps", ""), "apps/web", false, matchNamespace},
		{"both bare ns -> exact", w("apps", ""), "apps", false, matchExact},
		{"different ns -> none", w("apps", "payment-api"), "other/web", false, matchNone},
		{"empty entry -> none", w("apps", "payment-api"), "", false, matchNone},
		{"require workload + exact -> exact", w("apps", "web"), "apps/web", true, matchExact},
		{"require workload + ns-only -> none", w("apps", ""), "apps/web", true, matchNone},
	}
	for _, c := range cases {
		if got := resourceAgrees(c.reqW, c.entry, c.requireWL); got != c.want {
			t.Errorf("%s: resourceAgrees(%+v, %q, %v) = %v, want %v", c.name, c.reqW, c.entry, c.requireWL, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestResourceAgrees`
Expected: FAIL — `different names same ns -> none` gets `matchNamespace` (the old prefix behavior), want `matchNone`.

- [ ] **Step 3: Fix the namespace-strength branch**

In `internal/investigate/recall.go`, replace the namespace-strength branch of `resourceAgrees` (currently `if entryResource == reqW.Namespace || strings.HasPrefix(entryResource, reqW.Namespace+"/") { return matchNamespace }`) so it only applies when one side is a bare namespace:

```go
	// Namespace-level agreement only when one side is a bare namespace — never two
	// distinct named workloads (that would defeat disambiguation).
	if entryResource == reqW.Namespace { // entry is a bare namespace; reqW is in it
		return matchNamespace
	}
	if reqW.Name == "" && strings.HasPrefix(entryResource, reqW.Namespace+"/") { // reqW is a bare namespace; entry named in it
		return matchNamespace
	}
	return matchNone
```

(The function's earlier lines — the `entryResource == "" || reqW.Namespace == ""` guard, the `reqW.Ref() == entryResource` exact check, and the `requireWorkload` short-circuit — are unchanged.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/investigate/ -run 'TestResourceAgrees|TestLookupStructural|TestLookupNoStoredResource'`
Expected: PASS — the new table test AND the existing structural lookup tests (they use a *nameless* alert workload, which still namespace-matches).

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/recall.go internal/investigate/recall_test.go
git commit -m "fix(recall): structural gate no longer namespace-matches distinct workloads"
```

---

### Task 2: Derive `Workload.Name`/`Kind` from alert labels

**Files:**
- Modify: `internal/investigate/investigate.go` (add `workloadFromLabels`; use in `FromIncident`)
- Test: `internal/investigate/investigate_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/investigate/investigate_test.go`:

```go
func TestWorkloadFromLabels(t *testing.T) {
	cases := []struct {
		name             string
		labels           map[string]string
		wantKind, wantNm string
	}{
		{"deployment", map[string]string{"deployment": "payment-api"}, "Deployment", "payment-api"},
		{"pod only", map[string]string{"pod": "x-abc123"}, "Pod", "x-abc123"},
		{"controller beats pod", map[string]string{"deployment": "payment-api", "pod": "payment-api-abc"}, "Deployment", "payment-api"},
		{"workload with type", map[string]string{"workload": "w", "workload_type": "Rollout"}, "Rollout", "w"},
		{"none", map[string]string{"severity": "critical"}, "", ""},
	}
	for _, c := range cases {
		k, n := workloadFromLabels(c.labels)
		if k != c.wantKind || n != c.wantNm {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", c.name, k, n, c.wantKind, c.wantNm)
		}
	}
}

func TestFromIncidentDerivesWorkload(t *testing.T) {
	inc := config.Incident{AlertName: "Crash", Namespace: "apps", Labels: map[string]string{"namespace": "apps", "deployment": "payment-api"}}
	r := FromIncident(inc)
	if r.Workload.Namespace != "apps" || r.Workload.Name != "payment-api" || r.Workload.Kind != "Deployment" {
		t.Fatalf("FromIncident workload = %+v, want apps/payment-api Deployment", r.Workload)
	}
}
```

(`config` is already imported in this test file — `TestFromIncident` uses `config.Incident`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/investigate/ -run 'TestWorkloadFromLabels|TestFromIncidentDerivesWorkload'`
Expected: FAIL — compile error `undefined: workloadFromLabels`.

- [ ] **Step 3: Implement the helper and use it**

In `internal/investigate/investigate.go`, add the helper (e.g. just above `FromIncident`):

```go
// workloadFromLabels derives the affected workload (kind, name) from Alertmanager
// labels, preferring a stable controller name over an ephemeral pod name.
func workloadFromLabels(labels map[string]string) (kind, name string) {
	for _, c := range []struct{ label, kind string }{
		{"deployment", "Deployment"},
		{"statefulset", "StatefulSet"},
		{"daemonset", "DaemonSet"},
		{"replicaset", "ReplicaSet"},
		{"cronjob", "CronJob"},
		{"job", "Job"},
	} {
		if v := labels[c.label]; v != "" {
			return c.kind, v
		}
	}
	if v := labels["workload"]; v != "" {
		return labels["workload_type"], v // kind may be empty
	}
	if v := labels["pod"]; v != "" {
		return "Pod", v
	}
	return "", ""
}
```

And update `FromIncident` to derive the workload (add the `kind, name := workloadFromLabels(...)` line and use it in the `Workload` field; leave every other field as-is):

```go
// FromIncident builds a Request from a matched incident alert.
func FromIncident(inc config.Incident) Request {
	kind, name := workloadFromLabels(inc.Labels)
	return Request{
		Source:      SourceAlert,
		Title:       inc.AlertName,
		Workload:    providers.Workload{Namespace: inc.Namespace, Kind: kind, Name: name},
		Reason:      inc.Severity,
		Labels:      inc.Labels,
		At:          inc.StartsAt,
		Fingerprint: inc.Fingerprint,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/investigate/`
Expected: PASS — the two new tests plus all pre-existing tests (existing `TestFromIncident` checks only `Workload.Namespace`, so adding name/kind doesn't break it).

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/investigate.go internal/investigate/investigate_test.go
git commit -m "feat(investigate): derive workload name/kind from alert labels"
```

---

### Task 3: Record the discovered `affected_resource` (write side, parse)

**Files:**
- Modify: `internal/investigate/tools.go` (`submitFindingsSpec` schema; `findings` struct; `parseFindings`)
- Test: `internal/investigate/tools_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/investigate/tools_test.go`:

```go
func TestParseFindingsAffectedResource(t *testing.T) {
	args := `{"root_causes":[{"summary":"OOM in payment-api"}],
	  "affected_resource":{"kind":"Deployment","name":"payment-api","namespace":"apps"}}`
	inv, err := parseFindings(args)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Resource.Namespace != "apps" || inv.Resource.Name != "payment-api" || inv.Resource.Kind != "Deployment" {
		t.Fatalf("affected_resource not parsed into inv.Resource: %+v", inv.Resource)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestParseFindingsAffectedResource`
Expected: FAIL — `inv.Resource` is the zero `Workload` (parseFindings doesn't read `affected_resource` yet).

- [ ] **Step 3: Add the schema property, struct field, and parse**

In `internal/investigate/tools.go`:

(a) In `submitFindingsSpec`'s `Schema` string, add the `affected_resource` property. Insert it immediately after the `"confidence":{"type":"number"},` line (the line right after `"title"`):

```
"affected_resource":{"type":"object","description":"the workload your investigation identified as the failing/affected resource","properties":{"kind":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"}}},
```

(b) In the `findings` struct, add (after the `Confidence float64` field):

```go
	AffectedResource struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"affected_resource"`
```

(c) In `parseFindings`, before `return inv, nil`, set the discovered resource:

```go
	inv.Resource = providers.Workload{
		Kind:      f.AffectedResource.Kind,
		Name:      f.AffectedResource.Name,
		Namespace: f.AffectedResource.Namespace,
	}
```

(When `affected_resource` is absent this sets the zero `Workload`; the loop's fallback (Task 4) then uses the alert workload.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/investigate/`
Expected: PASS — the new test plus all pre-existing tests (`TestParseFindings` doesn't assert `inv.Resource`, and `TestSubmitFindingsSchemaNoEmptyEnum` still holds — no empty enum added).

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/tools.go internal/investigate/tools_test.go
git commit -m "feat(investigate): submit_findings affected_resource -> inv.Resource"
```

---

### Task 4: Prefer the discovered resource in the loop

**Files:**
- Modify: `internal/investigate/loop.go` (add `preferDiscoveredResource`; replace the `inv.Resource = req.Workload` line)
- Test: `internal/investigate/loop_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/investigate/loop_test.go`:

```go
func TestPreferDiscoveredResource(t *testing.T) {
	origin := providers.Workload{Namespace: "apps", Name: "web"}
	cases := []struct {
		name       string
		discovered providers.Workload
		want       providers.Workload
	}{
		{"discovered wins", providers.Workload{Namespace: "apps", Name: "payment-api", Kind: "Deployment"}, providers.Workload{Namespace: "apps", Name: "payment-api", Kind: "Deployment"}},
		{"namespace defaulted from origin", providers.Workload{Name: "payment-api"}, providers.Workload{Namespace: "apps", Name: "payment-api"}},
		{"empty falls back to origin", providers.Workload{}, origin},
	}
	for _, c := range cases {
		if got := preferDiscoveredResource(c.discovered, origin); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
```

(`providers` is already imported in `loop_test.go`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestPreferDiscoveredResource`
Expected: FAIL — compile error `undefined: preferDiscoveredResource`.

- [ ] **Step 3: Add the helper and use it in the loop**

In `internal/investigate/loop.go`, add the helper (e.g. near the bottom of the file, after `Investigate`):

```go
// preferDiscoveredResource keeps the workload the investigation identified,
// defaulting a missing namespace to the originating alert's, and falls back to the
// alert workload only when the model named none.
func preferDiscoveredResource(discovered, origin providers.Workload) providers.Workload {
	if discovered.Name != "" && discovered.Namespace == "" {
		discovered.Namespace = origin.Namespace
	}
	if discovered.Ref() == "" {
		return origin
	}
	return discovered
}
```

And replace the line `inv.Resource = req.Workload       // record the originating workload for structural recall` with:

```go
				// Prefer the workload the investigation identified; fall back to the
				// originating alert workload only when the model named none.
				inv.Resource = preferDiscoveredResource(inv.Resource, req.Workload)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/investigate/`
Expected: PASS — the helper test plus all pre-existing loop tests (when the model sets no `affected_resource`, `inv.Resource` is zero ⇒ `preferDiscoveredResource` returns `req.Workload` = today's behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/loop.go internal/investigate/loop_test.go
git commit -m "feat(investigate): loop prefers the discovered resource over the alert workload"
```

---

### Task 5: Whole-tree verification

- [ ] **Step 1: Build, test, vet**

Run:
```bash
go build ./... && go test ./... && go vet ./...
```
Expected: build clean; all tests PASS; vet clean.

No commit (verification only).

---

## Notes for the implementer

- The disambiguation hinges on Task 1: a named alert (`apps/payment-api`) must NOT recall a different named entry (`apps/web`). The namespace fallback is preserved only for genuinely namespace-scoped alerts/entries (one side a bare namespace).
- Do not flip the `RequireWorkloadMatch` default, touch the curator/KB frontmatter, index `resource` as a bleve field, or change decay/eval — all deferred (spec §7). The curator improves automatically because `draftKBEntry` already stores `inv.Resource.Ref()`.
- `providers.Workload` is all-string and comparable, so `got != want` works in the helper tests.
- If any pre-existing test fails due to the Task 1 semantics change, inspect it: a test that asserted a *named* alert matching a *different* named entry encoded the bug — update it with a comment. (None are expected to, since the existing structural tests use a nameless alert workload.)
