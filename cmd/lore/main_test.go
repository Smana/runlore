package main

import (
	"testing"

	"github.com/Smana/runlore/internal/catalog"
)

func TestReadyFunc(t *testing.T) {
	leaderTrue := func() bool { return true }
	leaderFalse := func() bool { return false }

	// No catalog configured → gate is pure leadership passthrough.
	if !readyFunc(leaderTrue, nil)() {
		t.Fatal("nil catalog + leader=true should be ready")
	}
	if readyFunc(leaderFalse, nil)() {
		t.Fatal("nil catalog + leader=false should not be ready")
	}

	// A not-yet-warm catalog blocks readiness even when leader.
	cold := catalog.NewEmpty()
	if readyFunc(leaderTrue, cold)() {
		t.Fatal("cold catalog must block readiness even when leader=true")
	}

	// A warm catalog is ready only when also leader.
	warm, err := catalog.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !readyFunc(leaderTrue, warm)() {
		t.Fatal("warm catalog + leader=true should be ready")
	}
	if readyFunc(leaderFalse, warm)() {
		t.Fatal("warm catalog + leader=false should not be ready")
	}
}
