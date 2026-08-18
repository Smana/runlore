// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/whatchanged"
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
	// source_diff's clone URLs come out of sourcerepo.Allowlist.Match, which
	// already emits HTTPS — so the SSH→HTTPS rewrite has nothing to do there and
	// must stay off. Setting SSHRewriteHost would arm a rewrite on a path whose
	// URLs are MODEL-chosen across the whole allowlist, which is exactly the input
	// the allowlist exists to distrust. TokenHost, by contrast, must stay set:
	// that is what stops a github.com token being sent to an allowlisted repo on
	// another host.
	t.Run("source_diff arms no SSH rewrite and keeps its token confined", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.SourceRepos.Allow = []string{"github.com/acme/*"}
		d := sourceDifferFrom(t, appendSourceDiffTool(cfg, nil, log))
		if d.SSHRewriteHost != "" {
			t.Fatalf("source_diff SSHRewriteHost = %q, want empty — its clone URLs are already "+
				"HTTPS and model-chosen; the rewrite must not be armed there", d.SSHRewriteHost)
		}
		if d.TokenHost != "github.com" {
			t.Fatalf("source_diff TokenHost = %q, want github.com — the token must stay confined "+
				"to the forge host across a multi-host allowlist", d.TokenHost)
		}
	})
}

// sourceDifferFrom digs the concrete Differ out of the registered source_diff
// tool so its credential wiring can be asserted.
func sourceDifferFrom(t *testing.T, tools []investigate.Tool) *whatchanged.Differ {
	t.Helper()
	for _, tl := range tools {
		sd, ok := tl.(investigate.SourceDiffTool)
		if !ok {
			continue
		}
		d, ok := sd.Source.(*whatchanged.Differ)
		if !ok {
			t.Fatalf("source_diff Source is %T, want *whatchanged.Differ", sd.Source)
		}
		return d
	}
	t.Fatal("source_diff tool not registered")
	return nil
}

// TestBuildGitOpsDifferConfinesSSHRewrite pins the what_changed credential
// wiring (RunLore #495).
//
// Three halves, all load-bearing:
//
//   - With a GitHub App credential, SSHRewriteHost must be SET, or the SSH→HTTPS
//     rewrite never fires and #495 is silently un-fixed — the exact silent-data-gap
//     failure the issue is about.
//   - TokenHost must stay UNSET, because it governs which clones may carry the
//     token and is derived from the forge API URL. A GitHub Enterprise install
//     whose API host differs from its git host would start withholding the
//     credential from repos that clone fine today.
//   - With NO credential, SSHRewriteHost must stay EMPTY: githubGitHost would
//     otherwise name github.com for a deployment holding no github.com token.
//
// The rewrite is confined by SSHRewriteHost instead of TokenHost, so a wrong host
// costs an unrewritten SSH URL rather than a broken clone or a leaked credential.
func TestBuildGitOpsDifferConfinesSSHRewrite(t *testing.T) {
	log := slog.Default()
	for _, tc := range []struct{ name, apiURL, want string }{
		{"default forge", "", "github.com"},
		{"github enterprise", "https://ghe.example.com/api/v3", "ghe.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := forgeConfigured(t)
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

	// No GitHub App (an unconfigured forge, or a GitLab one — BuildForgeTokenSource
	// mints GitHub App tokens only). githubGitHost would still say "github.com",
	// which must NOT become an armed rewrite host for a deployment that holds no
	// github.com token. #495 stays open on that path; buildGitOpsDiffer logs it.
	t.Run("no credential arms no rewrite", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Forge.Provider = "gitlab"
		d := buildGitOpsDiffer(cfg, log)
		if d.TokenSource != nil {
			t.Fatal("BuildForgeTokenSource must be nil without a GitHub App; this test's premise is stale")
		}
		if d.SSHRewriteHost != "" {
			t.Fatalf("SSHRewriteHost = %q with no credential, want empty — the rewrite must not trust "+
				"a host derived from a forge URL when no token for it exists", d.SSHRewriteHost)
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
