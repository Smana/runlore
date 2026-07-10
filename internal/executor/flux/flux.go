// SPDX-License-Identifier: Apache-2.0

// Package flux executes safe, reversible Flux operations (suspend / resume /
// reconcile) on the cluster — the executable half of the autonomy ladder. It is
// only ever invoked after explicit human approval (config.actions mode "approve").
package flux

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/Smana/runlore/internal/providers"
)

// Executor runs reversible Flux operations via the dynamic client.
type Executor struct {
	client dynamic.Interface
	now    func() string // injectable for tests
}

// New builds an Executor backed by a dynamic client.
func New(client dynamic.Interface) *Executor {
	return &Executor{client: client, now: func() string { return time.Now().UTC().Format(time.RFC3339) }}
}

var gvrByKind = map[string]schema.GroupVersionResource{
	"Kustomization": {Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"},
	"HelmRelease":   {Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"},
}

// Execute applies the action's reversible Flux operation to its target.
func (e *Executor) Execute(ctx context.Context, a providers.Action) error {
	// providers.Ops is the canonical op allowlist shared with the action gate; an op
	// absent there is never executed (keeps the gate and the executor from drifting).
	if _, ok := providers.Ops[a.Op]; !ok {
		return fmt.Errorf("unsupported op %q", a.Op)
	}
	gvr, ok := gvrByKind[a.Target.Kind]
	if !ok {
		return fmt.Errorf("unsupported target kind %q (want Kustomization or HelmRelease)", a.Target.Kind)
	}
	if a.Target.Name == "" || a.Target.Namespace == "" {
		return fmt.Errorf("action target needs name and namespace")
	}
	var patch string
	switch a.Op {
	case "suspend":
		patch = `{"spec":{"suspend":true}}`
	case "resume":
		patch = `{"spec":{"suspend":false}}`
	case "reconcile":
		patch = fmt.Sprintf(`{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":%q}}}`, e.now())
	default:
		return fmt.Errorf("unsupported op %q (want suspend, resume, or reconcile)", a.Op)
	}
	if _, err := e.client.Resource(gvr).Namespace(a.Target.Namespace).
		Patch(ctx, a.Target.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("flux %s %s/%s: %w", a.Op, a.Target.Namespace, a.Target.Name, err)
	}
	return nil
}
