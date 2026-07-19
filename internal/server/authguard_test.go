// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func guardAt(now *time.Time) *authGuard {
	g := newAuthGuard()
	g.now = func() time.Time { return *now }
	return g
}

func TestAuthGuardBlocksAfterThreshold(t *testing.T) {
	now := time.Unix(0, 0)
	g := guardAt(&now)
	for i := 0; i < failThreshold-1; i++ {
		g.fail("10.0.0.1")
		if g.blocked("10.0.0.1") {
			t.Fatalf("blocked after only %d failures (threshold %d)", i+1, failThreshold)
		}
	}
	g.fail("10.0.0.1")
	if !g.blocked("10.0.0.1") {
		t.Fatal("must block after reaching the failure threshold")
	}
	if g.blocked("10.0.0.2") {
		t.Fatal("other hosts must be unaffected")
	}
}

func TestAuthGuardBlockExpiresAndIsCapped(t *testing.T) {
	now := time.Unix(0, 0)
	g := guardAt(&now)
	for i := 0; i < failThreshold; i++ {
		g.fail("10.0.0.1")
	}
	now = now.Add(baseBlock + time.Millisecond)
	if g.blocked("10.0.0.1") {
		t.Fatal("first block must expire after baseBlock")
	}
	// Pile on failures: the block must never exceed maxBlock.
	for i := 0; i < 100; i++ {
		g.fail("10.0.0.1")
	}
	now = now.Add(maxBlock + time.Millisecond)
	if g.blocked("10.0.0.1") {
		t.Fatal("block must be capped at maxBlock — lockouts are never permanent")
	}
}

func TestAuthGuardSuccessResets(t *testing.T) {
	now := time.Unix(0, 0)
	g := guardAt(&now)
	for i := 0; i < failThreshold-1; i++ {
		g.fail("10.0.0.1")
	}
	g.success("10.0.0.1")
	g.fail("10.0.0.1")
	if g.blocked("10.0.0.1") {
		t.Fatal("success must reset the consecutive-failure count")
	}
}

func TestAuthGuardMapBounded(t *testing.T) {
	now := time.Unix(0, 0)
	g := guardAt(&now)
	for i := 0; i < maxHosts+100; i++ {
		g.fail(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	g.mu.Lock()
	n := len(g.hosts)
	g.mu.Unlock()
	if n > maxHosts {
		t.Fatalf("guard map must stay bounded at %d, got %d", maxHosts, n)
	}
}

func TestRemoteHost(t *testing.T) {
	if got := remoteHost("192.0.2.1:1234"); got != "192.0.2.1" {
		t.Fatalf("want 192.0.2.1, got %q", got)
	}
	if got := remoteHost("no-port"); got != "no-port" {
		t.Fatalf("want passthrough for portless addr, got %q", got)
	}
}

func testServerWithGuard(token string) *Server {
	return &Server{
		token: token,
		guard: newAuthGuard(),
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestAuthorizedBackoff pins the end-to-end behavior: enough wrong tokens from
// one host block it — and, during the (≤60s) block window, even the RIGHT token
// is rejected without being compared. That last property is the point: a
// blocked host learns nothing.
func TestAuthorizedBackoff(t *testing.T) {
	s := testServerWithGuard("secret")
	bad := httptest.NewRequest(http.MethodPost, "/actions/x/approve", nil)
	bad.Header.Set("X-Approval-Token", "wrong")
	for i := 0; i < failThreshold; i++ {
		if s.authorized(bad) {
			t.Fatal("wrong token must never authorize")
		}
	}
	good := httptest.NewRequest(http.MethodPost, "/actions/x/approve", nil)
	good.Header.Set("X-Approval-Token", "secret")
	if s.authorized(good) {
		t.Fatal("correct token must be rejected while the host is blocked")
	}
}

// TestAuthorizedSuccessResetsGuard pins that a correct token BEFORE the
// threshold clears the failure count (no creeping lockout for fat fingers).
func TestAuthorizedSuccessResetsGuard(t *testing.T) {
	s := testServerWithGuard("secret")
	bad := httptest.NewRequest(http.MethodPost, "/actions/x/approve", nil)
	bad.Header.Set("X-Approval-Token", "wrong")
	for i := 0; i < failThreshold-1; i++ {
		s.authorized(bad)
	}
	good := httptest.NewRequest(http.MethodPost, "/actions/x/approve", nil)
	good.Header.Set("X-Approval-Token", "secret")
	if !s.authorized(good) {
		t.Fatal("correct token below threshold must authorize")
	}
	if s.guard.blocked(remoteHost(good.RemoteAddr)) {
		t.Fatal("success must have cleared the guard")
	}
}
