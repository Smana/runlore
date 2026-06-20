package notify

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func sampleInvestigation() providers.Investigation {
	return providers.Investigation{
		Confidence: 0.82,
		RootCauses: []providers.Hypothesis{{
			Summary:    "chart 1.15 enabled DB migrations; harbor-db CrashLoopBackOff",
			Confidence: 0.82, Evidence: []string{"pg_up=0", "migration lock timeout"},
			SuggestedAction: "flux rollback hr/harbor", Reversible: true,
		}},
		Unresolved: []string{"why the migration lock never released"},
	}
}

func TestFormat(t *testing.T) {
	out := Format(sampleInvestigation())
	for _, want := range []string{"82%", "chart 1.15", "pg_up=0", "flux rollback hr/harbor", "reversible", "why the migration lock"} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted message missing %q:\n%s", want, out)
		}
	}
}
