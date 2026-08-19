// SPDX-License-Identifier: Apache-2.0

package argocd

import (
	"context"
	"slices"
	"testing"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
)

// TestChangesLookupReportsWhatItEstablished pins the reason at the point it is DETERMINED.
// An empty change list used to be indistinguishable from any other empty change list, and
// what_changed rendered all of them as "no changes found" — a claim about Git in answer to
// a question about the tool's scope (runlore#503, one layer down).
func TestChangesLookupReportsWhatItEstablished(t *testing.T) {
	diffable := application{
		Name: "monitoring", Namespace: "argocd", RepoURL: "https://github.com/org/repo",
		Path: "apps/monitoring", Revision: "newsha", PrevRevision: "oldsha",
	}
	// Matches the selector but carries nothing to locate a source with — the Application
	// EXISTS, which is why this must not read as absence.
	undiffable := application{Name: "monitoring", Namespace: "argocd", RepoURL: "", Revision: ""}

	for _, tc := range []struct {
		name       string
		apps       []application
		sel        providers.Selector
		wantReason providers.LookupReason
		wantScopes []string
	}{{
		name:       "nothing matched the namespace",
		apps:       []application{diffable},
		sel:        providers.Selector{Namespace: "observability"},
		wantReason: providers.LookupAbsent,
		wantScopes: []string{"observability"},
	}, {
		name: "nothing matched the name, and the cluster-wide retry ran",
		apps: []application{diffable},
		// A pod name can never match an Application name; this is the incident's shape.
		sel:        providers.Selector{Namespace: "observability", Name: "vmagent-vmagent-0"},
		wantReason: providers.LookupAbsent,
		wantScopes: []string{"observability", providers.AllNamespaces},
	}, {
		name:       "matched, but no diffable source",
		apps:       []application{undiffable},
		sel:        providers.Selector{Namespace: "argocd"},
		wantReason: providers.LookupUndiffable,
		wantScopes: []string{"argocd"},
	}, {
		name:       "an empty selector searches all namespaces, and says so once",
		apps:       []application{undiffable},
		sel:        providers.Selector{},
		wantReason: providers.LookupUndiffable,
		wantScopes: []string{providers.AllNamespaces},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(fakeReader{apps: tc.apps}, &whatchanged.Differ{})
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

// TestChangesDelegatesToChangesLookup: Changes must stay a thin wrapper, so the two can
// never disagree about what the enumeration found.
func TestChangesDelegatesToChangesLookup(t *testing.T) {
	r := fakeReader{apps: []application{{
		Name: "harbor", Namespace: "argocd", RepoURL: "https://github.com/org/repo",
		Path: "apps/harbor", Revision: "newsha", PrevRevision: "oldsha",
	}}}
	p := New(r, &whatchanged.Differ{})
	viaChanges, err := p.Changes(context.Background(), providers.TimeWindow{}, providers.Selector{})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	viaLookup, _, err := p.ChangesLookup(context.Background(), providers.TimeWindow{}, providers.Selector{})
	if err != nil {
		t.Fatalf("ChangesLookup: %v", err)
	}
	if len(viaChanges) != len(viaLookup) || len(viaChanges) != 1 {
		t.Fatalf("Changes returned %d, ChangesLookup %d, want 1 each", len(viaChanges), len(viaLookup))
	}
}

// TestProviderImplementsChangesLookupReporter pins the optional capability, since consumers
// type-assert for it and a silent loss degrades what_changed to the neutral answer.
// Compile-time assertion, not a test: consumers type-assert for this optional capability,
// so losing it would silently degrade what_changed and incident_timeline to the neutral
// answer rather than fail anything.
var _ providers.ChangesLookupReporter = (*Provider)(nil)
