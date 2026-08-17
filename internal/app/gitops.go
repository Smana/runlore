// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"

	"k8s.io/client-go/dynamic"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/config"
	argoexec "github.com/Smana/runlore/internal/executor/argocd"
	fluxexec "github.com/Smana/runlore/internal/executor/flux"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/providers/gitops/argocd"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
	"github.com/Smana/runlore/internal/whatchanged"
)

// BuildGitOps builds the GitOps provider for the configured engine (flux default).
// The differ clones the GitOps source repo over HTTPS; it authenticates private
// repos with the shared GitHub App installation token (the App needs contents:read
// on the source repo). A nil token source (no App configured) means public/local
// repos only.
func BuildGitOps(cfg *config.Config, dc dynamic.Interface, log *slog.Logger) providers.GitOpsProvider {
	differ := buildGitOpsDiffer(cfg, log)
	if GitopsEngine(cfg) == "argocd" {
		log.Info("gitops engine", "engine", "argocd")
		return argocd.New(argocd.NewDynamicReader(dc), differ)
	}
	log.Info("gitops engine", "engine", "flux")
	return flux.New(flux.NewDynamicReader(dc), differ)
}

// buildGitOpsDiffer builds the what_changed differ. Split out of BuildGitOps so
// the credential wiring is reachable by a test: BuildGitOps hands the differ to a
// provider that keeps it unexported, and an unset SSHRewriteHost would silently
// leave RunLore #495 unfixed.
//
// SSHRewriteHost is set — and TokenHost deliberately is not. The differ may only
// rewrite an SSH repoURL into a live HTTPS request toward the host the App token
// is actually for, so an attacker-chosen "repoURL: ssh://git@attacker.example/x"
// stays SSH and dies at "invalid auth method" with nothing transmitted. Leaving
// TokenHost unset keeps every HTTPS clone that works today byte-identical,
// including a GitHub Enterprise install whose API host differs from its git host
// (githubGitHost derives from the API URL, so confining auth on it could withhold
// the credential from a repo that clones fine today).
func buildGitOpsDiffer(cfg *config.Config, log *slog.Logger) *whatchanged.Differ {
	differ := &whatchanged.Differ{
		TokenSource:    BuildForgeTokenSource(cfg, log),
		SSHRewriteHost: githubGitHost(cfg.Forge.GitHubAPIURL),
	}
	if cfg.GitOps.Mirror.IsEnabled() {
		if mc, err := whatchanged.NewMirrorCache(cfg.GitOps.Mirror.Dir, cfg.GitOps.Mirror.Max); err != nil {
			log.Warn("gitops: mirror cache unavailable; falling back to clone-per-call", "err", err)
		} else {
			differ.Mirrors = mc
		}
	}
	return differ
}

// BuildExecutor returns the rung-2/3 action executor for the configured GitOps
// engine — the same engine switch as BuildGitOps, so the executor always
// matches the provider that proposed the target (an Argo Application action
// must never reach the Flux executor and vice versa).
func BuildExecutor(cfg *config.Config, dc dynamic.Interface) action.Executor {
	if GitopsEngine(cfg) == "argocd" {
		return argoexec.New(dc)
	}
	return fluxexec.New(dc)
}

// GitOpsFromKube builds the GitOps provider from the ambient kubeconfig (best-effort).
func GitOpsFromKube(cfg *config.Config, log *slog.Logger) providers.GitOpsProvider {
	restCfg, err := RestConfig()
	if err != nil {
		log.Warn("no kube client; what-changed disabled", "err", err)
		return nil
	}
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		log.Warn("dynamic client unavailable; what-changed disabled", "err", err)
		return nil
	}
	return BuildGitOps(cfg, dc, log)
}
