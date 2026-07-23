# M3 — Argo CD executor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Argo CD users parity with Flux on the "approve" autonomy rung. Today `internal/app/serve.go:105` wires `fluxexec.New(dc)` unconditionally, so under `gitops.engine: argocd` every approved action fails with `unsupported target kind "Application"` (`internal/executor/flux/flux.go:44-46`) — the approve rung silently does nothing for Argo users. This plan adds an `internal/executor/argocd` package implementing the reversible Argo CD equivalents of the three registry ops — **suspend** = pause auto-sync (remove `spec.syncPolicy.automated`, preserving the prior value in an annotation), **resume** = restore it, **reconcile** = the `argocd.argoproj.io/refresh` annotation — selected per-engine by a new `app.BuildExecutor`, so the existing server-authoritative policy gates (reversible-only, blast-radius, kind allowlist, namespace allowlist — `internal/action/policy.go:59-101`) apply identically with **zero policy-code changes**.

**Architecture:**

- **Op names are reused, not added.** `providers.Ops` (`internal/providers/providers.go:177-181`) stays exactly `{suspend, resume, reconcile}`, each `{Reversible: true, Blast: 1}`. The op vocabulary is engine-neutral; the executor selected for the configured engine translates it. This means: the model-facing `submit_findings` op enum (registry-derived via `opEnumJSON()`, `internal/investigate/tools.go:99-107`) needs no change; `deriveSafety` (`internal/action/safety.go:19-30`) derives the same reversibility/blast for an `Application` target as for a `Kustomization`; and the approve/auto exec-boundary re-checks (`internal/action/approvals.go:110-115`, `internal/action/auto.go:111-121`) work untouched.
- **Access path matches the provider.** The read-side Argo provider talks to the Kubernetes API directly — Application CRs via a dynamic client (`internal/providers/gitops/argocd/dynamic.go:21,39-41`), never the Argo API server. The executor does the same: merge-patches on the `argoproj.io/v1alpha1 applications` GVR, mirroring `internal/executor/flux/flux.go:62-65`. No new credentials, no new config keys (the RunLore simplicity constraint).
- **Suspend must be lossless.** `spec.syncPolicy.automated` is an object (`{prune, selfHeal, allowEmpty}` flags, or `{}`); deleting it loses the prior value, which would make the registry's `Reversible: true` derivation a lie. The executor GETs the Application first, marshals the current `automated` object into the `runlore.io/paused-sync-automated` annotation, then removes `automated` in the same merge patch. Resume reads the annotation back, restores `automated`, and deletes the annotation. A manual-sync app (no `automated`) is a no-op on suspend; an app RunLore never paused is a no-op on resume (never invent a sync policy).
- **Reconcile = refresh annotation, not `.operation`.** `argocd.argoproj.io/refresh: "normal"` is consumed (removed) by the application controller — self-cleaning, the exact analogue of Flux's `reconcile.fluxcd.io/requestedAt` (`internal/executor/flux/flux.go:58`). Patching `.operation` (a declarative sync) was rejected: a direct CR write bypasses the Argo API server's in-flight-operation guard and can clobber a running sync.
- **Already engine-agnostic — verified, no changes needed:**
  - **Audit:** every execution is audited at the single `Execute` seam by `NewAuditedExecutor` (`internal/action/executor.go:39-54`); the record carries `Op` + `Target` as strings (`internal/action/auto.go:147-149`) — kind-agnostic. Task 5 adds one parity test proving an `Application` target flows through.
  - **Slack approval buttons:** the block builder renders only `a.Description` and `a.ApprovalID` (`internal/notify/slack.go:550-563`) — resource-kind-agnostic, so **no notifier change is needed**; Argo actions get Approve/Reject buttons for free.
  - **F2 unobserved-target guard:** matches on server-observed *names*, not kinds (`internal/investigate/observedresources.go:66-79,119-148`); the failure trigger seeds the `Application` workload, so trigger-subject actions pass.
  - **Kind allowlist:** `actions.allow.kinds` is empty by default = all kinds allowed (`internal/action/policy.go:71`); operators who set it must add `Application` — a docs item, not code.
- **Protected namespaces:** `builtinProtectedNamespaces` stays `{flux-system, kube-system}` (`internal/action/safety.go:13`). `argocd` is deliberately NOT added: Argo `Application` objects (the pause lever for *user* apps) conventionally live there, so protecting it would neuter the feature; the opt-in `actions.allow.namespaces` allowlist (empty = nothing) plus `rbac.actionNamespaces` bound the blast radius instead. Documented in Task 9.

**Tech Stack:** Go 1.24+, client-go `dynamic.Interface` + `types.MergePatchType`, `k8s.io/client-go/dynamic/fake` for unit tests (same as `internal/executor/flux/flux_test.go:18-29`), Helm chart RBAC (`deploy/helm/runlore/templates/rbac.yaml`), bash k3d/kind e2e (`hack/e2e-local.sh` step 11) with the scripted mock model (`hack/e2e/mock/main.go`).

**Locked decisions:**

| Decision | Choice | Why |
|---|---|---|
| Op vocabulary | Reuse `suspend`/`resume`/`reconcile`; no new `providers.Ops` entries | Registry stays the single source of truth; model schema (`opEnumJSON`), policy derivations, and both exec-boundary re-checks work unchanged. Blast stays 1 = one GitOps object (an `Application` fans out to many workloads exactly like a `Kustomization` does — same existing convention) |
| Suspend mechanism | Remove `spec.syncPolicy.automated`, save prior JSON in `runlore.io/paused-sync-automated` annotation | Works on every Argo CD version. `spec.syncPolicy.automated.enabled: false` (lossless toggle) only exists on Argo CD ≥ 3.0 — rejected for version compatibility |
| Reconcile mechanism | `argocd.argoproj.io/refresh: "normal"` annotation | Self-cleaning (controller removes it), mirrors Flux `requestedAt`; `.operation` patch rejected (bypasses in-flight-operation guard) |
| Access path | Direct CR merge-patch via dynamic client | Same path the argocd provider already reads through (`dynamic.go:39-41`); no Argo API server session/token |
| Executor selection | New `app.BuildExecutor(cfg, dc)` beside `BuildGitOps` (`internal/app/gitops.go:22-37`) | One engine switch, mirrored; `serve.go` stays a one-liner |
| Suspend GET-then-PATCH race | Accepted (non-transactional) | Approve rung is human-clicked, low concurrency; the Flux executor's blind patch has the same exposure. Documented in the package comment |
| `argocd` namespace | NOT builtin-protected | See Architecture; allowlist is opt-in and empty-by-default anyway |

