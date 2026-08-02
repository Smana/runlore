// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/eval"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/model/replay"
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

// demoDefaultTranscript is the recorded transcript `--offline` replays when no path
// is given: a REAL investigation captured once against a live model, so a first-time
// user sees genuine model output with no key and no network. Re-record it with
// `lore demo investigate --record <path>`.
const demoDefaultTranscript = "examples/demo/harbor-chart-bump.transcript.json"

// demoDefaultCatalog is the shipped one-entry knowledge catalog `--catalog default`
// loads. It holds the CURATED result of the harbor-chart-bump investigation, so
// running that same scenario twice shows the loop closing: the first run reasons its
// way to the cause, the second is answered from the entry the first one produced.
//
// That second run is the only offline way to see an INSTANT RECALL card. Recall
// short-circuits before the model is ever called, so unlike a full investigation it
// cannot be captured in a transcript — there are no model turns to record.
const demoDefaultCatalog = "examples/demo/catalog"

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
	offline := fs.String("offline", "", "replay a recorded transcript instead of calling a model — no API key, no network (use \"default\" for the shipped one)")
	record := fs.String("record", "", "record this run's model turns to a transcript file for later --offline replay")
	deliver := fs.Bool("notify", false, "also DELIVER the findings through the notifiers in --config (Slack, Matrix, webhook), not just print them")
	catalogDir := fs.String("catalog", "", "knowledge-catalog directory to consult with INSTANT RECALL before investigating (use \"default\" for the shipped demo entry)")
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

	cfg, apiKeyEnv, err := demoConfig(*cfgPath)
	if err != nil {
		return err
	}

	// --offline replays a transcript, so load it before picking the scenario: with no
	// explicit --scenario, the transcript's OWN Scenario field is the default. Without
	// this, an omitted --scenario would fall back to the first scenario in the
	// directory, which need not be the one the transcript was recorded against — the
	// demo would then announce one incident and replay tool calls for another.
	scenarioID := *scenario
	var transcript *replay.Transcript
	if model == nil && *offline != "" {
		path := *offline
		if path == "default" {
			path = demoDefaultTranscript
		}
		t, err := replay.Load(path)
		if err != nil {
			return err
		}
		transcript = t
		if scenarioID == "" {
			scenarioID = t.Scenario
		}
	}
	c, err := pickScenario(cases, scenarioID)
	if err != nil {
		return err
	}

	// Resolve the model. Three paths, in precedence order:
	//   1. a test-injected model (the existing seam) — used verbatim;
	//   2. --offline — replay the transcript loaded above: no key, no network;
	//   3. the live model built from config, optionally wrapped by --record.
	//
	// Paths 1 and 2 both answer the verify turns from the SAME model (verifyModel is
	// nil), because a transcript is one ordered stream. --record forces the same
	// shape, so what is recorded is exactly what will later replay.
	verifyModel := BuildVerifyModel(cfg)
	var recorder *replay.Recorder
	switch {
	case model != nil:
		verifyModel = nil // the injected model answers verify turns itself
	case transcript != nil:
		model, verifyModel = replay.New(transcript), nil
	default:
		if apiKeyEnv != "" && os.Getenv(apiKeyEnv) == "" {
			return fmt.Errorf("the demo needs a model API key: set %s to your key "+
				"(or point --config at a runlore.yaml with a configured model, or run with "+
				"--offline default to replay a recorded investigation with no key at all). "+
				"Everything else runs against built-in fake providers — no cluster required", apiKeyEnv)
		}
		apiKey := ""
		if apiKeyEnv != "" {
			apiKey = os.Getenv(apiKeyEnv)
		}
		model = BuildModel(cfg, apiKey)
		if *record != "" {
			recorder = replay.NewRecorder(model,
				replay.Recorded{Provider: cfg.Model.Provider, Model: cfg.Model.Model}, c.DisplayName())
			model, verifyModel = recorder, nil // record one ordered stream, replayable as-is
		}
	}

	log := logging.FromConfig(errOut, cfg.Logging.Format, cfg.Logging.Level)
	ctx := context.Background()

	if transcript != nil {
		demoPrintf(out, "== RunLore demo: investigating %q (recorded model turns, fake providers, no cluster) ==\n", c.DisplayName())
		demoPrintf(out, "   model turns recorded %s with %s/%s\n\n",
			transcript.RecordedAt, transcript.RecordedWith.Provider, transcript.RecordedWith.Model)
	} else {
		demoPrintf(out, "== RunLore demo: investigating %q (fake providers, no cluster) ==\n\n", c.DisplayName())
	}
	demoPrintf(out, "incident: %s\n\n", oneLineIndent(c.Symptom()))

	// Wrap each fake tool so every ReAct step (tool name + short args + truncated
	// result) streams to stdout as the loop runs — the whole point of the demo. The
	// fakes and the loop are the REAL production types; only the providers are canned.
	var tools []investigate.Tool
	for _, t := range c.FakeTools() {
		tools = append(tools, tracingTool{inner: t, out: out})
	}

	// --catalog consults the knowledge base before investigating, through the SAME
	// Recall gate production uses. When an entry matches strongly enough the loop
	// short-circuits and the delivered card is a recall card — the "answered in
	// seconds" half of the product, which no transcript can demonstrate.
	var recall *investigate.Recall
	if *catalogDir != "" {
		dir := *catalogDir
		if dir == "default" {
			dir = demoDefaultCatalog
		}
		cat, cerr := catalog.New(dir)
		if cerr != nil {
			return fmt.Errorf("load demo catalog %s: %w", dir, cerr)
		}
		// Gate values, stated plainly because they are NOT production's.
		//
		// Production gates instant recall on the RERANKER's calibrated confidence; the
		// BM25 SoloFloor (default 4.0) is the legacy fallback and is not reachable by
		// raw BM25, whose scores run ~0.1-1.2 (see config.InstantRecall). The reranker
		// needs a model, and this demo's whole point is that it needs neither model nor
		// key — so an offline recall demo has to gate on BM25 at a reachable value.
		//
		// The alternative was to leave the thresholds at the zero-config defaults,
		// which are 0 — meaning ANY hit fires. That is worse: it looks like a gate and
		// is not one. These are explicit, and the demo prints them, so nobody mistakes
		// this for the production bar.
		const demoMinScore, demoSoloFloor = 0.02, 0.02
		recall = &investigate.Recall{
			Catalog:   cat,
			MinScore:  demoMinScore,
			SoloFloor: demoSoloFloor,
			// Structural agreement is NOT relaxed: the entry must still name the
			// incident's workload, which is the gate that stops a generic entry
			// answering a specific incident. That one is production's.
			RequireWorkloadMatch: cfg.Catalog.InstantRecall.RequireWorkloadMatch,
			MarginGap:            cfg.Catalog.InstantRecall.MarginGap,
			Log:                  log,
		}
		demoPrintf(out, "knowledge catalog: %d entr(ies) from %s\n", cat.Len(), dir)
		demoPrintf(out, "   recall gate: BM25 min_score=%.2f solo_floor=%.2f — DEMO values, not production's.\n"+
			"   production gates on the reranker's calibrated confidence, which needs a model.\n",
			demoMinScore, demoSoloFloor)
	}

	var result *providers.Investigation
	li := &investigate.LoopInvestigator{
		Model:         model,
		VerifyModel:   verifyModel,
		Tools:         tools,
		Recall:        recall,
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

	// --notify sends the SAME findings through the real notifiers. The demo already
	// runs the real loop over recorded evidence; this makes the delivered artifact
	// real too, so a Slack card can be produced without a cluster, an incident, or
	// an API key.
	//
	// It is opt-in because the demo's whole promise is that it touches nothing.
	// Posting to a webhook is the one thing here that leaves the machine.
	if *deliver {
		n, nerr := BuildNotifier(cfg, log)
		if nerr != nil {
			return fmt.Errorf("build notifiers for --notify: %w", nerr)
		}
		if n.Len() == 0 {
			return fmt.Errorf("--notify needs at least one notifier configured in --config " +
				"(notify.slack / notify.matrix / notify.webhook); none found")
		}
		if derr := n.Deliver(ctx, *result); derr != nil {
			return fmt.Errorf("deliver findings: %w", derr)
		}
		demoPrintf(out, "\ndelivered to %d notifier(s)\n", n.Len())
	}

	if recorder != nil {
		if err := recorder.Write(*record, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("write transcript: %w", err)
		}
		demoPrintf(out, "\ntranscript written to %s — replay it with `lore demo investigate --offline %s`\n", *record, *record)
	}
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
		if c.Name == id {
			return c, nil
		}
		have = append(have, c.Name)
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
