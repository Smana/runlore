package action

import (
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
