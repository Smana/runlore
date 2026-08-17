// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/eval"
	"github.com/Smana/runlore/internal/providers"
)

// compareDefaultN is the runs-per-case used by the comparison benchmark when -n is
// not given. It matches the rubric's "median over N=3": enough repeats to compute a
// stable median and a k-of-n pass rate, without the cost of the live default (10).
const compareDefaultN = 3

// RunEvalCompare benchmarks every model in the comparison spec against the replay
// suite and writes one aggregated report (markdown + JSON) to reportDir. The judge
// is fixed across entries (blind grading already anonymizes which model produced a
// result), so scores are comparable. It reuses the single-run replay machinery per
// entry via eval.ComparisonRunner.
func RunEvalCompare(cfg *config.Config, budget *eval.CampaignBudget,
	comparePath, casesDir, reportDir, stamp string, n int,
	jProvider, jBaseURL, jModel, jKeyEnv string) error {
	spec, err := eval.LoadCompareSpec(comparePath)
	if err != nil {
		return err
	}
	cases, err := eval.Load(casesDir)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no eval cases found in %s", casesDir)
	}

	judgeModel, judgeLabel, err := compareJudgeModel(cfg, spec.Judge, jProvider, jBaseURL, jModel, jKeyEnv)
	if err != nil {
		return err
	}
	// The judge grades every run of every entry — len(models) x len(cases) x n
	// completions of a model no per-entry counter sees, since each entry's
	// CountingModel wraps only the entry under test. Counting it separately keeps the
	// per-entry attribution honest AND stops the judge being free.
	judgeSpend := &eval.CountingModel{Inner: budget.Wrap(judgeModel)}
	judge := eval.ModelJudge{Model: judgeSpend}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	maxTokens := effectiveMaxTokens(cfg.Model.MaxTokens)
	spend := evalSpend(cfg)
	ctx, cancel := campaignContext(budget)
	defer cancel()

	models := make([]eval.ModelComparison, 0, len(spec.Models))
	ranCases := 0
	for _, entry := range spec.Models {
		if ctx.Err() != nil {
			break
		}
		apiKey := ""
		if entry.APIKeyEnv != "" {
			apiKey = os.Getenv(entry.APIKeyEnv)
		}
		counting := &eval.CountingModel{
			Inner: budget.Wrap(NewModelClient(entry.Provider, entry.BaseURL, entry.Model, apiKey, maxTokens, entry.Effort, "")),
		}
		// One Spend across every entry: an entry allowed to spend more than its rivals
		// is not being compared with them.
		runner := &eval.ComparisonRunner{Model: counting, Judge: judge, Log: log, Spend: spend}
		fmt.Printf("comparing %-20s (%s)\n", entry.Name, providerModel(entry.Provider, entry.Model))
		compared := runner.RunCases(ctx, cases, n)
		ranCases += len(compared)
		models = append(models, eval.AggregateModel(entry, compared, counting.Total()))
	}
	reportCampaignHalt(budget, ranCases, len(spec.Models)*len(cases))
	reportJudgeSpend(cfg, judgeSpend)

	if stamp == "" {
		stamp = time.Now().UTC().Format(time.RFC3339)
	}
	rep := eval.NewComparisonReport(stamp, n, judgeLabel, models)
	md := rep.Markdown()
	fmt.Print("\n" + md)

	if reportDir == "" {
		return nil
	}
	if err := os.MkdirAll(reportDir, 0o750); err != nil {
		return fmt.Errorf("create report dir: %w", err)
	}
	base := filepath.Join(reportDir, strings.ReplaceAll(stamp, ":", "-")+"-compare")
	jsonBytes, err := rep.JSON()
	if err != nil {
		return fmt.Errorf("render comparison JSON: %w", err)
	}
	if err := os.WriteFile(base+".json", jsonBytes, 0o600); err != nil {
		return fmt.Errorf("write comparison JSON: %w", err)
	}
	if err := os.WriteFile(base+".md", []byte(md), 0o600); err != nil {
		return fmt.Errorf("write comparison markdown: %w", err)
	}
	fmt.Printf("\nreport: %s.md / .json\n", base)
	return nil
}

// compareJudgeModel resolves the fixed judge MODEL for a comparison run, and returns
// a "provider/model" disclosure label for the report. Precedence: --judge-* flags,
// then the spec's judge block, then the configured investigation model. This is
// documented as the ONLY way `--compare` runs without a runlore.yaml (the spec
// supplies its own judge), so unlike a plain judge-optional benchmark, silently
// falling back to "no judge" here would contradict that promise — none of the
// three sources present is a hard error, not a quiet "rubric grading disabled".
//
// It returns the bare model rather than a ready-made eval.Judge so the caller can
// wrap it (campaign budget + spend counter) before it disappears behind the Judge
// interface — a judge already boxed into an interface cannot be metered.
func compareJudgeModel(cfg *config.Config, specJudge *eval.JudgeSpec, jProvider, jBaseURL, jModel, jKeyEnv string) (providers.ModelProvider, string, error) {
	if jModel != "" || jProvider != "" {
		return BuildJudgeModel(cfg, jProvider, jBaseURL, jModel, jKeyEnv), providerModel(jProvider, jModel), nil
	}
	if specJudge != nil {
		m := NewModelClient(specJudge.Provider, specJudge.BaseURL, specJudge.Model,
			os.Getenv(specJudge.APIKeyEnv), effectiveMaxTokens(cfg.Model.MaxTokens), "", "")
		return m, providerModel(specJudge.Provider, specJudge.Model), nil
	}
	if ModelConfigured(cfg) {
		return BuildJudgeModel(cfg, "", "", "", ""), providerModel(cfg.Model.Provider, cfg.Model.Model), nil
	}
	return nil, "", fmt.Errorf("compare requires a judge: set --judge-model (or --judge-provider), " +
		"add a judge: block to the compare spec, or set config.model in the config file")
}

// providerModel renders a "provider/model" label, defaulting an empty provider to
// the OpenAI-compatible wire protocol (mirrors NewModelClient's routing).
func providerModel(provider, model string) string {
	if provider == "" {
		provider = "openai"
	}
	return provider + "/" + model
}

var _ providers.ModelProvider = (*eval.CountingModel)(nil)
