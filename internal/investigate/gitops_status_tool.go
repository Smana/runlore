// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// GitOpsStatusTool exposes a GitOps/Kubernetes resource's status (a Flux Ready
// condition or an Argo CD Application's health/sync), key spec refs, and recent
// Events — so the model can learn WHY a resource is failing, not just that it is.
type GitOpsStatusTool struct {
	Inspector providers.GitOpsInspector
	// Engine is the configured GitOps engine ("argocd"/"flux"), which bounds the kinds
	// this tool advertises and accepts. See GitOpsKinds for why that bound matters:
	// a kind the engine cannot own is answerable only with a misleading negative.
	Engine string
}

// Name returns the tool name.
func (t GitOpsStatusTool) Name() string { return "gitops_resource_status" }

// Description returns the tool description.
func (t GitOpsStatusTool) Description() string {
	return "Get a GitOps resource's status (a Flux Ready condition or an Argo CD " +
		"Application's health/sync: status, reason, message), key spec refs, and recent Events. Use " +
		"this to learn WHY a resource is failing — follow its refs to the root. This reads GitOps " +
		"objects ONLY, not arbitrary Kubernetes resources: " + gitopsKindProse(t.Engine)
}

// Schema returns the JSON schema for the arguments.
func (t GitOpsStatusTool) Schema() string { return gitopsKindSchema(t.Engine) }

// Call fetches the resource status and renders it.
func (t GitOpsStatusTool) Call(ctx context.Context, args string) (string, error) {
	var in struct{ Kind, Name, Namespace string }
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	// Refuse an out-of-scope kind BEFORE the lookup. The schema enum should already have
	// prevented it, but a provider that ignores enums (or an MCP client) must not reach a
	// path whose only possible answer is a misleading negative.
	if !gitopsKindSupported(t.Engine, in.Kind) {
		return gitopsUnsupportedKind(t.Engine, in.Kind), nil
	}
	rs, err := t.Inspector.ResourceStatus(ctx, providers.Workload{Kind: in.Kind, Name: in.Name, Namespace: in.Namespace})
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s %s/%s", in.Kind, in.Namespace, in.Name)
	if rs.NotFound {
		// Reports the lookup, not a verdict. This previously read "the object genuinely
		// does not exist (likely the cascade root)" — two claims past what one name lookup
		// establishes, and the model quoted them as evidence. Absence is a fact worth
		// stating; that the absence CAUSED the incident is for the loop to establish.
		return id + ": NOT FOUND — searched the given namespace, flux-system, and all namespaces " +
			"by name, and no such object exists. Whether that absence explains the incident is for " +
			"you to establish from other evidence — do not assume it is the root cause. If you " +
			"expected it to exist, re-check the kind/name rather than concluding it was deleted.", nil
	}
	// F2: the inspector confirmed this resource exists server-side — record it observed.
	recordObserved(ctx, providers.Workload{Kind: in.Kind, Name: in.Name, Namespace: in.Namespace})
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
