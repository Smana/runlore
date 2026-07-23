// SPDX-License-Identifier: Apache-2.0

// Package sourcerepo gates which source repositories the source_diff
// investigation tool may clone. The allowlist match is the security boundary:
// the model names a repo, but only operator-listed patterns ever reach the
// network — no SSRF / arbitrary-clone, regardless of what the model writes.
package sourcerepo

import (
	"fmt"
	"path"
	"strings"
)

// Allowlist holds the operator's source-repo allow patterns, pre-validated.
// Patterns are host/org/repo shaped and matched with path.Match, so '*' never
// crosses a '/': "github.com/acme/*" allows every repo directly under acme
// but not "github.com/acme/x/y". A local filesystem pattern (leading '/') is
// supported for tests/dev and matched the same way.
type Allowlist struct {
	patterns []string
}

// New validates and compiles allow patterns. Rejected at load time (config
// validation calls this): an empty list, an empty pattern, a scheme, "..",
// whitespace, or a glob path.Match itself rejects.
func New(patterns []string) (*Allowlist, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("empty allowlist")
	}
	compiled := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
			return nil, fmt.Errorf("empty pattern")
		case strings.Contains(p, "://"):
			return nil, fmt.Errorf("pattern %q must not carry a scheme — write host/org/repo", p)
		case strings.Contains(p, ".."):
			return nil, fmt.Errorf("pattern %q must not contain '..'", p)
		case strings.ContainsAny(p, " \t"):
			return nil, fmt.Errorf("pattern %q must not contain whitespace", p)
		}
		if _, err := path.Match(p, "probe"); err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", p, err)
		}
		compiled = append(compiled, lowerHost(p))
	}
	return &Allowlist{patterns: compiled}, nil
}

// Patterns returns the normalized allow patterns, for the tool description
// (the model picks a repo from this list) and for error messages.
func (a *Allowlist) Patterns() []string {
	out := make([]string, len(a.patterns))
	copy(out, a.patterns)
	return out
}

// Match normalizes a model-supplied repo reference and reports whether it is
// allowed, returning the canonical clone URL. It accepts the shapes a model
// plausibly emits — "github.com/acme/x", "https://github.com/acme/x.git",
// "git@github.com:acme/x.git", "ssh://git@github.com/acme/x" — all reduced to
// host/org/repo BEFORE matching, so a scheme or userinfo can never smuggle a
// non-allowed host past the gate. The returned clone URL is built from the
// NORMALIZED form ("https://" + host/org/repo, or the path itself for a local
// pattern), never from the raw input.
func (a *Allowlist) Match(raw string) (cloneURL string, ok bool) {
	cand, err := normalize(raw)
	if err != nil {
		return "", false
	}
	for _, p := range a.patterns {
		if m, err := path.Match(p, cand); err == nil && m {
			if strings.HasPrefix(cand, "/") {
				return cand, true
			}
			return "https://" + cand, true
		}
	}
	return "", false
}

// normalize reduces a repo reference to matchable host/org/repo (or a local
// absolute path). It strips a scheme and scp-style git@host: prefix, drops a
// trailing .git and slash, lowercases the host segment, and rejects anything
// that still smells like smuggling (userinfo '@', '..', whitespace, empties).
func normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	for _, scheme := range []string{"https://", "http://", "ssh://"} {
		s = strings.TrimPrefix(s, scheme)
	}
	// Handle both ssh-style formats:
	// - scp-style: git@github.com:acme/x → github.com/acme/x
	// - ssh URL: git@github.com/acme/x → github.com/acme/x
	// Only strip a leading userinfo when the part before '@' is a bare username
	// (no '/', ':', or further '@'). Any other '@'-containing input is left as-is
	// and rejected by the strings.Contains(s, "@") guard below.
	if at := strings.Index(s, "@"); at >= 0 && !strings.HasPrefix(s, "/") {
		userinfo := s[:at]
		if !strings.ContainsAny(userinfo, "/:@") {
			host := s[at+1:]
			if idx := strings.IndexAny(host, ":/"); idx >= 0 {
				if host[idx] == ':' {
					s = strings.Replace(host, ":", "/", 1)
				} else {
					s = host
				}
			} else {
				s = host
			}
		}
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	switch {
	case s == "":
		return "", fmt.Errorf("empty repo")
	case strings.Contains(s, ".."):
		return "", fmt.Errorf("repo %q contains '..'", raw)
	case strings.Contains(s, "@"):
		return "", fmt.Errorf("repo %q contains userinfo", raw)
	case strings.ContainsAny(s, " \t"):
		return "", fmt.Errorf("repo %q contains whitespace", raw)
	}
	return lowerHost(s), nil
}

// lowerHost lowercases the first path segment (the host — DNS names are
// case-insensitive; org/repo are not). Local paths (leading '/') pass through.
func lowerHost(s string) string {
	if strings.HasPrefix(s, "/") {
		return s
	}
	host, rest, found := strings.Cut(s, "/")
	if !found {
		return strings.ToLower(s)
	}
	return strings.ToLower(host) + "/" + rest
}
