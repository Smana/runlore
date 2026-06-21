package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeInspector serves crafted status/tree results for the tool layer.
type fakeInspector struct {
	status providers.ResourceStatus
	tree   providers.DepNode
}

func (f fakeInspector) ResourceStatus(context.Context, providers.Workload) (providers.ResourceStatus, error) {
	return f.status, nil
}
func (f fakeInspector) DependencyTree(context.Context, providers.Workload) (providers.DepNode, error) {
	return f.tree, nil
}

func TestFluxStatusTool(t *testing.T) {
	tool := FluxStatusTool{Inspector: fakeInspector{status: providers.ResourceStatus{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Ready:    "False", Reason: "DependencyNotReady", Message: "dependency not ready",
		Refs:   map[string]string{"sourceRef": "GitRepository/flux-system/infra-artifact", "dependsOn": "flux-system/infra"},
		Events: []string{"Warning DependencyNotReady dependency not ready"},
	}}}
	out, err := tool.Call(context.Background(), `{"kind":"Kustomization","name":"apps","namespace":"flux-system"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, want := range []string{"Ready=False", "DependencyNotReady", "sourceRef:", "infra-artifact", "Events:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestFluxStatusToolNotFound(t *testing.T) {
	tool := FluxStatusTool{Inspector: fakeInspector{status: providers.ResourceStatus{
		Workload: providers.Workload{Kind: "GitRepository", Name: "infra-artifact", Namespace: "flux-system"}, NotFound: true,
	}}}
	out, _ := tool.Call(context.Background(), `{"kind":"GitRepository","name":"infra-artifact","namespace":"flux-system"}`)
	if !strings.Contains(out, "NOT FOUND") {
		t.Fatalf("expected NOT FOUND, got: %s", out)
	}
}

func TestFluxTreeTool(t *testing.T) {
	tool := FluxTreeTool{Inspector: fakeInspector{tree: providers.DepNode{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}, Ready: "False", Reason: "DependencyNotReady",
		Children: []providers.DepNode{{
			Workload: providers.Workload{Kind: "Kustomization", Name: "infra", Namespace: "flux-system"}, Ready: "False", Reason: "DependencyNotReady",
			Children: []providers.DepNode{{
				Workload: providers.Workload{Kind: "GitRepository", Name: "infra-artifact", Namespace: "flux-system"}, NotFound: true,
			}},
		}},
	}}}
	out, err := tool.Call(context.Background(), `{"kind":"Kustomization","name":"apps","namespace":"flux-system"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "NOT FOUND  ← root") || !strings.Contains(out, "infra-artifact") {
		t.Fatalf("tree output missing the root marker:\n%s", out)
	}
	// The root should be indented deeper than the top node.
	if !strings.Contains(out, "    GitRepository flux-system/infra-artifact") {
		t.Fatalf("expected nested indentation:\n%s", out)
	}
}
