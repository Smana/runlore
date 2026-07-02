package app

import (
	"context"
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
func RunEvalCompare(cfg *config.Config, comparePath, casesDir, reportDir, stamp string, n int,
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

	judge, judgeLabel := buildCompareJudge(cfg, spec.Judge, jProvider, jBaseURL, jModel, jKeyEnv)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	maxTokens := effectiveMaxTokens(cfg.Model.MaxTokens)
	ctx := context.Background()

	models := make([]eval.ModelComparison, 0, len(spec.Models))
	for _, entry := range spec.Models {
		apiKey := ""
		if entry.APIKeyEnv != "" {
			apiKey = os.Getenv(entry.APIKeyEnv)
		}
		counting := &eval.CountingModel{
			Inner: NewModelClient(entry.Provider, entry.BaseURL, entry.Model, apiKey, maxTokens, entry.Effort),
		}
		runner := &eval.ComparisonRunner{Model: counting, Judge: judge, Log: log}
		fmt.Printf("comparing %-20s (%s)\n", entry.Name, providerModel(entry.Provider, entry.Model))
		compared := runner.RunCases(ctx, cases, n)
		models = append(models, eval.AggregateModel(entry, compared, counting.Total()))
	}

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

// buildCompareJudge resolves the fixed judge for a comparison run, and returns a
// "provider/model" disclosure label for the report. Precedence: --judge-* flags,
// then the spec's judge block, then the configured investigation model. A nil
// judge (no flags, no spec judge, no config model) disables rubric grading —
// pass/coverage/token columns still populate.
func buildCompareJudge(cfg *config.Config, specJudge *eval.JudgeSpec, jProvider, jBaseURL, jModel, jKeyEnv string) (eval.Judge, string) {
	if jModel != "" || jProvider != "" {
		return eval.ModelJudge{Model: BuildJudgeModel(cfg, jProvider, jBaseURL, jModel, jKeyEnv)}, providerModel(jProvider, jModel)
	}
	if specJudge != nil {
		m := NewModelClient(specJudge.Provider, specJudge.BaseURL, specJudge.Model,
			os.Getenv(specJudge.APIKeyEnv), effectiveMaxTokens(cfg.Model.MaxTokens), "")
		return eval.ModelJudge{Model: m}, providerModel(specJudge.Provider, specJudge.Model)
	}
	if ModelConfigured(cfg) {
		return eval.ModelJudge{Model: BuildJudgeModel(cfg, "", "", "", "")}, providerModel(cfg.Model.Provider, cfg.Model.Model)
	}
	return nil, "(none — rubric grading disabled)"
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
