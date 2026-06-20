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
