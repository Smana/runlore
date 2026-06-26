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
