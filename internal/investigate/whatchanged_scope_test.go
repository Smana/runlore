// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// lookupGitOps is a provider that implements the optional ChangesLookupReporter, so a test
// can drive each reason the real providers can report.
type lookupGitOps struct {
	fakeGitOps // Diff + WatchFailures; its `changes` is the answer for both entry points
	lk         providers.Lookup
	// errOnCall fails the Nth ChangesLookup only (1-based, 0 = never). The reason read is
	// a SECOND call after Call's own enumeration came back empty, so failing every call
	// would test the pre-existing "Changes errored" path instead of this one.
	errOnCall int
	err       error
	calls     int
}

func (f *lookupGitOps) ChangesLookup(context.Context, providers.TimeWindow, providers.Selector) ([]providers.Change, providers.Lookup, error) {
	f.calls++
	if f.err != nil && f.calls == f.errOnCall {
		return nil, providers.Lookup{Reason: providers.LookupFailed}, f.err
	}
	return f.changes, f.lk, nil
}

func (f *lookupGitOps) Changes(ctx context.Context, w providers.TimeWindow, sel providers.Selector) ([]providers.Change, error) {
	c, _, err := f.ChangesLookup(ctx, w, sel)
	return c, err
}

// assertRefusesAbsence checks the SHAPE of the claim rather than blocklisting phrasings.
// gitops_kinds.go records why: a forbidden-word list shipped there once and "no such object
// exists" slipped straight past it, so the package standardised on asserting the disclaimer
// marker is PRESENT.
func assertRefusesAbsence(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "says NOTHING about whether the configuration changed") {
		t.Errorf("output does not carry the not-a-config-change disclaimer:\n%s", out)
	}
	// The one literal worth pinning: this exact string is what the model quoted as evidence.
	if strings.Contains(out, "no changes found") {
		t.Errorf("output still carries the shipped absence claim:\n%s", out)
	}
}

func callUnresolved(t *testing.T, gp providers.GitOpsProvider, args string) string {
	t.Helper()
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call(%s): %v", args, err)
	}
	return out
}

const unresolvedArgs = `{"namespace":"observability","name":"vmagent-vmagent-0"}`

// TestWhatChangedEmptyIsReportedPerReason is the regression pin: each reason the provider
// can establish gets its own answer, none of them claims the config is unchanged, and the
// two that matter most are distinguishable from a plain absence.
func TestWhatChangedEmptyIsReportedPerReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lk     providers.Lookup
		want   []string
		reject []string
	}{{
		name: "absent — nothing matched the selector",
		lk:   providers.Lookup{Reason: providers.LookupAbsent, Scopes: []string{"observability", providers.AllNamespaces}},
		// Names the scopes actually completed, and says an unmanaged cluster looks like this.
		want: []string{"no GitOps object matched", "observability", "all namespaces", "does not manage"},
	}, {
		name: "denied — a source read was refused and never ran",
		lk:   providers.Lookup{Reason: providers.LookupDenied, Scopes: []string{"observability"}},
		want: []string{"DENIED by RBAC", "never ran", "fix the grant"},
		// A denial must never read as an absence: that is #503's exact mechanism.
		reject: []string{"no GitOps object matched"},
	}, {
		name: "undiffable — the object EXISTS but has no resolvable source",
		lk:   providers.Lookup{Reason: providers.LookupUndiffable, Scopes: []string{"observability"}},
		// The object is a lead, not an absence — it must point at inspecting it.
		want:   []string{"DID match a GitOps object", "never reconciled", "gitops_resource_status"},
		reject: []string{"no GitOps object matched"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			out := callUnresolved(t, &lookupGitOps{lk: tc.lk}, unresolvedArgs)
			assertRefusesAbsence(t, out)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(out, r) {
					t.Errorf("output wrongly reads as a plain absence (%q):\n%s", r, out)
				}
			}
			// Always attributable to what was asked.
			if !strings.Contains(out, "vmagent-vmagent-0") {
				t.Errorf("output does not name the selector it answered for:\n%s", out)
			}
		})
	}
}

// TestWhatChangedScopesAreNotInvented pins the half of #503 that shipped a false message:
// with no scopes recorded, the answer must stay silent about scope rather than name a
// search that never happened.
func TestWhatChangedScopesAreNotInvented(t *testing.T) {
	out := callUnresolved(t, &lookupGitOps{lk: providers.Lookup{Reason: providers.LookupAbsent}}, unresolvedArgs)
	assertRefusesAbsence(t, out)
	for _, invented := range []string{"all namespaces", "flux-system"} {
		if strings.Contains(out, invented) {
			t.Errorf("output names scope %q the provider never recorded:\n%s", invented, out)
		}
	}
}

// TestWhatChangedWithoutTheCapabilityClaimsNoReason: a provider that does not implement
// ChangesLookupReporter established nothing, so the answer must not pick a reason for it.
func TestWhatChangedWithoutTheCapabilityClaimsNoReason(t *testing.T) {
	out := callUnresolved(t, fakeGitOps{}, `{"namespace":"observability"}`)
	assertRefusesAbsence(t, out)
	for _, r := range []string{"DENIED by RBAC", "DID match a GitOps object"} {
		if strings.Contains(out, r) {
			t.Errorf("output claims reason %q with no provider support:\n%s", r, out)
		}
	}
}

