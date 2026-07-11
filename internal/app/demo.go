// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/eval"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
)

// demoDefaultScenarios is the built-in curated scenario set shipped with RunLore, used
// when --scenarios is not given. Reusing the eval Case fixture shape means these same
// files also replay under `lore eval --cases`.
const demoDefaultScenarios = "examples/scenarios"

// demoDefaultModel / demoDefaultKeyEnv keep the demo zero-config: with no runlore.yaml
// on disk the demo runs against Anthropic keyed off ANTHROPIC_API_KEY, so a first-time
// user needs only their API key — no config ceremony. A real runlore.yaml (via
// --config) overrides this entirely.
const (
	demoDefaultProvider = "anthropic"
	demoDefaultModel    = "claude-sonnet-4-5"
	demoDefaultKeyEnv   = "ANTHROPIC_API_KEY"
)

// RunDemo dispatches the `lore demo <subcommand>` family. Today only `investigate` is
// wired: a zero-cluster, full-loop demonstration of the real investigator against fake
// providers seeded from a fixture incident. The demo is opt-in and adds no required
// user config — its whole point is to REDUCE onboarding friction (watch the agent work
// before wiring a cluster).
func RunDemo(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lore demo investigate [--scenario <name>] [--scenarios <dir>] [--config <path>]")
	}
	switch args[0] {
	case "investigate":
		return runDemoInvestigate(args[1:], os.Stdout, os.Stderr)
	default:
		return fmt.Errorf("unknown demo subcommand %q (want: investigate)", args[0])
	}
}

// runDemoInvestigate parses flags, resolves the scenario + model, then runs the loop.
// It builds the REAL model (BuildModel) from the resolved config; the tests use the
// runDemoInvestigateWithModel seam to inject a scripted, no-network model instead.
func runDemoInvestigate(args []string, out, errOut io.Writer) error {
	return runDemoInvestigateWithModel(args, out, errOut, nil)
}

