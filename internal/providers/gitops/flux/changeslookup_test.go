// SPDX-License-Identifier: Apache-2.0

package flux

import (
	"context"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

// forbiddenReader answers GetGitRepository with a real RBAC Forbidden, which is the case
// this whole restructure exists for: changesFor used to `continue` past it with a bare
// error check, so a refused read arrived at the tool indistinguishable from a benign skip
// and was reported as "nothing with a resolvable source" — a denial rendered as an absence,
// which is precisely runlore#503's mechanism.
type forbiddenReader struct{ fakeReader }

func (f forbiddenReader) GetGitRepository(_ context.Context, _, name string) (gitRepository, error) {
	return gitRepository{}, apierrors.NewForbidden(
		schema.GroupResource{Group: "source.toolkit.fluxcd.io", Resource: "gitrepositories"},
		name, nil)
}

func TestChangesLookupReportsWhatItEstablished(t *testing.T) {
	diffable := kustomization{
		Name: "apps", Namespace: "flux-system", Path: "./apps",
		SourceName: "flux-system", SourceNamespace: "flux-system", Revision: "main@sha1:abc123",
	}
	grs := map[string]gitRepository{"flux-system/flux-system": {URL: "https://github.com/org/repo"}}

	for _, tc := range []struct {
		name       string
		reader     Reader
		sel        providers.Selector
		wantReason providers.LookupReason
		wantScopes []string
	}{{
		name:       "nothing matched the namespace",
		reader:     fakeReader{ks: []kustomization{diffable}, grs: grs},
		sel:        providers.Selector{Namespace: "observability"},
		wantReason: providers.LookupAbsent,
		wantScopes: []string{"observability"},
	}, {
		name: "matched but never reconciled — the object EXISTS",
		reader: fakeReader{ks: []kustomization{{
			Name: "vmagent", Namespace: "observability", Path: "./vmagent",
			SourceName: "flux-system", SourceNamespace: "flux-system", Revision: "", // never applied
		}}, grs: grs},
		sel:        providers.Selector{Namespace: "observability"},
		wantReason: providers.LookupUndiffable,
		wantScopes: []string{"observability"},
	}, {
		name: "matched but the source read was REFUSED — never an absence",
		reader: forbiddenReader{fakeReader{ks: []kustomization{{
			Name: "vmagent", Namespace: "observability", Path: "./vmagent",
			SourceName: "flux-system", SourceNamespace: "flux-system", Revision: "main@sha1:abc",
		}}}},
		sel:        providers.Selector{Namespace: "observability"},
		wantReason: providers.LookupDenied,
		wantScopes: []string{"observability"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(tc.reader, &whatchanged.Differ{})
			changes, lk, err := p.ChangesLookup(context.Background(), providers.TimeWindow{}, tc.sel)
			if err != nil {
				t.Fatalf("ChangesLookup: %v", err)
			}
			if len(changes) != 0 {
				t.Fatalf("want an empty change list to exercise the Lookup, got %d", len(changes))
			}
			if lk.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", lk.Reason, tc.wantReason)
			}
			if !slices.Equal(lk.Scopes, tc.wantScopes) {
				t.Errorf("Scopes = %v, want %v", lk.Scopes, tc.wantScopes)
			}
		})
	}
}

// TestDeniedIsNeverDowngraded pins the precedence. A namespace holding BOTH a refused
// source and a merely-undiffable object must report the denial: a search that never ran
// cannot be reported as one that came back empty, whatever else the loop saw afterwards.
func TestDeniedIsNeverDowngraded(t *testing.T) {
	r := forbiddenReader{fakeReader{ks: []kustomization{
		{Name: "refused", Namespace: "observability", Path: "./a", SourceName: "src", SourceNamespace: "observability", Revision: "main@sha1:abc"},
		{Name: "never-applied", Namespace: "observability", Path: "./b", SourceName: "src", SourceNamespace: "observability", Revision: ""},
	}}}
	p := New(r, &whatchanged.Differ{})
	_, lk, err := p.ChangesLookup(context.Background(), providers.TimeWindow{}, providers.Selector{Namespace: "observability"})
	if err != nil {
		t.Fatalf("ChangesLookup: %v", err)
	}
	if lk.Reason != providers.LookupDenied {
		t.Errorf("Reason = %q, want %q — a denial must outrank an undiffable skip", lk.Reason, providers.LookupDenied)
	}
}

// Compile-time assertion, not a test: consumers type-assert for this optional capability,
// so losing it would silently degrade what_changed and incident_timeline to the neutral
// answer rather than fail anything.
var _ providers.ChangesLookupReporter = (*Provider)(nil)
