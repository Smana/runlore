// SPDX-License-Identifier: Apache-2.0

package flux

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/Smana/runlore/internal/providers"
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

// fluxScene builds a fake cluster: a Kustomization "apps" depends on "infra";
// "infra" sources GitRepository "infra-artifact" — which is ABSENT (the root).
func fluxScene() *Provider {
	apps := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "apps", "namespace": "flux-system"},
		"spec": map[string]any{
			"dependsOn": []any{map[string]any{"name": "infra"}},
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"},
		},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "DependencyNotReady", "message": "dependency 'flux-system/infra' is not ready"},
		}},
	}}
	infra := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "infra", "namespace": "flux-system"},
		"spec":     map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "infra-artifact"}},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "DependencyNotReady", "message": "dependency not ready"},
		}},
	}}
	gvrToListKind := map[schema.GroupVersionResource]string{
		kustomizationGVR: "KustomizationList",
		gitRepositoryGVR: "GitRepositoryList",
		eventsGVR:        "EventList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, apps, infra)
	return New(NewDynamicReader(client), nil)
}

func TestSourceRevision(t *testing.T) {
	grObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "GitRepository",
		"metadata":   map[string]any{"name": "flux-system", "namespace": "flux-system"},
		"spec":       map[string]any{"url": "https://github.com/org/repo"},
		"status":     map[string]any{"artifact": map[string]any{"revision": "main@sha1:bbb"}},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gitRepositoryGVR: "GitRepositoryList"}, grObj)
	r := NewDynamicReader(client)

	// An empty kind defaults to GitRepository, matching Flux's own default.
	rev, err := r.SourceRevision(context.Background(), "", "flux-system", "flux-system")
	if err != nil {
		t.Fatalf("SourceRevision: %v", err)
	}
	if rev != "main@sha1:bbb" {
		t.Fatalf("unexpected revision: %q", rev)
	}

	// An unknown source kind is reported, not silently swallowed.
	if _, err := r.SourceRevision(context.Background(), "Mystery", "flux-system", "flux-system"); err == nil {
		t.Fatal("expected an error for an unsupported source kind")
	}
}

func TestGetResourceNamespaceFallback(t *testing.T) {
	// The Kustomization lives in flux-system, but a caller passes the workload's
	// namespace ("apps", from an alert). GetResource must resolve it via the
	// flux-system / all-namespaces fallback instead of returning NotFound.
	ksObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "apps", "namespace": "flux-system"},
		"spec":       map[string]any{"path": "./apps"},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{kustomizationGVR: "KustomizationList"}, ksObj)
	r := NewDynamicReader(client)

	got, err := r.GetResource(context.Background(), "Kustomization", "apps", "apps")
	if err != nil {
		t.Fatalf("expected fallback resolution, got error: %v", err)
	}
	if got.GetNamespace() != "flux-system" || got.GetName() != "apps" {
		t.Fatalf("expected flux-system/apps, got %s/%s", got.GetNamespace(), got.GetName())
	}

	// A genuinely absent object still returns NotFound.
	if _, err := r.GetResource(context.Background(), "Kustomization", "apps", "does-not-exist"); err == nil {
		t.Fatal("expected NotFound for an absent object")
	}
}

func TestResourceStatus(t *testing.T) {
	p := fluxScene()
	// A present, failing Kustomization: conditions + refs are surfaced.
	rs, err := p.ResourceStatus(context.Background(), providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"})
	if err != nil {
		t.Fatalf("ResourceStatus: %v", err)
	}
	if rs.Ready != "False" || rs.Reason != "DependencyNotReady" {
		t.Fatalf("unexpected status: %+v", rs)
	}
	if rs.Refs["dependsOn"] != "flux-system/infra" || rs.Refs["sourceRef"] != "GitRepository/flux-system/flux-system" {
		t.Fatalf("unexpected refs: %v", rs.Refs)
	}
	// A missing object: NotFound (the cascade root), not an error.
	miss, err := p.ResourceStatus(context.Background(), providers.Workload{Kind: "GitRepository", Name: "infra-artifact", Namespace: "flux-system"})
	if err != nil {
		t.Fatalf("ResourceStatus(missing): %v", err)
	}
	if !miss.NotFound {
		t.Fatalf("expected NotFound for missing GitRepository, got %+v", miss)
	}
}

func TestDependencyTree(t *testing.T) {
	p := fluxScene()
	root, err := p.DependencyTree(context.Background(), providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"})
	if err != nil {
		t.Fatalf("DependencyTree: %v", err)
	}
	// apps → (dependsOn infra, sourceRef flux-system); infra → (sourceRef infra-artifact = NOT FOUND root)
	var missing []string
	var walk func(n providers.DepNode)
	walk = func(n providers.DepNode) {
		if n.NotFound {
			missing = append(missing, n.Workload.Kind+"/"+n.Workload.Name)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	found := false
	for _, m := range missing {
		if m == "GitRepository/infra-artifact" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dependency tree did not surface the missing root GitRepository/infra-artifact; missing=%v", missing)
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
