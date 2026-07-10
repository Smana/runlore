// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestObservedResourcesMatching(t *testing.T) {
	ctx := WithObservedResources(context.Background(), providers.Workload{Namespace: "apps", Name: "web"})
	// A Flux Kustomization object lives in flux-system but manages "apps" — recorded
	// with its object namespace.
	recordObserved(ctx, providers.Workload{Kind: "Kustomization", Namespace: "flux-system", Name: "broken-app"})
	o := observedFrom(ctx)

	cases := []struct {
		name   string
		target providers.Workload
		want   bool
	}{
		{"exact ns+name", providers.Workload{Namespace: "apps", Name: "web"}, true},
		// The KEY fix over the first narrow attempt: the model targets broken-app in
		// "apps" (workload view) while it was observed in "flux-system" (object view)
		// — the name fallback matches.
		{"name fallback across namespaces", providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "broken-app"}, true},
		{"name only (no ns)", providers.Workload{Name: "web"}, true},
		{"unobserved name", providers.Workload{Namespace: "apps", Name: "evil"}, false},
		{"empty name", providers.Workload{Namespace: "apps"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := o.matches(tc.target); got != tc.want {
				t.Fatalf("matches(%+v) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

// TestRequestWorkloadAlwaysObserved locks the rule that keeps the e2e/auto path
// working: the investigation request's own workload (the alert/failure subject)
// counts as observed purely by being the trigger — no tool has to re-read it — so an
// action targeting it stays executable even under auto's strict downgrade.
func TestRequestWorkloadAlwaysObserved(t *testing.T) {
	// Exactly what Investigate does first thing: seed with req.Workload.
	reqWorkload := providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "broken-app"}
	ctx := WithObservedResources(context.Background(), reqWorkload)

	actions := []providers.Action{
		{Op: "suspend", Mutating: true, Description: "suspend the failing Kustomization", Target: reqWorkload},
	}
	out := guardUnobservedTargets(ctx, actions, true /* auto: the strictest mode */, nil)
	if out[0].Op != "suspend" || !out[0].Mutating {
		t.Fatalf("action targeting the request's own workload must stay executable, got %+v", out[0])
	}
	if strings.Contains(out[0].Description, "WARNING") {
		t.Fatalf("action targeting the request's own workload must not be flagged, got %q", out[0].Description)
	}
}

// TestGuardUnobservedTargetsAuto is the F2 core for rung 3 (no human in the loop): an
// executable action on an observed target survives; one on an unobserved (possibly
// injected) target is stripped of its Op so it can never auto-execute; an existing
// suggestion is untouched.
func TestGuardUnobservedTargetsAuto(t *testing.T) {
	ctx := WithObservedResources(context.Background())
	recordObserved(ctx, providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "broken-app"})

	actions := []providers.Action{
		{Op: "suspend", Mutating: true, Target: providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "broken-app"}}, // observed → keep
		{Op: "suspend", Mutating: true, Target: providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "evil-app"}},   // NOT observed → downgrade
		{Op: "", Target: providers.Workload{Namespace: "apps", Name: "note"}},                                                     // already a suggestion
	}
	out := guardUnobservedTargets(ctx, actions, true, nil)

	if out[0].Op != "suspend" || !out[0].Mutating {
		t.Fatalf("observed-target action should be unchanged, got %+v", out[0])
	}
	if out[1].Op != "" || out[1].Mutating {
		t.Fatalf("unobserved-target action should be downgraded to a non-executable suggestion, got %+v", out[1])
	}
	if out[2].Op != "" || out[2].Target.Name != "note" {
		t.Fatalf("existing suggestion should be untouched, got %+v", out[2])
	}
}

// TestGuardUnobservedTargetsApprove locks the human-gated failure mode that prevents
// the rung-2 e2e regression: under approve/suggest an unobserved target is NOT
// downgraded — it stays executable (so it still registers for approval and an
// approved execution still works) but its description carries the explicit
// unobserved-target warning for the approver. Observed targets are never flagged.
func TestGuardUnobservedTargetsApprove(t *testing.T) {
	ctx := WithObservedResources(context.Background())
	recordObserved(ctx, providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "broken-app"})

	actions := []providers.Action{
		{Op: "suspend", Mutating: true, Description: "suspend broken-app", Target: providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "broken-app"}},
		{Op: "suspend", Mutating: true, Description: "suspend evil-app", Target: providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "evil-app"}},
	}
	out := guardUnobservedTargets(ctx, actions, false /* approve/suggest */, nil)

	if out[0].Op != "suspend" || strings.Contains(out[0].Description, "WARNING") {
		t.Fatalf("observed-target action should be unchanged, got %+v", out[0])
	}
	if out[1].Op != "suspend" || !out[1].Mutating {
		t.Fatalf("under a human gate the unobserved-target action must stay executable, got %+v", out[1])
	}
	if !strings.HasPrefix(out[1].Description, unobservedTargetWarning) {
		t.Fatalf("unobserved-target action should carry the approver warning, got %q", out[1].Description)
	}
}

func TestGuardNoCollectorIsNoop(t *testing.T) {
	actions := []providers.Action{{Op: "suspend", Mutating: true, Target: providers.Workload{Namespace: "apps", Name: "web"}}}
	out := guardUnobservedTargets(context.Background(), actions, true, nil) // no collector in ctx
	if out[0].Op != "suspend" {
		t.Fatalf("without a collector the actions must be unchanged, got %+v", out[0])
	}
}
