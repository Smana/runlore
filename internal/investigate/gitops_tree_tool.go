// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// GitOpsTreeTool walks a resource's dependency graph (Flux dependsOn/sourceRef, or an
// Argo CD Application's managed-resource tree) and renders it, so the model can find
// the ROOT failing resource behind a cascade (the not-Ready/Degraded or missing
// node), not just the downstream symptom.
type GitOpsTreeTool struct {
	Inspector providers.GitOpsInspector
}

// Name returns the tool name.
func (t GitOpsTreeTool) Name() string { return "gitops_tree" }

// Description returns the tool description.
func (t GitOpsTreeTool) Description() string {
	return "Walk a GitOps resource's dependency graph (a Flux resource's dependsOn/sourceRef, or an " +
		"Argo CD Application's managed-resource tree) and render it with each node's Ready/health state. " +
		"Use it on a failing resource to find the ROOT cause — the first not-Ready/Degraded or NOT FOUND " +
		"node — instead of the downstream symptom."
}

// Schema returns the JSON schema for the arguments.
func (t GitOpsTreeTool) Schema() string {
	return `{"type":"object","properties":{"kind":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"}},"required":["kind","name","namespace"]}`
}

// Call walks the dependency tree and renders it.
func (t GitOpsTreeTool) Call(ctx context.Context, args string) (string, error) {
	var in struct{ Kind, Name, Namespace string }
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	root, err := t.Inspector.DependencyTree(ctx, providers.Workload{Kind: in.Kind, Name: in.Name, Namespace: in.Namespace})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	renderDepNode(&b, root, 0)
	return b.String(), nil
}

// renderDepNode renders a node and its children with indentation, flagging the
// not-Ready / NOT FOUND nodes that are candidate roots.
func renderDepNode(b *strings.Builder, n providers.DepNode, depth int) {
	indent := strings.Repeat("  ", depth)
	id := fmt.Sprintf("%s %s/%s", n.Workload.Kind, n.Workload.Namespace, n.Workload.Name)
	switch {
	case n.NotFound:
		fmt.Fprintf(b, "%s%s: NOT FOUND  ← root\n", indent, id)
	case n.Ready == "False" || n.Ready == "Unknown":
		fmt.Fprintf(b, "%s%s (Ready=%s%s)\n", indent, id, n.Ready, reasonSuffix(n.Reason))
	case n.Ready == "":
		fmt.Fprintf(b, "%s%s (Ready=unknown)\n", indent, id)
	default:
		fmt.Fprintf(b, "%s%s (Ready=%s)\n", indent, id, n.Ready)
	}
	for _, c := range n.Children {
		renderDepNode(b, c, depth+1)
	}
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ", " + reason
}
