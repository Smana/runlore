package trigger

import (
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
)

func TestEngineDecide(t *testing.T) {
	p := config.IncidentTrigger{
		Enabled: true,
		Match:   config.IncidentMatch{Severity: []string{"critical"}, Environment: []string{"prod"}},
		Dedup:   config.Dedup{Window: config.Duration(30 * time.Minute)},
	}
	e := NewEngine(p)

	crit := config.Incident{AlertName: "A", Severity: "critical", Environment: "prod", Fingerprint: "fp1"}
	if d := e.Decide(crit); !d.Investigate {
		t.Fatalf("critical/prod should investigate, got %q", d.Reason)
	}
	if d := e.Decide(crit); d.Investigate {
		t.Fatal("repeat within window should be deduped")
	}
	warn := config.Incident{AlertName: "B", Severity: "warning", Environment: "prod", Fingerprint: "fp2"}
	if d := e.Decide(warn); d.Investigate {
		t.Fatal("warning should be filtered by policy")
	}
}

// dedupKey must keep environment in its fingerprint-less fallback, so two
// same-name/same-namespace criticals from different environments don't collide.
func TestDedupKeyFallbackEnvironmentDistinct(t *testing.T) {
	prod := config.Incident{AlertName: "AppDown", Namespace: "shop", Environment: "prod"}
	stg := config.Incident{AlertName: "AppDown", Namespace: "shop", Environment: "staging"}
	if got, other := dedupKey(prod), dedupKey(stg); got == other {
		t.Fatalf("different environments must yield distinct dedup keys, both = %q", got)
	}
	// same env still collapses (real dedup of a repeat)
	if dedupKey(prod) != dedupKey(config.Incident{AlertName: "AppDown", Namespace: "shop", Environment: "prod"}) {
		t.Fatal("same name/namespace/env must share a dedup key")
	}
	// fingerprint, when present, still wins regardless of environment
	a := config.Incident{AlertName: "X", Namespace: "n", Environment: "prod", Fingerprint: "fp"}
	b := config.Incident{AlertName: "X", Namespace: "n", Environment: "staging", Fingerprint: "fp"}
	if dedupKey(a) != dedupKey(b) {
		t.Fatal("a shared fingerprint must dedup across environments")
	}
}
