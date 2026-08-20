// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/Smana/runlore/internal/providers"
)

// maxListItems bounds one listing. Identities are small (a name and a namespace), but a
// busy namespace has thousands of ConfigMaps or Secrets-adjacent objects, and the
// investigation loop's global tool-output cap would then be spent enumerating one kind.
//
// The cap is disclosed rather than silent: a truncated listing says so, because a name
// missing from a capped page would otherwise be read as proof the object does not exist —
// reintroducing the exact wrong inference this lister removes.
const maxListItems = 200

var _ providers.ResourceLister = (*SpecReader)(nil)

// ResourceList enumerates the objects of q.Kind in q.Namespace.
//
// It shares SpecReader's resolution and refusals deliberately (see resolveOne): a kind
// that is ambiguous or refused for a by-name read must be ambiguous or refused here too.
//
// The outcome taxonomy is the by-name reader's, with one meaningful difference. For
// ResourceSpec only ABSENT is evidence about an object; here a successful listing with
// ZERO items is itself evidence — the cluster serves the kind, this agent may list it, and
// there are none. That is the answer a by-name reader structurally cannot produce, because
// it can only answer about names that were already guessed.
func (r *SpecReader) ResourceList(ctx context.Context, q providers.ResourceListQuery) (providers.ResourceList, error) {
	out := providers.ResourceList{Query: q}
	if q.Kind == "" {
		return out, fmt.Errorf("kind is required")
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
	// Echo the identity that was actually resolved, not the caller's spelling — and drop
	// the namespace for a cluster-scoped kind, so the answer never states a caller's
	// mistake ("StorageClass in made-up-ns") as a fact about the cluster.
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
	list, err := ri.List(ctx, metav1.ListOptions{
		LabelSelector: q.LabelSelector,
		Limit:         maxListItems,
	})
	switch {
	case err == nil:
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		// RBAC is the boundary. A denial must never be rendered as an empty namespace:
		// "may not list" and "there are none" are different facts, and only the second is
		// evidence about the cluster's contents.
		out.Outcome = providers.ResourceForbidden
		out.Detail = err.Error()
		return out, nil
	case apierrors.IsNotFound(err):
		// For a LIST there is no object to be absent, so a 404 is always about the path:
		// a CRD deleted since discovery ran, or an aggregated APIService that stopped
		// serving. Reporting that as "no objects" would be the #503 conflation again.
		out.Outcome = providers.ResourceKindUnknown
		out.Detail = fmt.Sprintf("the API server no longer serves %s (discovery may be stale); "+
			"nothing was listed", match.gvr.GroupResource().String())
		return out, nil
	default:
		return out, fmt.Errorf("list %s: %w", match.gvr.GroupResource().String(), err)
	}
	out.Outcome = providers.ResourceFound
	out.APIVersion = match.gvr.GroupVersion().String()
	items := list.Items
	// A continue token means the server had more than the page asked for. Trust it over
	// len(items): a server may return a short page for its own reasons.
	out.Truncated = list.GetContinue() != ""
	if len(items) > maxListItems {
		items = items[:maxListItems]
		out.Truncated = true
	}
	out.Items = make([]providers.ResourceListItem, 0, len(items))
	for i := range items {
		out.Items = append(out.Items, providers.ResourceListItem{
			Name:      items[i].GetName(),
			Namespace: items[i].GetNamespace(),
		})
	}
	return out, nil
}
