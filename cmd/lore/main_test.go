package main

import (
	"net/http"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
)

// TestNewHTTPServer asserts the serving http.Server is built with every inbound
// timeout/size bound set (non-zero) — Go's defaults are zero (unlimited), the
// Slowloris/DoS gap R9(a) closes.
func TestNewHTTPServer(t *testing.T) {
	s := newHTTPServer(":0", http.NewServeMux())
	if s.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is zero (unbounded slow-header read)")
	}
	if s.ReadTimeout == 0 {
		t.Error("ReadTimeout is zero (unbounded slow-body read)")
	}
	if s.WriteTimeout == 0 {
		t.Error("WriteTimeout is zero (unbounded slow write)")
	}
	if s.IdleTimeout == 0 {
		t.Error("IdleTimeout is zero (unbounded idle keep-alive)")
	}
	if s.MaxHeaderBytes == 0 {
		t.Error("MaxHeaderBytes is zero (defaults to 1MB but should be explicit)")
	}
}

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