---

### Task 1: Engine-neutral op surface + Application policy-parity test

**Files:**
- Modify: `internal/providers/providers.go` (registry comment, lines 172-176)
- Modify: `internal/investigate/tools.go` (op description string, line 87)
- Modify: `internal/app/action.go` (log line, line 63)
- Test: `internal/action/policy_test.go` (append)

- [ ] **Step 1: Write the parity characterization test**

  Append to `internal/action/policy_test.go` (package `action`; `config` and `providers` are already imported there):

  ```go
  // TestReviewArgoApplicationParity locks in M3's central invariant: an
  // Application-targeted registry op passes the SAME server-authoritative
  // envelope as a Flux target — reversibility/blast derived from providers.Ops
  // (never the model's fields), no default kind restriction, namespace
  // allowlisted — with NO engine- or kind-specific branches in the gate.
  func TestReviewArgoApplicationParity(t *testing.T) {
  	p := New(config.ActionPolicy{Mode: config.ActionApprove, Allow: config.ActionAllow{
  		ReversibleOnly: true,
  		Namespaces:     []string{"argocd"},
  	}})
  	acts := []providers.Action{{
  		Name:        "pause-auto-sync",
  		Op:          "suspend",
  		Reversible:  false, // model-supplied lie — must be discarded
  		BlastRadius: 99,    // model-supplied lie — must be discarded
  		Target:      providers.Workload{Kind: "Application", Name: "web", Namespace: "argocd"},
  	}}
  	kept, withheld := p.Review(acts)
  	if len(withheld) != 0 || len(kept) != 1 {
  		t.Fatalf("kept=%d withheld=%v; want the Application action kept", len(kept), withheld)
  	}
  	if !kept[0].Reversible || kept[0].BlastRadius != 1 {
  		t.Fatalf("derived (reversible=%v, blast=%d); want (true, 1) from providers.Ops",
  			kept[0].Reversible, kept[0].BlastRadius)
  	}
  }
  ```

- [ ] **Step 2: Run it — expected PASS immediately (deliberate exception to fail-first)**

  ```bash
  go test ./internal/action/ -run TestReviewArgoApplicationParity -v
  ```

  Expected: `--- PASS: TestReviewArgoApplicationParity`. This is a *characterization* test: the invariant already holds because the gate is registry-derived and kind-agnostic (`policy.go:59-82`, `safety.go:19-30`). It exists to fail loudly if anyone adds engine special-casing during the rest of this plan. Every other task in this plan follows strict fail-first TDD.

- [ ] **Step 3: Make the three Flux-specific strings engine-neutral**

  In `internal/providers/providers.go`, replace the `Ops` doc comment (lines 172-176) with:

  ```go
  // Ops is the canonical registry of executable remediation operations and their
  // server-authoritative safety metadata. The action gate (internal/action) derives
  // reversibility/blast from this — never from model output — and the per-engine
  // executors (internal/executor/flux, internal/executor/argocd) run only ops
  // listed here. Op names are engine-neutral; the executor for the configured
  // gitops.engine translates them (Flux: spec.suspend / requestedAt annotation;
  // Argo CD: pause/restore spec.syncPolicy.automated / refresh annotation). One
  // entry per op is the single source of truth that keeps the gate and the
  // executors from drifting.
  ```

  In `internal/investigate/tools.go:87`, change the schema description
  `"executable op (Flux); omit for a suggestion only"` →
  `"executable GitOps op (Flux or Argo CD); omit for a suggestion only"`.

  In `internal/app/action.go:63`, change
  `log.Info("rung-2 approval-gated actions enabled (Flux suspend/resume/reconcile)")` →
  `log.Info("rung-2 approval-gated actions enabled (GitOps suspend/resume/reconcile)")`
  (the e2e grep at `hack/e2e-local.sh:346` matches on `'approval-gated actions enabled'`, so it stays green).

- [ ] **Step 4: Verify nothing broke**

  ```bash
  go test ./internal/action/ ./internal/investigate/ ./internal/app/ ./internal/providers/
  ```

  Expected: all PASS (the tools.go string is a description field; no test asserts its exact text — if one does, update it in the same commit).

- [ ] **Step 5: Commit**

  ```bash
  git add internal/providers/providers.go internal/investigate/tools.go internal/app/action.go internal/action/policy_test.go
  git commit -m "refactor(action): engine-neutral op surface + Application policy-parity test"
  ```

---

### Task 2: `internal/executor/argocd` — scaffold, validation, reconcile (refresh)

**Files:**
- Create: `internal/executor/argocd/argocd.go`
- Test: `internal/executor/argocd/argocd_test.go`

