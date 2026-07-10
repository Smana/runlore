// SPDX-License-Identifier: Apache-2.0

package flux

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

func newClient() *dynamicfake.FakeDynamicClient {
	gvrToListKind := map[schema.GroupVersionResource]string{
		gvrByKind["Kustomization"]: "KustomizationList",
	}
	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "apps", "namespace": "flux-system"},
		"spec":       map[string]any{"suspend": false},
	}}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, ks)
}

func get(t *testing.T, c *dynamicfake.FakeDynamicClient) *unstructured.Unstructured {
	t.Helper()
	u, err := c.Resource(gvrByKind["Kustomization"]).Namespace("flux-system").Get(context.Background(), "apps", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSuspend(t *testing.T) {
	c := newClient()
	e := New(c)
	a := providers.Action{Op: "suspend", Target: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}}
	if err := e.Execute(context.Background(), a); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if s, _, _ := unstructured.NestedBool(get(t, c).Object, "spec", "suspend"); !s {
		t.Fatal("spec.suspend not set to true")
	}
}

func TestReconcile(t *testing.T) {
	c := newClient()
	e := New(c)
	e.now = func() string { return "2026-06-20T00:00:00Z" }
	a := providers.Action{Op: "reconcile", Target: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}}
	if err := e.Execute(context.Background(), a); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ann, _, _ := unstructured.NestedStringMap(get(t, c).Object, "metadata", "annotations")
	if ann["reconcile.fluxcd.io/requestedAt"] != "2026-06-20T00:00:00Z" {
		t.Fatalf("reconcile annotation not set: %v", ann)
	}
}

func TestUnsupported(t *testing.T) {
	e := New(newClient())
	if err := e.Execute(context.Background(), providers.Action{Op: "delete", Target: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}}); err == nil {
		t.Fatal("expected error for unsupported op")
	}
	if err := e.Execute(context.Background(), providers.Action{Op: "suspend", Target: providers.Workload{Kind: "Pod", Name: "x", Namespace: "y"}}); err == nil {
		t.Fatal("expected error for unsupported kind")
	}
}
