// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// resolution is the outcome of turning a bare Kind (plus an optional API group) into the
// one served resource to act on.
//
// It is shared by the by-name reader and the lister so the two cannot drift: a Kind that
// is ambiguous for resource_spec must be ambiguous for list_resources, and a refusal that
// fails closed for one must fail closed for the other. Duplicating this logic would let
// them disagree, and the disagreement would be a security-relevant one — the second
// refusal check below is what catches a `secrets` resource served under some other Kind.
type resolution struct {
	match resolved
	// outcome is empty when resolution succeeded; otherwise it is the terminal outcome to
	// report, with detail carrying the server-facing explanation.
	outcome providers.ResourceSpecOutcome
	detail  string
}

// resolveOne resolves a bare Kind to exactly one served resource, or explains why it could
// not. group narrows to a single API group ("" means no preference).
func (r *SpecReader) resolveOne(kind, group string) (resolution, error) {
	if why, refused := refusalFor(kind); refused {
		return resolution{outcome: providers.ResourceRefused, detail: why}, nil
	}
	matches, err := r.resolveKind(kind)
	if err != nil {
		return resolution{}, err
	}
	if group != "" {
		kept := matches[:0]
		for _, m := range matches {
			if matchGroup(m, group) {
				kept = append(kept, m)
			}
		}
		matches = kept
	}
	switch len(matches) {
	case 0:
		detail := fmt.Sprintf("this cluster serves no kind %q", kind)
		if group != "" {
			detail += fmt.Sprintf(" in API group %q", group)
		}
		return resolution{outcome: providers.ResourceKindUnknown, detail: detail}, nil
	case 1:
	default:
		// Ambiguous: several API groups serve this Kind. Reporting it beats guessing —
		// acting on the wrong group's objects and naming them confidently is the failure
		// this family of tools exists to prevent. The candidates are named, and so is the
		// argument that resolves it, so the ambiguity is not a dead end.
		groups := make([]string, 0, len(matches))
		for _, m := range matches {
			groups = append(groups, m.gvr.GroupVersion().String())
		}
		return resolution{
			outcome: providers.ResourceKindAmbiguous,
			detail: fmt.Sprintf("kind %q is served by more than one API group (%s); "+
				"nothing was read. Call again with the group argument set to the one you mean",
				kind, strings.Join(groups, ", ")),
		}, nil
	}
	match := matches[0]
	// Refuse AGAIN, on what resolution actually produced. The pre-check only ever sees the
	// caller's spelling of the KIND, so it cannot refuse a Secret reached under a Kind of
	// somebody else's choosing — which is what an aggregated API server or a CRD serving a
	// `secrets` resource does.
	if why, refused := refusedResources[match.gvr.Resource]; refused {
		return resolution{outcome: providers.ResourceRefused, detail: why}, nil
	}
	return resolution{match: match}, nil
}
