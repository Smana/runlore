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
