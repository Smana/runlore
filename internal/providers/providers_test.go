package providers_test

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestCompletionResponseRefused locks in the refusal classification: a safety/policy
// stop reason (case-insensitive) reports Refused()==true; a normal termination
// (end_turn/stop/length/max_tokens) or an empty reason reports false.
func TestCompletionResponseRefused(t *testing.T) {
	refused := []string{
		"refusal", "content_filter", "safety", "prohibited_content", "blocklist", "spii",
		"Refusal", "CONTENT_FILTER", "Safety", "SPII", // case-insensitive
	}
	for _, sr := range refused {
		if !(providers.CompletionResponse{StopReason: sr}).Refused() {
			t.Errorf("StopReason %q should report Refused()==true", sr)
		}
	}
	notRefused := []string{"end_turn", "stop", "max_tokens", "length", "MAX_TOKENS", "tool_use", ""}
	for _, sr := range notRefused {
		if (providers.CompletionResponse{StopReason: sr}).Refused() {
			t.Errorf("StopReason %q should report Refused()==false", sr)
		}
	}
}

func TestFingerprintMarkerRoundTrip(t *testing.T) {
	const fp = "abc123def456"
	body := "Drafted by RunLore — x\n\n" + providers.FingerprintMarker(fp)
	if got := providers.ParseFingerprintMarker(body); got != fp {
		t.Fatalf("round-trip: want %q, got %q", fp, got)
	}
	if providers.FingerprintMarker("") != "" {
		t.Fatal("empty fingerprint must render an empty marker")
	}
	if got := providers.ParseFingerprintMarker("no marker here"); got != "" {
		t.Fatalf("absent marker must parse to empty, got %q", got)
	}
}
