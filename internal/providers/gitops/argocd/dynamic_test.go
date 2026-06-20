package argocd

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestApplicationFromUnstructured(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "harbor", "namespace": "argocd"},
		"spec":       map[string]any{"source": map[string]any{"repoURL": "https://github.com/org/repo", "path": "apps/harbor"}},
		"status": map[string]any{
			"sync":           map[string]any{"revision": "newsha", "status": "Synced"},
			"health":         map[string]any{"status": "Degraded"},
			"operationState": map[string]any{"message": "boom"},
			"history": []any{
				map[string]any{"revision": "oldsha"},
				map[string]any{"revision": "newsha"},
			},
		},
	}}
	a := applicationFromUnstructured(u)
	if a.RepoURL != "https://github.com/org/repo" || a.Path != "apps/harbor" || a.Revision != "newsha" ||
		a.PrevRevision != "oldsha" || a.HealthStatus != "Degraded" || a.SyncStatus != "Synced" || a.Message != "boom" {
		t.Fatalf("unexpected application: %+v", a)
	}
}

func TestDynamicReaderWatch(t *testing.T) {
	gvrToListKind := map[schema.GroupVersionResource]string{applicationGVR: "ApplicationList"}
	degraded := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "bad", "namespace": "apps"},
		"spec":       map[string]any{"source": map[string]any{"repoURL": "u", "path": "p"}},
		"status":     map[string]any{"health": map[string]any{"status": "Degraded"}},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, degraded)
	r := NewDynamicReader(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := r.WatchApplications(ctx)
	if err != nil {
		t.Fatalf("WatchApplications: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Application.Name != "bad" || ev.Application.HealthStatus != "Degraded" {
			t.Fatalf("unexpected event: %+v", ev.Application)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for informer event")
	}
}
