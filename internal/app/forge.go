// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"os"

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
