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
// provider that keeps it unexported, and either half of this wiring fails
// silently — an unset SSHRewriteHost leaves RunLore #495 unfixed, an unset
// TokenHost hands the credential to whatever host a repoURL names.
//
// The credential is CONFINED to the forge's own git host, and that is the point.
// A GitOps spec.source.repoURL is cluster state: in a shared cluster anyone who
// can create an Argo CD Application or a Flux GitRepository picks it, so an
// unconfined differ sends the forge credential wherever "repoURL:
// https://attacker.example/org/repo.git" points. Confinement takes away nothing
// an operator relies on — a forge credential authenticates only on its own forge,
// so every clone that succeeds today still succeeds, and the clones that stop
// carrying the token were only ever leaking it. Which host that is comes from
// forgeGitHost, whose one ambiguous input (a GHE API on an api.* subdomain) is a
// config-load failure rather than a guess, because guessing it wrong would
// withhold the credential from every GitOps repo and show up only as an empty
// what_changed.
//
// SSHRewriteHost is the same host for a narrower reason: the SSH→HTTPS rewrite
// turns a repoURL into a live HTTPS request, so it may only aim at the host the
// credential is for (see whatchanged.Differ.effectiveCloneURL). Both stay EMPTY
// when there is no credential at all — nothing to confine, and a rewrite with no
// token would trade go-git's SSH-agent path for an anonymous 401.
//
// The credential follows forge.provider (BuildCloneTokenSource), not the GitHub
// App alone. It was the App alone, which left a GitLab forge with no clone
// credential whatsoever: a private GitOps repo cloned ANONYMOUSLY and
// what_changed produced nothing for any object — the same silent severing catalog
// sync suffered, in the same shape, at a second seam.
func buildGitOpsDiffer(cfg *config.Config, log *slog.Logger) *whatchanged.Differ {
	differ := &whatchanged.Differ{TokenSource: BuildCloneTokenSource(cfg, log)}
	if differ.TokenSource != nil {
		host := forgeGitHost(cfg)
		differ.TokenHost, differ.SSHRewriteHost = host, host
		log.Info("what_changed: forge credential confined to the forge git host", "git_host", host)
	} else {
		log.Warn("what_changed: no forge credential; GitOps repos are cloned anonymously and SSH "+
			"repoURLs are not rewritten to HTTPS — change correlation stays empty for any private "+
			"repo, and for any GitOps object whose source is an SSH URL",
			"forge_provider", cfg.Forge.Provider)
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
