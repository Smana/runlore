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
)

// maxSpecBytes bounds one rendered section (spec or status). A spec is normally a few
// hundred bytes; a Deployment's status or a big CRD can be larger, and an
// unstructured object has no natural ceiling. The investigation loop caps tool output
// globally (investigation.max_tool_output_bytes), but a section cap keeps ONE object
// from consuming that whole budget and evicting the rest of the evidence.
const maxSpecBytes = 8 << 10

// refusedKinds are never readable through this path, by kind rather than by redaction.
//
// Secret is the obvious one and redaction is NOT considered sufficient for it: a Secret
// IS the payload, so a redactor missing one pattern leaks the whole object, whereas for
// every other kind a credential is an incidental field. Refusing by kind fails closed;
// relying on redaction fails open.
var refusedKinds = map[string]string{
	"secret": "Secret objects are never readable through this tool — a Secret is entirely " +
		"sensitive, so refusing the kind fails closed where redaction would fail open. " +
		"Read the workload that CONSUMES it instead (its spec names the keys it mounts).",
}

// resourceDiscoverer is the slice of discovery this reader needs. Narrow on purpose: it
// keeps the reader unit-testable without a live API server, and ServerPreferredResources
// (rather than the full group/version listing) means one preferred version per
// group-resource, so a kind served at several versions cannot be ambiguous here — only a
// kind served by several GROUPS can, which is a genuine ambiguity worth reporting.
type resourceDiscoverer interface {
	ServerPreferredResources() ([]*metav1.APIResourceList, error)
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
	namespaced bool
}

// resolveKind finds every served resource whose Kind matches, case-insensitively.
//
// Returning ALL matches rather than the first is the honest choice: "Ingress" is served by
// both networking.k8s.io and (historically) extensions, and silently picking one would be
// the same class of quiet wrongness this tool exists to remove. Subresources are skipped —
// "pods/log" is not an object you can read a spec from.
func (r *SpecReader) resolveKind(kind string) ([]resolved, error) {
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
				namespaced: ar.Namespaced,
			})
		}
	}
	return out, nil
}

var _ providers.ResourceSpecReader = (*SpecReader)(nil)

// ResourceSpec reads w's .spec and .status.
//
// The outcome taxonomy is the point of this function, not an implementation detail.
// Four endings, and only ONE of them is evidence that the object is absent:
//
//	found        — read it
//	absent       — the API server says no such object
//	forbidden    — RBAC denied the read; the object may well exist
//	kind_unknown — this cluster serves no such kind; says nothing about any object
//
// Flattening the last two into "not found" is exactly the defect that made
// gitops_resource_status produce a confidently wrong root cause, so they are kept
// distinct all the way out to the model.
func (r *SpecReader) ResourceSpec(ctx context.Context, w providers.Workload) (providers.ResourceSpec, error) {
	out := providers.ResourceSpec{Workload: w}
	if w.Kind == "" || w.Name == "" {
		return out, fmt.Errorf("kind and name are required")
	}
	if why, refused := refusedKinds[strings.ToLower(w.Kind)]; refused {
		out.Outcome = providers.ResourceForbidden
		out.Detail = why
		return out, nil
	}
	matches, err := r.resolveKind(w.Kind)
	if err != nil {
		return out, err
	}
	switch len(matches) {
	case 0:
		out.Outcome = providers.ResourceKindUnknown
		out.Detail = fmt.Sprintf("this cluster serves no kind %q", w.Kind)
		return out, nil
	case 1:
	default:
		// Ambiguous: several API groups serve this Kind. Reporting it beats guessing —
		// reading the wrong object and describing it confidently is the failure this tool
		// exists to prevent, so the ambiguity is handed back with the candidates named.
		groups := make([]string, 0, len(matches))
		for _, m := range matches {
			groups = append(groups, m.gvr.GroupVersion().String())
		}
		out.Outcome = providers.ResourceKindUnknown
		out.Detail = fmt.Sprintf("kind %q is served by more than one API group (%s); "+
			"this is ambiguous, so nothing was read", w.Kind, strings.Join(groups, ", "))
		return out, nil
	}
	match := matches[0]
	var ri dynamic.ResourceInterface = r.client.Resource(match.gvr)
	if match.namespaced {
		if w.Namespace == "" {
			return out, fmt.Errorf("kind %q is namespaced: namespace is required", w.Kind)
		}
		ri = r.client.Resource(match.gvr).Namespace(w.Namespace)
	}
	obj, err := ri.Get(ctx, w.Name, metav1.GetOptions{})
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		out.Outcome = providers.ResourceAbsent
		out.Detail = "the API server reports no such object"
		return out, nil
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		// RBAC is the boundary for this tool, exactly as for pod_logs — and a denial
		// must never be rendered as absence. Whatever the ClusterRole grants is what is
		// readable; anything else says nothing about whether the object exists.
		out.Outcome = providers.ResourceForbidden
		out.Detail = strings.TrimSpace(err.Error())
		return out, nil
	default:
		var status apierrors.APIStatus
		if errors.As(err, &status) {
			return out, fmt.Errorf("get %s %s/%s: %s", w.Kind, w.Namespace, w.Name, status.Status().Message)
		}
		return out, fmt.Errorf("get %s %s/%s: %w", w.Kind, w.Namespace, w.Name, err)
	}
	out.Outcome = providers.ResourceFound
	// Report the version the read actually used. A kind can be served at several
	// versions, so "which one answered" is part of reading the spec honestly.
	out.APIVersion = match.gvr.GroupVersion().String()
	out.Spec = renderSection(obj, "spec")
	out.Status = renderSection(obj, "status")
	return out, nil
}

// renderSection returns one top-level field as YAML, bounded. Missing is "" rather than
// an error: plenty of kinds legitimately have no spec (ConfigMap) or no status yet.
func renderSection(obj *unstructured.Unstructured, field string) string {
	v, found, err := unstructured.NestedFieldNoCopy(obj.Object, field)
	if err != nil || !found || v == nil {
		return ""
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	s := strings.TrimRight(string(b), "\n")
	if len(s) > maxSpecBytes {
		// Cut on a line boundary so the result stays parseable-looking YAML rather than
		// ending mid-key, which reads as corruption instead of truncation.
		cut := strings.LastIndexByte(s[:maxSpecBytes], '\n')
		if cut <= 0 {
			cut = maxSpecBytes
		}
		s = s[:cut] + "\n… (truncated)"
	}
	return s
}
