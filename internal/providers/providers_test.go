package providers_test

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

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
