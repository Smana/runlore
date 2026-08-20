// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

// maxSpecBytes bounds one rendered section (spec or status). A spec is normally a few
// hundred bytes; a Deployment's status or a big CRD can be larger, and an
// unstructured object has no natural ceiling. The investigation loop caps tool output
// globally (investigation.max_tool_output_bytes), but a section cap keeps ONE object
// from consuming that whole budget and evicting the rest of the evidence.
const maxSpecBytes = 8 << 10

// refusedKind is a kind this tool never reads, refused by policy rather than left to
// redaction.
type refusedKind struct{ kind, why string }

// refusedKinds are never readable through this path, by kind rather than by redaction.
//
// Secret is the obvious one and redaction is NOT considered sufficient for it: a Secret
// IS the payload, so a redactor missing one pattern leaks the whole object, whereas for
// every other kind a credential is an incidental field. Refusing by kind fails closed;
// relying on redaction fails open.
var refusedKinds = []refusedKind{{
	kind: "Secret",
	why: "Secret objects are never readable through this tool — a Secret is entirely " +
		"sensitive, so refusing the kind fails closed where redaction would fail open. " +
		"Read the workload that CONSUMES it instead (its spec names the keys it mounts).",
}}

// refusedResources are refused by RESOURCE name after resolution, in ANY API group.
//
// The kind pre-check cannot see this: an aggregated API server or a CRD may serve a
// `secrets` resource under a Kind of its own choosing, which the pre-check would wave
// through because it only ever compares the caller's spelling against a kind list.
var refusedResources = map[string]string{
	"secrets": "a resource named \"secrets\" is never readable through this tool, whatever " +
		"Kind or API group serves it — the payload is the secret, so refusing fails closed " +
		"where redaction would fail open.",
}

// refusalFor reports why kind is refused, comparing with EqualFold on BOTH sides.
//
// The folding MUST match the one resolution uses. It did not: the refusal keyed a
// lowercase map with strings.ToLower while resolution matched with strings.EqualFold, and
// the two disagree on Unicode simple folding — U+017F ("ſ") lowercases to itself but folds
// to "s", so `ſecret` missed the refusal and still resolved to v1/secrets. That is the
// same defect class as the IDN homoglyph bypass in #498: a case-folding mismatch between
// the check and the use.
func refusalFor(kind string) (string, bool) {
	for _, r := range refusedKinds {
		if strings.EqualFold(r.kind, kind) {
			return r.why, true
		}
	}
	return "", false
}

// resourceDiscoverer is the slice of discovery this reader needs. Narrow on purpose: it
// keeps the reader unit-testable without a live API server, and ServerPreferredResources
// (rather than the full group/version listing) means one preferred version per
// group-resource, so a kind served at several versions cannot be ambiguous here — only a
// kind served by several GROUPS can, which is a genuine ambiguity worth reporting.
type resourceDiscoverer interface {
	ServerPreferredResources() ([]*metav1.APIResourceList, error)
}

// discoveryInvalidator is the optional half of discovery: dropping a memoised result.
// The production client (client-go's memcache) implements it; the narrow interface above
// deliberately does not require it, so a plain discovery client still works.
type discoveryInvalidator interface {
	Invalidate()
}

// SpecReader reads one object's spec/status for an arbitrary kind.
//
// Resolution goes through DISCOVERY rather than a compiled-in kind→GVR table, which is what
// lets an operator's CRD be read without a code change per operator — the limitation that
// makes the Flux/ArgoCD inspectors blind to every kind they were not built with.
//
// A bare Kind is deliberately what callers pass, because that is what a model has: it reads
// "VMServiceScrape" off an alert or a manifest, not "vmservicescrapes.v1beta1.operator.
// victoriametrics.com". Resolving that means searching served resources by Kind, which is
// also the only way to notice that two API GROUPS serve the same Kind.
type SpecReader struct {
	client dynamic.Interface
	disco  resourceDiscoverer
}

// NewSpecReader builds a read-only spec reader from a dynamic client and a discovery
// client.
func NewSpecReader(client dynamic.Interface, disco resourceDiscoverer) *SpecReader {
	return &SpecReader{client: client, disco: disco}
}

// resolved is one served resource matching a Kind.
type resolved struct {
	gvr        schema.GroupVersionResource
	kind       string // the Kind as the server spells it, not as the caller did
	namespaced bool
}

