// SPDX-License-Identifier: Apache-2.0

package app

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

type investigateOptions struct {
	cfgPath     string
	alert       string
	namespace   string
	message     string
	provider    string
	modelName   string
	baseURL     string
	effort      string
	metricsURL  string
	logsURL     string
	kubeContext string
	jsonOut     bool
	quiet       bool

	workload providers.Workload
	title    string
}

// parseTarget parses a positional workload target of the form "<ns>/<name>" or
// "<kind>/<ns>/<name>" into a providers.Workload.
func parseTarget(raw string) (providers.Workload, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return providers.Workload{}, fmt.Errorf("invalid target %q: want <namespace>/<name> or <kind>/<namespace>/<name>", raw)
	}
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return providers.Workload{}, fmt.Errorf("invalid target %q: empty segment", raw)
		}
	}
	if len(parts) == 2 {
		return providers.Workload{Namespace: parts[0], Name: parts[1]}, nil
	}
	return providers.Workload{Kind: parts[0], Namespace: parts[1], Name: parts[2]}, nil
}

func parseInvestigateArgs(args []string) (investigateOptions, error) {
	fs := flag.NewFlagSet("investigate", flag.ContinueOnError)
	var opts investigateOptions
	fs.StringVar(&opts.cfgPath, "config", "", "path to config file (default: ./runlore.yaml if present, else the environment)")
	fs.StringVar(&opts.alert, "alert", "", "alert/symptom name to investigate")
	fs.StringVar(&opts.namespace, "namespace", "", "namespace of the affected workload")
	fs.StringVar(&opts.message, "message", "", "free-text symptom description")
	fs.StringVar(&opts.provider, "provider", "", "override model provider (openai, anthropic, gemini)")
	fs.StringVar(&opts.modelName, "model", "", "override the model name")
	fs.StringVar(&opts.baseURL, "base-url", "", "override the OpenAI-compatible endpoint")
	fs.StringVar(&opts.effort, "effort", "", "override model reasoning effort level")
	fs.StringVar(&opts.metricsURL, "metrics-url", "", "PromQL endpoint — enables query_metrics")
	fs.StringVar(&opts.logsURL, "logs-url", "", "logs endpoint — enables query_logs")
	fs.StringVar(&opts.kubeContext, "kube-context", "", "kubeconfig context to use")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit final investigation findings as JSON on stdout")
	fs.BoolVar(&opts.quiet, "quiet", false, "suppress stderr progress and startup notices")

	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return investigateOptions{}, err
	}
	if len(rest) > 1 {
		return investigateOptions{}, fmt.Errorf("expected at most one positional target (<ns>/<name> or <kind>/<ns>/<name>), got %d", len(rest))
	}
	var rawTarget string
	if len(rest) == 1 {
		rawTarget = rest[0]
		w, err := parseTarget(rawTarget)
		if err != nil {
			return investigateOptions{}, err
		}
		if opts.namespace != "" && opts.namespace != w.Namespace {
			return investigateOptions{}, fmt.Errorf("--namespace %q conflicts with target namespace %q", opts.namespace, w.Namespace)
		}
		opts.workload = w
	} else {
		opts.workload = providers.Workload{Namespace: opts.namespace}
	}
	if rawTarget == "" && opts.alert == "" && opts.message == "" {
		return investigateOptions{}, fmt.Errorf("provide a target (<ns>/<name> or <kind>/<ns>/<name>), --alert, and/or --message")
	}
	opts.title = opts.alert
	if opts.title == "" {
		if rawTarget != "" {
			opts.title = "Investigate " + rawTarget
		} else {
			opts.title = "on-demand investigation"
		}
	}
	return opts, nil
}

