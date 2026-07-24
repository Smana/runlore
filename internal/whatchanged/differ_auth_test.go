// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestDifferAuth(t *testing.T) {
	const anyURL = "https://github.com/acme/x"

	t.Run("nil source disables auth", func(t *testing.T) {
		m, err := (&Differ{}).auth(context.Background(), anyURL)
		if err != nil || m != nil {
			t.Fatalf("want (nil, nil), got (%v, %v)", m, err)
		}
	})

	t.Run("token yields x-access-token basic auth", func(t *testing.T) {
		d := &Differ{TokenSource: func(context.Context) (string, error) { return "ghs_tok", nil }}
		m, err := d.auth(context.Background(), anyURL)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ba, ok := m.(*http.BasicAuth)
		if !ok {
			t.Fatalf("want *http.BasicAuth, got %T", m)
		}
		if ba.Username != "x-access-token" || ba.Password != "ghs_tok" {
			t.Fatalf("bad basic auth: %+v", ba)
		}
	})

	t.Run("empty token disables auth", func(t *testing.T) {
		d := &Differ{TokenSource: func(context.Context) (string, error) { return "", nil }}
		m, err := d.auth(context.Background(), anyURL)
		if err != nil || m != nil {
			t.Fatalf("want (nil, nil), got (%v, %v)", m, err)
		}
	})

	t.Run("source error is surfaced, not swallowed", func(t *testing.T) {
		sentinel := errors.New("mint failed")
		d := &Differ{TokenSource: func(context.Context) (string, error) { return "", sentinel }}
		m, err := d.auth(context.Background(), anyURL)
		if m != nil {
			t.Fatalf("want nil auth on error, got %v", m)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("want wrapped sentinel error, got %v", err)
		}
	})

	t.Run("TokenHost confines the token to its host", func(t *testing.T) {
		called := false
		d := &Differ{
			TokenHost:   "github.com",
			TokenSource: func(context.Context) (string, error) { called = true; return "ghs_tok", nil },
		}
		// On-host clone: token attaches.
		m, err := d.auth(context.Background(), "https://github.com/acme/x")
		if err != nil || m == nil {
			t.Fatalf("on-host: want auth, got (%v, %v)", m, err)
		}
		// Off-host clone (gitlab.com): NO token, and the source is never even minted.
		called = false
		m, err = d.auth(context.Background(), "https://gitlab.com/acme/x")
		if err != nil || m != nil {
			t.Fatalf("off-host: want (nil, nil) — token must not leak, got (%v, %v)", m, err)
		}
		if called {
			t.Fatal("off-host: token source was called; the credential must not be minted for a foreign host")
		}
		// Local path (no host): treated as off-host, no token.
		if m, err := d.auth(context.Background(), "/tmp/fixtures/repo"); err != nil || m != nil {
			t.Fatalf("local path: want (nil, nil), got (%v, %v)", m, err)
		}
	})
}

// TestDifferRemoteTokenError ensures a token-source failure aborts the clone
// (fail loud) rather than silently attempting an unauthenticated clone.
func TestDifferRemoteTokenError(t *testing.T) {
	sentinel := errors.New("mint failed")
	d := &Differ{TokenSource: func(context.Context) (string, error) { return "", sentinel }}
	if _, err := d.Remote(context.Background(), "https://example.invalid/repo.git", "a", "b", ""); !errors.Is(err, sentinel) {
		t.Fatalf("want token error surfaced from Remote, got %v", err)
	}
}
