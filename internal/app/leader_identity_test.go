// SPDX-License-Identifier: Apache-2.0

package app

import "testing"

func TestEncodeLeaseIdentity(t *testing.T) {
	tests := []struct {
		name, podName, podIP, want string
	}{
		{"name plus ip", "runlore-abc12", "10.1.2.3", "runlore-abc12_10.1.2.3"},
		{"no ip falls back to name-only (old format)", "runlore-abc12", "", "runlore-abc12"},
		// Never publish an unroutable identity: garbage in POD_IP must not
		// produce an identity followers would try (and fail) to dial.
		{"invalid ip falls back to name-only", "runlore-abc12", "not-an-ip", "runlore-abc12"},
		{"ipv6", "runlore-0", "fd00::1", "runlore-0_fd00::1"},
	}
	for _, tt := range tests {
		if got := EncodeLeaseIdentity(tt.podName, tt.podIP); got != tt.want {
			t.Errorf("%s: EncodeLeaseIdentity(%q, %q) = %q, want %q", tt.name, tt.podName, tt.podIP, got, tt.want)
		}
	}
}

func TestParseLeaseIdentity(t *testing.T) {
	tests := []struct {
		name, id, wantName, wantIP string
	}{
		{"new format", "runlore-abc12_10.1.2.3", "runlore-abc12", "10.1.2.3"},
		{"ipv6", "runlore-0_fd00::1", "runlore-0", "fd00::1"},
		// OLD-format identity (pre-#264 replica during a mixed-version rollout):
		// no IP ⇒ forwarding unavailable ⇒ callers behave as before (503 + retry).
		{"old format name-only", "runlore-abc12", "runlore-abc12", ""},
		// Defensive: a suffix that isn't a valid IP is NOT split off — the whole
		// string is the name, and no bogus dial target is ever produced.
		{"underscore but invalid ip", "pod_notanip", "pod_notanip", ""},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		name, ip := ParseLeaseIdentity(tt.id)
		if name != tt.wantName || ip != tt.wantIP {
			t.Errorf("%s: ParseLeaseIdentity(%q) = (%q, %q), want (%q, %q)",
				tt.name, tt.id, name, ip, tt.wantName, tt.wantIP)
		}
	}
}

func TestLeaderTrackerAddr(t *testing.T) {
	var tr LeaderTracker
	// No holder observed yet: nothing to route to.
	if got := tr.Addr("8080"); got != "" {
		t.Errorf("empty tracker Addr = %q, want \"\"", got)
	}
	// Old-format holder (no IP): forwarding unavailable, not a bogus address.
	tr.Set("runlore-old")
	if got := tr.Addr("8080"); got != "" {
		t.Errorf("old-format holder Addr = %q, want \"\"", got)
	}
	tr.Set("runlore-abc12_10.1.2.3")
	if got := tr.Addr("8080"); got != "10.1.2.3:8080" {
		t.Errorf("Addr = %q, want 10.1.2.3:8080", got)
	}
	// IPv6 must be bracketed for use as a URL host.
	tr.Set("runlore-0_fd00::1")
	if got := tr.Addr("8080"); got != "[fd00::1]:8080" {
		t.Errorf("ipv6 Addr = %q, want [fd00::1]:8080", got)
	}
	// Defensive: without a port there is no routable address.
	if got := tr.Addr(""); got != "" {
		t.Errorf("empty-port Addr = %q, want \"\"", got)
	}
}

func TestServePort(t *testing.T) {
	tests := []struct{ addr, want string }{
		{":8080", "8080"},
		{"0.0.0.0:9090", "9090"},
		{"not-an-addr", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ServePort(tt.addr); got != tt.want {
			t.Errorf("ServePort(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}
