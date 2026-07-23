// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// TestSourceRepos covers the three behaviours of the source_repos.allow block:
// off-by-default (no config), valid patterns, and a bad pattern that must fail
// Validate with a message naming source_repos.allow.
func TestSourceRepos(t *testing.T) {
	t.Run("empty is valid (feature off)", func(t *testing.T) {
		c := &Config{} // no SourceRepos set — tool is disabled; must validate clean
		if err := c.Validate(); err != nil {
			t.Fatalf("empty source_repos must validate clean, got: %v", err)
		}
	})

	t.Run("good patterns validate", func(t *testing.T) {
		c := &Config{}
		c.SourceRepos.Allow = []string{"github.com/acme/*", "gitlab.com/acme/infra-modules"}
		if err := c.Validate(); err != nil {
			t.Fatalf("valid source_repos.allow must validate clean, got: %v", err)
		}
	})

	t.Run("bad pattern fails loudly", func(t *testing.T) {
		c := &Config{}
		c.SourceRepos.Allow = []string{"https://github.com/acme/x"} // scheme disallowed
		err := c.Validate()
		if err == nil {
			t.Fatal("expected Validate to fail for a pattern with a scheme, got nil")
		}
		if !strings.Contains(err.Error(), "source_repos.allow") {
			t.Fatalf("error must contain 'source_repos.allow', got: %v", err)
		}
	})
}
