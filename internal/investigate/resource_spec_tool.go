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

// ResourceSpecTool reads one Kubernetes object's desired state (.spec) and observed
// state (.status), for any kind the cluster serves.
//
// It exists because every other reader answers "what is happening" while a large class
// of incidents is "the SPEC says X and reality is Y": a Service selector matching no
// pods, a scrape CR targeting a namespace that does not exist, a NetworkPolicy with no
// egress rule to kube-dns, an HPA on a metric that never reports. Those were previously
// only inferable from consequences, and the inference goes wrong — an investigation
// concluded a VMServiceScrape had been deleted and advised restoring it from Git history
// when the object was present and its namespaceSelector simply pointed nowhere. One read
// of .spec would have settled it.
type ResourceSpecTool struct {
	Reader providers.ResourceSpecReader
}

// Name returns the tool name.
func (t ResourceSpecTool) Name() string { return "resource_spec" }

// Description returns the tool description.
func (t ResourceSpecTool) Description() string {
	return "Read one Kubernetes object's DESIRED state (.spec) and observed state (.status), for " +
		"any kind the cluster serves, including CRDs. Use it when the question is whether " +
		"configuration matches reality — a selector that matches nothing, a namespaceSelector " +
		"pointing at a namespace that does not exist, a missing egress rule, a metric an HPA " +
		"references but nothing reports. Prefer this over inferring config from its consequences. " +
		"Secret is refused. Namespaced kinds require namespace; cluster-scoped kinds take none. " +
		"When a kind is served by several API groups the read is refused as ambiguous — call again " +
		"with group set to the one you mean. A denial or an unknown kind is " +
		"reported as such and is NOT evidence that the object is absent."
}

// Schema returns the JSON schema for the arguments.
//
// group is optional and exists so an ambiguous kind is not a dead end: Event is served by
// both the core group and events.k8s.io on every cluster, and NetworkPolicy by both
// networking.k8s.io and crd.projectcalico.org on any cluster running Calico. Without it,
// the reader's refusal to guess would leave the model no way to say which one it meant.
func (t ResourceSpecTool) Schema() string {
	return `{"type":"object","properties":{"kind":{"type":"string"},"name":{"type":"string"},` +
		`"namespace":{"type":"string"},` +
		`"group":{"type":"string","description":"optional API group, e.g. networking.k8s.io (\"core\" for the core group) — only needed when a kind is served by more than one group"}},` +
		`"required":["kind","name"]}`
}

// Call reads the object and renders it.
//
// Each outcome renders differently ON PURPOSE. Collapsing "you may not read this" and
// "this cluster has no such kind" into "not found" is the defect that made
// gitops_resource_status hand the model a fabricated fact, so the distinction is carried
// all the way into the text the model sees, together with an explicit statement of what
// the answer does NOT establish.
func (t ResourceSpecTool) Call(ctx context.Context, args string) (string, error) {
	var in struct{ Kind, Name, Namespace, Group string }
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if t.Reader == nil {
		return "resource_spec is not configured (no cluster access).", nil
	}
	q := providers.ResourceSpecQuery{Kind: in.Kind, Name: in.Name, Namespace: in.Namespace, Group: in.Group}
	rs, err := t.Reader.ResourceSpec(ctx, q)
	if err != nil {
		return "", err
	}
	// Identify the object by what the reader RESOLVED, not by what was asked for: a
	// cluster-scoped kind called with a namespace would otherwise be reported back as
	// "StorageClass totally-made-up-ns/fast", stating the caller's mistake as fact.
	id := specID(rs.Query)
	if rs.Query.Name == "" {
		id = specID(q)
	}
	switch rs.Outcome {
	case providers.ResourceAbsent:
		return id + ": ABSENT — the API server reports no such object. This IS evidence the " +
			"object does not exist. Whether that absence explains the incident is for you to " +
			"establish from other evidence.", nil
	case providers.ResourceRefused:
		// Distinct from FORBIDDEN on purpose. Rendered as an RBAC denial, the human fix
		// looks like "widen the ClusterRole" — which is how a policy refusal turns into a
		// grant on secrets. This one is not about permissions and cannot be configured away.
		return id + ": REFUSED — " + redact.Secrets(rs.Detail) + "\nThis is a policy of this " +
			"agent, NOT an RBAC denial: widening the ClusterRole will not change it, and it says " +
			"NOTHING about whether the object exists.", nil
	case providers.ResourceForbidden:
		// The one the model most needs spelled out: a denial is not absence. Left
		// unstated, "cannot read" gets reasoned about as "not there".
		return id + ": FORBIDDEN — " + redact.Secrets(rs.Detail) + "\nThis says NOTHING about " +
			"whether the object exists; it says this agent may not read it. Do NOT treat it as " +
			"evidence of absence.", nil
	case providers.ResourceKindAmbiguous:
		return id + ": AMBIGUOUS KIND — " + redact.Secrets(rs.Detail) + "\nNothing was read, " +
			"because reading the wrong object and describing it confidently is worse than " +
			"answering nothing. This says NOTHING about whether any object exists.", nil
	case providers.ResourceKindUnknown:
		return id + ": UNKNOWN KIND — " + redact.Secrets(rs.Detail) + "\nThis says NOTHING about " +
			"whether any object exists. Re-check the kind's spelling, or use a tool suited to it.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", id, rs.APIVersion)
	// Redaction on the way out, at the boundary that crosses to the provider. A spec can
	// carry an inline credential (an env value, a connection string, a token in an
	// annotation-shaped field), so this is the same treatment pod_logs output gets — the
	// difference being that here it is the only line of defence for non-Secret kinds,
	// which is why Secret itself is refused by kind rather than trusted to this.
	if rs.Spec != "" {
		fmt.Fprintf(&b, "spec:\n%s\n", indentBlock(redact.Secrets(rs.Spec)))
	} else {
		b.WriteString("spec: (none — this kind has no spec)\n")
	}
	if rs.Status != "" {
		fmt.Fprintf(&b, "status:\n%s\n", indentBlock(redact.Secrets(rs.Status)))
	}
	return b.String(), nil
}

// specID renders the object's identity, "Kind namespace/name" — or "Kind name" for a
// cluster-scoped object, which has no namespace to render.
func specID(q providers.ResourceSpecQuery) string {
	ref := q.Name
	if q.Namespace != "" {
		ref = q.Namespace + "/" + q.Name
	}
	return strings.TrimSpace(q.Kind + " " + ref)
}

// indentBlock indents a YAML block by two spaces so spec and status read as nested
// sections rather than running together with the header line.
func indentBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "\n")
}
