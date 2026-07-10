// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestDifferAuth(t *testing.T) {
	t.Run("nil source disables auth", func(t *testing.T) {
		m, err := (&Differ{}).auth(context.Background())
		if err != nil || m != nil {
			t.Fatalf("want (nil, nil), got (%v, %v)", m, err)
		}
	})

	t.Run("token yields x-access-token basic auth", func(t *testing.T) {
		d := &Differ{TokenSource: func(context.Context) (string, error) { return "ghs_tok", nil }}
		m, err := d.auth(context.Background())
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
		m, err := d.auth(context.Background())
		if err != nil || m != nil {
			t.Fatalf("want (nil, nil), got (%v, %v)", m, err)
		}
	})

	t.Run("source error is surfaced, not swallowed", func(t *testing.T) {
		sentinel := errors.New("mint failed")
		d := &Differ{TokenSource: func(context.Context) (string, error) { return "", sentinel }}
		m, err := d.auth(context.Background())
		if m != nil {
			t.Fatalf("want nil auth on error, got %v", m)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("want wrapped sentinel error, got %v", err)
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
