// SPDX-License-Identifier: Apache-2.0

package app

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
)

// RunInvestigate runs a single on-demand investigation and prints the findings.
func RunInvestigate(args []string) error {
	fs := flag.NewFlagSet("investigate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config file (default: ./runlore.yaml if present, else the environment)")
	alert := fs.String("alert", "", "alert/symptom name to investigate")
	namespace := fs.String("namespace", "", "namespace of the affected workload")
	message := fs.String("message", "", "free-text symptom description")
	modelName := fs.String("model", "", "override the model name")
	baseURL := fs.String("base-url", "", "override the OpenAI-compatible endpoint")
	metricsURL := fs.String("metrics-url", "", "PromQL endpoint — enables query_metrics")
	logsURL := fs.String("logs-url", "", "logs endpoint — enables query_logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *alert == "" && *message == "" {
		return fmt.Errorf("provide --alert and/or --message")
	}
	explicit := *cfgPath != ""
	cfg, err := resolveInvestigateConfig(cmp.Or(*cfgPath, defaultConfigPath), explicit)
	if err != nil {
		return err
	}
	// Flags override the resolved config. They are how a user points the CLI at their
	// stack without writing a file.
	if *modelName != "" {
		cfg.Model.Model = *modelName
	}
	if *baseURL != "" {
		cfg.Model.BaseURL = *baseURL
	}
	if *metricsURL != "" {
		cfg.Metrics.URL = *metricsURL
	}
	if *logsURL != "" {
		cfg.Logs.URL = *logsURL
	}
	if !ModelConfigured(cfg) {
		return fmt.Errorf("investigate requires a configured model (set config.model)")
	}
	if off := disabledTools(cfg); len(off) > 0 {
		fmt.Fprintf(os.Stderr, "note: running without %s — pass --metrics-url/--logs-url or a --config to enable them\n",
			strings.Join(off, ", "))
	}
	// Progress logs go to stderr; the findings go to stdout.
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	ctx := context.Background()

	model, tools, recall, _ := BuildModelAndTools(ctx, cfg, GitOpsFromKube(cfg, log), nil, log)
	// The one-shot CLI bills the operator's model exactly as the server does, so it
	// gets the same spend ceilings. It ran with NONE of them until now, contradicting
	// internal/config/load.go, which applies the bounded defaults precisely so that
	// "anyone running `lore serve --config` or `lore investigate` directly" is covered
	// — the defaults were computed and then never handed to the loop.
	loopPricing, verifyPricing := investigationPricing(cfg)
	var result *providers.Investigation
	li := &investigate.LoopInvestigator{
		Model: model, Tools: tools, Recall: recall, Actions: action.New(cfg.Actions), Log: log, Verify: true,
		ModelProvider:             cfg.Model.Provider,
		Pricing:                   loopPricing,
		Verifier:                  investigate.VerifyOn(BuildVerifyModel(cfg), verifyPricing),
		MaxSteps:                  cfg.Investigation.MaxSteps,
		MaxToolOutputBytes:        cfg.Investigation.MaxToolOutputBytes,
		MaxTokensPerInvestigation: cfg.Investigation.MaxTokensPerInvestigation,
		MaxCostPerInvestigation:   cfg.Investigation.MaxCostPerInvestigation,
		Timeout:                   cfg.Investigation.Timeout.Std(),
		KBMatchScore:              kbMatchScore(recall),       // visibility bar tracks the configured recall floor
		KindScope:                 kindScoperFromTools(tools), // discovery decides namespaced-ness, not a kind list
		OnComplete:                func(inv providers.Investigation) { result = &inv },
	}
	title := *alert
	if title == "" {
		title = "on-demand investigation"
	}
	req := investigate.Request{
		Source: investigate.SourceAlert, Title: title, Message: *message,
		Workload: providers.Workload{Namespace: *namespace},
	}
	if err := li.Investigate(ctx, req); err != nil {
		return err
	}
	if result == nil {
		return fmt.Errorf("investigation produced no findings")
	}
	fmt.Println(notify.Format(*result))
	return nil
}

// disabledTools names the investigation signals this run will NOT have, so a thin
// answer is explainable rather than mysterious. The CLI deliberately degrades
// instead of demanding a full stack — but silence about it would look like a bug.
func disabledTools(cfg *config.Config) []string {
	var off []string
	if cfg.Metrics.URL == "" {
		off = append(off, "metrics (query_metrics)")
	}
	if cfg.Logs.URL == "" {
		off = append(off, "logs (query_logs)")
	}
	// Reuse the enablement helper rather than restating its condition: the notice
	// must name exactly what the tool wiring actually gates on, and two copies of
	// that rule would eventually disagree.
	if !CatalogConfigured(cfg) {
		off = append(off, "knowledge catalog (kb_search, instant recall)")
	}
	return off
}
