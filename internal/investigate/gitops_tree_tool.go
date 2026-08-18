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
// Argo CD Application's managed-resource tree) and renders it, so the model can look for
// the ROOT failing resource behind a cascade — the not-Ready/Degraded node, or one the
// API did not return — rather than the downstream symptom.
type GitOpsTreeTool struct {
	Inspector providers.GitOpsInspector
	// Engine bounds the kinds this tool advertises and accepts, exactly as on
	// GitOpsStatusTool, and selects the engine-specific half of its description.
	//
	// It matters MORE here because a node this tool cannot read is still rendered, so
	// an out-of-scope or unresolvable kind does not merely go unanswered — it produces a
	// line in a dependency tree. Every such line now says what the lookup was
	// (renderDepNode + gitopsLookupNote); it used to say "NOT FOUND  ← root", nominating
	// the absence as the cause outright.
	Engine string
}

// Name returns the tool name.
func (t GitOpsTreeTool) Name() string { return "gitops_tree" }

// Description returns the tool description, describing only the engine this deployment
// actually runs — see GitOpsStatusTool.Description.
func (t GitOpsTreeTool) Description() string {
	graph := "a Flux resource's dependsOn/sourceRef edges"
	if t.Engine == "argocd" {
		graph = "an Argo CD Application's managed-resource tree"
	}
	return "Walk a GitOps resource's dependency graph (" + graph + ") and render it with each " +
		"node's Ready/health state. Use it on a failing resource to look for the ROOT cause — the " +
		"first not-Ready/Degraded node, or one the API did not return — instead of the downstream " +
		"symptom. A node the API did not return is a lookup result, not a root cause: establish " +
		"that from other evidence. GitOps objects ONLY, not arbitrary Kubernetes resources: " +
		gitopsKindProse(t.Engine)
}

// Schema returns the JSON schema for the arguments.
func (t GitOpsTreeTool) Schema() string { return gitopsKindSchema(t.Engine) }

// Call walks the dependency tree and renders it.
func (t GitOpsTreeTool) Call(ctx context.Context, args string) (string, error) {
	var in struct{ Kind, Name, Namespace string }
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	// Refuse an out-of-scope kind before the walk, for the same reason as
	// gitops_resource_status — and canonicalise it, which matters MORE here: this tool
	// swallows a resolution failure and renders the node anyway, so a kind that passes
	// the case-insensitive guard and then misses the exact-match resolver comes back as
	// "helmrelease apps/api (Ready=unknown)" — a node for an object nobody looked up.
	kind, ok := canonicalGitOpsKind(t.Engine, in.Kind)
	if !ok {
		return gitopsUnsupportedKind(t.Engine, in.Kind), nil
	}
	in.Kind = kind
	root, err := t.Inspector.DependencyTree(ctx, providers.Workload{Kind: in.Kind, Name: in.Name, Namespace: in.Namespace})
	if err != nil {
		return "", err
	}
	// F2: every existing node in the tree is a server-confirmed resource — record them
	// observed so an action may legitimately target the root failure or a dependency.
	recordObservedTree(ctx, root)
	var b strings.Builder
	renderDepNode(&b, root, 0)
	return b.String(), nil
}

// recordObservedTree records every node whose object was actually READ. A node carrying
// a Lookup is one the API did not return — absent, denied, an unserved or unresolvable
// kind, or a failed read — and recording it as observed would let an action target a
// resource nothing confirmed server-side (see guardUnobservedTargets).
func recordObservedTree(ctx context.Context, n providers.DepNode) {
	if n.Lookup.Reason == "" && !n.NotFound {
		recordObserved(ctx, n.Workload)
	}
	for _, c := range n.Children {
		recordObservedTree(ctx, c)
	}
}

// renderDepNode renders a node and its children with indentation, flagging the
// not-Ready nodes and the ones the API did not return.
func renderDepNode(b *strings.Builder, n providers.DepNode, depth int) {
	indent := strings.Repeat("  ", depth)
	id := fmt.Sprintf("%s %s/%s", n.Workload.Kind, n.Workload.Namespace, n.Workload.Name)
	switch {
	case n.Lookup.Reason != "" || n.NotFound:
		// No "← root". The branch's own commit message says that arrow "hands back a
		// root cause outright", which was the argument for refusing an out-of-scope kind
		// here — and then it stayed on the supported path, so the two tools contradicted
		// each other about the same object. Nominating the root is the loop's job.
		fmt.Fprintf(b, "%s%s: %s\n", indent, id, gitopsLookupNote(n.Lookup))
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
