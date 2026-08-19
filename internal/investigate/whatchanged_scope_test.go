// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// scopedGitOps answers Changes() per selector, so a test can model the shape that
// caused the incident: a name that resolves to no GitOps object in a namespace that
// may or may not be managed at all. It records every selector it was asked for,
// because the number of provider calls is part of the contract here — the probe must
// not fire when there is nothing to probe.
type scopedGitOps struct {
	fakeGitOps                     // Diff + WatchFailures; its `changes` is the by-name answer
	byNamespace []providers.Change // returned when sel.Name == ""
	nsErr       error              // returned instead of byNamespace
	seen        []providers.Selector
}

func (f *scopedGitOps) Changes(_ context.Context, _ providers.TimeWindow, sel providers.Selector) ([]providers.Change, error) {
	f.seen = append(f.seen, sel)
	if sel.Name != "" {
		return f.changes, nil
	}
	if f.nsErr != nil {
		return nil, f.nsErr
	}
	return f.byNamespace, nil
}

// assertRefusesAbsence checks the SHAPE of the claim rather than blocklisting phrasings.
// gitops_kinds.go records why: a forbidden-word list shipped here once and "no such object
// exists" slipped straight past it, so the package standardised on asserting that the
// disclaimer marker is PRESENT. Every unresolved answer must carry notAConfigStatement, and
// none may still carry the exact sentence that caused the incident.
func assertRefusesAbsence(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, notAConfigStatement) {
		t.Errorf("output does not carry the not-a-config-statement disclaimer:\n%s", out)
	}
	// The one literal worth pinning: this exact string is what the model quoted as evidence.
	if strings.Contains(out, "no changes found") {
		t.Errorf("output still carries the shipped absence claim:\n%s", out)
	}
}