- [ ] **Step 1: Write the failing tests (new package — fails to build first)**

  Create `internal/executor/argocd/argocd_test.go`, mirroring `internal/executor/flux/flux_test.go:18-38`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package argocd

  import (
  	"context"
  	"testing"

  	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
  	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
  	"k8s.io/apimachinery/pkg/runtime"
  	"k8s.io/apimachinery/pkg/runtime/schema"
  	dynamicfake "k8s.io/client-go/dynamic/fake"

  	"github.com/Smana/runlore/internal/providers"
  )

  // app builds a minimal Application named web in argocd. automated == nil means
  // spec.syncPolicy is absent entirely (a manual-sync app); a non-nil (possibly
  // empty) map becomes spec.syncPolicy.automated. ann, when non-nil, becomes
  // metadata.annotations.
  func app(automated map[string]any, ann map[string]any) *unstructured.Unstructured {
  	meta := map[string]any{"name": "web", "namespace": "argocd"}
  	if ann != nil {
  		meta["annotations"] = ann
  	}
  	spec := map[string]any{"project": "default"}
  	if automated != nil {
  		spec["syncPolicy"] = map[string]any{"automated": automated}
  	}
  	return &unstructured.Unstructured{Object: map[string]any{
  		"apiVersion": "argoproj.io/v1alpha1",
  		"kind":       "Application",
  		"metadata":   meta,
  		"spec":       spec,
  	}}
  }

  func newClient(objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
  	gvrToListKind := map[schema.GroupVersionResource]string{applicationGVR: "ApplicationList"}
  	rs := make([]runtime.Object, len(objs))
  	for i, o := range objs {
  		rs[i] = o
  	}
  	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, rs...)
  }

  func get(t *testing.T, c *dynamicfake.FakeDynamicClient) *unstructured.Unstructured {
  	t.Helper()
  	u, err := c.Resource(applicationGVR).Namespace("argocd").Get(context.Background(), "web", metav1.GetOptions{})
  	if err != nil {
  		t.Fatal(err)
  	}
  	return u
  }

  func action(op string) providers.Action {
  	return providers.Action{Op: op, Target: providers.Workload{Kind: "Application", Name: "web", Namespace: "argocd"}}
  }

  func TestReconcileSetsRefreshAnnotation(t *testing.T) {
  	c := newClient(app(map[string]any{"prune": true}, nil))
  	if err := New(c).Execute(context.Background(), action("reconcile")); err != nil {
  		t.Fatalf("Execute: %v", err)
  	}
  	if v := get(t, c).GetAnnotations()["argocd.argoproj.io/refresh"]; v != "normal" {
  		t.Fatalf("refresh annotation = %q, want %q", v, "normal")
  	}
  }

  func TestUnsupported(t *testing.T) {
  	e := New(newClient(app(nil, nil)))
  	if err := e.Execute(context.Background(), providers.Action{Op: "delete",
  		Target: providers.Workload{Kind: "Application", Name: "web", Namespace: "argocd"}}); err == nil {
  		t.Fatal("expected error for unsupported op")
  	}
  	if err := e.Execute(context.Background(), providers.Action{Op: "suspend",
  		Target: providers.Workload{Kind: "Kustomization", Name: "web", Namespace: "argocd"}}); err == nil {
  		t.Fatal("expected error for unsupported kind")
  	}
  	if err := e.Execute(context.Background(), providers.Action{Op: "suspend",
  		Target: providers.Workload{Kind: "Application", Name: "web"}}); err == nil {
  		t.Fatal("expected error for missing namespace")
  	}
  }
  ```

- [ ] **Step 2: Run — expected FAIL (build error)**

  ```bash
  go test ./internal/executor/argocd/ -v
  ```

  Expected FAIL: build errors — `undefined: applicationGVR`, `undefined: New` (only the test file exists; the package has no production source yet).

- [ ] **Step 3: Implement the scaffold + reconcile**

  Create `internal/executor/argocd/argocd.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  // Package argocd executes safe, reversible Argo CD operations on Application
  // CRs — the Argo half of the autonomy ladder's executable rungs, mirroring
  // internal/executor/flux. Ops map as: suspend = pause auto-sync (remove
  // spec.syncPolicy.automated, preserving the prior value in an annotation so
  // resume can restore it losslessly), resume = restore it, reconcile = the
  // self-cleaning argocd.argoproj.io/refresh annotation (the analogue of Flux's
  // requestedAt). It patches the Application custom resource directly via the
  // dynamic client — the same access path the argocd GitOps provider reads
  // through (internal/providers/gitops/argocd) — never the Argo API server.
  //
  // suspend/resume are GET-then-PATCH and deliberately not transactional: a
  // concurrent syncPolicy edit in the window can be overwritten. Accepted — the
  // approve rung is human-clicked and the Flux executor's blind patch carries
  // the same exposure.
  package argocd

  import (
  	"context"
  	"encoding/json"
  	"fmt"

  	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
  	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
  	"k8s.io/apimachinery/pkg/runtime/schema"
  	"k8s.io/apimachinery/pkg/types"
  	"k8s.io/client-go/dynamic"

  	"github.com/Smana/runlore/internal/providers"
  )

  // applicationGVR is the Argo CD Application resource — the same GVR the
  // read-side provider uses (internal/providers/gitops/argocd/dynamic.go).
  var applicationGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

  // PausedPolicyAnnotation stores the JSON of spec.syncPolicy.automated at pause
  // time so resume restores the EXACT prior policy (prune/selfHeal/allowEmpty).
  // Removing automated without saving it would make the op registry's
  // Reversible=true derivation a lie.
  const PausedPolicyAnnotation = "runlore.io/paused-sync-automated"

  // refreshAnnotation asks the application controller to re-compare the app
  // against its source; the controller consumes (removes) it — self-cleaning.
  const refreshAnnotation = "argocd.argoproj.io/refresh"

  // Executor runs reversible Argo CD operations via the dynamic client.
  type Executor struct {
  	client dynamic.Interface
  }

  // New builds an Executor backed by a dynamic client.
  func New(client dynamic.Interface) *Executor { return &Executor{client: client} }

  // Execute applies the action's reversible Argo CD operation to its target.
  func (e *Executor) Execute(ctx context.Context, a providers.Action) error {
  	// providers.Ops is the canonical op allowlist shared with the action gate; an op
  	// absent there is never executed (keeps the gate and the executor from drifting).
  	if _, ok := providers.Ops[a.Op]; !ok {
  		return fmt.Errorf("unsupported op %q", a.Op)
  	}
  	if a.Target.Kind != "Application" {
  		return fmt.Errorf("unsupported target kind %q (want Application)", a.Target.Kind)
  	}
  	if a.Target.Name == "" || a.Target.Namespace == "" {
  		return fmt.Errorf("action target needs name and namespace")
  	}
  	switch a.Op {
  	case "suspend":
  		return e.pauseAutoSync(ctx, a)
  	case "resume":
  		return e.resumeAutoSync(ctx, a)
  	case "reconcile":
  		return e.patch(ctx, a, map[string]any{
  			"metadata": map[string]any{"annotations": map[string]any{refreshAnnotation: "normal"}},
  		})
  	default:
  		return fmt.Errorf("unsupported op %q (want suspend, resume, or reconcile)", a.Op)
  	}
  }

  // pauseAutoSync and resumeAutoSync are implemented in Tasks 3-4; stub them for
  // this task so the package compiles:
  func (e *Executor) pauseAutoSync(ctx context.Context, a providers.Action) error {
  	return fmt.Errorf("not implemented")
  }
  func (e *Executor) resumeAutoSync(ctx context.Context, a providers.Action) error {
  	return fmt.Errorf("not implemented")
  }

  // get fetches the target Application (needed by pause/resume to read the
  // current sync policy / saved annotation before patching).
  func (e *Executor) get(ctx context.Context, a providers.Action) (*unstructured.Unstructured, error) {
  	u, err := e.client.Resource(applicationGVR).Namespace(a.Target.Namespace).
  		Get(ctx, a.Target.Name, metav1.GetOptions{})
  	if err != nil {
  		return nil, fmt.Errorf("argocd %s %s/%s: %w", a.Op, a.Target.Namespace, a.Target.Name, err)
  	}
  	return u, nil
  }

  // patch merge-patches the target Application. The patch is built as a map and
  // marshalled (never fmt.Sprintf) because pause embeds JSON inside a JSON
  // string value — hand-rolled escaping would be a bug factory.
  func (e *Executor) patch(ctx context.Context, a providers.Action, patch map[string]any) error {
  	b, err := json.Marshal(patch)
  	if err != nil {
  		return fmt.Errorf("marshal patch: %w", err)
  	}
  	if _, err := e.client.Resource(applicationGVR).Namespace(a.Target.Namespace).
  		Patch(ctx, a.Target.Name, types.MergePatchType, b, metav1.PatchOptions{}); err != nil {
  		return fmt.Errorf("argocd %s %s/%s: %w", a.Op, a.Target.Namespace, a.Target.Name, err)
  	}
  	return nil
  }
  ```

  (The `unstructured` import becomes used in Task 3; if the compiler flags it now, keep it referenced by `get`'s return type — it already is.)

- [ ] **Step 4: Run — expected PASS**

  ```bash
  go test ./internal/executor/argocd/ -run 'TestReconcileSetsRefreshAnnotation|TestUnsupported' -v
  ```

  Expected: both PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/executor/argocd/
  git commit -m "feat(executor): argocd executor scaffold — validation + reconcile via refresh annotation"
  ```