// resolveKind finds every served resource whose Kind matches, case-insensitively.
//
// Returning ALL matches rather than the first is the honest choice: "Ingress" is served by
// both networking.k8s.io and (historically) extensions, and silently picking one would be
// the same class of quiet wrongness this tool exists to remove. Subresources are skipped —
// "pods/log" is not an object you can read a spec from.
//
// A miss RETRIES once against fresh discovery. The production client memoises, and
// client-go's memcache caches failures exactly as permanently as successes: without this,
// a CRD installed after RunLore started is "this cluster serves no such kind" for the
// pod's whole lifetime, and a single aggregated-APIService blip poisons a group forever.
// Both would be reported to the model as a fact about the cluster.
func (r *SpecReader) resolveKind(kind string) ([]resolved, error) {
	matches, err := r.resolveKindCached(kind)
	if len(matches) > 0 {
		return matches, nil
	}
	inv, ok := r.disco.(discoveryInvalidator)
	if !ok {
		return matches, err
	}
	inv.Invalidate()
	return r.resolveKindCached(kind)
}

// resolveKindCached is one pass over whatever discovery currently holds.
func (r *SpecReader) resolveKindCached(kind string) ([]resolved, error) {
	lists, err := r.disco.ServerPreferredResources()
	// A partial discovery failure (one broken aggregated APIService) still returns usable
	// lists, so the error is only fatal when nothing came back at all.
	if err != nil && len(lists) == 0 {
		return nil, fmt.Errorf("discover served resources: %w", err)
	}
	var out []resolved
	for _, l := range lists {
		if l == nil {
			continue
		}
		gv, perr := schema.ParseGroupVersion(l.GroupVersion)
		if perr != nil {
			continue
		}
		for _, ar := range l.APIResources {
			if strings.Contains(ar.Name, "/") || !strings.EqualFold(ar.Kind, kind) {
				continue
			}
			out = append(out, resolved{
				gvr:        gv.WithResource(ar.Name),
				kind:       ar.Kind,
				namespaced: ar.Namespaced,
			})
		}
	}
	return out, nil
}

// matchGroup reports whether m belongs to the API group the caller asked for. An empty
// request means "no preference"; "core" is accepted as a spelling of the core group,
// whose real name is the empty string and which a model cannot otherwise name.
func matchGroup(m resolved, group string) bool {
	if group == "" {
		return true
	}
	if strings.EqualFold(group, "core") {
		return m.gvr.Group == ""
	}
	return strings.EqualFold(m.gvr.Group, group)
}

var _ providers.ResourceSpecReader = (*SpecReader)(nil)

