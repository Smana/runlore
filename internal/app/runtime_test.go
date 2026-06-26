package app

import (
	"testing"

	"github.com/Smana/runlore/internal/catalog"
)

func TestReadyFunc(t *testing.T) {
	leaderTrue := func() bool { return true }
	leaderFalse := func() bool { return false }

	// No catalog configured → gate is pure leadership passthrough.
	if !ReadyFunc(leaderTrue, nil, false)() {
		t.Fatal("unconfigured + nil catalog + leader=true should be ready")
	}
	if ReadyFunc(leaderFalse, nil, false)() {
		t.Fatal("unconfigured + nil catalog + leader=false should not be ready")
	}

	// A catalog was CONFIGURED but failed to load (cat == nil). Never serve incident
	// traffic with no knowledge base: stay 503 even while leader. This is the bug —
	// a configured-but-failed catalog used to be indistinguishable from "unconfigured"
	// and collapsed readiness to pure leadership.
	if ReadyFunc(leaderTrue, nil, true)() {
		t.Fatal("configured + failed-to-load catalog (nil) must block readiness even when leader=true")
	}

	// A not-yet-warm configured catalog blocks readiness even when leader.
	cold := catalog.NewEmpty()
	if ReadyFunc(leaderTrue, cold, true)() {
		t.Fatal("cold catalog must block readiness even when leader=true")
	}

	// A warm catalog is ready only when also leader.
	warm, err := catalog.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !ReadyFunc(leaderTrue, warm, true)() {
		t.Fatal("warm catalog + leader=true should be ready")
	}
	if ReadyFunc(leaderFalse, warm, true)() {
		t.Fatal("warm catalog + leader=false should not be ready")
	}
}
