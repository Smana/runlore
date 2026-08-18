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
