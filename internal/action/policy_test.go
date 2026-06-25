package action

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

func TestDisabledSurfacesNothing(t *testing.T) {
	p := New(config.ActionPolicy{}) // mode "" == off
	if p.Enabled() {
		t.Fatal("empty policy should be disabled")
	}
	kept, _ := p.Review([]providers.Action{{Description: "flux rollback", Reversible: true}})
	if kept != nil {
		t.Fatalf("disabled policy surfaced actions: %+v", kept)
	}
}

func TestReviewEnvelope(t *testing.T) {
	p := New(config.ActionPolicy{
		Mode: config.ActionSuggest,
		Allow: config.ActionAllow{
			ReversibleOnly: true,
			MaxBlastRadius: 5,
			Kinds:          []string{"HelmRelease", "Kustomization"},
		},
	})
	actions := []providers.Action{
		{Description: "flux rollback hr/harbor", Reversible: true, BlastRadius: 1, Target: providers.Workload{Kind: "HelmRelease"}},
		{Description: "delete pvc", Reversible: false, BlastRadius: 1, Target: providers.Workload{Kind: "PersistentVolumeClaim"}},
		{Description: "scale everything", Reversible: true, BlastRadius: 50, Target: providers.Workload{Kind: "Kustomization"}},
		{Description: "kubectl delete ns", Reversible: true, BlastRadius: 1, Target: providers.Workload{Kind: "Namespace"}},
	}
	kept, withheld := p.Review(actions)
	if len(kept) != 1 || kept[0].Description != "flux rollback hr/harbor" {
		t.Fatalf("kept = %+v", kept)
	}
	if len(withheld) != 3 {
		t.Fatalf("want 3 withheld (irreversible, blast, kind), got %d: %v", len(withheld), withheld)
	}
}

// TestReviewNamespaceGate drives the namespace allow/deny gate through the public
// Review API with EXECUTABLE actions (Op set, not advisory suggestions), so the
// load-bearing namespaceViolation path is exercised at the same boundary
// suggestions are surfaced — not only at the auto exec boundary. It asserts both
// the keep/withhold decision and the exact reason string, so a bug emitting the
// wrong reason (or mis-scoping the gate at Review) is caught.
func TestReviewNamespaceGate(t *testing.T) {
	// suspend is a real registry op (reversible, blast 1), so deriveSafety keeps it
	// executable and the action clears the reversible_only/blast/kind checks,
	// reaching the namespace gate.
	execAction := func(ns string) providers.Action {
		return providers.Action{
			Description: "suspend ks/web",
			Op:          "suspend",
			Target:      providers.Workload{Kind: "Kustomization", Name: "web", Namespace: ns},
		}
	}

	tests := []struct {
		name       string
		namespace  string
		namespaces []string // Allow.Namespaces (allowlist)
		protected  []string // Allow.ProtectedNamespaces (operator-configured)
		wantKept   bool
		wantReason string // substring expected in the withheld entry (empty when kept)
	}{
		{
			name:       "allowed namespace is kept",
			namespace:  "apps",
			namespaces: []string{"apps"},
			wantKept:   true,
		},
		{
			name:       "namespace not in allowlist is denied",
			namespace:  "restricted",
			namespaces: []string{"apps"},
			wantReason: "namespace restricted not in the action allowlist",
		},
		{
			name:       "built-in protected flux-system is denied even when allowlisted",
			namespace:  "flux-system",
			namespaces: []string{"flux-system"}, // operator tries to allow it; built-in deny must win
			wantReason: "namespace flux-system is protected (never an action target)",
		},
		{
			name:       "built-in protected kube-system is denied even when allowlisted",
			namespace:  "kube-system",
			namespaces: []string{"kube-system"},
			wantReason: "namespace kube-system is protected (never an action target)",
		},
		{
			name:       "operator-configured protected namespace is denied even when allowlisted",
			namespace:  "security",
			namespaces: []string{"security"},
			protected:  []string{"security"},
			wantReason: "namespace security is protected (never an action target)",
		},
		{
			name:       "empty namespace on an executable action is denied",
			namespace:  "",
			namespaces: []string{"apps"},
			wantReason: "target namespace required",
		},
		{
			name:       "empty allowlist permits no executable target",
			namespace:  "apps",
			namespaces: nil, // empty allowlist == permits nothing
			wantReason: "namespace apps not in the action allowlist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(config.ActionPolicy{
				Mode: config.ActionSuggest,
				Allow: config.ActionAllow{
					Namespaces:          tt.namespaces,
					ProtectedNamespaces: tt.protected,
				},
			})
			kept, withheld := p.Review([]providers.Action{execAction(tt.namespace)})

			if tt.wantKept {
				if len(kept) != 1 || len(withheld) != 0 {
					t.Fatalf("want action kept, got kept=%+v withheld=%v", kept, withheld)
				}
				return
			}
			if len(kept) != 0 {
				t.Fatalf("want action withheld, but it was kept: %+v", kept)
			}
			if len(withheld) != 1 {
				t.Fatalf("want exactly one withheld entry, got %d: %v", len(withheld), withheld)
			}
			if !strings.Contains(withheld[0], tt.wantReason) {
				t.Fatalf("withheld reason = %q, want substring %q", withheld[0], tt.wantReason)
			}
		})
	}
}

// TestReviewExecutableNeedsTargetKind closes the branch guarding the namespace
// gate: an executable action with a namespace but no target kind must be withheld
// with the kind reason, never silently passed through to the namespace check.
func TestReviewExecutableNeedsTargetKind(t *testing.T) {
	p := New(config.ActionPolicy{
		Mode:  config.ActionSuggest,
		Allow: config.ActionAllow{Namespaces: []string{"apps"}},
	})
	// Op set (executable) and a valid namespace, but Kind left empty.
	act := providers.Action{Op: "suspend", Target: providers.Workload{Name: "web", Namespace: "apps"}}
	kept, withheld := p.Review([]providers.Action{act})
	if len(kept) != 0 {
		t.Fatalf("executable action without a kind must be withheld, got kept=%+v", kept)
	}
	if len(withheld) != 1 || !strings.Contains(withheld[0], "executable action needs a target kind") {
		t.Fatalf("withheld = %v, want the kind reason", withheld)
	}
}
