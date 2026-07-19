// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net"
	"sync"
	"time"
)

// authGuard throttles repeated FAILED authentications per remote host with an
// exponential, capped block — bounding brute-force attempts on the shared-token
// endpoints. Invariants:
//
//   - Only consecutive failures count; success() clears the host, so a correct
//     token is never punished for a NAT-mate's noise — except DURING a live
//     block window (≤ maxBlock), the accepted trade-off: without a pre-compare
//     block, backoff would slow nothing.
//   - blocked() is consulted BEFORE the token compare — a blocked host learns
//     nothing, not even timing.
//   - The map is bounded: past maxHosts, expired entries are swept; if all are
//     live, the new host goes untracked. Fail open on memory, never on auth.
type authGuard struct {
	mu    sync.Mutex
	hosts map[string]*hostState
	now   func() time.Time
}

type hostState struct {
	fails      int
	blockUntil time.Time
}

const (
	failThreshold = 10               // consecutive failures before the first block
	baseBlock     = 1 * time.Second  // first block; doubles per further failure
	maxBlock      = 60 * time.Second // hard cap — a lockout is never permanent
	maxHosts      = 4096             // guard-map bound
)

func newAuthGuard() *authGuard {
	return &authGuard{hosts: map[string]*hostState{}, now: time.Now}
}

// blocked reports whether host is inside a live block window.
func (g *authGuard) blocked(host string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.hosts[host]
	return ok && g.now().Before(st.blockUntil)
}

// fail records one failed authentication, arming/extending the block once
// failThreshold consecutive failures accumulate.
func (g *authGuard) fail(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.hosts[host]
	if !ok {
		if len(g.hosts) >= maxHosts {
			g.sweepLocked()
		}
		if len(g.hosts) >= maxHosts {
			return
		}
		st = &hostState{}
		g.hosts[host] = st
	}
	st.fails++
	if st.fails >= failThreshold {
		d := baseBlock << uint(st.fails-failThreshold)
		if d <= 0 || d > maxBlock { // <= 0 catches shift overflow
			d = maxBlock
		}
		st.blockUntil = g.now().Add(d)
	}
}

// success clears host — a correct token ends any backoff immediately.
func (g *authGuard) success(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.hosts, host)
}

// sweepLocked drops entries with no live block (expired or never armed).
func (g *authGuard) sweepLocked() {
	now := g.now()
	for h, st := range g.hosts {
		if !now.Before(st.blockUntil) {
			delete(g.hosts, h)
		}
	}
}

// remoteHost extracts the host half of an "ip:port" http RemoteAddr.
func remoteHost(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}
