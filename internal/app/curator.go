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
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"

	github "github.com/Smana/runlore/internal/forge/github"
)

// BuildCurator returns a Curator when the GitHub App token + KB repo are
// configured, else nil.
func BuildCurator(cfg *config.Config, token ForgeToken, cat *catalog.Catalog, metrics *telemetry.Metrics, log *slog.Logger) *curator.Curator {
	if token == nil || cfg.Forge.KBRepo == "" {
		return nil
	}
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		log.Warn("curator disabled: kb_repo must be owner/name", "kb_repo", cfg.Forge.KBRepo)
		return nil
	}
	base := cfg.Forge.BaseBranch
	if base == "" {
		base = "main"
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
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, github.TokenFunc(token))
	log.Info("curator enabled", "repo", cfg.Forge.KBRepo, "dup_score", dup, "min_confidence", minConf, "skip_verdicts", cfg.Forge.SkipVerdicts)
	cur := &curator.Curator{Forge: client, DupScore: dup, MinConfidence: minConf, SkipVerdicts: skipVerdicts, Metrics: metrics, Log: log}
	if cat != nil { // assign via concrete check to avoid a typed-nil interface
		cur.Catalog = cat
	}
	return cur
}

// BuildReinvestigator returns a poller that re-runs KB issues labelled
// "reinvestigate" and posts the fresh findings back, or nil when the forge isn't
// configured. RunLore polls the forge (outbound) — it has no inbound webhooks.
func BuildReinvestigator(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, metrics *telemetry.Metrics, log *slog.Logger) *investigate.Reinvestigator {
	token := BuildForgeTokenSource(cfg, log)
	if token == nil || cfg.Forge.KBRepo == "" {
		return nil
	}
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		return nil
	}
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, cfg.Forge.BaseBranch, github.TokenFunc(token))
	model, tools, recall, _ := BuildModelAndTools(ctx, cfg, gp, metrics, log)
	if recall != nil {
		recall.Metrics = metrics
		recall.Log = log
	}
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
