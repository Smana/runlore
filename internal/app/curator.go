// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"

	github "github.com/Smana/runlore/internal/forge/github"
	gitlabforge "github.com/Smana/runlore/internal/forge/gitlab"
)

// forgeClient is the Learn-loop forge surface: both curator.Curator.Forge
// (providers.CurationForge) and investigate.Reinvestigator.Forge
// (providers.ReinvestForge) need a client satisfying BOTH, so buildForge
// returns one concrete type wired for whichever provider is configured.
type forgeClient interface {
	providers.CurationForge
	providers.ReinvestForge
}

// buildForge constructs the curation/reinvestigation forge client selected by
// cfg.Forge.Provider ("github", the default, or "gitlab"), or nil when it
// cannot be built: no kb_repo, no usable credential, or (github only, since
// config.Validate already fail-closes gitlab's kb_repo shape at load time) a
// kb_repo that isn't "owner/name". Centralizing this here is what lets
// BuildCurator and BuildReinvestigator share one provider-selection point
// instead of drifting.
func buildForge(cfg *config.Config, log *slog.Logger) forgeClient {
	if cfg.Forge.KBRepo == "" {
		return nil
	}
	base := cfg.Forge.BaseBranch
	if base == "" {
		base = "main"
	}
	switch cfg.Forge.Provider {
	case "", "github":
		token := BuildForgeTokenSource(cfg, log)
		if token == nil {
			return nil
		}
		owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
		if !ok {
			log.Warn("forge disabled: kb_repo must be owner/name", "kb_repo", cfg.Forge.KBRepo)
			return nil
		}
		return github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, github.TokenFunc(token))
	case "gitlab":
		token := BuildGitLabTokenSource(cfg, log)
		if token == nil {
			return nil
		}
		return gitlabforge.New(cfg.Forge.GitLab.BaseURL, cfg.Forge.KBRepo, base, gitlabforge.TokenFunc(token))
	default:
		// config.Validate already rejects an unknown provider at load time; this
		// is an extra fail-safe for a *Config built outside Load.
		log.Warn("forge disabled: unknown forge.provider", "provider", cfg.Forge.Provider)
		return nil
	}
}

// BuildCurator returns a Curator when the forge credentials + KB repo are
// configured, else nil.
func BuildCurator(cfg *config.Config, cat *catalog.Catalog, metrics *telemetry.Metrics, log *slog.Logger) *curator.Curator {
	client := buildForge(cfg, log)
	if client == nil {
		return nil
	}
	dup := cfg.Forge.DupScore
	if dup == 0 {
		dup = 5.0
	}
	minConf := cfg.Forge.MinConfidence
	if minConf == 0 {
		minConf = 0.75
	}
	// Verdicts the operator has configured to stay out of the KB review queue (e.g.
	// no_action). nil when unset ⇒ every verdict is eligible to draft a PR. Validated
	// against the verdict enum in Config.Validate, so entries here are already known.
	var skipVerdicts map[providers.Verdict]bool
	if len(cfg.Forge.SkipVerdicts) > 0 {
		skipVerdicts = make(map[providers.Verdict]bool, len(cfg.Forge.SkipVerdicts))
		for _, v := range cfg.Forge.SkipVerdicts {
			skipVerdicts[providers.Verdict(v)] = true
		}
	}
	log.Info("curator enabled", "provider", forgeProviderName(cfg), "repo", cfg.Forge.KBRepo, "dup_score", dup, "min_confidence", minConf, "skip_verdicts", cfg.Forge.SkipVerdicts)
	cur := &curator.Curator{Forge: client, DupScore: dup, MinConfidence: minConf, SkipVerdicts: skipVerdicts, Metrics: metrics, Log: log}
	if cat != nil { // assign via concrete check to avoid a typed-nil interface
		cur.Catalog = cat
	}
	return cur
}

// forgeProviderName returns the effective forge provider name for logging
// ("github" when Provider is left at its empty default).
func forgeProviderName(cfg *config.Config) string {
	if cfg.Forge.Provider == "" {
		return "github"
	}
	return cfg.Forge.Provider
}

// BuildReinvestigator returns a poller that re-runs KB issues labelled
// "reinvestigate" and posts the fresh findings back, or nil when the forge isn't
// configured. RunLore polls the forge (outbound) — it has no inbound webhooks.
func BuildReinvestigator(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, metrics *telemetry.Metrics, ledger *outcome.Ledger, log *slog.Logger) *investigate.Reinvestigator {
	client := buildForge(cfg, log)
	if client == nil {
		return nil
	}
	model, tools, recall, _ := BuildModelAndTools(ctx, cfg, gp, metrics, log)
	// Same wiring as the investigator, including the outcome ledger — a
	// re-investigation must not fire recall on an entry a normal investigation
	// would have rejected on its outcome history.
	wireRecall(recall, metrics, ledger, log)
	run := func(ctx context.Context, req investigate.Request) (providers.Investigation, error) {
		var res providers.Investigation
		var got bool
		li := &investigate.LoopInvestigator{
			Model: model, VerifyModel: BuildVerifyModel(cfg), Tools: tools, Recall: recall, Verify: true, Log: log,
			Metrics:                   metrics,
			ModelProvider:             cfg.Model.Provider,
			MaxSteps:                  cfg.Investigation.MaxSteps,
			MaxToolOutputBytes:        cfg.Investigation.MaxToolOutputBytes,
			MaxTokensPerInvestigation: cfg.Investigation.MaxTokensPerInvestigation,
			Timeout:                   cfg.Investigation.Timeout.Std(),
			KBMatchScore:              kbMatchScore(recall), // visibility bar tracks the configured recall floor
			OnComplete:                func(inv providers.Investigation) { res, got = inv, true },
		}
		if err := li.Investigate(ctx, req); err != nil {
			return providers.Investigation{}, err
		}
		if !got {
			return providers.Investigation{}, fmt.Errorf("re-investigation was inconclusive")
		}
		return res, nil
	}
	return &investigate.Reinvestigator{Forge: client, Run: run, Log: log}
}
