package investigate

import (
	"context"
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
		// The KEY fix: the model targets broken-app in "apps" (workload view) while it
		// was observed in "flux-system" (object view) — the name fallback matches.
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

// TestDowngradeUnobservedTargets is the F2 core: an executable action on an observed
// target survives; one on an unobserved (possibly injected) target is stripped of its
// Op so it can't reach the approve/auto path; a non-executable suggestion is untouched.
func TestDowngradeUnobservedTargets(t *testing.T) {
	ctx := WithObservedResources(context.Background())
	recordObserved(ctx, providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "broken-app"})

	actions := []providers.Action{
		{Op: "suspend", Mutating: true, Target: providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "broken-app"}}, // observed → keep
		{Op: "suspend", Mutating: true, Target: providers.Workload{Kind: "Kustomization", Namespace: "apps", Name: "evil-app"}},   // NOT observed → downgrade
		{Op: "", Target: providers.Workload{Namespace: "apps", Name: "note"}},                                                     // already a suggestion
	}
	out := downgradeUnobservedTargets(ctx, actions, nil)

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

func TestDowngradeNoCollectorIsNoop(t *testing.T) {
	actions := []providers.Action{{Op: "suspend", Mutating: true, Target: providers.Workload{Namespace: "apps", Name: "web"}}}
	out := downgradeUnobservedTargets(context.Background(), actions, nil) // no collector in ctx
	if out[0].Op != "suspend" {
		t.Fatalf("without a collector the actions must be unchanged, got %+v", out[0])
	}
}
