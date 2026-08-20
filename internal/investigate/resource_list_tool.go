// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

// ResourceListTool enumerates the objects of one kind in one namespace.
//
// It is the complement resource_spec structurally cannot be. That tool reads a NAMED
// object, which presumes the name is already known; a large class of incidents is instead
// "which object is doing this to me" — which NetworkPolicy denies this flow, which
// ValidatingWebhookConfiguration rejects this apply, which ScaledObject governs this
// Deployment. With only a by-name reader, a model can do nothing but guess names.
//
// It guesses badly, and expensively. A real investigation into a denied readiness probe
// spent 32 of its 58 tool calls trying CiliumNetworkPolicy names (checkout-ui, orders-api,
// default-deny, runlore-demo, …), got ABSENT for every one, and shipped a card whose own
// Data gaps section read: "resource_spec reads named objects only (not lists), so I could
// not enumerate all NetworkPolicy/CiliumNetworkPolicy objects". It had PROVEN the denial
// from flow logs and still could not name the policy, so it published at 55% confidence
// with the object identification left as an open question for a human.
type ResourceListTool struct {
	Lister providers.ResourceLister
}

// Name returns the tool name.
func (t ResourceListTool) Name() string { return "list_resources" }

// Description returns the tool description.
//
// It leads with the distinction from resource_spec because the two are otherwise easy to
// confuse, and states the empty-result semantics up front: an empty listing is the one
// answer here that IS evidence of absence, and it is the answer guessing names can never
// produce.
func (t ResourceListTool) Description() string {
	return "List the NAMES of every object of one Kubernetes kind in a namespace, for any kind " +
		"the cluster serves, including CRDs. Use it whenever you need to find WHICH object is " +
		"responsible and do not already know its name — which NetworkPolicy or " +
		"CiliumNetworkPolicy denies a flow, which webhook rejects an apply, which ScaledObject " +
		"governs a Deployment. Do NOT guess names with resource_spec: that only answers about " +
		"the name you guessed, and a wrong guess returns ABSENT, which proves nothing. " +
		"A successful EMPTY listing IS evidence that no such object exists there. Returns " +
		"identities only — read one with resource_spec once you know its name. Secret is " +
		"refused. Namespaced kinds take a namespace; cluster-scoped kinds take none. When a " +
		"kind is served by several API groups the listing is refused as ambiguous — call again " +
		"with group set. A denial or an unknown kind is reported as such and is NOT evidence " +
		"that no objects exist."
}

// Schema returns the JSON schema for the arguments.
func (t ResourceListTool) Schema() string {
	return `{"type":"object","properties":{"kind":{"type":"string"},` +
		`"namespace":{"type":"string","description":"namespace to list in; omit for a cluster-scoped kind"},` +
		`"group":{"type":"string","description":"optional API group, e.g. cilium.io (\"core\" for the core group) — only needed when a kind is served by more than one group"},` +
		`"labelSelector":{"type":"string","description":"optional label selector, e.g. app=orders-api"}},` +
		`"required":["kind"]}`
}

// Call lists the objects and renders them.
//
// The outcome taxonomy is ResourceSpec's, and for the same reason: collapsing "you may not
// read this" into "there are none" is exactly the fabrication this family of tools exists
// to prevent. The one addition is that here a successful empty result is POSITIVE evidence
// and says so, because that is the whole point of being able to enumerate.
func (t ResourceListTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Kind, Namespace, Group, LabelSelector string
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if t.Lister == nil {
		return "list_resources is not configured (no cluster access).", nil
	}
	q := providers.ResourceListQuery{
		Kind:          in.Kind,
		Namespace:     in.Namespace,
		Group:         in.Group,
		LabelSelector: in.LabelSelector,
	}
	rl, err := t.Lister.ResourceList(ctx, q)
	if err != nil {
		return "", err
	}
	// Identify by what the lister RESOLVED, falling back to the request when it resolved
	// nothing — same reasoning as resource_spec: echoing a caller's mistake back as fact
	// ("StorageClass made-up-ns") states the mistake as cluster truth.
	id := listID(rl.Query)
	if rl.Query.Kind == "" {
		id = listID(q)
	}
	switch rl.Outcome {
	case providers.ResourceRefused:
		return id + ": REFUSED — " + redact.Secrets(rl.Detail) + "\nThis is a policy of this " +
			"agent, NOT an RBAC denial: widening the ClusterRole will not change it, and it says " +
			"NOTHING about whether any such objects exist.", nil
	case providers.ResourceForbidden:
		return id + ": FORBIDDEN — " + redact.Secrets(rl.Detail) + "\nThis says NOTHING about " +
			"whether any such objects exist; it says this agent may not list them. Do NOT treat " +
			"it as evidence that there are none.", nil
	case providers.ResourceKindAmbiguous:
		return id + ": AMBIGUOUS KIND — " + redact.Secrets(rl.Detail) + "\nNothing was listed, " +
			"because listing the wrong group's objects and naming them confidently is worse " +
			"than answering nothing. This says NOTHING about whether any objects exist.", nil
	case providers.ResourceKindUnknown:
		return id + ": UNKNOWN KIND — " + redact.Secrets(rl.Detail) + "\nThis says NOTHING about " +
			"whether any objects exist. Re-check the kind's spelling, or use a tool suited to it.", nil
	case providers.ResourceAbsent:
		// The namespace itself is gone. Distinct from an empty listing: "no namespace" and
		// "a namespace with no such objects" are different facts about the cluster.
		return id + ": ABSENT — " + redact.Secrets(rl.Detail) + "\nThe place asked about does " +
			"not exist, so nothing could be listed in it.", nil
	}
	if len(rl.Items) == 0 && !rl.Truncated {
		return id + ": NONE — the cluster serves this kind and this agent may list it, and " +
			"there are no such objects here. This IS evidence that no object of this kind " +
			"exists in this scope.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) — %d object(s)\n", id, rl.APIVersion, len(rl.Items))
	for _, it := range rl.Items {
		ref := it.Name
		if it.Namespace != "" {
			ref = it.Namespace + "/" + it.Name
		}
		fmt.Fprintf(&b, "  %s\n", redact.Secrets(ref))
	}
	if rl.Truncated {
		// Without this, a name missing from a capped page reads as proof of absence — the
		// exact wrong inference this tool was built to remove.
		b.WriteString("TRUNCATED — the server had more objects than are listed above. A name " +
			"NOT shown here is NOT evidence that it does not exist; narrow with labelSelector.\n")
	}
	b.WriteString("Read any one of them with resource_spec, using its name.\n")
	return b.String(), nil
}

// listID renders the scope being listed, "Kind in namespace" — or "Kind (cluster-scoped)"
// when there is no namespace to render.
func listID(q providers.ResourceListQuery) string {
	kind := strings.TrimSpace(q.Kind)
	if q.Namespace == "" {
		return kind + " (cluster-scoped)"
	}
	return kind + " in " + q.Namespace
}
