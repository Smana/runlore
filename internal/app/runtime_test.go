// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"github.com/Smana/runlore/internal/catalog"
)

// TestReadyFunc pins the #264 readiness model: /readyz is process + catalog
// health ONLY — leadership is deliberately NOT an input. Before #264 readiness
// also required holding the leader Lease, which kept every standby replica
// permanently NotReady, so `helm upgrade --wait` / kstatus (Flux
// helm-controller) could never see a replicaCount>1 release become Ready and
// timed out on every upgrade. Leader/follower is now a routing concern
// (server.Forward proxies work-bearing requests to the leader), not a
// readiness concern.
func TestReadyFunc(t *testing.T) {
	// No catalog configured → always ready (leader AND follower alike).
	if !ReadyFunc(nil, false)() {
		t.Fatal("unconfigured + nil catalog should be ready")
	}

	// A catalog was CONFIGURED but failed to load (cat == nil). Never serve
	// incident traffic with no knowledge base: stay 503. A static catalog has no
	// syncer to recover, so the misconfiguration must surface loudly.
	if ReadyFunc(nil, true)() {
		t.Fatal("configured + failed-to-load catalog (nil) must block readiness")
	}

	// A not-yet-warm catalog blocks readiness — on every replica: each replica
	// syncs/indexes its own catalog copy and must be warm before it can either
	// investigate (leader) or take over on failover (follower).
	cold := catalog.NewEmpty()
	if ReadyFunc(cold, true)() {
		t.Fatal("cold configured catalog must block readiness")
	}
	if ReadyFunc(cold, false)() {
		t.Fatal("cold catalog must block readiness even when not gate-configured")
	}

	// A warm catalog is ready — with no leadership condition attached.
	warm, err := catalog.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !ReadyFunc(warm, true)() {
		t.Fatal("warm catalog should be ready")
	}
	if !ReadyFunc(warm, false)() {
		t.Fatal("warm catalog should be ready regardless of the configured flag")
	}
}