// applyInvestigateOverrides applies CLI flag overrides to cfg and validates the
// resulting config so invalid flags (e.g. bad --effort or cleartext --base-url)
// fail fast before any model or cluster call.
func applyInvestigateOverrides(cfg *config.Config, opts investigateOptions) error {
	if opts.provider != "" {
		cfg.Model.Provider = strings.ToLower(strings.TrimSpace(opts.provider))
	}
	if opts.modelName != "" {
		cfg.Model.Model = opts.modelName
	}
	if opts.baseURL != "" {
		cfg.Model.BaseURL = opts.baseURL
	}
	if opts.effort != "" {
		cfg.Model.Effort = opts.effort
	}
	if opts.metricsURL != "" {
		cfg.Metrics.URL = opts.metricsURL
	}
	if opts.logsURL != "" {
		cfg.Logs.URL = opts.logsURL
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !ModelConfigured(cfg) {
		return fmt.Errorf("investigate requires a configured model (set config.model)")
	}
	return nil
}

// formatToolCallProgress renders a single concise, secret-redacted stderr progress
// line for a tool call.
func formatToolCallProgress(call providers.ToolCall) string {
	args := strings.TrimSpace(call.Args)
	if args == "" || args == "{}" {
		return fmt.Sprintf("→ %s()", call.Name)
	}
	args = redact.Secrets(args)
	args = strings.Join(strings.Fields(args), " ")
	const maxPreview = 80
	if len(args) > maxPreview {
		args = args[:maxPreview-1] + "…"
	}
	return fmt.Sprintf("→ %s(%s)", call.Name, args)
}

func writeInvestigationJSON(w io.Writer, inv providers.Investigation) error {
	b, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return fmt.Errorf("encode investigation JSON: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// RunInvestigate runs a single on-demand investigation and prints the findings.
func RunInvestigate(args []string) error {
	opts, err := parseInvestigateArgs(args)
	if err != nil {
		return err
	}
	explicit := opts.cfgPath != ""
	cfg, source, err := resolveInvestigateConfig(cmp.Or(opts.cfgPath, defaultConfigPath), explicit, opts.provider)
	if err != nil {
		return err
	}
	if err := applyInvestigateOverrides(cfg, opts); err != nil {
		return err
	}
	if opts.kubeContext != "" {
		if _, err := RestConfigForContext(opts.kubeContext); err != nil {
			return fmt.Errorf("kube-context %q: %w", opts.kubeContext, err)
		}
	}
	verboseStderr := !opts.quiet && !opts.jsonOut
	if verboseStderr {
		fmt.Fprintf(os.Stderr, "using %s/%s (source: %s)\n", cmp.Or(cfg.Model.Provider, "openai"), cfg.Model.Model, source)
		if off := disabledTools(cfg); len(off) > 0 {
			fmt.Fprintf(os.Stderr, "note: running without %s — pass --metrics-url/--logs-url or a --config to enable them\n",
				strings.Join(off, ", "))
		}
	}
	// Progress logs go to stderr; the findings go to stdout.
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	ctx := context.Background()

	gp := GitOpsFromKubeContext(cfg, opts.kubeContext, log)
	model, tools, recall, _ := BuildModelAndTools(withKubeContext(ctx, opts.kubeContext), cfg, gp, nil, log)
	// The one-shot CLI bills the operator's model exactly as the server does, so it
	// gets the same spend ceilings.
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
		HostStyles:                provenanceHostStyles(cfg),  // server-derived Git provenance permalinks
		KindScope:                 kindScoperFromTools(tools), // discovery decides namespaced-ness, not a kind list
		OnComplete:                func(inv providers.Investigation) { result = &inv },
	}
	if verboseStderr {
		li.OnToolCall = func(_ int, call providers.ToolCall) {
			fmt.Fprintln(os.Stderr, formatToolCallProgress(call))
		}
	}
	req := investigate.Request{
		Source:   investigate.SourceAlert,
		Title:    opts.title,
		Message:  opts.message,
		Workload: opts.workload,
	}
	if err := li.Investigate(ctx, req); err != nil {
		return err
	}
	if result == nil {
		return fmt.Errorf("investigation produced no findings")
	}
	if opts.jsonOut {
		return writeInvestigationJSON(os.Stdout, *result)
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
