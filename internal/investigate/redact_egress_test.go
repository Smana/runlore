package investigate

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestRedactInvestigation locks down the egress catch-all: secrets in any of a
// finished investigation's human-facing fields are masked before delivery to chat
// or a (possibly public) KB PR — even if they reached the finding via a non-model
// path (so ingress redaction wouldn't have seen them).
func TestRedactInvestigation(t *testing.T) {
	inv := &providers.Investigation{
		Title: "DB down: password=hunter2horse",
		RootCauses: []providers.Hypothesis{{
			Summary:         "leaked token ghp_0123456789abcdefghijABCDEFGHIJ0123",
			Evidence:        []string{"controller log: token xoxb-123456789012-abcdefuvwxyz"},
			SuggestedAction: "rotate key AKIAIOSFODNN7EXAMPLE",
		}},
		Unresolved: []string{"DB_SECRET=s3cr3t-value-xyz seen in events"},
		Actions:    []providers.Action{{Description: "suspend (OPENAI_API_KEY=sk-abcdefghijklmnopqrst)"}},
	}
	redactInvestigation(inv)

	blob := strings.Join([]string{
		inv.Title, inv.RootCauses[0].Summary, inv.RootCauses[0].Evidence[0],
		inv.RootCauses[0].SuggestedAction, inv.Unresolved[0], inv.Actions[0].Description,
	}, "|")
	for _, secret := range []string{
		"hunter2horse", "ghp_0123456789abcdefghijABCDEFGHIJ0123", "xoxb-123456789012-abcdefuvwxyz",
		"AKIAIOSFODNN7EXAMPLE", "s3cr3t-value-xyz", "sk-abcdefghijklmnopqrst",
	} {
		if strings.Contains(blob, secret) {
			t.Fatalf("secret survived egress redaction: %q", secret)
		}
	}
	if !strings.Contains(blob, "[REDACTED]") {
		t.Fatalf("expected redaction markers, got %q", blob)
	}
}