// TestWhatChangedUnresolvedSelectorDoesNotClaimAbsence is the regression pin. The
// selector resolves to no GitOps object AND the namespace holds none either, so no
// repository was searched — the tool must say that rather than answer a question
// about Git history it never asked.
func TestWhatChangedUnresolvedSelectorDoesNotClaimAbsence(t *testing.T) {
	gp := &scopedGitOps{}
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(),
		`{"namespace":"observability","name":"vmagent-vmagent-0"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	assertRefusesAbsence(t, out)
	// It must state the scope outcome: nothing resolved, so nothing was searched.
	// "repository was searched" without the leading article: notAConfigStatement opens a
	// sentence with it, so pinning the lowercase form would break on capitalisation alone.
	for _, want := range []string{"no GitOps object", "repository was searched"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// And it must name what it was asked about, so the answer is attributable.
	if !strings.Contains(out, "vmagent-vmagent-0") || !strings.Contains(out, "observability") {
		t.Errorf("output does not name the selector it answered for:\n%s", out)
	}
}

// TestWhatChangedUnresolvedNameListsWhatTheNamespaceDoesHave is the recover-don't-guess
// half: when the namespace IS managed, the name simply was not a GitOps object name
// (a pod name never is), so the reply lists the objects that do exist there rather
// than dead-ending. Mirrors alert_rule's unmatched-alertname list.
func TestWhatChangedUnresolvedNameListsWhatTheNamespaceDoesHave(t *testing.T) {
	gp := &scopedGitOps{byNamespace: []providers.Change{
		{Workload: providers.Workload{Kind: "Application", Name: "monitoring", Namespace: "argocd"}, Engine: providers.EngineArgoCD},
		{Workload: providers.Workload{Kind: "Application", Name: "essentials", Namespace: "argocd"}, Engine: providers.EngineArgoCD},
	}}
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(),
		`{"namespace":"observability","name":"vmagent-vmagent-0"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	assertRefusesAbsence(t, out)
	for _, want := range []string{"monitoring", "essentials", "vmagent-vmagent-0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The distinction that matters: the namespace resolved, the NAME did not.
	if !strings.Contains(out, "did not resolve") {
		t.Errorf("output does not say the name failed to resolve:\n%s", out)
	}
}

// TestWhatChangedNamespaceOnlySelectorDoesNotReprobe pins the call count. With no
// name there is nothing to narrow, so the namespace-only probe would re-ask the
// identical question — a wasted provider round-trip on every empty namespace lookup.
func TestWhatChangedNamespaceOnlySelectorDoesNotReprobe(t *testing.T) {
	gp := &scopedGitOps{}
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(), `{"namespace":"observability"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	assertRefusesAbsence(t, out)
	if len(gp.seen) != 1 {
		t.Errorf("namespace-only selector made %d provider calls, want 1: %+v", len(gp.seen), gp.seen)
	}
}

// TestWhatChangedProbeFailureStillRefusesAbsence: the probe is a best-effort aid, so
// its failure must degrade to the scope statement rather than either erroring the
// investigation or falling back to the absence claim it exists to remove.
func TestWhatChangedProbeFailureStillRefusesAbsence(t *testing.T) {
	gp := &scopedGitOps{nsErr: errors.New("argocd api unreachable")}
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(),
		`{"namespace":"observability","name":"vmagent-vmagent-0"}`)
	if err != nil {
		t.Fatalf("Call must not fail the investigation on a probe error: %v", err)
	}
	assertRefusesAbsence(t, out)
	if !strings.Contains(out, "no GitOps object") {
		t.Errorf("output missing the scope statement:\n%s", out)
	}
	// The probe-error branch alone must NOT assert a cause: whether the namespace is
	// managed is exactly what the failed probe left unestablished.
	if strings.Contains(out, unresolvedCauses) {
		t.Errorf("probe-error branch names causes it did not establish:\n%s", out)
	}
}

// TestWhatChangedResolvedSelectorIsUnaffected guards the happy path: when the
// selector does resolve, none of the above wording appears and no probe is issued.
func TestWhatChangedResolvedSelectorIsUnaffected(t *testing.T) {
	gp := &scopedGitOps{fakeGitOps: fakeGitOps{changes: []providers.Change{{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Engine:   providers.EngineFlux, Type: providers.ChangeSync, FromRev: "aaa", ToRev: "bbb",
	}}}}
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(),
		`{"namespace":"flux-system","name":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// NOT "must not contain 'no GitOps object'" — the pre-existing B2 note prints exactly
	// that ("note: no GitOps object in namespace %q; matched by name across namespaces")
	// whenever a name resolves in another namespace, which is the standard Flux bootstrap
	// shape. Pin the sentences this change introduced instead.
	if strings.Contains(out, notAConfigStatement) || strings.Contains(out, "repository was searched") {
		t.Errorf("resolved selector must not carry the unresolved-selector answer:\n%s", out)
	}
	if len(gp.seen) != 1 {
		t.Errorf("resolved selector made %d provider calls, want 1", len(gp.seen))
	}
}

// TestWhatChangedDescriptionDoesNotInviteAWorkloadName pins the other half of the
// defect. The shipped description said "optionally a named workload", but `name` is
// matched against the GitOps OBJECT — an Application or a Kustomization — which is
// never named after the pod it renders. A model that believes it may pass a workload
// name will, and the empty result it gets back is what the absence claim was built on.
func TestWhatChangedDescriptionDoesNotInviteAWorkloadName(t *testing.T) {
	d := WhatChangedTool{}.Description()
	if strings.Contains(strings.ToLower(d), "named workload") {
		t.Errorf("description invites a workload name for `name`, which cannot match:\n%s", d)
	}
	// It must say what the argument IS, and that a pod name is not it.
	// And it must not over-correct the other way: a BARE workload name often does match,
	// so the claim has to be about the suffix, not about workload names.
	if strings.Contains(d, "never match") && !strings.Contains(d, "suffix") {
		t.Errorf("description claims workload names never match, losing the bare-name retry:\n%s", d)
	}
	for _, want := range []string{"Application", "Kustomization", "NEVER a pod", "suffix"} {
		if !strings.Contains(d, want) {
			t.Errorf("description missing %q:\n%s", want, d)
		}
	}
}

// TestWhatChangedPeerListIsStableAndDeduped: several in-window revisions of one object
// arrive as several Changes, and the model is being handed names to re-call with — not
// a change count. Duplicates would read as several distinct objects.
func TestWhatChangedPeerListIsStableAndDeduped(t *testing.T) {
	rev := func(name, to string) providers.Change {
		return providers.Change{
			Workload: providers.Workload{Kind: "Application", Name: name, Namespace: "argocd"},
			Engine:   providers.EngineArgoCD, ToRev: to,
		}
	}
	gp := &scopedGitOps{byNamespace: []providers.Change{
		rev("monitoring", "c"), rev("monitoring", "b"), rev("essentials", "a"), rev("monitoring", "a"),
	}}
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(),
		`{"namespace":"observability","name":"vmagent-vmagent-0"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if n := strings.Count(out, "Application argocd/monitoring"); n != 1 {
		t.Errorf("monitoring listed %d times, want 1 (three revisions are one object):\n%s", n, out)
	}
	// Sorted, so the rendering does not change between identical calls.
	if strings.Index(out, "argocd/essentials") > strings.Index(out, "argocd/monitoring") {
		t.Errorf("peer list is not sorted:\n%s", out)
	}
}