---

### Task 3: suspend — pause auto-sync, preserving the prior policy

**Files:**
- Modify: `internal/executor/argocd/argocd.go` (replace the `pauseAutoSync` stub)
- Test: `internal/executor/argocd/argocd_test.go` (append)

- [ ] **Step 1: Write the failing tests**

  Append to `internal/executor/argocd/argocd_test.go`:

  ```go
  func TestSuspendPausesAutoSyncAndStoresPriorPolicy(t *testing.T) {
  	c := newClient(app(map[string]any{"prune": true, "selfHeal": true}, nil))
  	if err := New(c).Execute(context.Background(), action("suspend")); err != nil {
  		t.Fatalf("Execute: %v", err)
  	}
  	u := get(t, c)
  	if _, found, _ := unstructured.NestedMap(u.Object, "spec", "syncPolicy", "automated"); found {
  		t.Fatal("spec.syncPolicy.automated still present; auto-sync not paused")
  	}
  	// json.Marshal orders map keys alphabetically, so this is deterministic.
  	if v := u.GetAnnotations()[PausedPolicyAnnotation]; v != `{"prune":true,"selfHeal":true}` {
  		t.Fatalf("saved policy annotation = %q, want the prior automated object", v)
  	}
  }

  func TestSuspendManualSyncAppIsNoop(t *testing.T) {
  	c := newClient(app(nil, nil)) // no syncPolicy at all: manual sync
  	if err := New(c).Execute(context.Background(), action("suspend")); err != nil {
  		t.Fatalf("Execute: %v", err)
  	}
  	if v, ok := get(t, c).GetAnnotations()[PausedPolicyAnnotation]; ok {
  		t.Fatalf("no-op suspend must not invent a saved policy (got %q)", v)
  	}
  }

  func TestSuspendAlreadyPausedPreservesSavedPolicy(t *testing.T) {
  	// Paused earlier: automated absent, annotation holds the real prior policy.
  	c := newClient(app(nil, map[string]any{PausedPolicyAnnotation: `{"prune":true}`}))
  	if err := New(c).Execute(context.Background(), action("suspend")); err != nil {
  		t.Fatalf("Execute: %v", err)
  	}
  	if v := get(t, c).GetAnnotations()[PausedPolicyAnnotation]; v != `{"prune":true}` {
  		t.Fatalf("double-suspend clobbered the saved policy: %q", v)
  	}
  }
  ```

- [ ] **Step 2: Run — expected FAIL**

  ```bash
  go test ./internal/executor/argocd/ -run TestSuspend -v
  ```

  Expected FAIL: all three with `Execute: not implemented` (the Task 2 stub).

- [ ] **Step 3: Implement `pauseAutoSync`**

  Replace the stub in `internal/executor/argocd/argocd.go`:

  ```go
  // pauseAutoSync removes spec.syncPolicy.automated (Argo CD's "stop deploying
  // this" lever — the analogue of Flux spec.suspend), saving the prior automated
  // object into PausedPolicyAnnotation in the SAME patch so resume can restore it
  // exactly. An app with no automated policy (manual sync, or already paused) is
  // a no-op — idempotent like re-suspending in Flux, and it never clobbers a
  // previously saved policy.
  func (e *Executor) pauseAutoSync(ctx context.Context, a providers.Action) error {
  	u, err := e.get(ctx, a)
  	if err != nil {
  		return err
  	}
  	automated, found, _ := unstructured.NestedMap(u.Object, "spec", "syncPolicy", "automated")
  	if !found {
  		return nil // manual-sync or already paused: nothing to pause
  	}
  	saved, err := json.Marshal(automated)
  	if err != nil {
  		return fmt.Errorf("marshal prior sync policy: %w", err)
  	}
  	return e.patch(ctx, a, map[string]any{
  		"metadata": map[string]any{"annotations": map[string]any{PausedPolicyAnnotation: string(saved)}},
  		"spec":     map[string]any{"syncPolicy": map[string]any{"automated": nil}}, // merge-patch null deletes the key
  	})
  }
  ```

