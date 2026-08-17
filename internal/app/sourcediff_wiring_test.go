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

// TestBuildGitOpsDifferConfinesSSHRewrite pins the what_changed credential
// wiring (RunLore #495).
//
// Two halves, and BOTH are load-bearing:
//
//   - SSHRewriteHost must be SET, or the SSH→HTTPS rewrite never fires and #495
//     is silently un-fixed — the exact silent-data-gap failure the issue is about.
//   - TokenHost must stay UNSET, because it governs which clones may carry the
//     token and is derived from the forge API URL. A GitHub Enterprise install
//     whose API host differs from its git host would start withholding the
//     credential from repos that clone fine today.
//
// The rewrite is confined by SSHRewriteHost instead, so a wrong host costs an
// unrewritten SSH URL rather than a broken clone or a leaked credential.
func TestBuildGitOpsDifferConfinesSSHRewrite(t *testing.T) {
	log := slog.Default()
	for _, tc := range []struct{ name, apiURL, want string }{
		{"default forge", "", "github.com"},
		{"github enterprise", "https://ghe.example.com/api/v3", "ghe.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Forge.GitHubAPIURL = tc.apiURL
			d := buildGitOpsDiffer(cfg, log)
			if d.SSHRewriteHost != tc.want {
				t.Fatalf("SSHRewriteHost = %q, want %q — an unset host leaves SSH repoURLs unclonable (#495)",
					d.SSHRewriteHost, tc.want)
			}
			if d.TokenHost != "" {
				t.Fatalf("TokenHost = %q, want empty — confining auth here changes every HTTPS clone that "+
					"works today; the SSH rewrite is confined by SSHRewriteHost instead", d.TokenHost)
			}
		})
	}
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
