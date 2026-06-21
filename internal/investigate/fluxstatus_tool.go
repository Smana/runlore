package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// FluxStatusTool exposes a Flux/Kubernetes resource's Ready condition, key spec
// refs (sourceRef, dependsOn) and recent Events — so the model can learn WHY a
// resource is failing, not just that it is.
type FluxStatusTool struct {
	Inspector providers.GitOpsInspector
}

// Name returns the tool name.
func (t FluxStatusTool) Name() string { return "flux_resource_status" }

// Description returns the tool description.
func (t FluxStatusTool) Description() string {
	return "Get a Flux/Kubernetes resource's Ready condition (status/reason/message), key spec refs " +
		"(sourceRef, dependsOn) and recent Events. Use this to learn WHY a resource is failing — " +
		"follow its sourceRef/dependsOn to the root. kind is one of: Kustomization, HelmRelease, " +
		"GitRepository, OCIRepository, HelmRepository, HelmChart, Bucket."
}

// Schema returns the JSON schema for the arguments.
func (t FluxStatusTool) Schema() string {
	return `{"type":"object","properties":{"kind":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"}},"required":["kind","name","namespace"]}`
}

// Call fetches the resource status and renders it.
func (t FluxStatusTool) Call(ctx context.Context, args string) (string, error) {
	var in struct{ Kind, Name, Namespace string }
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	rs, err := t.Inspector.ResourceStatus(ctx, providers.Workload{Kind: in.Kind, Name: in.Name, Namespace: in.Namespace})
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s %s/%s", in.Kind, in.Namespace, in.Name)
	if rs.NotFound {
		return id + ": NOT FOUND (the object does not exist — likely the cascade root)", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  Ready=%s", id, emptyDash(rs.Ready))
	if rs.Reason != "" {
		fmt.Fprintf(&b, " (%s)", rs.Reason)
	}
	b.WriteString("\n")
	if rs.Message != "" {
		fmt.Fprintf(&b, "  message: %s\n", rs.Message)
	}
	for _, k := range sortedKeys(rs.Refs) {
		fmt.Fprintf(&b, "  %s: %s\n", k, rs.Refs[k])
	}
	if len(rs.Events) > 0 {
		b.WriteString("Events:\n")
		for i, e := range rs.Events {
			if i >= maxToolRows {
				fmt.Fprintf(&b, "  … (%d more)\n", len(rs.Events)-i)
				break
			}
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}
	return b.String(), nil
}

func emptyDash(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