// runDemoInvestigateWithModel is runDemoInvestigate with an injectable model seam: a
// nil model means "build the real one from config" (the CLI path); a non-nil model is
// used verbatim (the end-to-end test path, no network, no API key). Everything else —
// the fake providers, the loop, verify — is the real production wiring.
func runDemoInvestigateWithModel(args []string, out, errOut io.Writer, model providers.ModelProvider) error {
	fs := flag.NewFlagSet("demo investigate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	scenario := fs.String("scenario", "", "scenario id to run (default: the first in --scenarios)")
	scenariosDir := fs.String("scenarios", demoDefaultScenarios, "directory of curated scenario fixtures")
	cfgPath := fs.String("config", "", "optional runlore.yaml; when omitted the demo uses a zero-config default model")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cases, err := eval.Load(*scenariosDir)
	if err != nil {
		return fmt.Errorf("load scenarios from %s: %w", *scenariosDir, err)
	}
	if len(cases) == 0 {
		return fmt.Errorf("no scenarios found in %s", *scenariosDir)
	}
	c, err := pickScenario(cases, *scenario)
	if err != nil {
		return err
	}

	cfg, apiKeyEnv, err := demoConfig(*cfgPath)
	if err != nil {
		return err
	}

	// Build the real model unless the test injected one. When building it, insist on a
	// model API key first (the one thing the demo genuinely needs) and fail with clear,
	// friendly guidance rather than a stack trace or a hang on the first model call.
	verifyModel := BuildVerifyModel(cfg)
	if model == nil {
		if apiKeyEnv != "" && os.Getenv(apiKeyEnv) == "" {
			return fmt.Errorf("the demo needs a model API key: set %s to your key "+
				"(or point --config at a runlore.yaml with a configured model). "+
				"Everything else runs against built-in fake providers — no cluster required", apiKeyEnv)
		}
		apiKey := ""
		if apiKeyEnv != "" {
			apiKey = os.Getenv(apiKeyEnv)
		}
		model = BuildModel(cfg, apiKey)
	} else {
		verifyModel = nil // the scripted test model answers verify turns itself
	}

	log := logging.FromConfig(errOut, cfg.Logging.Format, cfg.Logging.Level)
	ctx := context.Background()

	demoPrintf(out, "== RunLore demo: investigating %q (fake providers, no cluster) ==\n\n", c.DisplayName())
	demoPrintf(out, "incident: %s\n\n", oneLineIndent(c.Symptom()))

	// Wrap each fake tool so every ReAct step (tool name + short args + truncated
	// result) streams to stdout as the loop runs — the whole point of the demo. The
	// fakes and the loop are the REAL production types; only the providers are canned.
	var tools []investigate.Tool
	for _, t := range c.FakeTools() {
		tools = append(tools, tracingTool{inner: t, out: out})
	}

	var result *providers.Investigation
	li := &investigate.LoopInvestigator{
		Model:         model,
		VerifyModel:   verifyModel,
		Tools:         tools,
		Log:           log,
		Verify:        true,
		ModelProvider: cfg.Model.Provider,
		Timeout:       cfg.Investigation.Timeout.Std(),
		OnComplete:    func(inv providers.Investigation) { result = &inv },
	}
	req := investigate.Request{
		Source:   investigate.SourceAlert,
		Title:    c.DisplayName(),
		Message:  c.Symptom(),
		Workload: c.AffectedWorkload(),
	}
	if err := li.Investigate(ctx, req); err != nil {
		return fmt.Errorf("investigation failed: %w", err)
	}
	if result == nil {
		return fmt.Errorf("the loop produced no findings")
	}
	demoPrintf(out, "\n== submit_findings ==\n%s\n", notify.Format(*result))
	return nil
}

// demoPrintf writes a demo trace line to out, ignoring the write error: the demo prints
// to stdout for a human watching, and a broken stdout pipe must not fail the demo (nor
// clutter every call site with an unactionable error check).
func demoPrintf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// pickScenario returns the requested scenario by id, or the first case when id is
// empty. An unknown id lists the available ids so the user can retry.
func pickScenario(cases []eval.Case, id string) (eval.Case, error) {
	if id == "" {
		return cases[0], nil
	}
	var have []string
	for _, c := range cases {
		if c.DisplayName() == id {
			return c, nil
		}
		have = append(have, c.DisplayName())
	}
	return eval.Case{}, fmt.Errorf("scenario %q not found; available: %s", id, strings.Join(have, ", "))
}

// tracingTool decorates a demo tool so each call streams a compact ReAct step
// (tool name, short args, truncated result) to out before returning the inner
// result unchanged. It only observes — the loop's behavior is identical to
// production; this is how the demo shows the agent's reasoning without touching
// loop.go.
type tracingTool struct {
	inner investigate.Tool
	out   io.Writer
}

func (t tracingTool) Name() string        { return t.inner.Name() }
func (t tracingTool) Description() string { return t.inner.Description() }
func (t tracingTool) Schema() string      { return t.inner.Schema() }

func (t tracingTool) Call(ctx context.Context, args string) (string, error) {
	demoPrintf(t.out, "→ %s(%s)\n", t.Name(), truncate(oneLineArgs(args), 80))
	res, err := t.inner.Call(ctx, args)
	if err != nil {
		demoPrintf(t.out, "  ✗ %v\n", err)
		return res, err
	}
	demoPrintf(t.out, "  %s\n", truncate(oneLineIndent(res), 200))
	return res, err
}

// oneLineArgs flattens a JSON args blob to a single line for the step trace ("{}" for
// the common empty-object case reads as no-args).
func oneLineArgs(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

// oneLineIndent flattens whitespace-heavy multi-line tool output / incident text to a
// single spaced line so the trace stays one row per step.
func oneLineIndent(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate caps s at n runes, appending an ellipsis marker when it cut something.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …"
}

// demoConfig resolves the config the demo runs with. With --config it loads and returns
// that config unchanged (a real runlore.yaml wins). Without it (the zero-config path) it
// synthesizes a minimal default: the built-in Anthropic model keyed off
// ANTHROPIC_API_KEY, so a first-run user needs only their key. The returned string is
// the API-key env var name to read (empty ⇒ keyless, e.g. a local vLLM base_url).
func demoConfig(path string) (cfg *config.Config, keyEnv string, err error) {
	if path != "" {
		loaded, lerr := config.Load(path)
		if lerr != nil {
			return nil, "", lerr
		}
		return loaded, loaded.Model.APIKeyEnv, nil
	}
	return &config.Config{Model: config.Model{
		Provider:  demoDefaultProvider,
		Model:     demoDefaultModel,
		APIKeyEnv: demoDefaultKeyEnv,
	}}, demoDefaultKeyEnv, nil
}
