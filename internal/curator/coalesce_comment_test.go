package curator

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestCoalesceCommentSurfacesNewCause(t *testing.T) {
	inv := providers.Investigation{
		Confidence: 0.82,
		RootCauses: []providers.Hypothesis{{Summary: "Missing ExternalSecret for database credentials"}},
	}
	got := coalesceComment(inv)
	// The open-PR dedup keys on the trigger (resource+reason / alert fingerprint),
	// NOT the cause, so a recurrence with a genuinely DIFFERENT root cause is
	// coalesced onto this PR. The comment must therefore name the cause it observed
	// this time, or a different new cause vanishes without a trace.
	if !strings.Contains(got, "Missing ExternalSecret for database credentials") {
		t.Fatalf("comment must surface the observed root cause, got:\n%s", got)
	}
	if !strings.Contains(strings.ToLower(got), "differ") {
		t.Fatalf("comment must hint that a differing cause warrants a human check, got:\n%s", got)
	}
}

func TestCoalesceCommentWithoutCause(t *testing.T) {
	got := coalesceComment(providers.Investigation{Confidence: 0.5})
	if !strings.Contains(got, "saw this incident again") {
		t.Fatalf("unexpected comment:\n%s", got)
	}
}
