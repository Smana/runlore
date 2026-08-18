// SPDX-License-Identifier: Apache-2.0

package flux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

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

// TestListEventsRendering proves G2: ListEvents leads each line with the event's
// lastTimestamp (RFC3339, UTC) and appends "(xN)" when count>1 — mirroring
// kube_events — while omitting both when the API left them unset.
func TestListEventsRendering(t *testing.T) {
	mkEvent := func(name string, extra map[string]any) *unstructured.Unstructured {
		o := map[string]any{
			"apiVersion":     "v1",
			"kind":           "Event",
			"metadata":       map[string]any{"name": name, "namespace": "flux-system"},
			"involvedObject": map[string]any{"kind": "Kustomization", "name": "apps"},
			"type":           "Warning",
			"reason":         "ReconciliationFailed",
			"message":        "build failed",
		}
		for k, v := range extra {
			o[k] = v
		}
		return &unstructured.Unstructured{Object: o}
	}
	// Repeated event: lastTimestamp + count>1 both render.
	repeated := mkEvent("e1", map[string]any{
		"lastTimestamp": "2026-07-01T14:05:00Z",
		"count":         int64(3),
	})
	// Sparse event: no timestamp, count=1 — both omitted, back to bare "Type Reason Message".
	sparse := mkEvent("e2", nil)

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{eventsGVR: "EventList"}, repeated, sparse)
	r := NewDynamicReader(client)

	lines, err := r.ListEvents(context.Background(), "flux-system", "apps", "Kustomization")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 event lines, got %d: %v", len(lines), lines)
	}
	var gotRepeated, gotSparse bool
	for _, l := range lines {
		switch {
		case strings.Contains(l, "(x3)"):
			gotRepeated = true
			if want := "2026-07-01T14:05:00Z Warning ReconciliationFailed(x3) build failed"; l != want {
				t.Fatalf("repeated event line = %q, want %q", l, want)
			}
		default:
			gotSparse = true
			if want := "Warning ReconciliationFailed build failed"; l != want {
				t.Fatalf("sparse event line = %q, want %q", l, want)
			}
		}
	}
	if !gotRepeated || !gotSparse {
		t.Fatalf("missing expected lines: %v", lines)
	}
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
		"spec":       map[string]any{"path": "./apps", "targetNamespace": "harbor", "sourceRef": map[string]any{"name": "flux-system"}},
		"status": map[string]any{
			"lastAppliedRevision": "main@sha1:abc",
			"conditions": []any{
				map[string]any{"type": "Healthy", "status": "True"},
				map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed", "message": "kustomize build failed", "lastTransitionTime": "2026-07-01T14:05:00Z"},
			},
		},
	}}
	k := kustomizationFromUnstructured(u)
	if k.ReadyStatus != "False" || k.ReadyReason != "BuildFailed" || k.ReadyMessage != "kustomize build failed" {
		t.Fatalf("unexpected ready condition: %+v", k)
	}
	if k.TargetNamespace != "harbor" {
		t.Fatalf("targetNamespace not parsed: %q", k.TargetNamespace)
	}
	if !k.ReadyTime.Equal(time.Date(2026, 7, 1, 14, 5, 0, 0, time.UTC)) {
		t.Fatalf("ReadyTime not parsed from lastTransitionTime: %v", k.ReadyTime)
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

// TestGetResourceReportsWhatTheLookupEstablished pins the three outcomes a bare 404
// used to collapse into one.
//
// The dynamic client reports "no object of that name" and "the API serves no such kind"
// as the same NotFound, and the cluster-wide List's error was discarded outright
// (`if lerr == nil`), so a search RBAC refused was reported as a search that found
// nothing. Those are the states in which "and no such object exists" was false —
// runlore#503's mechanism, alive on the SUPPORTED-kind path that engine scoping never
// touched.
//
// The discriminator for an unserved kind is the List: against a served resource it
// returns an empty list, never a 404.
func TestGetResourceReportsWhatTheLookupEstablished(t *testing.T) {
	gvrToListKind := map[schema.GroupVersionResource]string{helmReleaseGVR: "HelmReleaseList"}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "helm.toolkit.fluxcd.io", Resource: "helmreleases"},
		"", errors.New("no permission"))
	unserved := apierrors.NewNotFound(schema.GroupResource{Group: "helm.toolkit.fluxcd.io", Resource: "helmreleases"}, "")

	for _, tc := range []struct {
		name       string
		listErr    error
		wantReason providers.LookupReason
		wantScopes []string
	}{
		{
			name:       "served kind, nothing by that name anywhere",
			wantReason: providers.LookupAbsent,
			wantScopes: []string{"apps", "flux-system", providers.AllNamespaces},
		},
		{
			name:       "cluster-wide search refused by RBAC",
			listErr:    forbidden,
			wantReason: providers.LookupDenied,
			// "all namespaces" must NOT appear: it never ran, and claiming it did is the
			// half of the old message that was simply untrue.
			wantScopes: []string{"apps", "flux-system"},
		},
		{
			name:       "CRD not served by this API server",
			listErr:    unserved,
			wantReason: providers.LookupKindNotServed,
			wantScopes: []string{"apps", "flux-system"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind)
			if tc.listErr != nil {
				client.PrependReactor("list", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.listErr
				})
			}
			_, err := NewDynamicReader(client).GetResource(context.Background(), "HelmRelease", "apps", "api")
			if err == nil {
				t.Fatal("want an error for a missing object")
			}
			if !apierrors.IsNotFound(err) {
				t.Errorf("the API's NotFound must stay recoverable through the wrapper: %v", err)
			}
			lk, ok := providers.LookupOf(err)
			if !ok {
				t.Fatalf("no Lookup attached, so the caller can only guess at a verdict: %v", err)
			}
			if lk.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", lk.Reason, tc.wantReason)
			}
			if !slices.Equal(lk.Scopes, tc.wantScopes) {
				t.Errorf("Scopes = %v, want %v — a reply may only name searches that ran", lk.Scopes, tc.wantScopes)
			}
		})
	}
}

// TestGetResourceUnresolvableKindIsNotANegative pins that a kind this provider has no
// mapping for is reported as a scope limit, not as a lookup that found nothing — no
// request is ever issued, so there is nothing to report about the object.
func TestGetResourceUnresolvableKindIsNotANegative(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{helmReleaseGVR: "HelmReleaseList"})
	_, err := NewDynamicReader(client).GetResource(context.Background(), "ArtifactGenerator", "flux-system", "monorepo-split")
	lk, ok := providers.LookupOf(err)
	if !ok || lk.Reason != providers.LookupUnresolvable {
		t.Fatalf("Lookup = %+v (ok=%v), want reason %q", lk, ok, providers.LookupUnresolvable)
	}
	if apierrors.IsNotFound(err) {
		t.Error("an unresolvable kind must not masquerade as a NotFound: nothing was looked up")
	}
}
