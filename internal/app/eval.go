// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/eval"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/providers"
)

// evalCostUSD estimates the campaign cost from the optional config pricing; nil
// when unpriced so the report omits cost_usd instead of claiming $0.00.
func evalCostUSD(cfg *config.Config, u providers.Usage) *float64 {
	p := cfg.Model.Pricing
	if p == nil {
		return nil
	}
	c := eval.EstimateCostUSD(u, p.InputUSDPerMTok, p.CachedInputUSDPerMTok, p.OutputUSDPerMTok)
	return &c
}

// RunEval replays recorded incident cases through the investigation loop and
// reports the RCA-identification rate. Requires a configured model.
func RunEval(args []string) error {
	if len(args) > 0 && args[0] == "scorecard" {
		return RunEvalScorecard(args[1:])
	}
	flags := flag.NewFlagSet("eval", flag.ContinueOnError)
	cfgPath := flags.String("config", "runlore.yaml", "path to config file")
	casesDir := flags.String("cases", "examples/eval", "directory of replay cases")
	live := flags.Bool("live", false, "live-fire mode: run scenarios against the real cluster")
	scnDir := flags.String("scenarios", "eval/scenarios", "directory of live-fire scenarios")
	recordDir := flags.String("record", "eval/fixtures", "where to write recorded runs (replay corpus)")
	reportDir := flags.String("report-dir", "eval/reports", "where to write the campaign report")
	prevReport := flags.String("baseline", "", "previous report JSON for regression diff")
	n := flags.Int("n", 1, "runs per case: replay defaults to 1, live to 10 when unset")
	failUnder := flags.Float64("fail-under", 0, "fail (non-zero exit) when campaign pass-rate < this (0 = no gate)")
	stamp := flags.String("stamp", "", "report timestamp (RFC3339); blank = now")
	jProvider := flags.String("judge-provider", "", "judge model provider (default: investigation model)")
	jBaseURL := flags.String("judge-base-url", "", "judge model base URL")
	jModel := flags.String("judge-model", "", "judge model name")
	jKeyEnv := flags.String("judge-api-key-env", "", "env var holding the judge API key")
	compare := flags.String("compare", "", "path to a model-comparison spec (benchmark several models over the replay suite)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	nExplicit, cfgExplicit := false, false
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "n":
			nExplicit = true
		case "config":
			cfgExplicit = true
		}
	})
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// --compare's spec carries its own per-entry models (and, via judge:, often its
		// own judge), so it is the one path that can run with no runlore.yaml at all.
		// Forgive ONLY that exact case: the default path (--config not given) AND the
		// file simply absent. An explicitly-passed --config must still hard-fail (a
		// typo'd path must never be silently ignored), and a file that exists but fails
		// to parse/validate must still hard-fail on the default path too (it is broken,
		// not absent) — errors.Is(fs.ErrNotExist) is what tells "absent" apart from
		// "broken".
		if *compare == "" || cfgExplicit || !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		cfg = &config.Config{}
	}
	// The comparison spec carries its own per-entry models (and optionally its own
	// judge), so it does NOT require a configured config.model — it only borrows the
	// config's output-token cap and, as a fallback, the config judge.
	if *compare != "" {
		if !nExplicit {
			*n = compareDefaultN
		}
		return RunEvalCompare(cfg, *compare, *casesDir, *reportDir, *stamp, *n,
			*jProvider, *jBaseURL, *jModel, *jKeyEnv)
	}
	if !ModelConfigured(cfg) {
		return fmt.Errorf("eval requires a configured model (set config.model)")
	}
	if *live {
		if !nExplicit {
			*n = 10
		}
		return RunEvalLive(cfg, *scnDir, *recordDir, *reportDir, *prevReport, *stamp, *n,
			*jProvider, *jBaseURL, *jModel, *jKeyEnv)
	}
	// ---- existing replay path (unchanged) ----
	cases, err := eval.Load(*casesDir)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no eval cases found in %s", *casesDir)
	}
	apiKey := ""
	if cfg.Model.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Model.APIKeyEnv)
	}
	counting := &eval.CountingModel{Inner: BuildModel(cfg, apiKey)}
	runner := &eval.Runner{Model: counting, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	camp := runner.RunN(context.Background(), cases, *n)
	for _, a := range camp.Aggregates {
		status := "MISSED"
		if a.Reached {
			status = "REACHED"
		}
		flaky := ""
		if a.Flaky {
			flaky = " flaky"
		}
		fmt.Printf("%-7s  %-32s  pass-rate=%.0f%% (n=%d)%s", status, a.Name, a.PassRate*100, a.Runs, flaky)
		if len(a.Missing) > 0 {
			fmt.Printf("  missing: %s", strings.Join(a.Missing, ", "))
		}
		fmt.Println()
	}
	if len(camp.Aggregates) == 0 {
		fmt.Print("\nno eval cases ran")
	} else {
		fmt.Printf("\nreached %d/%d cases (%.0f%%)", camp.ReachedCases(), len(camp.Aggregates), camp.PassRate()*100)
		if *failUnder > 0 {
			fmt.Printf("  threshold=%.0f%%", *failUnder*100)
		}
	}
	fmt.Println()

	if *reportDir != "" {
		st := *stamp
		if st == "" {
			st = time.Now().UTC().Format(time.RFC3339)
		}
		usage := counting.Total()
		rep := camp.Report(st, cfg.Model.Provider+"/"+cfg.Model.Model, usage, evalCostUSD(cfg, usage))
		// Carry the rates in the report so the scorecard renderer — which reads a
		// report from disk with no config in hand — can show the per-path cost split.
		if p := cfg.Model.Pricing; p != nil {
			rep.InputUSDPerMTok = p.InputUSDPerMTok
			rep.CachedInputUSDPerMTok = p.CachedInputUSDPerMTok
			rep.OutputUSDPerMTok = p.OutputUSDPerMTok
		}
		if b, err := rep.JSON(); err != nil {
			fmt.Fprintf(os.Stderr, "eval: report not written: %v\n", err)
		} else if mkErr := os.MkdirAll(*reportDir, 0o750); mkErr != nil {
			fmt.Fprintf(os.Stderr, "eval: report not written: %v\n", mkErr)
		} else {
			path := filepath.Join(*reportDir, strings.ReplaceAll(st, ":", "-")+"-replay.json")
			if wErr := os.WriteFile(path, b, 0o600); wErr != nil {
				fmt.Fprintf(os.Stderr, "eval: report not written: %v\n", wErr)
			} else {
				fmt.Printf("report: %s\n", path)
			}
		}
	}

	return eval.GateError(camp, *failUnder)
}

