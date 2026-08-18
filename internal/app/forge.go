// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/Smana/runlore/internal/config"

	github "github.com/Smana/runlore/internal/forge/github"
)

// ForgeToken mints GitHub App installation tokens.
type ForgeToken func(context.Context) (string, error)

// BuildForgeTokenSource builds the GitHub App installation-token source shared by
// the curator (issues/PRs) and catalog git-sync (clone auth) — one identity for
// both forge writes and reads. Returns nil when no App is configured.
func BuildForgeTokenSource(cfg *config.Config, log *slog.Logger) ForgeToken {
	ga := cfg.Forge.GitHubApp
	if ga.AppID == 0 || ga.InstallationID == 0 || ga.PrivateKeyEnv == "" {
		return nil
	}
	pemData := os.Getenv(ga.PrivateKeyEnv)
	if pemData == "" {
		log.Warn("forge auth disabled: empty private key env", "env", ga.PrivateKeyEnv)
		return nil
	}
	key, err := github.ParsePrivateKey(pemData)
	if err != nil {
		log.Warn("forge auth disabled: bad private key", "err", err)
		return nil
	}
	return github.NewAppTokenSource(cfg.Forge.GitHubAPIURL, ga.AppID, ga.InstallationID, key).Token
}

// BuildGitLabTokenSource builds the curation/reinvestigation token source for
// the GitLab forge provider. Unlike BuildForgeTokenSource (which mints and
// caches a short-lived GitHub App installation token), GitLab has no App-
// equivalent bot identity: the credential is a static project or group access
// token read once from the configured env var and wrapped in a TokenFunc for
// shape-parity with the GitHub path. Returns nil when the env var itself is
// unset/empty at runtime — config.Validate already fails closed on an empty
// token_env at config-load time, but an operator can still forget to actually
// set the env var when deploying, so this stays a warn-and-disable (not a
// panic) exactly like BuildForgeTokenSource's own credential checks.
func BuildGitLabTokenSource(cfg *config.Config, log *slog.Logger) ForgeToken {
	te := cfg.Forge.GitLab.TokenEnv
	if te == "" {
		return nil
	}
	tok := os.Getenv(te)
	if tok == "" {
		log.Warn("gitlab forge auth disabled: empty token env", "env", te)
		return nil
	}
	return func(context.Context) (string, error) { return tok, nil }
}

// BuildCloneTokenSource picks the credential for CLONING a git repository over
// HTTPS, by forge provider. Every clone path uses it: the catalog git-sync that
// reads the KB repo, the what_changed differ that clones GitOps source repos,
// and source_diff.
//
// It is deliberately separate from BuildForgeTokenSource, which mints GitHub App
// installation tokens and is correct only for the paths that talk to the GitHub
// API itself (curate, sweeps). A clone is not one of those: it authenticates to
// whichever forge the operator configured.
//
// Getting that wrong fails SILENTLY, which is why this exists at all. Catalog
// sync used to fall back to the GitHub App identity on every deployment; on
// GitLab that source is nil, so the syncer cloned ANONYMOUSLY — with a private KB
// project, RunLore opened the merge request, a human merged it, and the merged
// knowledge never came back into the catalog. The learning loop was severed at
// the seam, silently: the log line explaining the fallback did not even fire,
// because the token source was nil rather than wrong. The what_changed differ
// held the identical bug against a private GitOps repo — the differ cloned
// anonymously and what_changed produced nothing for EVERY object — until it was
// moved onto this same rule.
//
// One username serves both forges: the token is the basic-auth PASSWORD, and
// GitLab accepts any non-blank username alongside a project/group access token
// (see catalog.Syncer.auth and whatchanged.Differ.auth, both "x-access-token").
//
// catalog.git.token_env still wins over this for the KB repo; it is the explicit
// escape hatch.
func BuildCloneTokenSource(cfg *config.Config, log *slog.Logger) ForgeToken {
	if cfg.Forge.Provider == "gitlab" {
		return BuildGitLabTokenSource(cfg, log)
	}
	return BuildForgeTokenSource(cfg, log)
}

// forgeGitHost names the ONE host a clone credential from BuildCloneTokenSource
// may be sent to — the host the configured forge serves GIT REMOTES from.
//
// It is the credential boundary for what_changed and source_diff, so it is TOTAL
// on purpose: it always names a host, and never returns "" (which a Differ reads
// as "attach the token everywhere"). An operator override wins; otherwise the
// host is derived, and it is derived from forgeWebHost because git and web share
// a host on both forges — only GitHub's API can be split onto an api.* subdomain,
// which is precisely the case config.Validate refuses without forge.git_host.
//
// A wrong value here does not leak the credential — it withholds it, which shows
// up as an empty what_changed. That is why the ambiguous case is a startup
// failure rather than a default.
//
// Every branch folds through asciiLowerHost, so no input reaching this function
// can produce a boundary that is fold-equal to an ASCII host it is not. The loud
// remedy lives in config.Validate (validateForgeGitHost / asciiForgeHost), which
// refuses a non-ASCII forge host at startup naming the key it came from; this is
// the backstop for a *config.Config assembled without Load.
func forgeGitHost(cfg *config.Config) string {
	if h := asciiLowerHost(strings.TrimSpace(cfg.Forge.GitHost)); h != "" {
		return h
	}
	return forgeWebHost(cfg)
}

// asciiLowerHost lowercases host when it is 7-bit ASCII and returns it UNCHANGED
// otherwise. It is the single fold every forge host derivation goes through —
// forgeGitHost, forgeWebHost and githubGitHost — because each of their results
// becomes a credential boundary somewhere: Differ.TokenHost, Differ.SSHRewriteHost,
// the source_diff token host, the forge repo reference a thread link is matched
// against.
//
// Folding a non-ASCII host is the bug, not the input. strings.ToLower applies
// Unicode simple case mapping and maps U+0130 to plain 'i'; whatever dials the
// host resolves it through idna.Lookup.ToASCII, which does not. So a forge at
// "gİtlab.example.com" folded to the boundary "gitlab.example.com" — handing the
// token to anyone who registers that ASCII name, and withholding it from the
// operator's own instance, which is the silent what_changed gap of #495.
//
// Returning the host UNFOLDED (rather than "") is deliberate on both counts: ""
// reads as "attach the token everywhere" in whatchanged.Differ, while a retained
// non-ASCII host equals no clone URL's host at all — whatchanged.hostOf refuses
// non-ASCII and returns "" — so the credential is confined to nothing until the
// operator fixes the config that config.Validate already refused to load.
func asciiLowerHost(host string) string {
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			return host
		}
	}
	return strings.ToLower(host)
}
