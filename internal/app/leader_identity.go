// SPDX-License-Identifier: Apache-2.0

package app

import (
	"net"
	"strings"
	"sync/atomic"
)

// Lease identity format (#264): "<podName>_<podIP>". Encoding the pod IP into
// the leader-election Lease identity makes the holder ROUTABLE: a non-leader
// replica that receives a work-bearing request (webhook, slack interaction,
// action control) learns the leader's address from the Lease it already
// watches (OnNewLeader) and proxies the request there — which is what lets
// /readyz stop reflecting leadership without giving up "only the leader's
// queue processes work".
//
// "_" is a safe separator because pod names are DNS-1123 subdomains (lowercase
// alphanumerics, '-' and '.') — an underscore can never appear in a pod name.
// IPv6 pod IPs contain ':' but never '_', so the encoding stays unambiguous.

// EncodeLeaseIdentity builds the routable lease identity. With no (or an
// invalid) pod IP it degrades to the bare name — the pre-#264 format — so a
// misconfigured POD_IP never publishes a dial target that cannot work.
func EncodeLeaseIdentity(podName, podIP string) string {
	if podIP == "" || net.ParseIP(podIP) == nil {
		return podName
	}
	return podName + "_" + podIP
}

// ParseLeaseIdentity splits a lease holder identity into name and IP. It is
// deliberately defensive: an OLD-format identity (no "_", from a pre-#264
// replica during a mixed-version rollout) or a suffix that is not a valid IP
// yields ip == "" — forwarding is then unavailable and callers shed with
// 503 + Retry-After instead of dialing garbage.
func ParseLeaseIdentity(id string) (name, ip string) {
	i := strings.LastIndex(id, "_")
	if i < 0 {
		return id, ""
	}
	if net.ParseIP(id[i+1:]) == nil {
		return id, ""
	}
	return id[:i], id[i+1:]
}

// LeaderTracker atomically tracks the current lease holder identity as
// reported by the leader-election OnNewLeader callback, so a follower can
// route work-bearing requests to the leader. Safe for concurrent use: the
// election goroutine writes, every request-serving goroutine reads.
//
// The view can be briefly stale (the holder just died and no successor has
// renewed yet) — the forwarding layer treats a failed dial as "retry shortly"
// (502 + Retry-After) rather than trusting the tracker blindly.
type LeaderTracker struct {
	v atomic.Value // string: the raw holder identity
}

// Set records the observed holder identity (called from OnNewLeader).
func (t *LeaderTracker) Set(identity string) { t.v.Store(identity) }

// Identity returns the last observed holder identity ("" before the first
// OnNewLeader callback).
func (t *LeaderTracker) Identity() string {
	s, _ := t.v.Load().(string)
	return s
}

// Addr returns "ip:port" (IPv6 bracketed) for the current holder on the given
// serve port — every replica serves on the same port, so the follower's own
// port is the leader's too. It returns "" when no holder is known, the
// holder's identity carries no routable IP (old format), or port is empty.
func (t *LeaderTracker) Addr(port string) string {
	if port == "" {
		return ""
	}
	_, ip := ParseLeaseIdentity(t.Identity())
	if ip == "" {
		return ""
	}
	return net.JoinHostPort(ip, port)
}

// ServePort extracts the port from a listen address (":8080", "0.0.0.0:8080").
// "" when the address does not parse — the forwarding layer then reports no
// routable leader instead of building a broken URL.
func ServePort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}
