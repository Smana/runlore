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
			"operationState": map[string]any{"phase": "Succeeded", "message": "boom"},
			"history": []any{
				map[string]any{"revision": "oldsha"},
				map[string]any{"revision": "newsha"},
			},
		},
	}}
	a := applicationFromUnstructured(u)
	if a.RepoURL != "https://github.com/org/repo" || a.Path != "apps/harbor" || a.Revision != "newsha" ||
		a.PrevRevision != "oldsha" || a.HealthStatus != "Degraded" || a.SyncStatus != "Synced" || a.Message != "boom" ||
		a.OperationPhase != "Succeeded" {
		t.Fatalf("unexpected application: %+v", a)
	}
}

// TestApplicationFromUnstructuredMultiSource verifies that a multi-source app
// (spec.sources[] / status.sync.revisions[], no singular spec.source) is mapped
// from its FIRST source + first revision instead of being silently dropped.
func TestApplicationFromUnstructuredMultiSource(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "multi", "namespace": "argocd"},
		"spec": map[string]any{"sources": []any{
			map[string]any{"repoURL": "https://github.com/org/manifests", "path": "apps/multi"},
			map[string]any{"repoURL": "https://github.com/org/values", "ref": "values"},
		}},
		"status": map[string]any{
			"sync":   map[string]any{"revisions": []any{"newsha", "valsha"}, "status": "Synced"},
			"health": map[string]any{"status": "Healthy"},
			"history": []any{
				map[string]any{"revisions": []any{"oldsha", "oldval"}},
				map[string]any{"revisions": []any{"newsha", "valsha"}},
			},
		},
	}}
	a := applicationFromUnstructured(u)
	if a.RepoURL != "https://github.com/org/manifests" || a.Path != "apps/multi" || a.Revision != "newsha" {
		t.Fatalf("multi-source first source/revision not mapped: %+v", a)
	}
	if a.PrevRevision != "oldsha" {
		t.Fatalf("multi-source prev revision not mapped from history[-2].revisions[0]: %+v", a)
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