// shellStepRunner executes a scenario step as a shell command (kubectl/flux/test).
type shellStepRunner struct{}

func (shellStepRunner) Run(ctx context.Context, step string) error {
	// step is an operator-authored eval scenario command (kubectl/flux/test), not
	// untrusted input — executing it as a shell command is the runner's purpose.
	cmd := exec.CommandContext(ctx, "sh", "-c", step) //nolint:gosec // G204: step is operator-authored scenario YAML

	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr // step output is progress, not findings
	return cmd.Run()
}

// RunEvalLive runs the live-fire campaign and writes a dated report.
func RunEvalLive(cfg *config.Config, scnDir, recordDir, reportDir, prevReport, stamp string, n int,
	jProvider, jBaseURL, jModel, jKeyEnv string) error {
	scns, err := eval.LoadScenarios(scnDir)
	if err != nil {
		return err
	}
	if len(scns) == 0 {
		return fmt.Errorf("no scenarios found in %s", scnDir)
	}
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	ctx := context.Background()
	model, tools, recall, _ := BuildModelAndTools(ctx, cfg, GitOpsFromKube(cfg, log), nil, log)
	judge := eval.ModelJudge{Model: BuildJudgeModel(cfg, jProvider, jBaseURL, jModel, jKeyEnv)}

	runner := &eval.LiveRunner{
		Model: model, BaseTools: tools, Judge: judge, Steps: shellStepRunner{}, Log: log, N: n, Recall: recall,
		OnRecord: func(scn eval.Scenario, calls []eval.Call) {
			if err := eval.WriteCase(recordDir, eval.RecordedCase(scn, calls)); err != nil {
				log.Warn("record case failed", "id", scn.ID, "err", err)
			}
		},
	}
	var results []eval.LiveResult
	for _, scn := range scns {
		log.Info("running scenario", "id", scn.ID)
		results = append(results, runner.RunScenario(ctx, scn))
	}

	if stamp == "" {
		stamp = time.Now().UTC().Format(time.RFC3339)
	}
	rep := eval.NewLiveReport(stamp, n, results)
	if err := os.MkdirAll(reportDir, 0o750); err != nil {
		return err
	}
	base := filepath.Join(reportDir, strings.ReplaceAll(stamp, ":", "-"))
	if err := os.WriteFile(base+".json", rep.JSON(), 0o600); err != nil {
		return err
	}
	md := rep.Markdown()
	if prevReport != "" {
		// prevReport is an operator-supplied --prev report path, not untrusted input.
		if data, rerr := os.ReadFile(prevReport); rerr == nil { //nolint:gosec // G304: operator-supplied baseline report path
			var prev eval.LiveReport
			if json.Unmarshal(data, &prev) == nil {
				if reg := rep.RegressionsVS(prev); len(reg) > 0 {
					md += "\n## ⚠️ Regressions vs baseline\n\n- " + strings.Join(reg, "\n- ") + "\n"
				}
			}
		}
	}
	if err := os.WriteFile(base+".md", []byte(md), 0o600); err != nil {
		return err
	}
	fmt.Print(md)
	fmt.Printf("\nreport: %s.md / .json\n", base)
	return nil
}
