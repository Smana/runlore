// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"

	"k8s.io/client-go/dynamic"

	"github.com/Smana/runlore/internal/config"
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
	differ := &whatchanged.Differ{TokenSource: BuildForgeTokenSource(cfg, log)}
	if GitopsEngine(cfg) == "argocd" {
		log.Info("gitops engine", "engine", "argocd")
		return argocd.New(argocd.NewDynamicReader(dc), differ)
	}
	log.Info("gitops engine", "engine", "flux")
	return flux.New(flux.NewDynamicReader(dc), differ)
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
