// SPDX-License-Identifier: Apache-2.0

package argocd

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

// appObj builds an unstructured Argo Application for inspector tests.
func appObj(health, sync, phase string, conditions, resources []any) *unstructured.Unstructured {
	status := map[string]any{
		"health": map[string]any{"status": health},
		"sync":   map[string]any{"status": sync},
	}
	if phase != "" {
		status["operationState"] = map[string]any{"phase": phase, "message": "sync failed: image pull error"}
	}
	if conditions != nil {
		status["conditions"] = conditions
	}
	if resources != nil {
		status["resources"] = resources
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "broken-app", "namespace": "argocd"},
		"spec": map[string]any{
			"source":      map[string]any{"repoURL": "https://example.com/repo", "path": "apps/web", "targetRevision": "main"},
			"destination": map[string]any{"namespace": "apps"},
		},
		"status": status,
	}}
}

func TestArgoResourceStatusDegraded(t *testing.T) {
	p := New(fakeReader{
		obj: appObj("Degraded", "OutOfSync", "", []any{
			map[string]any{"type": "ComparisonError", "message": "rpc error: repo not found"},
		}, nil),
		eventLines: []string{"Warning SyncFailed manifests failed to apply"},
	}, &whatchanged.Differ{})

	rs, err := p.ResourceStatus(context.Background(), providers.Workload{Kind: "Application", Name: "broken-app", Namespace: "argocd"})
	if err != nil {
		t.Fatalf("ResourceStatus: %v", err)
	}
	if rs.Ready != "False" {
		t.Fatalf("Degraded health → Ready False, got %q", rs.Ready)
	}
	if rs.Reason != "Degraded" {
		t.Fatalf("Reason: %q", rs.Reason)
	}
	if rs.Refs["repoURL"] == "" || rs.Refs["sync"] != "OutOfSync" {
		t.Fatalf("refs missing/wrong: %v", rs.Refs)
	}
	if !strings.Contains(rs.Message, "repo not found") {
		t.Fatalf("message should carry the error condition: %q", rs.Message)
	}
	if len(rs.Events) != 1 {
		t.Fatalf("events: %v", rs.Events)
	}
}

func TestArgoResourceStatusHealthy(t *testing.T) {
	p := New(fakeReader{obj: appObj("Healthy", "Synced", "", nil, nil)}, &whatchanged.Differ{})
	rs, _ := p.ResourceStatus(context.Background(), providers.Workload{Kind: "Application", Name: "ok", Namespace: "argocd"})
	if rs.Ready != "True" {
		t.Fatalf("Healthy → Ready True, got %q", rs.Ready)
	}
}

func TestArgoResourceStatusSyncFailedForcesNotReady(t *testing.T) {
	// Health hasn't flipped (still Healthy) but the sync operation Failed → not ready.
	p := New(fakeReader{obj: appObj("Healthy", "Synced", "Failed", nil, nil)}, &whatchanged.Differ{})
	rs, _ := p.ResourceStatus(context.Background(), providers.Workload{Kind: "Application", Name: "x", Namespace: "argocd"})
	if rs.Ready != "False" {
		t.Fatalf("failed sync → Ready False, got %q", rs.Ready)
	}
	if !strings.Contains(rs.Reason, "SyncFailed") {
		t.Fatalf("reason should note SyncFailed: %q", rs.Reason)
	}
}

func TestArgoResourceStatusMultiSource(t *testing.T) {
	// Multi-source app: spec.source is absent; spec.sources[] carries the refs.
	// ResourceStatus must populate repoURL/path/targetRevision from sources[0],
	// matching the behaviour of the Changes() path (sourceRepoPath in dynamic.go).
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "multi-src", "namespace": "argocd"},
		"spec": map[string]any{
			"sources": []any{
				map[string]any{
					"repoURL":        "https://example.com/infra",
					"path":           "apps/payment",
					"targetRevision": "v1.2.3",
				},
				map[string]any{
					"repoURL":        "https://example.com/values",
					"targetRevision": "HEAD",
				},
			},
			"destination": map[string]any{"namespace": "payment"},
		},
		"status": map[string]any{
			"health": map[string]any{"status": "Degraded"},
			"sync":   map[string]any{"status": "OutOfSync"},
		},
	}}
	p := New(fakeReader{obj: obj}, &whatchanged.Differ{})
	rs, err := p.ResourceStatus(context.Background(), providers.Workload{Kind: "Application", Name: "multi-src", Namespace: "argocd"})
	if err != nil {
		t.Fatalf("ResourceStatus: %v", err)
	}
	if rs.Refs["repoURL"] != "https://example.com/infra" {
		t.Fatalf("repoURL should come from sources[0], got %q", rs.Refs["repoURL"])
	}
	if rs.Refs["path"] != "apps/payment" {
		t.Fatalf("path from sources[0]: got %q", rs.Refs["path"])
	}
	if rs.Refs["targetRevision"] != "v1.2.3" {
		t.Fatalf("targetRevision from sources[0]: got %q", rs.Refs["targetRevision"])
	}
}

func TestArgoResourceStatusNotFound(t *testing.T) {
	p := New(fakeReader{notFound: true}, &whatchanged.Differ{})
	rs, err := p.ResourceStatus(context.Background(), providers.Workload{Kind: "Application", Name: "ghost", Namespace: "argocd"})
	if err != nil {
		t.Fatalf("NotFound must not be an error: %v", err)
	}
	if !rs.NotFound {
		t.Fatal("expected NotFound")
	}
}

func TestArgoDependencyTreeSurfacesFailingResources(t *testing.T) {
	resources := []any{
		map[string]any{"kind": "Deployment", "name": "web", "namespace": "apps", "health": map[string]any{"status": "Degraded"}},
		map[string]any{"kind": "Service", "name": "web", "namespace": "apps", "health": map[string]any{"status": "Healthy"}},   // filtered
		map[string]any{"kind": "Pod", "name": "web-x", "namespace": "apps", "health": map[string]any{"status": "Progressing"}}, // filtered (transient)
	}
	p := New(fakeReader{obj: appObj("Degraded", "Synced", "", nil, resources)}, &whatchanged.Differ{})
	tree, err := p.DependencyTree(context.Background(), providers.Workload{Kind: "Application", Name: "broken-app", Namespace: "argocd"})
	if err != nil {
		t.Fatalf("DependencyTree: %v", err)
	}
	if tree.Ready != "False" {
		t.Fatalf("root app Ready: %q", tree.Ready)
	}
	if len(tree.Children) != 1 {
		t.Fatalf("only the Degraded resource should surface, got %d: %+v", len(tree.Children), tree.Children)
	}
	c := tree.Children[0]
	if c.Workload.Kind != "Deployment" || c.Workload.Name != "web" || c.Ready != "False" || c.Reason != "Degraded" {
		t.Fatalf("failing child wrong: %+v", c)
	}
}