- [ ] **Step 4: Run — expected PASS**

  ```bash
  go test ./internal/executor/argocd/ -v
  ```

  Expected: all tests PASS (including Task 2's).

- [ ] **Step 5: Commit**

  ```bash
  git add internal/executor/argocd/
  git commit -m "feat(executor): argocd suspend pauses auto-sync preserving the prior sync policy"
  ```

---

### Task 4: resume — restore the saved sync policy

**Files:**
- Modify: `internal/executor/argocd/argocd.go` (replace the `resumeAutoSync` stub)
- Test: `internal/executor/argocd/argocd_test.go` (append)

- [ ] **Step 1: Write the failing tests**

  Append to `internal/executor/argocd/argocd_test.go`:

  ```go
  func TestResumeRestoresSavedPolicyAndClearsAnnotation(t *testing.T) {
  	c := newClient(app(nil, map[string]any{PausedPolicyAnnotation: `{"prune":true,"selfHeal":true}`}))
  	if err := New(c).Execute(context.Background(), action("resume")); err != nil {
  		t.Fatalf("Execute: %v", err)
  	}
  	u := get(t, c)
  	automated, found, _ := unstructured.NestedMap(u.Object, "spec", "syncPolicy", "automated")
  	if !found {
  		t.Fatal("spec.syncPolicy.automated not restored")
  	}
  	if automated["prune"] != true || automated["selfHeal"] != true {
  		t.Fatalf("restored policy = %v, want prune+selfHeal true", automated)
  	}
  	if _, ok := u.GetAnnotations()[PausedPolicyAnnotation]; ok {
  		t.Fatal("saved-policy annotation not removed after resume")
  	}
  }

  func TestResumeRestoresEmptyAutomatedObject(t *testing.T) {
  	// automated: {} is valid Argo config (auto-sync with default flags) and must
  	// round-trip distinctly from "no automated at all".
  	c := newClient(app(nil, map[string]any{PausedPolicyAnnotation: `{}`}))
  	if err := New(c).Execute(context.Background(), action("resume")); err != nil {
  		t.Fatalf("Execute: %v", err)
  	}
  	if _, found, _ := unstructured.NestedMap(get(t, c).Object, "spec", "syncPolicy", "automated"); !found {
  		t.Fatal("empty automated object not restored")
  	}
  }

  func TestResumeWithoutPriorPauseIsNoop(t *testing.T) {
  	c := newClient(app(nil, nil)) // RunLore never paused this app
  	if err := New(c).Execute(context.Background(), action("resume")); err != nil {
  		t.Fatalf("Execute: %v", err)
  	}
  	if _, found, _ := unstructured.NestedMap(get(t, c).Object, "spec", "syncPolicy", "automated"); found {
  		t.Fatal("resume invented an auto-sync policy on an app RunLore never paused")
  	}
  }

  func TestResumeCorruptSavedPolicyErrors(t *testing.T) {
  	c := newClient(app(nil, map[string]any{PausedPolicyAnnotation: `not-json`}))
  	if err := New(c).Execute(context.Background(), action("resume")); err == nil {
  		t.Fatal("expected error for unreadable saved policy (must not guess a policy)")
  	}
  }
  ```

- [ ] **Step 2: Run — expected FAIL**

  ```bash
  go test ./internal/executor/argocd/ -run TestResume -v
  ```

  Expected FAIL: all four with `Execute: not implemented`.

- [ ] **Step 3: Implement `resumeAutoSync`**

  Replace the stub:

  ```go
  // resumeAutoSync restores spec.syncPolicy.automated from PausedPolicyAnnotation
  // and removes the annotation in the same patch. An app with no saved policy is
  // a no-op: RunLore never paused it, and inventing an auto-sync policy an
  // operator never configured would be a mutation outside the op's contract. A
  // saved policy that no longer parses is an ERROR, not a guess.
  func (e *Executor) resumeAutoSync(ctx context.Context, a providers.Action) error {
  	u, err := e.get(ctx, a)
  	if err != nil {
  		return err
  	}
  	saved, ok := u.GetAnnotations()[PausedPolicyAnnotation]
  	if !ok {
  		return nil // never paused by RunLore: nothing to restore
  	}
  	var automated map[string]any
  	if err := json.Unmarshal([]byte(saved), &automated); err != nil {
  		return fmt.Errorf("argocd resume %s/%s: saved sync policy unreadable: %w",
  			a.Target.Namespace, a.Target.Name, err)
  	}
  	if automated == nil {
  		automated = map[string]any{} // JSON "null" round-trips to the empty automated object
  	}
  	return e.patch(ctx, a, map[string]any{
  		"metadata": map[string]any{"annotations": map[string]any{PausedPolicyAnnotation: nil}},
  		"spec":     map[string]any{"syncPolicy": map[string]any{"automated": automated}},
  	})
  }
  ```

- [ ] **Step 4: Run — expected PASS**

  ```bash
  go test ./internal/executor/argocd/ -v
  ```

  Expected: all tests in the package PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/executor/argocd/
  git commit -m "feat(executor): argocd resume restores the saved sync policy"
  ```

---

### Task 5: audited-execution parity test (audit entries for Argo targets)

The audit seam is already engine-agnostic — `NewAuditedExecutor` wraps any `action.Executor` and records `Op` + `Target` strings (`internal/action/executor.go:39-54`, record shape `internal/action/auto.go:147-149`, target format `internal/action/auto.go:170-172`). This task proves it composes with the new executor and that an `Application` execution lands in the audit trail exactly like a Flux one — no production change expected.

**Files:**
- Test: `internal/executor/argocd/audit_test.go` (create — `internal/action` does not import `internal/executor/*`, so no import cycle)

- [ ] **Step 1: Write the test**

  Create `internal/executor/argocd/audit_test.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package argocd

  import (
  	"context"
  	"testing"

  	"github.com/Smana/runlore/internal/action"
  	"github.com/Smana/runlore/internal/audit"
  )

  // recorder captures audit records in memory.
  type recorder struct{ records []audit.Record }

  func (r *recorder) Log(rec audit.Record) error { r.records = append(r.records, rec); return nil }

  // TestAuditedExecutionRecordsApplicationTarget proves the audit seam
  // (action.NewAuditedExecutor) is engine-agnostic in practice: an executed
  // Argo CD pause is recorded with the approver actor and the Application
  // target, byte-identical in shape to a Flux record.
  func TestAuditedExecutionRecordsApplicationTarget(t *testing.T) {
  	c := newClient(app(map[string]any{"prune": true}, nil))
  	rec := &recorder{}
  	exec := action.NewAuditedExecutor(New(c), rec)
  	ctx := action.ContextWithActor(context.Background(), "approve:slack:U_TEST")
  	if err := exec.Execute(ctx, action2("suspend")); err != nil {
  		t.Fatalf("Execute: %v", err)
  	}
  	if len(rec.records) != 1 {
  		t.Fatalf("audit records = %d, want 1", len(rec.records))
  	}
  	r := rec.records[0]
  	if r.Actor != "approve:slack:U_TEST" || r.Op != "suspend" ||
  		r.Target != "Application/argocd/web" || r.Decision != audit.DecisionExecuted {
  		t.Fatalf("record = %+v, want executed suspend on Application/argocd/web by the approver", r)
  	}
  }
  ```

  Note: the test file lives in package `argocd`, where `action(op string)` (Task 2 helper) collides with the imported `action` package — rename the Task 2 helper to `action2` (or `mkAction`) across `argocd_test.go` in this step, or alias the import (`act "github.com/Smana/runlore/internal/action"`). Pick ONE and apply consistently; the snippet above assumes the helper was renamed to `action2`.

- [ ] **Step 2: Run — expected FAIL first iff the recorder shape is wrong, else PASS**

  ```bash
  go test ./internal/executor/argocd/ -run TestAuditedExecutionRecordsApplicationTarget -v
  ```

  This is a wiring-verification test: expected PASS once it compiles (the seam already exists; verify `audit.Auditor`'s method set against `internal/audit/audit.go` if the compile fails — the `recorder` must satisfy it exactly).

- [ ] **Step 3: Commit**

  ```bash
  git add internal/executor/argocd/
  git commit -m "test(executor): audited-execution parity for argocd Application targets"
  ```

---

### Task 6: engine-aware executor selection

**Files:**
- Modify: `internal/app/gitops.go` (add `BuildExecutor` beside `BuildGitOps`, lines 22-37)
- Modify: `internal/app/serve.go` (lines 96 and 105)
- Test: `internal/app/gitops_test.go` (create — no such file exists today)

- [ ] **Step 1: Write the failing test**

  Create `internal/app/gitops_test.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package app

  import (
  	"testing"

  	dynamicfake "k8s.io/client-go/dynamic/fake"
  	"k8s.io/apimachinery/pkg/runtime"

  	"github.com/Smana/runlore/internal/config"
  	argoexec "github.com/Smana/runlore/internal/executor/argocd"
  	fluxexec "github.com/Smana/runlore/internal/executor/flux"
  )

  // TestBuildExecutorFollowsGitopsEngine pins the M3 wiring: the action executor
  // must track gitops.engine exactly as BuildGitOps does, or approve-rung actions
  // fail with "unsupported target kind" on one of the engines.
  func TestBuildExecutorFollowsGitopsEngine(t *testing.T) {
  	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())

  	cfg := &config.Config{}
  	if _, ok := BuildExecutor(cfg, dc).(*fluxexec.Executor); !ok {
  		t.Fatalf("default engine: got %T, want *flux.Executor", BuildExecutor(cfg, dc))
  	}

  	cfg.GitOps.Engine = "argocd"
  	if _, ok := BuildExecutor(cfg, dc).(*argoexec.Executor); !ok {
  		t.Fatalf("argocd engine: got %T, want *argocd.Executor", BuildExecutor(cfg, dc))
  	}
  }
  ```

- [ ] **Step 2: Run — expected FAIL**

  ```bash
  go test ./internal/app/ -run TestBuildExecutorFollowsGitopsEngine -v
  ```

  Expected FAIL: build error `undefined: BuildExecutor`.

- [ ] **Step 3: Implement `BuildExecutor` and rewire `serve.go`**

  In `internal/app/gitops.go`, add (imports: `"github.com/Smana/runlore/internal/action"`, `argoexec "github.com/Smana/runlore/internal/executor/argocd"`, `fluxexec "github.com/Smana/runlore/internal/executor/flux"`):

  ```go
  // BuildExecutor returns the rung-2/3 action executor for the configured GitOps
  // engine — the same engine switch as BuildGitOps, so the executor always
  // matches the provider that proposed the target (an Argo Application action
  // must never reach the Flux executor and vice versa).
  func BuildExecutor(cfg *config.Config, dc dynamic.Interface) action.Executor {
  	if GitopsEngine(cfg) == "argocd" {
  		return argoexec.New(dc)
  	}
  	return fluxexec.New(dc)
  }
  ```

  In `internal/app/serve.go`:
  - line 96: comment `// rung-2 action executor (Flux), when a cluster is reachable` → `// rung-2/3 action executor for the configured GitOps engine, when a cluster is reachable`
  - line 105: `executor = fluxexec.New(dc)` → `executor = BuildExecutor(cfg, dc)`
  - remove the now-unused `fluxexec` import at serve.go:25 (`goimports` will do it).

- [ ] **Step 4: Run — expected PASS + full build**

  ```bash
  go test ./internal/app/ -run TestBuildExecutorFollowsGitopsEngine -v && go build ./... && go vet ./...
  ```

  Expected: PASS, clean build, clean vet.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/app/gitops.go internal/app/gitops_test.go internal/app/serve.go
  git commit -m "feat(app): select the rung-2/3 action executor by gitops engine"
  ```

---

### Task 7: chart RBAC — Application get/patch in the namespaced actions Role

**Files:**
- Modify: `deploy/helm/runlore/templates/rbac.yaml` (actions Role, lines 130-149)
- Modify: `deploy/helm/runlore/values.yaml` (comments, lines 366-372)

- [ ] **Step 1: Baseline — prove the grant is missing today**

  ```bash
  helm template runlore deploy/helm/runlore --set rbac.allowActions=true \
    --set 'rbac.actionNamespaces={apps}' \
    | yq eval-all 'select(.kind == "Role" and (.metadata.name | test("-actions$"))) | .rules[].apiGroups[]' -
  ```

  Expected output (FAIL state — no `argoproj.io`):

  ```
  kustomize.toolkit.fluxcd.io
  helm.toolkit.fluxcd.io
  ```

- [ ] **Step 2: Add the rule**

  In `deploy/helm/runlore/templates/rbac.yaml`, update the actions-Role header comment (line 133) from `patch Flux resources in THIS` to `patch GitOps resources (Flux Kustomizations/HelmReleases, Argo CD Applications) in THIS`, and append after the `helmreleases` rule (line 149):

  ```yaml
    # Argo CD executor (config.gitops.engine: argocd): pause/resume auto-sync and
    # refresh are merge patches on the Application; pause/resume also GET the app
    # first to read/restore the prior sync policy, so the Role is self-contained.
    - apiGroups: ["argoproj.io"]
      resources: ["applications"]
      verbs: ["get", "patch"]
  ```

  In `deploy/helm/runlore/values.yaml`:
  - line 366: `# allowActions grants patch on Flux resources for rung-2 execution` → `# allowActions grants patch on GitOps resources (Flux Kustomizations/HelmReleases, Argo CD Applications) for rung-2 execution`
  - line 369: `# Namespaces where the agent may patch Flux resources (suspend/resume/reconcile).` → `# Namespaces where the agent may patch GitOps resources (suspend/resume/reconcile; for Argo CD this is where your Application objects live, e.g. argocd).`

- [ ] **Step 3: Verify**

  Re-run the Step 1 command. Expected output now includes `argoproj.io`:

  ```
  kustomize.toolkit.fluxcd.io
  helm.toolkit.fluxcd.io
  argoproj.io
  ```

  Also run `helm lint deploy/helm/runlore` — expected `1 chart(s) linted, 0 chart(s) failed`.

- [ ] **Step 4: Commit**

  ```bash
  git add deploy/helm/runlore/templates/rbac.yaml deploy/helm/runlore/values.yaml
  git commit -m "feat(chart): grant Application get/patch in the namespaced actions Role"
  ```

---

### Task 8: k3d e2e — drive the approve rung against an Argo CD Application

**Files:**
- Modify: `hack/e2e/mock/main.go` (the `submit_findings` case, lines 126-140)
- Modify: `hack/e2e-local.sh` (step 11, lines 723-760; `hack/e2e-k3d.sh` is a thin wrapper and needs no change)

- [ ] **Step 1: Teach the mock model to propose an Application action for the Argo incident**

  In `hack/e2e/mock/main.go`, replace the `default:` case of the `switch toolResults` (line 138-139) with:

  ```go
  	default:
  		// The action targets whichever GitOps object triggered THIS incident: the
  		// argocd e2e phase names Application/broken-argo in the incident text, the
  		// flux phases name broken-app. Sniffing the request body keeps the mock
  		// stateless across both engine phases.
  		actionJSON := `{"description":"suspend the failing Kustomization to stop the reconcile loop","op":"suspend","reversible":true,"blast_radius":1,"target":{"kind":"Kustomization","name":"broken-app","namespace":"apps"}}`
  		if strings.Contains(string(body), "broken-argo") {
  			actionJSON = `{"description":"pause auto-sync on the degraded Application to stop the flapping rollout","op":"suspend","reversible":true,"blast_radius":1,"target":{"kind":"Application","name":"broken-argo","namespace":"apps"}}`
  		}
  		name, args = "submit_findings", `{"confidence":0.9,"root_causes":[{"summary":"mock: chart bump broke harbor-db","confidence":0.9,"evidence":["pg_up=0"],"suggested_action":"flux rollback hr/harbor","reversible":true}],"unresolved":["mock unresolved"],"actions":[`+actionJSON+`]}`
  	}
  ```

  Compile check:

  ```bash
  go vet -tags e2e ./hack/e2e/mock/
  ```

  Expected: clean (`strings` is already imported at main.go:20).

- [ ] **Step 2: Extend e2e step 11**

  In `hack/e2e-local.sh`:

  (a) The step-11 `helm upgrade` (lines 744-745) switches back to the approve rung — M3's target — by adding one flag:

  ```bash
  helm upgrade runlore deploy/helm/runlore -n "$NS" --reuse-values \
    --set replicaCount=1 --set-string config.gitops.engine=argocd \
    --set-string config.actions.mode=approve >/dev/null
  ```

  (b) The `broken-argo` manifest (lines 749-754) gains an auto-sync policy to pause:

  ```bash
  kubectl apply -f - <<'YAML'
  apiVersion: argoproj.io/v1alpha1
  kind: Application
  metadata: { name: broken-argo, namespace: apps }
  spec:
    source: { repoURL: "https://github.com/org/repo", path: apps }
    syncPolicy: { automated: { prune: true, selfHeal: true } }
  YAML
  ```

  (c) After the two existing argocd checks (line 759-760), append:

  ```bash
  # Rung 2 parity (M3): the mock proposed pausing auto-sync on the Application;
  # approve it via the token endpoint and verify the executor (a) removed
  # spec.syncPolicy.automated and (b) preserved the prior policy in the
  # runlore.io/paused-sync-automated annotation — the reversibility contract.
  check "argocd action queued for approval" /tmp/runlore.log 'actions registered for approval'
  APORT=18092; free_port "$APORT"
  kubectl -n "$NS" port-forward svc/runlore "$APORT:8080" >/tmp/runlore-argo-pf.log 2>&1 &
  APF=$!; sleep 3
  AID=$(curl -s -H "X-Approval-Token: e2e-secret" "localhost:$APORT/actions" | first_action_id) || true
  curl -s -o /dev/null -w "argo approve HTTP %{http_code}\n" -X POST \
    -H "X-Approval-Token: e2e-secret" "localhost:$APORT/actions/$AID/approve" || true
  kill "$APF" 2>/dev/null || true; free_port "$APORT"
  sleep 3
  AUTOMATED=$(kubectl get application broken-argo -n apps -o jsonpath='{.spec.syncPolicy.automated}' 2>/dev/null || true)
  SAVED=$(kubectl get application broken-argo -n apps -o jsonpath='{.metadata.annotations.runlore\.io/paused-sync-automated}' 2>/dev/null || true)
  if [[ -z "$AUTOMATED" && "$SAVED" == *'"prune":true'* ]]; then
    green "PASS: approved argocd action paused auto-sync and preserved the prior policy ($SAVED)"; PASS=$((PASS+1))
  else
    red "FAIL: argocd pause (automated='$AUTOMATED' saved='$SAVED'; want automated removed + policy saved)"; FAIL=$((FAIL+1))
  fi
  kubectl -n "$NS" logs deploy/runlore > /tmp/runlore.log 2>&1
  check "argocd execution audit-logged" /tmp/runlore.log 'action approved and executed'
  ```

  Notes for the implementer: `first_action_id` is defined earlier in the script (lines 527-529) and is in scope here; `config.actions.allow.namespaces={apps}` and `rbac.actionNamespaces={apps}` were set at install (lines 287-288) and `broken-argo` lives in `apps`, so no allowlist change is needed; the F2 observed-target guard passes because the failure trigger seeds `Application/broken-argo` as observed (`internal/investigate/observedresources.go:38-44`).

- [ ] **Step 3: Syntax-check the script**

  ```bash
  bash -n hack/e2e-local.sh && shellcheck hack/e2e-local.sh || true
  ```

  Expected: `bash -n` silent; triage any NEW shellcheck findings in the added block only.

- [ ] **Step 4: Run the suite (requires docker + k3d; ~10 min)**

  ```bash
  hack/e2e-k3d.sh
  ```

  Expected: `ALL FEATURES VERIFIED` with the two new PASS lines (`approved argocd action paused auto-sync…`, `argocd execution audit-logged`) and every pre-existing check still green (the flux phases run first and must be unaffected by the mock change — the body-sniff only fires on `broken-argo`). If no docker is available locally, state so and rely on the e2e-k3d CI workflow on the PR.

- [ ] **Step 5: Commit**

  ```bash
  git add hack/e2e/mock/main.go hack/e2e-local.sh
  git commit -m "test(e2e): drive the approve rung against an Argo CD Application"
  ```

---

### Task 9: docs — README status + configuration.md per-engine op semantics

**Files:**
- Modify: `README.md` (project-status bullet, lines 241-242)
- Modify: `docs/configuration.md` (actions section, after the `allow` bullet at lines 241-243)

- [ ] **Step 1: README project-status bullet**

  Replace lines 241-242 of `README.md`:

  ```markdown
  - **Argo CD is now end-to-end tested**, alongside Flux: the k3d suite reconfigures to the `argocd`
    engine and drives an `Application Degraded` failure through a full investigation.
  ```

  with:

  ```markdown
  - **Argo CD is now end-to-end tested**, alongside Flux — including the **`approve` rung**: the k3d
    suite reconfigures to the `argocd` engine, drives an `Application Degraded` failure through a full
    investigation, then human-approves a **pause-auto-sync** action that executes reversibly (the prior
    `syncPolicy.automated` is preserved for resume). Both engines share the same reversible-only,
    allowlisted action envelope.
  ```

- [ ] **Step 2: configuration.md — per-engine op semantics**

  In `docs/configuration.md`, insert a new bullet directly after the `allow` bullet (after line 243), before `require_approval`:

  ```markdown
  - **Per-engine op semantics** — the executable ops (`suspend` / `resume` / `reconcile`) are
    engine-neutral names; the executor for the configured `gitops.engine` translates them:

    | op | Flux (`Kustomization` / `HelmRelease`) | Argo CD (`Application`) |
    |---|---|---|
    | `suspend` | sets `spec.suspend: true` | removes `spec.syncPolicy.automated` (pauses auto-sync); the prior value is preserved in the `runlore.io/paused-sync-automated` annotation |
    | `resume` | sets `spec.suspend: false` | restores `spec.syncPolicy.automated` from that annotation (no-op if RunLore didn't pause it) |
    | `reconcile` | `reconcile.fluxcd.io/requestedAt` annotation | `argocd.argoproj.io/refresh: normal` annotation |

    All three are reversible with blast radius 1 in the server-authoritative op registry, so the same
    policy envelope gates both engines identically. **Argo CD notes:** `Application` objects usually
    live in the `argocd` namespace — add it (or your apps-in-any-namespace app namespaces) to
    `actions.allow.namespaces` **and** the chart's `rbac.actionNamespaces`. If you set `allow.kinds`,
    include `Application`. `argocd` is deliberately **not** a built-in protected namespace (unlike
    `flux-system`): it is where the reversible pause lever lives; the empty-by-default namespace
    allowlist is what bounds it.
  ```

- [ ] **Step 3: Sanity-check rendering and cross-references**

  ```bash
  grep -n "paused-sync-automated" README.md docs/configuration.md internal/executor/argocd/argocd.go deploy/helm/runlore/values.yaml
  ```

  Expected: the annotation name is byte-identical everywhere it appears.

- [ ] **Step 4: Commit**

  ```bash
  git add README.md docs/configuration.md
  git commit -m "docs: document Argo CD approve-rung parity and per-engine op semantics"
  ```

---

### Final verification

- [ ] Full test suite + vet + build:

  ```bash
  go build ./... && go vet ./... && go test ./...
  ```

  Expected: everything green.

- [ ] `helm lint deploy/helm/runlore` — clean.
- [ ] Do NOT tag, release, or merge — per the roadmap staging pattern the branch/PR is handed to the maintainer for review.

---

## Acceptance criteria

- [ ] `internal/executor/argocd` exists, mirrors the flux executor's shape (dynamic client, `providers.Ops`-gated, merge patches), and only accepts `Application` targets with name + namespace.
- [ ] `suspend` on an auto-synced Application removes `spec.syncPolicy.automated` AND stores the prior object in the `runlore.io/paused-sync-automated` annotation in the same patch; manual-sync and already-paused apps are safe no-ops that never clobber a saved policy.
- [ ] `resume` restores the exact saved policy (including the empty `{}` object), removes the annotation, no-ops when RunLore never paused the app, and errors (not guesses) on a corrupt saved policy.
- [ ] `reconcile` sets `argocd.argoproj.io/refresh: normal` — the self-cleaning analogue of Flux `requestedAt`.
- [ ] `providers.Ops` gained NO new entries; the policy gate (`internal/action/policy.go`) gained NO engine/kind branches; `TestReviewArgoApplicationParity` proves an Application target flows through the reversible-only + blast-radius + kind + namespace gates unchanged.
- [ ] `app.BuildExecutor` selects the executor by `gitops.engine`, `serve.go:105` uses it, and a unit test pins both branches.
- [ ] Executed Argo actions are audited at the `NewAuditedExecutor` seam with the `Application/<ns>/<name>` target (unit test) — no audit-code changes.
- [ ] Slack approve/reject buttons required NO changes (kind-agnostic block builder, `internal/notify/slack.go:550-563`) — verified, documented in this plan.
- [ ] Chart's namespaced actions Role grants `argoproj.io/applications` `get, patch`; `helm template` proves it.
- [ ] k3d e2e step 11 approves a pause-auto-sync action on `broken-argo` and asserts (a) `automated` removed, (b) prior policy saved in the annotation, (c) the execution is audit-logged — all pre-existing flux checks still pass.
- [ ] README status bullet + `docs/configuration.md` actions section document the parity, the per-engine op table, and the Argo namespace/kinds allowlist guidance.
- [ ] No new required config keys (simplicity constraint); no commit carries a Co-Authored-By line.
