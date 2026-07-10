// SPDX-License-Identifier: Apache-2.0

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

// TestNormalizeWorkloadName pins the shared pod-hash normalization now homed in the
// providers package (both curator dedup and instant-recall matching call it). The
// boundary is the safe one the curator tests already established: strip a
// Deployment <rs-hash>-<pod-hash> and a DaemonSet/StatefulSet 5-char hash, but
// never a legitimate trailing word like "redis-cache". It must be idempotent.
func TestNormalizeWorkloadName(t *testing.T) {
	cases := map[string]string{
		"node-exporter-prometheus-node-exporter-km6ld": "node-exporter-prometheus-node-exporter", // DaemonSet pod hash
		"web-7d9c8b6f5-abcde":                          "web",                                    // Deployment <rs-hash>-<pod-hash>
		"harbor-registry-59598dbd57-ltkzw":             "harbor-registry",                        // the live pod-scoped alert
		"node-exporter-prometheus-node-exporter":       "node-exporter-prometheus-node-exporter", // controller name, unchanged
		"redis-cache":                                  "redis-cache",                            // 5-char tail but no digit → kept
		"web":                                          "web",
		"":                                             "",
	}
	for in, want := range cases {
		if got := providers.NormalizeWorkloadName(in); got != want {
			t.Errorf("NormalizeWorkloadName(%q) = %q, want %q", in, got, want)
		}
		// Idempotency: a second pass must be a no-op (the recall gate normalizes both
		// sides, sometimes an already-normalized value).
		if got := providers.NormalizeWorkloadName(want); got != want {
			t.Errorf("NormalizeWorkloadName not idempotent for %q: %q", want, got)
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
