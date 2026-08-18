// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"

	"github.com/Smana/runlore/internal/providers"
)

var _ providers.KindScoper = (*SpecReader)(nil)

// KindScope reports whether kind is namespaced on this cluster, from the API server's
// own discovery — the same ServerPreferredResources listing ResourceSpec resolves
// against, whose APIResource.Namespaced is authoritative for every served kind,
// including CRDs no compiled-in table could enumerate.
//
// It is the supply side of Workload.Scope: without it, downstream code that needs to
// know whether an object HAS a namespace has to infer it from the kind's name, and no
// list of names can be right for arbitrary CRDs.
//
// Unknown, not a guess, in three cases — this is the contract, not a fallback:
//
//   - no served resource matches the kind. The cluster may simply not run the operator,
//     or the "kind" may name something outside Kubernetes entirely (an RDS instance, a
//     CloudTrail resource type). Discovery cannot tell those apart and does not try.
//   - two API groups serve the kind and DISAGREE about namespacing. A bare kind carries
//     no group, so nothing here can say which was meant; picking one is exactly the
//     quiet wrongness ResourceSpec refuses to commit for the same ambiguity. Groups that
//     AGREE do answer — that is knowledge, and withholding it would buy nothing.
//   - the kind is empty. Nothing was asked, so nothing is known.
//
// Unlike ResourceSpec's resolution it does NOT invalidate and re-fetch discovery on a
// miss. A miss here is the common case — every non-Kubernetes resource RunLore reasons
// about misses — and it is not a dead end: the answer is "unknown", the caller keeps
// whatever it had, and nothing is reported to a model as a fact about the cluster. That
// makes a full discovery fan-out per miss a cost with no matching benefit. The
// consequence is stated rather than hidden: a CRD installed AFTER this process's
// discovery cache was filled reads as unknown until something else invalidates it.
func (r *SpecReader) KindScope(_ context.Context, kind string) (providers.ResourceScope, error) {
	if kind == "" {
		return providers.ScopeUnknown, nil
	}
	matches, err := r.resolveKindCached(kind)
	if err != nil {
		return providers.ScopeUnknown, err
	}
	scope := providers.ScopeUnknown
	for _, m := range matches {
		got := providers.ScopeClusterScoped
		if m.namespaced {
			got = providers.ScopeNamespaced
		}
		if scope != providers.ScopeUnknown && scope != got {
			return providers.ScopeUnknown, nil // groups disagree; a bare kind cannot choose
		}
		scope = got
	}
	return scope, nil
}