// ResourceSpec reads q's .spec and .status.
//
// The outcome taxonomy is the point of this function, not an implementation detail.
// The endings, and only ONE of them is evidence that the object is absent:
//
//	found          — read it
//	absent         — the API server says no such OBJECT
//	forbidden      — RBAC denied the read; the object may well exist
//	kind_unknown   — this cluster serves no such kind; says nothing about any object
//	kind_ambiguous — several groups serve this kind; nothing was read, name one
//	refused        — this agent refuses the kind by policy; not an RBAC problem
//
// Flattening any of them into "not found" is exactly the defect that made
// gitops_resource_status produce a confidently wrong root cause, so they are kept
// distinct all the way out to the model.
func (r *SpecReader) ResourceSpec(ctx context.Context, q providers.ResourceSpecQuery) (providers.ResourceSpec, error) {
	out := providers.ResourceSpec{Query: q}
	if q.Kind == "" || q.Name == "" {
		return out, fmt.Errorf("kind and name are required")
	}
	res, err := r.resolveOne(q.Kind, q.Group)
	if err != nil {
		return out, err
	}
	if res.outcome != "" {
		out.Outcome, out.Detail = res.outcome, res.detail
		return out, nil
	}
	match := res.match
	// Echo the identity that was actually resolved: the server's spelling of the Kind,
	// the group that answered, and NO namespace for a cluster-scoped kind. Repeating the
	// request verbatim would render a caller's mistake as fact — "StorageClass
	// totally-made-up-ns/fast" for an object that has no namespace at all.
	out.Query.Kind, out.Query.Group = match.kind, match.gvr.Group
	var ri dynamic.ResourceInterface = r.client.Resource(match.gvr)
	if match.namespaced {
		if q.Namespace == "" {
			return out, fmt.Errorf("kind %q is namespaced: namespace is required", match.kind)
		}
		ri = r.client.Resource(match.gvr).Namespace(q.Namespace)
	} else {
		out.Query.Namespace = ""
	}
	obj, err := ri.Get(ctx, q.Name, metav1.GetOptions{})
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		// IsNotFound is true for BOTH "no such object" and "no such resource at this
		// path" — a CRD deleted since discovery ran, an aggregated APIService that
		// stopped serving. Only the first is evidence about an object, and reporting
		// the second as absence is the #503 conflation this tool exists to remove.
		if !objectLevelNotFound(err) {
			out.Outcome = providers.ResourceKindUnknown
			out.Detail = fmt.Sprintf("the API server no longer serves %s (discovery may be stale); "+
				"no object was looked up", match.gvr.GroupResource().String())
			return out, nil
		}
		out.Outcome = providers.ResourceAbsent
		out.Detail = "the API server reports no such object"
		return out, nil
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		// RBAC is the boundary for this tool. A denial must never be rendered as
		// absence: whatever the ClusterRole grants is what is readable, and anything
		// else says nothing about whether the object exists.
		out.Outcome = providers.ResourceForbidden
		out.Detail = strings.TrimSpace(err.Error())
		return out, nil
	default:
		var status apierrors.APIStatus
		if errors.As(err, &status) {
			return out, fmt.Errorf("get %s %s/%s: %s", match.kind, q.Namespace, q.Name, status.Status().Message)
		}
		return out, fmt.Errorf("get %s %s/%s: %w", match.kind, q.Namespace, q.Name, err)
	}
	out.Outcome = providers.ResourceFound
	out.Query.Name, out.Query.Namespace = obj.GetName(), obj.GetNamespace()
	// Report the version the read actually used. A kind can be served at several
	// versions, so "which one answered" is part of reading the spec honestly.
	out.APIVersion = match.gvr.GroupVersion().String()
	// Mask the {name, value} credential shape STRUCTURALLY, before anything is
	// marshalled. redact.Secrets is key-name oriented — it masks `password: <value>` —
	// while a container's env inverts that: the sensitive word is the value of `name`
	// and the credential sits under the literal key `value`, which is in no keyword
	// vocabulary. A copy is masked so the client's object is left untouched.
	obj = obj.DeepCopy()
	redact.SensitiveNameValues(obj.Object)
	out.Spec = renderSection(obj, "spec")
	out.Status = renderSection(obj, "status")
	return out, nil
}

// objectLevelNotFound reports whether a 404 was about an OBJECT rather than about the
// resource path. The API server names the object it could not find in Status.Details;
// an unserved path carries no details at all ("the server could not find the requested
// resource").
func objectLevelNotFound(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	d := status.Status().Details
	return d != nil && d.Name != ""
}

// renderSection returns one top-level field as YAML, redacted and bounded. Missing is ""
// rather than an error: plenty of kinds legitimately have no spec (ConfigMap) or no
// status yet.
//
// Redaction runs BEFORE the cap, which is the documented invariant for every other
// boundary (see the tool-output chokepoint in the investigation loop): a secret that
// straddles the cap must be MASKED, not sliced. Cutting first broke anchored rules — a
// PEM block whose -----END----- fell past the cap had no terminator left to match, and
// the key body shipped verbatim.
func renderSection(obj *unstructured.Unstructured, field string) string {
	v, found, err := unstructured.NestedFieldNoCopy(obj.Object, field)
	if err != nil || !found || v == nil {
		return ""
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	s := redact.Secrets(strings.TrimRight(string(b), "\n"))
	if len(s) <= maxSpecBytes {
		return s
	}
	// Cut on a line boundary so the result stays parseable-looking YAML rather than
	// ending mid-key, which reads as corruption instead of truncation. A partial line is
	// dropped rather than sliced: a fragment of a token the ruleset did not recognise is
	// still a fragment of a secret, and it is no longer recognisable to anything
	// downstream either.
	cut := strings.LastIndexByte(s[:maxSpecBytes], '\n')
	if cut <= 0 {
		return "… (truncated: a single line exceeded the section cap)"
	}
	return s[:cut] + "\n… (truncated)"
}
