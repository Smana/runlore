// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

func TestAppendSourceDiffTool(t *testing.T) {
	log := slog.Default()
	t.Run("unset config registers nothing", func(t *testing.T) {
		cfg := &config.Config{}
		if got := appendSourceDiffTool(cfg, nil, log); len(got) != 0 {
			t.Fatalf("tools = %d, want 0", len(got))
		}
	})
	t.Run("allowlist registers the tool", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.SourceRepos.Allow = []string{"github.com/acme/*"}
		got := appendSourceDiffTool(cfg, nil, log)
		if !toolNames(got)["source_diff"] {
			t.Fatalf("source_diff not registered; got %v", toolNames(got))
		}
	})
}

func TestGithubGitHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "github.com"},
		{"https://api.github.com", "github.com"},
		{"https://api.github.com/", "github.com"},
		{"https://ghe.example.com/api/v3", "ghe.example.com"},
		{"https://GHE.Example.COM/api/v3", "ghe.example.com"},
		{"not a url", "github.com"},
	} {
		if got := githubGitHost(tc.in); got != tc.want {
			t.Fatalf("githubGitHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