// TestWhatChangedLookupErrorDoesNotFailTheInvestigation: recovering the reason is an aid,
// never a gate. Its failure must neither fail the tool nor fall back to the absence claim.
func TestWhatChangedLookupErrorDoesNotFailTheInvestigation(t *testing.T) {
	gp := &lookupGitOps{errOnCall: 2, err: errors.New("argocd api unreachable")}
	out := callUnresolved(t, gp, unresolvedArgs)
	assertRefusesAbsence(t, out)
	if gp.calls != 2 {
		t.Fatalf("want the reason read to have been attempted (2 calls), got %d", gp.calls)
	}
	// No reason may be claimed: the read that would have established one failed.
	for _, r := range []string{"DENIED by RBAC", "DID match a GitOps object"} {
		if strings.Contains(out, r) {
			t.Errorf("output claims reason %q after the reason read failed:\n%s", r, out)
		}
	}
}

// TestWhatChangedResolvedSelectorIsUnaffected guards the happy path.
//
// It does NOT assert the absence of "no GitOps object": the pre-existing B2 note prints
// exactly that ("note: no GitOps object in namespace %q; matched by name across
// namespaces") whenever a name resolves in another namespace, which is the standard Flux
// bootstrap shape. Pin the sentences this change introduced instead.
func TestWhatChangedResolvedSelectorIsUnaffected(t *testing.T) {
	gp := &lookupGitOps{fakeGitOps: fakeGitOps{changes: []providers.Change{{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Engine:   providers.EngineFlux, Type: providers.ChangeSync, FromRev: "aaa", ToRev: "bbb",
	}}}}
	out := callUnresolved(t, gp, `{"namespace":"flux-system","name":"apps"}`)
	for _, r := range []string{"says NOTHING about whether", "No repository was searched"} {
		if strings.Contains(out, r) {
			t.Errorf("resolved selector must not carry the unresolved answer (%q):\n%s", r, out)
		}
	}
	// One enumeration, not two: the reason is only read when the list came back empty.
	if gp.calls != 1 {
		t.Errorf("resolved selector made %d enumerations, want 1", gp.calls)
	}
}

// TestWhatChangedDescriptionNamesTheArgumentAccurately pins the other half of the defect.
// The shipped description said "optionally a named workload", but `name` matches the GitOps
// OBJECT, so a pod name carrying a hash or ordinal cannot match. It must also not
// over-correct: a BARE workload name frequently DOES match, and both providers retry a name
// across namespaces for exactly that case, so claiming workload names never match would
// lose that narrowing.
func TestWhatChangedDescriptionNamesTheArgumentAccurately(t *testing.T) {
	d := WhatChangedTool{}.Description()
	if strings.Contains(strings.ToLower(d), "named workload") {
		t.Errorf("description invites a workload name for `name`, which cannot match:\n%s", d)
	}
	if strings.Contains(d, "never match") && !strings.Contains(d, "suffix") {
		t.Errorf("description claims workload names never match, losing the bare-name retry:\n%s", d)
	}
	for _, want := range []string{"Application", "Kustomization", "NEVER a pod", "suffix"} {
		if !strings.Contains(d, want) {
			t.Errorf("description missing %q:\n%s", want, d)
		}
	}
}

// TestTimelineEmptyDoesNotClaimNoGitOpsChanges: incident_timeline is the OTHER Changes
// consumer, and it printed "(no dated GitOps/cloud changes and no Warning events)" — the
// same claim about the world for a namespace the engine may not manage. The what_changed
// guard is file-local and would never have caught it.
func TestTimelineEmptyDoesNotClaimNoGitOpsChanges(t *testing.T) {
	gp := &lookupGitOps{lk: providers.Lookup{Reason: providers.LookupDenied, Scopes: []string{"observability"}}}
	out, err := IncidentTimelineTool{GitOps: gp}.Call(context.Background(), `{"namespace":"observability"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "DENIED by RBAC") {
		t.Errorf("timeline did not report what the gitops enumeration established:\n%s", out)
	}
	assertRefusesAbsence(t, out)
}

// TestTimelineUndatedChangesAreNotSilentlyDropped: the timeline needs a wall-clock anchor,
// so a change with no When cannot be a row — but dropping it silently is how "no dated
// GitOps changes" gets printed while the provider DID return changes.
func TestTimelineUndatedChangesAreNotSilentlyDropped(t *testing.T) {
	gp := &lookupGitOps{fakeGitOps: fakeGitOps{changes: []providers.Change{{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Engine:   providers.EngineFlux, ToRev: "bbb", // When deliberately zero
	}}}}
	out, err := IncidentTimelineTool{GitOps: gp}.Call(context.Background(), `{"namespace":"flux-system"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "carry no timestamp") {
		t.Errorf("timeline dropped an undated change without saying so:\n%s", out)
	}
}
