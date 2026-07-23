// SPDX-License-Identifier: Apache-2.0

package sourcerepo

import (
	"strings"
	"testing"
)

func TestNewRejectsBadPatterns(t *testing.T) {
	for _, tc := range []struct{ name string; patterns []string }{
		{"empty list", nil},
		{"empty pattern", []string{""}},
		{"whitespace only", []string{"   "}},
		{"scheme", []string{"https://github.com/acme/x"}},
		{"dotdot", []string{"github.com/acme/../evil"}},
		{"inner whitespace", []string{"github.com/acme/a b"}},
		{"bad glob", []string{"github.com/acme/[x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.patterns); err == nil {
				t.Fatalf("New(%q) = nil error, want error", tc.patterns)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	a, err := New([]string{"github.com/acme/*", "gitlab.com/acme/infra-modules"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, in, wantURL string
		wantOK            bool
	}{
		{"bare", "github.com/acme/checkout", "https://github.com/acme/checkout", true},
		{"https + .git", "https://github.com/acme/checkout.git", "https://github.com/acme/checkout", true},
		{"scp-style ssh", "git@github.com:acme/checkout.git", "https://github.com/acme/checkout", true},
		{"ssh scheme", "ssh://git@github.com/acme/checkout", "https://github.com/acme/checkout", true},
		{"host case-insensitive", "GitHub.com/acme/checkout", "https://github.com/acme/checkout", true},
		{"exact entry", "gitlab.com/acme/infra-modules", "https://gitlab.com/acme/infra-modules", true},
		{"glob must not cross a segment", "github.com/acme/a/b", "", false},
		{"wrong org", "github.com/evil/checkout", "", false},
		{"wrong host", "gitlab.com/acme/checkout", "", false},
		// bypass attempts — all must be rejected
		{"traversal", "github.com/acme/../evil", "", false},
		{"userinfo host smuggle", "github.com/acme/x@evil.com/y", "", false},
		{"whitespace", "github.com/acme/x y", "", false},
		{"empty", "", "", false},
		{"scheme to other host", "https://evil.com/github.com/acme/x", "", false},
		{"userinfo with path prefix", "evil.com/acme@github.com/acme/checkout", "", false},
		{"double at", "git@x@github.com/acme/checkout", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, ok := a.Match(tc.in)
			if ok != tc.wantOK || url != tc.wantURL {
				t.Fatalf("Match(%q) = (%q, %v), want (%q, %v)", tc.in, url, ok, tc.wantURL, tc.wantOK)
			}
		})
	}
}

// Local filesystem patterns support the test/dev path the differ already
// accepts (a local dir as the clone URL). A URL-shaped pattern must never
// match a local path and vice versa.
func TestMatchLocalPath(t *testing.T) {
	a, err := New([]string{"/tmp/fixtures/*"})
	if err != nil {
		t.Fatal(err)
	}
	if url, ok := a.Match("/tmp/fixtures/repo1"); !ok || url != "/tmp/fixtures/repo1" {
		t.Fatalf("local match = (%q, %v)", url, ok)
	}
	if _, ok := a.Match("/etc/passwd"); ok {
		t.Fatal("out-of-pattern local path matched")
	}
}

func TestPatterns(t *testing.T) {
	a, err := New([]string{"github.com/acme/*"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(a.Patterns(), ","); got != "github.com/acme/*" {
		t.Fatalf("Patterns() = %q", got)
	}
}

// Patterns() must return a copy: a caller mutating the slice must never be
// able to widen the live allowlist.
func TestPatternsMutationIsolated(t *testing.T) {
	a, err := New([]string{"github.com/acme/x"})
	if err != nil {
		t.Fatal(err)
	}
	a.Patterns()[0] = "github.com/evil/x"
	if got := a.Patterns()[0]; got != "github.com/acme/x" {
		t.Fatalf("mutation leaked into the allowlist: %q", got)
	}
	if _, ok := a.Match("github.com/evil/x"); ok {
		t.Fatal("mutated pattern changed Match behavior")
	}
}
