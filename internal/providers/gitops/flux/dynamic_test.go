package flux

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestDynamicReader(t *testing.T) {
	ksObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "apps", "namespace": "flux-system"},
		"spec": map[string]any{
			"path":      "./apps",
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"},
		},
		"status": map[string]any{"lastAppliedRevision": "main@sha1:abc123"},
	}}
	grObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "GitRepository",
		"metadata":   map[string]any{"name": "flux-system", "namespace": "flux-system"},
		"spec":       map[string]any{"url": "https://github.com/org/repo"},
	}}

	gvrToListKind := map[schema.GroupVersionResource]string{
		kustomizationGVR: "KustomizationList",
		gitRepositoryGVR: "GitRepositoryList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, ksObj, grObj)
	r := NewDynamicReader(client)

	ks, err := r.ListKustomizations(context.Background())
	if err != nil {
		t.Fatalf("ListKustomizations: %v", err)
	}
	if len(ks) != 1 {
		t.Fatalf("want 1 kustomization, got %d", len(ks))
	}
	got := ks[0]
	if got.Name != "apps" || got.Namespace != "flux-system" || got.Path != "./apps" ||
		got.SourceName != "flux-system" || got.SourceNamespace != "flux-system" || got.Revision != "main@sha1:abc123" {
		t.Fatalf("unexpected kustomization: %+v", got)
	}

	gr, err := r.GetGitRepository(context.Background(), "flux-system", "flux-system")
	if err != nil {
		t.Fatalf("GetGitRepository: %v", err)
	}
	if gr.URL != "https://github.com/org/repo" {
		t.Fatalf("unexpected url: %q", gr.URL)
	}
}

func TestKustomizationReadyCondition(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "apps", "namespace": "flux-system"},
		"spec":       map[string]any{"path": "./apps", "sourceRef": map[string]any{"name": "flux-system"}},
		"status": map[string]any{
			"lastAppliedRevision": "main@sha1:abc",
			"conditions": []any{
				map[string]any{"type": "Healthy", "status": "True"},
				map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed", "message": "kustomize build failed"},
			},
		},
	}}
	k := kustomizationFromUnstructured(u)
	if k.ReadyStatus != "False" || k.ReadyReason != "BuildFailed" || k.ReadyMessage != "kustomize build failed" {
		t.Fatalf("unexpected ready condition: %+v", k)
	}
}

func TestDynamicReaderWatch(t *testing.T) {
	gvrToListKind := map[schema.GroupVersionResource]string{
		kustomizationGVR: "KustomizationList",
		gitRepositoryGVR: "GitRepositoryList",
	}
	bad := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "bad", "namespace": "apps"},
		"spec":       map[string]any{"path": "./apps", "sourceRef": map[string]any{"name": "flux-system"}},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed", "message": "boom"},
		}},
	}}
	// Seed the object before starting the informer so the initial list surfaces it.
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, bad)
	r := NewDynamicReader(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := r.WatchKustomizations(ctx)
	if err != nil {
		t.Fatalf("WatchKustomizations: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Kustomization.Name != "bad" || ev.Kustomization.ReadyStatus != "False" {
			t.Fatalf("unexpected event: %+v", ev.Kustomization)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for informer event")
	}
}
