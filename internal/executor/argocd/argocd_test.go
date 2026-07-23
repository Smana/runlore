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
