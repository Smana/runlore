# F2 — Public eval scorecard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the nightly replay-eval results public and reproducible. Every night the existing `.github/workflows/eval.yaml` run publishes a **scorecard** — per-scenario pass/fail, recall outcomes, confidence calibration, the model used, the date, and the estimated cost — to a dedicated `eval-scorecard` branch in this repo, and the README carries a live pass-rate badge linking to it. Failures publish exactly like passes: in a market where vendors claim 92–94% accuracy against an independent <50% ITBench-AA ceiling, honest reproducible numbers are the credibility wedge.

**Architecture:** The publishing mechanism (locked in): **an orphan `eval-scorecard` branch in the same repo, force-free pushed by the existing nightly workflow.** No gh-pages setup, no nightly PR noise on `main`, no external services beyond shields.io (already used by four README badges), and no new required user config. The branch carries three files: `scorecard.md` (browsable per-scenario table + calibration + history), `badge.json` (shields.io *endpoint* schema, served raw via `raw.githubusercontent.com`), and `history.jsonl` (append-only, one line per run, capped at 365). The data flows in three stages: (1) the replay campaign report (`*-replay.json`, written today by `lore eval -report-dir`) is **extended in place** — the `internal/eval.Campaign` JSON gains provenance (`at`, `model`, tokens, `cost_usd`) and per-case recall telemetry (`Result.RecallFired`/`RecallShortCircuit` are currently *dropped* by `aggregateCase`; they get aggregated); (2) a new pure renderer `internal/eval/scorecard.go` turns that report + the prior history into the three published files, exposed as a new keyless subcommand `lore eval scorecard -report <json> -dir <out>`; (3) a new workflow step (job permission `contents: write`) runs the renderer into a git worktree of the `eval-scorecard` branch and pushes. Cost comes from wrapping the model in the already-existing `eval.CountingModel` (today only used by `--compare`) plus the already-existing optional `config.Model.Pricing` rates, which we add to the checked-in `eval/ci.runlore.yaml` — repo-side config, nothing new required from users.

**Tech Stack:** Go 1.x (stdlib only — `encoding/json`, `strings`, `bytes`, `flag`), existing packages `internal/eval` / `internal/app` / `internal/config` / `internal/providers`, GitHub Actions (`git worktree` + orphan branch push with the default `GITHUB_TOKEN`), shields.io endpoint badge.

---

## Reference: current state (verified 2026-07-23)

- Nightly workflow `.github/workflows/eval.yaml` runs `./lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7 -report-dir eval/reports`, uploads `eval/reports/` as an artifact. Workflow permission is `contents: read`. Skips loudly when `RUNLORE_EVAL_API_KEY` is absent.
- `internal/app/eval.go: RunEval` writes `eval/reports/<stamp>-replay.json` via `Campaign.JSON()` **after** printing results and **before** returning `eval.GateError` — so a gate failure still writes the report (good: honest publishing works without changes to control flow).
- `internal/eval/eval.go`: `Campaign.JSON()` emits `{n, pass_rate, reached, total, cases:[{name, runs, pass_rate, reached, flaky, confidence, missing, over_claimed}]}` via a local `row` struct that relies on field-order conversion `row(a)` — adding fields to `CaseAggregate` breaks that conversion, so the marshal moves to an explicit `Report` type.
- `Result` (score.go) has `RecallFired` / `RecallShortCircuit`, set by `runOne` from the `RecallDecision`; `aggregateCase` drops them today.
- `eval.CountingModel` (compare_run.go) wraps a `providers.ModelProvider` and sums `providers.Usage` — reusable as-is.
- `config.Pricing` (config.go: `input_usd_per_mtok`, `output_usd_per_mtok`, `cached_input_usd_per_mtok`) exists, optional, validated non-negative. The cost formula to mirror is `internal/investigate/cost.go:cost()` (cached input bills at the cached rate, remainder at the input rate).
- `evalMinPassRate = 0.7` (stats.go) is the shared k-of-n bar and the CI gate.
- README badges live at lines 12–16; the docs link row is the `🔒 [Security model]…` paragraph; CONTRIBUTING has a "Nightly eval (CI)" section.
- Repo commit rule: conventional messages, **never** a `Co-Authored-By` line.

---

### Task 1: Extend the replay report — provenance + recall telemetry (`internal/eval`)

**Files:**
- `internal/eval/replay_report.go` (new)
- `internal/eval/replay_report_test.go` (new)
- `internal/eval/eval.go` (modify: `CaseAggregate` fields, `aggregateCase` → pure `aggregateResults`, `Campaign.JSON` becomes a thin wrapper, delete the `row` struct)

**Steps:**

- [ ] Write the failing tests in `internal/eval/replay_report_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestCampaignReportCarriesProvenance(t *testing.T) {
	camp := Campaign{N: 5, Aggregates: []CaseAggregate{
		{Name: "harbor-chart-bump", Runs: 5, PassRate: 1, Reached: true, Confidence: 0.82},
	}}
	cost := 0.42
	rep := camp.Report("2026-07-23T06:00:00Z", "anthropic/claude-haiku-4-5-20251001",
		providers.Usage{InputTokens: 120000, OutputTokens: 9000}, &cost)
	b, err := rep.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.At != "2026-07-23T06:00:00Z" || got.Model != "anthropic/claude-haiku-4-5-20251001" {
		t.Fatalf("provenance not carried: %+v", got)
	}
	if got.InputTokens != 120000 || got.OutputTokens != 9000 || got.CostUSD == nil || *got.CostUSD != 0.42 {
		t.Fatalf("usage/cost not carried: %+v", got)
	}
	if got.N != 5 || got.Total != 1 || got.Reached != 1 || got.PassRate != 1.0 {
		t.Fatalf("campaign header wrong: %+v", got)
	}
}

func TestCaseAggregateCountsRecall(t *testing.T) {
	c := Case{Name: "recall-case", CatalogDir: "kb", ExpectRecall: "short_circuit"}
	results := []Result{
		{Pass: true, Confidence: 0.9, RecallFired: true, RecallShortCircuit: true},
		{Pass: true, Confidence: 0.8, RecallFired: true},
		{Pass: false, Confidence: 0.4},
	}
	a := aggregateResults(c, results)
	if !a.HasRecall || a.ExpectRecall != "short_circuit" {
		t.Fatalf("recall case identity not carried: %+v", a)
	}
	if a.RecallFired != 2 || a.RecallShortCircuit != 1 {
		t.Fatalf("want fired=2 short-circuit=1, got %+v", a)
	}
	// Existing fold semantics must survive the refactor: 2/3 ≈ 0.67 < 0.7 ⇒ not reached, flaky.
	if a.Reached || !a.Flaky || a.Runs != 3 {
		t.Fatalf("k-of-n fold broken by refactor: %+v", a)
	}
}

func TestEstimateCostUSD(t *testing.T) {
	u := providers.Usage{InputTokens: 2_000_000, CachedInputTokens: 1_000_000, OutputTokens: 200_000}
	// 1M uncached × $1 + 1M cached × $0.10 + 0.2M out × $5 = $2.10
	got := EstimateCostUSD(u, 1.0, 0.10, 5.0)
	if got < 2.099 || got > 2.101 {
		t.Fatalf("want $2.10, got %v", got)
	}
}
```

- [ ] Run: `go test ./internal/eval/ -run 'TestCampaignReportCarriesProvenance|TestCaseAggregateCountsRecall|TestEstimateCostUSD'` — expect compile failure: `undefined: Report`, `undefined: aggregateResults`, `undefined: EstimateCostUSD`, unknown fields `HasRecall`/`ExpectRecall`/`RecallFired`/`RecallShortCircuit`.
- [ ] Create `internal/eval/replay_report.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"

	"github.com/Smana/runlore/internal/providers"
)

// Report is the serializable replay-campaign report — the `*-replay.json` schema
// the nightly eval writes and `lore eval scorecard` consumes. It is a strict
// superset of the pre-v0.11 Campaign.JSON schema: every existing key is
// unchanged; provenance (at/model/tokens/cost) and per-case recall telemetry
// are additive, so old reports still parse.
type Report struct {
	At           string       `json:"at,omitempty"`    // run timestamp (RFC3339, UTC)
	Model        string       `json:"model,omitempty"` // "<provider>/<model>" disclosure
	N            int          `json:"n"`
	PassRate     float64      `json:"pass_rate"`
	Reached      int          `json:"reached"`
	Total        int          `json:"total"`
	InputTokens  int          `json:"input_tokens,omitempty"`
	OutputTokens int          `json:"output_tokens,omitempty"`
	CostUSD      *float64     `json:"cost_usd,omitempty"` // present only when config.model.pricing is set
	Cases        []ReportCase `json:"cases"`
}

// ReportCase is one case's k-of-n verdict in the report.
type ReportCase struct {
	Name        string   `json:"name"`
	Runs        int      `json:"runs"`
	PassRate    float64  `json:"pass_rate"`
	Reached     bool     `json:"reached"`
	Flaky       bool     `json:"flaky"`
	Confidence  float64  `json:"confidence"`
	Missing     []string `json:"missing,omitempty"`
	OverClaimed []string `json:"over_claimed,omitempty"`

	// Recall telemetry (cases with a catalog fixture only).
	HasRecall          bool   `json:"has_recall,omitempty"`
	ExpectRecall       string `json:"expect_recall,omitempty"`
	RecallFired        int    `json:"recall_fired_runs,omitempty"`
	RecallShortCircuit int    `json:"recall_short_circuit_runs,omitempty"`
}

// Report projects the campaign plus its provenance into the serializable report.
func (c Campaign) Report(at, model string, usage providers.Usage, costUSD *float64) Report {
	rep := Report{
		At: at, Model: model, N: c.N,
		PassRate: c.PassRate(), Reached: c.ReachedCases(), Total: len(c.Aggregates),
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CostUSD: costUSD,
	}
	for _, a := range c.Aggregates {
		rep.Cases = append(rep.Cases, ReportCase{
			Name: a.Name, Runs: a.Runs, PassRate: a.PassRate, Reached: a.Reached, Flaky: a.Flaky,
			Confidence: a.Confidence, Missing: a.Missing, OverClaimed: a.OverClaimed,
			HasRecall: a.HasRecall, ExpectRecall: a.ExpectRecall,
			RecallFired: a.RecallFired, RecallShortCircuit: a.RecallShortCircuit,
		})
	}
	return rep
}

// JSON renders the indented machine-readable report.
func (rep Report) JSON() ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}

// EstimateCostUSD prices a usage total: cached input tokens bill at the cached
// rate and the non-cached remainder at the input rate (InputTokens INCLUDES
// cached — mirrors internal/investigate's cost()). Rates are USD per MILLION tokens.
func EstimateCostUSD(u providers.Usage, inputUSDPerMTok, cachedUSDPerMTok, outputUSDPerMTok float64) float64 {
	uncached := u.InputTokens - u.CachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	return float64(uncached)/1e6*inputUSDPerMTok +
		float64(u.CachedInputTokens)/1e6*cachedUSDPerMTok +
		float64(u.OutputTokens)/1e6*outputUSDPerMTok
}
```

- [ ] In `internal/eval/eval.go`, add the four recall fields to `CaseAggregate` (after `OverClaimed`):

```go
	// Recall telemetry aggregated over the repeats (cases with a catalog fixture only):
	// HasRecall marks the case as recall-exercising, ExpectRecall echoes its assertion,
	// and the counters say in how many of the N repeats recall fired / short-circuited.
	HasRecall          bool
	ExpectRecall       string
	RecallFired        int
	RecallShortCircuit int
```

- [ ] In `internal/eval/eval.go`, replace the body of `aggregateCase` with a run-collect loop plus a pure, testable fold (replace the whole existing `aggregateCase` function):

```go
func (r *Runner) aggregateCase(ctx context.Context, c Case, n int) CaseAggregate {
	results := make([]Result, 0, n)
	for i := 0; i < n; i++ {
		results = append(results, r.runOne(ctx, c))
	}
	return aggregateResults(c, results)
}

// aggregateResults folds the repeats of one case into its k-of-n aggregate. Pure —
// separated from the runner so the fold (including recall counting) is unit-testable.
func aggregateResults(c Case, results []Result) CaseAggregate {
	confs := make([]float64, 0, len(results))
	missSet := map[string]struct{}{}
	ocSet := map[string]struct{}{}
	passes, fired, shortCircuits := 0, 0, 0
	for _, res := range results {
		if res.Pass {
			passes++
		}
		if res.RecallFired {
			fired++
		}
		if res.RecallShortCircuit {
			shortCircuits++
		}
		confs = append(confs, res.Confidence)
		for _, m := range res.Missing {
			missSet[m] = struct{}{}
		}
		for _, o := range res.OverClaimed {
			ocSet[o] = struct{}{}
		}
	}
	rate := float64(passes) / float64(len(results))
	return CaseAggregate{
		Name:        c.Name,
		Runs:        len(results),
		PassRate:    rate,
		Reached:     rate >= evalMinPassRate,
		Flaky:       rate > 1-evalMinPassRate && rate < evalMinPassRate,
		Confidence:  medianFloat(confs),
		Missing:     sortedSet(missSet),
		OverClaimed: sortedSet(ocSet),
		HasRecall:          c.CatalogDir != "",
		ExpectRecall:       c.ExpectRecall,
		RecallFired:        fired,
		RecallShortCircuit: shortCircuits,
	}
}
```

- [ ] In `internal/eval/eval.go`, replace the whole `Campaign.JSON` method (including its local `row` struct, which breaks once `CaseAggregate` gains fields) with a thin wrapper:

```go
// JSON renders the campaign as an indented report without provenance. Kept for
// callers that have no model/usage context; the nightly eval uses
// Campaign.Report(...).JSON() so the published report carries provenance.
func (c Campaign) JSON() ([]byte, error) {
	return c.Report("", "", providers.Usage{}, nil).JSON()
}
```

- [ ] Run: `go test ./internal/eval/` — expect `ok` (the new tests pass; `TestCampaignJSON` and `TestReplayKOfNRepeats` still pass because the schema is a superset and the fold semantics are unchanged).
- [ ] Run: `go build ./... && go vet ./internal/eval/` — expect clean.
- [ ] Commit: `feat(eval): replay report carries provenance and recall telemetry`

---

### Task 2: Wire usage counting + pricing into `lore eval` (`internal/app`)

**Files:**
- `internal/app/eval.go` (modify `RunEval`, add `evalCostUSD` helper)
- `internal/app/eval_cost_test.go` (new)
- `eval/ci.runlore.yaml` (add optional `pricing:` block)

**Steps:**

- [ ] Write the failing test in `internal/app/eval_cost_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

func TestEvalCostUSDFromPricing(t *testing.T) {
	cfg := &config.Config{}
	if got := evalCostUSD(cfg, providers.Usage{InputTokens: 1_000_000}); got != nil {
		t.Fatalf("unpriced config must yield nil (omit cost, do not claim $0), got %v", *got)
	}
	cfg.Model.Pricing = &config.Pricing{
		InputUSDPerMTok: 1.0, OutputUSDPerMTok: 5.0, CachedInputUSDPerMTok: 0.10,
	}
	got := evalCostUSD(cfg, providers.Usage{
		InputTokens: 2_000_000, CachedInputTokens: 1_000_000, OutputTokens: 200_000,
	})
	// 1M uncached × $1 + 1M cached × $0.10 + 0.2M out × $5 = $2.10
	if got == nil || *got < 2.099 || *got > 2.101 {
		t.Fatalf("want ≈$2.10, got %v", got)
	}
}
```

- [ ] Run: `go test ./internal/app/ -run TestEvalCostUSDFromPricing` — expect compile failure: `undefined: evalCostUSD`.
- [ ] Add the helper to `internal/app/eval.go`:

```go
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
```

  (add `"github.com/Smana/runlore/internal/providers"` to the file's imports.)
- [ ] Run: `go test ./internal/app/ -run TestEvalCostUSDFromPricing` — expect `ok`.
- [ ] In `RunEval` (replay path), wrap the model so tokens are counted. Replace:

```go
	runner := &eval.Runner{Model: BuildModel(cfg, apiKey), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
```

  with:

```go
	counting := &eval.CountingModel{Inner: BuildModel(cfg, apiKey)}
	runner := &eval.Runner{Model: counting, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
```

- [ ] In the `if *reportDir != ""` block of `RunEval`, build the provenance-carrying report. Replace:

```go
		if b, err := camp.JSON(); err != nil {
```

  with:

```go
		usage := counting.Total()
		rep := camp.Report(st, cfg.Model.Provider+"/"+cfg.Model.Model, usage, evalCostUSD(cfg, usage))
		if b, err := rep.JSON(); err != nil {
```

  (the `st` stamp variable already exists two lines above; no other change to the block.)
- [ ] Append the optional pricing block to `eval/ci.runlore.yaml` (via `yq` or Edit; final file content):

```yaml
# Minimal config for the nightly CI eval (replay mode).
# Only the model block is needed — replay scenarios supply their own evidence.
# Set the repo secret RUNLORE_EVAL_API_KEY to the API key for this provider.
model:
  provider: anthropic
  model: claude-haiku-4-5-20251001
  api_key_env: RUNLORE_EVAL_API_KEY
  # Optional: token rates (USD per MILLION tokens) so the published scorecard
  # reports an honest per-run cost. claude-haiku-4-5 list prices, 2026-07
  # (same figures as eval/compare.example.yaml). Wrong rates only skew the
  # cost figure, never the pass/fail scoring.
  pricing:
    input_usd_per_mtok: 1.00
    output_usd_per_mtok: 5.00
    cached_input_usd_per_mtok: 0.10
```

- [ ] Run: `go build ./... && go test ./internal/app/ ./internal/eval/` — expect `ok` for both.
- [ ] Local dry-run that the config still loads: `go run ./cmd/lore eval -config eval/ci.runlore.yaml -cases examples/eval -report-dir /tmp/f2-dryrun 2>&1 | head -3` — without `RUNLORE_EVAL_API_KEY` set this must fail with a model/API-key error (e.g. missing key), **not** a config-parse error mentioning `pricing`.
- [ ] Commit: `feat(eval): count tokens and price the nightly replay report`

---

### Task 3: Scorecard renderer — markdown, badge, history (`internal/eval/scorecard.go`)

**Files:**
- `internal/eval/scorecard.go` (new)
- `internal/eval/scorecard_test.go` (new)

**Steps:**

- [ ] Write the failing tests in `internal/eval/scorecard_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"strings"
	"testing"
)

func scorecardFixtureReport() Report {
	cost := 0.16
	return Report{
		At: "2026-07-23T06:00:00Z", Model: "anthropic/claude-haiku-4-5-20251001",
		N: 5, PassRate: 0.5, Reached: 1, Total: 2,
		InputTokens: 120000, OutputTokens: 9000, CostUSD: &cost,
		Cases: []ReportCase{
			{Name: "harbor-chart-bump", Runs: 5, PassRate: 1, Reached: true, Confidence: 0.82},
			{Name: "poisoned-recall-verify", Runs: 5, PassRate: 0.4, Flaky: true, Confidence: 0.75,
				HasRecall: true, ExpectRecall: "withdrawn", RecallFired: 5,
				Missing: []string{"expect_recall=withdrawn but recall short_circuit"}},
		},
	}
}

func TestBadgeJSON(t *testing.T) {
	b := string(BadgeJSON(scorecardFixtureReport()))
	if !strings.Contains(b, `"schemaVersion":1`) {
		t.Fatalf("not a shields endpoint doc: %s", b)
	}
	if !strings.Contains(b, `"message":"1/2 scenarios · 50%"`) {
		t.Fatalf("badge message wrong: %s", b)
	}
	if !strings.Contains(b, `"color":"yellow"`) { // 0.5 is in [0.5, 0.7) ⇒ yellow
		t.Fatalf("badge color wrong: %s", b)
	}
	green := scorecardFixtureReport()
	green.PassRate, green.Reached = 1.0, 2
	if g := string(BadgeJSON(green)); !strings.Contains(g, `"color":"brightgreen"`) {
		t.Fatalf("1.0 should be brightgreen: %s", g)
	}
}

func TestAppendHistoryDedupesAndCaps(t *testing.T) {
	e := HistoryFromReport(scorecardFixtureReport())
	out, entries, err := AppendHistory(nil, e)
	if err != nil || len(entries) != 1 {
		t.Fatalf("first append: %v / %d entries", err, len(entries))
	}
	// Re-appending the same run (same At) must be idempotent.
	out2, entries2, err := AppendHistory(out, e)
	if err != nil || len(entries2) != 1 || string(out2) != string(out) {
		t.Fatalf("dedupe on At failed: %v / %d entries", err, len(entries2))
	}
	// Cap: appending beyond maxHistory drops the oldest.
	long := out
	for i := 0; i < maxHistory+10; i++ {
		e.At = "2026-07-23T06:00:00Z" + strings.Repeat("x", i+1) // unique At per line
		long, entries, err = AppendHistory(long, e)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(entries) != maxHistory {
		t.Fatalf("want cap %d, got %d", maxHistory, len(entries))
	}
}

func TestScorecardMarkdown(t *testing.T) {
	rep := scorecardFixtureReport()
	_, entries, err := AppendHistory(nil, HistoryFromReport(rep))
	if err != nil {
		t.Fatal(err)
	}
	md := ScorecardMarkdown(rep, entries)
	for _, want := range []string{
		"# RunLore nightly eval scorecard",
		"lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7", // reproduce command
		"anthropic/claude-haiku-4-5-20251001",                                              // model disclosure
		"**1/2 scenarios reached (50%)**",
		"est. cost $0.16",
		"| harbor-chart-bump | ✅ PASS |",
		"| poisoned-recall-verify | ⚠️ FLAKY |",
		"fired 5/5 · short-circuit 0/5 (expect: withdrawn)", // recall outcome column
		"## Confidence calibration",
		"poisoned-recall-verify", // 0.75 ≥ 0.70 and not reached ⇒ confidently wrong
		"## History",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("scorecard missing %q in:\n%s", want, md)
		}
	}
}
```

- [ ] Run: `go test ./internal/eval/ -run 'TestBadgeJSON|TestAppendHistory|TestScorecardMarkdown'` — expect compile failure (`undefined: BadgeJSON` etc.).
- [ ] Create `internal/eval/scorecard.go`:

```go
// SPDX-License-Identifier: Apache-2.0

// Scorecard rendering: turns the nightly replay report (Report) into the public
// artifacts published on the eval-scorecard branch — a browsable scorecard.md, a
// shields.io endpoint badge.json, and an append-only history.jsonl. Pure functions
// over bytes so the whole pipeline is testable without CI.
package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxHistory   = 365 // history.jsonl cap: one nightly line/day ≈ one year
	historyShown = 30  // history rows rendered in scorecard.md (newest first)

	// Calibration bands for the scorecard summary. confidentWrongFloor deliberately
	// matches evalMinPassRate's spirit: a missed case the model was ≥70% sure about
	// is the "confident and wrong" failure mode published benchmarks care about.
	confidentWrongFloor = 0.70
	underConfidentCeil  = 0.50
)

// HistoryEntry is one nightly run in the scorecard history (one JSONL line).
type HistoryEntry struct {
	At           string   `json:"at"`
	Model        string   `json:"model,omitempty"`
	N            int      `json:"n"`
	PassRate     float64  `json:"pass_rate"`
	Reached      int      `json:"reached"`
	Total        int      `json:"total"`
	InputTokens  int      `json:"input_tokens,omitempty"`
	OutputTokens int      `json:"output_tokens,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
}

// HistoryFromReport projects a replay report onto its one-line history record.
func HistoryFromReport(rep Report) HistoryEntry {
	return HistoryEntry{
		At: rep.At, Model: rep.Model, N: rep.N,
		PassRate: rep.PassRate, Reached: rep.Reached, Total: rep.Total,
		InputTokens: rep.InputTokens, OutputTokens: rep.OutputTokens, CostUSD: rep.CostUSD,
	}
}

// AppendHistory appends e to the JSONL history, replacing any line with the same
// At (so re-publishing one run is idempotent) and capping the log at maxHistory
// entries (oldest dropped). Returns the new JSONL bytes and the entries oldest-first.
func AppendHistory(existing []byte, e HistoryEntry) ([]byte, []HistoryEntry, error) {
	var entries []HistoryEntry
	for _, line := range bytes.Split(existing, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var h HistoryEntry
		if err := json.Unmarshal(line, &h); err != nil {
			return nil, nil, fmt.Errorf("parse history line %q: %w", line, err)
		}
		if h.At == e.At {
			continue // same run re-rendered: its fresh line replaces the old one
		}
		entries = append(entries, h)
	}
	entries = append(entries, e)
	if len(entries) > maxHistory {
		entries = entries[len(entries)-maxHistory:]
	}
	var out bytes.Buffer
	for _, h := range entries {
		b, err := json.Marshal(h)
		if err != nil {
			return nil, nil, err
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes(), entries, nil
}

// BadgeJSON renders the shields.io "endpoint" badge document
// (https://shields.io/badges/endpoint-badge) for the README pass-rate badge.
// Color bands: ≥90% brightgreen, ≥ the 70% CI gate green, ≥50% yellow, else red.
func BadgeJSON(rep Report) []byte {
	color := "red"
	switch {
	case rep.PassRate >= 0.9:
		color = "brightgreen"
	case rep.PassRate >= evalMinPassRate:
		color = "green"
	case rep.PassRate >= 0.5:
		color = "yellow"
	}
	b, _ := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"label":         "nightly eval",
		"message":       fmt.Sprintf("%d/%d scenarios · %.0f%%", rep.Reached, rep.Total, rep.PassRate*100),
		"color":         color,
	})
	return b
}

// ScorecardMarkdown renders the browsable public scorecard: reproduce command,
// provenance (model, date, cost), per-scenario table with recall outcomes, a
// confidence-calibration summary, and the run history.
func ScorecardMarkdown(rep Report, history []HistoryEntry) string {
	var b strings.Builder
	b.WriteString("# RunLore nightly eval scorecard\n\n")
	b.WriteString("Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml) — ")
	b.WriteString("the replay eval scores the model+loop over recorded incident evidence (no live cluster), so anyone can reproduce it:\n\n")
	b.WriteString("```\nlore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7\n```\n\n")

	fmt.Fprintf(&b, "**Latest run:** %s", rep.At)
	if rep.Model != "" {
		fmt.Fprintf(&b, " · model `%s`", rep.Model)
	}
	fmt.Fprintf(&b, " · **%d/%d scenarios reached (%.0f%%)** · n=%d runs/case, k-of-n bar %.0f%%",
		rep.Reached, rep.Total, rep.PassRate*100, rep.N, evalMinPassRate*100)
	if rep.CostUSD != nil {
		fmt.Fprintf(&b, " · est. cost $%.2f (%s in / %s out tokens)",
			*rep.CostUSD, compactTokens(rep.InputTokens), compactTokens(rep.OutputTokens))
	} else if rep.InputTokens+rep.OutputTokens > 0 {
		fmt.Fprintf(&b, " · %s in / %s out tokens", compactTokens(rep.InputTokens), compactTokens(rep.OutputTokens))
	}
	b.WriteString("\n\n## Scenarios (latest run)\n\n")
	b.WriteString("| scenario | result | pass-rate | median confidence | recall | notes |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, c := range rep.Cases {
		fmt.Fprintf(&b, "| %s | %s | %.0f%% (n=%d) | %.2f | %s | %s |\n",
			c.Name, resultCell(c), c.PassRate*100, c.Runs, c.Confidence, recallCell(c), notesCell(c))
	}

	b.WriteString("\n## Confidence calibration\n\n")
	var confidentWrong, underConfident []string
	for _, c := range rep.Cases {
		if !c.Reached && c.Confidence >= confidentWrongFloor {
			confidentWrong = append(confidentWrong, c.Name)
		}
		if c.Reached && c.Confidence < underConfidentCeil {
			underConfident = append(underConfident, c.Name)
		}
	}
	fmt.Fprintf(&b, "- **Confidently wrong** (missed with median confidence ≥ %.2f): %s\n", confidentWrongFloor, nameList(confidentWrong))
	fmt.Fprintf(&b, "- **Underconfident** (reached with median confidence < %.2f): %s\n", underConfidentCeil, nameList(underConfident))

	b.WriteString("\n## History\n\n")
	fmt.Fprintf(&b, "Newest first, last %d shown — the full log is [`history.jsonl`](history.jsonl). ", historyShown)
	b.WriteString("Runs below the CI gate publish here exactly like green ones.\n\n")
	b.WriteString("| date | model | reached | pass-rate | est. cost |\n|---|---|---|---|---|\n")
	shown := history
	if len(shown) > historyShown {
		shown = shown[len(shown)-historyShown:]
	}
	for i := len(shown) - 1; i >= 0; i-- {
		h := shown[i]
		cost := "—"
		if h.CostUSD != nil {
			cost = fmt.Sprintf("$%.2f", *h.CostUSD)
		}
		fmt.Fprintf(&b, "| %s | %s | %d/%d | %.0f%% | %s |\n", h.At, h.Model, h.Reached, h.Total, h.PassRate*100, cost)
	}
	return b.String()
}

func resultCell(c ReportCase) string {
	switch {
	case c.Reached:
		return "✅ PASS"
	case c.Flaky:
		return "⚠️ FLAKY"
	default:
		return "❌ MISS"
	}
}

func recallCell(c ReportCase) string {
	if !c.HasRecall {
		return "—"
	}
	s := fmt.Sprintf("fired %d/%d · short-circuit %d/%d", c.RecallFired, c.Runs, c.RecallShortCircuit, c.Runs)
	if c.ExpectRecall != "" {
		s += fmt.Sprintf(" (expect: %s)", c.ExpectRecall)
	}
	return s
}

func notesCell(c ReportCase) string {
	if len(c.Missing) == 0 {
		return "—"
	}
	return strings.Join(c.Missing, ", ")
}

func nameList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d — %s", len(names), strings.Join(names, ", "))
}

// compactTokens renders a token count as 1.2M / 84.0k / 512 for the summary line.
func compactTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
```

- [ ] Run: `go test ./internal/eval/` — expect `ok`.
- [ ] Run: `go vet ./internal/eval/ && go build ./...` — expect clean.
- [ ] Commit: `feat(eval): scorecard renderer — markdown, shields badge, history jsonl`

---

### Task 4: `lore eval scorecard` subcommand (keyless renderer CLI)

**Files:**
- `internal/app/eval_scorecard.go` (new)
- `internal/app/eval_scorecard_test.go` (new)
- `internal/app/eval.go` (modify: dispatch the subcommand before config load)
- `cmd/lore/main.go` (modify: usage string)

**Steps:**

- [ ] Write the failing test in `internal/app/eval_scorecard_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const scorecardReportFixture = `{
  "at": "2026-07-23T06:00:00Z",
  "model": "anthropic/claude-haiku-4-5-20251001",
  "n": 5, "pass_rate": 0.5, "reached": 1, "total": 2,
  "input_tokens": 120000, "output_tokens": 9000, "cost_usd": 0.16,
  "cases": [
    {"name": "harbor-chart-bump", "runs": 5, "pass_rate": 1, "reached": true, "flaky": false, "confidence": 0.82},
    {"name": "poisoned-recall-verify", "runs": 5, "pass_rate": 0.4, "reached": false, "flaky": true, "confidence": 0.75,
     "has_recall": true, "expect_recall": "withdrawn", "recall_fired_runs": 5, "recall_short_circuit_runs": 0,
     "missing": ["expect_recall=withdrawn but recall short_circuit"]}
  ]
}`

func TestRunEvalScorecard(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "2026-07-23T06-00-00Z-replay.json")
	if err := os.WriteFile(report, []byte(scorecardReportFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "scorecard")
	if err := RunEvalScorecard([]string{"-report", report, "-dir", out}); err != nil {
		t.Fatalf("RunEvalScorecard: %v", err)
	}
	// Re-running on the same report must be idempotent (same At ⇒ 1 history line).
	if err := RunEvalScorecard([]string{"-report", report, "-dir", out}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	md, err := os.ReadFile(filepath.Join(out, "scorecard.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**1/2 scenarios reached (50%)**", "⚠️ FLAKY", "expect: withdrawn"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("scorecard.md missing %q", want)
		}
	}
	badge, err := os.ReadFile(filepath.Join(out, "badge.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(badge), `"message":"1/2 scenarios · 50%"`) {
		t.Fatalf("badge.json wrong: %s", badge)
	}
	hist, err := os.ReadFile(filepath.Join(out, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(hist)), "\n") + 1; lines != 1 {
		t.Fatalf("want 1 history line after idempotent re-run, got %d:\n%s", lines, hist)
	}
}

func TestRunEvalScorecardRejectsMissingReport(t *testing.T) {
	if err := RunEvalScorecard([]string{"-dir", t.TempDir()}); err == nil {
		t.Fatal("want error when -report is missing")
	}
}
```

- [ ] Run: `go test ./internal/app/ -run TestRunEvalScorecard` — expect compile failure: `undefined: RunEvalScorecard`.
- [ ] Create `internal/app/eval_scorecard.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Smana/runlore/internal/eval"
)

// RunEvalScorecard renders the public scorecard artifacts (scorecard.md,
// badge.json, history.jsonl) from a replay report. Keyless and config-free: it
// only reads the report JSON and the output dir's existing history, so CI can run
// it after the eval and anyone can run it locally on a downloaded report artifact.
func RunEvalScorecard(args []string) error {
	fs := flag.NewFlagSet("eval scorecard", flag.ContinueOnError)
	report := fs.String("report", "", "path to a *-replay.json report (required)")
	dir := fs.String("dir", "scorecard", "output directory (scorecard.md, badge.json, history.jsonl)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *report == "" {
		return fmt.Errorf("eval scorecard: -report <replay.json> is required")
	}
	data, err := os.ReadFile(*report) //nolint:gosec // G304: operator-supplied report path
	if err != nil {
		return err
	}
	var rep eval.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return fmt.Errorf("parse report %s: %w", *report, err)
	}
	if rep.Total == 0 {
		return fmt.Errorf("report %s has no cases — refusing to publish an empty scorecard", *report)
	}
	if err := os.MkdirAll(*dir, 0o750); err != nil {
		return err
	}
	histPath := filepath.Join(*dir, "history.jsonl")
	existing, err := os.ReadFile(histPath) //nolint:gosec // G304: path derived from operator-supplied -dir
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	hist, entries, err := eval.AppendHistory(existing, eval.HistoryFromReport(rep))
	if err != nil {
		return err
	}
	if err := os.WriteFile(histPath, hist, 0o644); err != nil { //nolint:gosec // published artifact, world-readable
		return err
	}
	md := eval.ScorecardMarkdown(rep, entries)
	if err := os.WriteFile(filepath.Join(*dir, "scorecard.md"), []byte(md), 0o644); err != nil { //nolint:gosec
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "badge.json"), eval.BadgeJSON(rep), 0o644); err != nil { //nolint:gosec
		return err
	}
	fmt.Printf("scorecard: %s (badge.json, history.jsonl alongside)\n", filepath.Join(*dir, "scorecard.md"))
	return nil
}
```

- [ ] In `internal/app/eval.go`, dispatch the subcommand at the very top of `RunEval` (before the flag set, so it needs no config/model):

```go
	if len(args) > 0 && args[0] == "scorecard" {
		return RunEvalScorecard(args[1:])
	}
```

- [ ] In `cmd/lore/main.go`, add one usage line directly after the `lore eval --compare …` line:

```go
  lore eval scorecard -report <replay.json> -dir <out>  render the public scorecard (markdown + badge + history) from a replay report
```

- [ ] Run: `go test ./internal/app/ -run 'TestRunEvalScorecard|TestRunEvalScorecardRejectsMissingReport'` — expect `ok`.
- [ ] Local end-to-end dry-run (this is the CI step's exact renderer path, run on a fixture since CI's model call can't run locally):

```bash
go build -o /tmp/lore ./cmd/lore
cat > /tmp/f2-fixture-replay.json <<'EOF'
{
  "at": "2026-07-23T06:00:00Z",
  "model": "anthropic/claude-haiku-4-5-20251001",
  "n": 5, "pass_rate": 0.5, "reached": 1, "total": 2,
  "input_tokens": 120000, "output_tokens": 9000, "cost_usd": 0.16,
  "cases": [
    {"name": "harbor-chart-bump", "runs": 5, "pass_rate": 1, "reached": true, "flaky": false, "confidence": 0.82},
    {"name": "poisoned-recall-verify", "runs": 5, "pass_rate": 0.4, "reached": false, "flaky": true, "confidence": 0.75,
     "has_recall": true, "expect_recall": "withdrawn", "recall_fired_runs": 5, "recall_short_circuit_runs": 0,
     "missing": ["expect_recall=withdrawn but recall short_circuit"]}
  ]
}
EOF
/tmp/lore eval scorecard -report /tmp/f2-fixture-replay.json -dir /tmp/f2-scorecard
cat /tmp/f2-scorecard/badge.json
head -20 /tmp/f2-scorecard/scorecard.md
```

  Expected: prints `scorecard: /tmp/f2-scorecard/scorecard.md …`; `badge.json` is `{"color":"yellow","label":"nightly eval","message":"1/2 scenarios · 50%","schemaVersion":1}`; the markdown shows the header, the reproduce command, and the two-row scenario table.
- [ ] Run: `go build ./... && go test ./...` — expect all `ok`.
- [ ] Commit: `feat(cli): lore eval scorecard subcommand renders the public scorecard`

---

### Task 5: Nightly workflow publishes to the `eval-scorecard` branch

**Files:**
- `.github/workflows/eval.yaml` (modify: job permission + one publish step)

**Steps:**

- [ ] In `.github/workflows/eval.yaml`, replace the workflow-level permission comment/block:

```yaml
# Least privilege: the workflow only reads the repo; the replay-eval job elevates
# to contents:write solely to push the public scorecard to the eval-scorecard
# branch (never to main).
permissions:
  contents: read
```

  and add a job-level override right under `replay-eval:`:

```yaml
jobs:
  replay-eval:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    permissions:
      contents: write   # push the scorecard to the eval-scorecard branch only
```

- [ ] Append the publish step at the end of the job (after `upload report`). `always()` is deliberate: a run that fails the 70% gate still wrote its report, and honest publishing of red runs is the point. The `github.ref` guard keeps branch-dispatched runs from overwriting the published numbers:

```yaml
      - name: publish scorecard (eval-scorecard branch)
        # always(): a gate failure (score < 70%) must publish exactly like a pass —
        # the scorecard's value is honesty. Guarded to main so a workflow_dispatch
        # from a feature branch can't overwrite the published numbers, and to
        # has_key so a skipped run publishes nothing.
        if: always() && steps.guard.outputs.has_key == 'true' && github.ref == 'refs/heads/main'
        run: |
          set -euo pipefail
          REPORT=$(ls -t eval/reports/*-replay.json 2>/dev/null | head -1 || true)
          if [ -z "$REPORT" ]; then
            echo "::warning title=Scorecard skipped::no replay report found (eval crashed before writing one)"
            exit 0
          fi
          git config user.name "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          OUT="$RUNNER_TEMP/scorecard-branch"
          if git fetch origin eval-scorecard; then
            git worktree add -B eval-scorecard "$OUT" origin/eval-scorecard
          else
            git worktree add --detach "$OUT"
            git -C "$OUT" checkout --orphan eval-scorecard
            git -C "$OUT" rm -rf --quiet . || true
          fi
          ./lore eval scorecard -report "$REPORT" -dir "$OUT"
          git -C "$OUT" add -A
          git -C "$OUT" commit -m "chore(eval): nightly scorecard $(date -u +%Y-%m-%d)" || { echo "no changes to publish"; exit 0; }
          git -C "$OUT" push origin eval-scorecard
```

- [ ] Local verification (CI can't run here — no API key, no push rights):
  - `yq '.jobs.replay-eval.permissions.contents' .github/workflows/eval.yaml` → `write`
  - `yq '.jobs.replay-eval.steps[-1].name' .github/workflows/eval.yaml` → `publish scorecard (eval-scorecard branch)`
  - `actionlint .github/workflows/eval.yaml` if `actionlint` is installed (skip cleanly if not).
- [ ] Local dry-run of the branch-publish git choreography (simulates the orphan-branch first run against a scratch repo, using the Task 4 fixture):

```bash
go build -o /tmp/lore ./cmd/lore
T=$(mktemp -d) && git -C "$T" init -q -b main && git -C "$T" commit -q --allow-empty -m init
OUT="$T/scorecard-branch"
git -C "$T" worktree add --detach "$OUT"
git -C "$OUT" checkout --orphan eval-scorecard
git -C "$OUT" rm -rf --quiet . 2>/dev/null || true
/tmp/lore eval scorecard -report /tmp/f2-fixture-replay.json -dir "$OUT"
git -C "$OUT" add -A && git -C "$OUT" commit -q -m "chore(eval): nightly scorecard test"
git -C "$OUT" ls-tree --name-only eval-scorecard
```

  Expected final output: `badge.json`, `history.jsonl`, `scorecard.md` — three files, one orphan commit.
- [ ] **Post-merge verification note (cannot run pre-merge):** after the PR merges to `main`, the maintainer triggers `eval` via *Run workflow* (workflow_dispatch on `main`; the `RUNLORE_EVAL_API_KEY` secret must be set, otherwise the run skips loudly and publishes nothing). Then confirm: (1) branch `eval-scorecard` exists with the three files, (2) `https://raw.githubusercontent.com/Smana/runlore/eval-scorecard/badge.json` serves the shields document, (3) the README badge renders (it shows shields' "invalid" placeholder until this first publish — expected and harmless).
- [ ] Commit: `ci(eval): publish nightly scorecard to the eval-scorecard branch`

---

### Task 6: Link it — README badge + docs

**Files:**
- `README.md` (modify: badge row, honesty bullet, docs link row)
- `CONTRIBUTING.md` (modify: "Nightly eval (CI)" section)
- `docs/benchmarking.md` (modify: cross-link)

**Steps:**

- [ ] In `README.md`, insert the badge directly after the CI badge line (line 12). Old:

```markdown
[![CI](https://github.com/Smana/runlore/actions/workflows/ci.yaml/badge.svg)](https://github.com/Smana/runlore/actions/workflows/ci.yaml)
```

  New:

```markdown
[![CI](https://github.com/Smana/runlore/actions/workflows/ci.yaml/badge.svg)](https://github.com/Smana/runlore/actions/workflows/ci.yaml)
[![Nightly eval](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2FSmana%2Frunlore%2Feval-scorecard%2Fbadge.json)](https://github.com/Smana/runlore/blob/eval-scorecard/scorecard.md)
```

- [ ] In `README.md`, extend the honesty bullet. Old:

```markdown
- every claim is checked by a shipped eval harness.
```

  New:

```markdown
- every claim is checked by a shipped eval harness, and the nightly results are
  [published — pass, fail, model, and cost included](https://github.com/Smana/runlore/blob/eval-scorecard/scorecard.md).
```

- [ ] In the `README.md` docs link row, extend the benchmarking link. Old:

```markdown
📊 [Benchmarking models](docs/benchmarking.md) · 🛠 [Contributing](CONTRIBUTING.md)
```

  New:

```markdown
📊 [Benchmarking models](docs/benchmarking.md) · 🧮 [Nightly eval scorecard](https://github.com/Smana/runlore/blob/eval-scorecard/scorecard.md) · 🛠 [Contributing](CONTRIBUTING.md)
```

- [ ] In `CONTRIBUTING.md`, extend the "Nightly eval (CI)" section. After the paragraph ending `…then uploads the JSON report as a build artifact.` add:

```markdown
The run then **publishes a public scorecard** to the
[`eval-scorecard`](https://github.com/Smana/runlore/tree/eval-scorecard) branch:
`scorecard.md` (per-scenario pass/fail, recall outcomes, confidence calibration,
model, date, estimated cost), `badge.json` (the README's shields.io endpoint
badge), and `history.jsonl` (one line per run, capped at a year). Publishing is
deliberately unconditional on the gate: a night below 70% is published exactly
like a green one. Render the same artifacts locally from any report:

    lore eval scorecard -report eval/reports/<stamp>-replay.json -dir /tmp/scorecard

The per-run cost figure comes from the optional `pricing:` rates in
`eval/ci.runlore.yaml`; token totals are always reported, the dollar estimate
only when rates are set.
```

- [ ] In `docs/benchmarking.md`, add after the intro paragraph (the one ending `…publish an honest comparison.`):

```markdown
> RunLore's own nightly numbers are public: the replay eval publishes a
> per-scenario scorecard — pass/fail, recall outcomes, confidence calibration,
> model, date, and cost — to the
> [`eval-scorecard` branch](https://github.com/Smana/runlore/blob/eval-scorecard/scorecard.md)
> on every run, red or green.
```

- [ ] Verify: `grep -c "eval-scorecard" README.md CONTRIBUTING.md docs/benchmarking.md` — expect `README.md:3` (or more), `CONTRIBUTING.md:1`+, `docs/benchmarking.md:1`+. Render-check the README badge line has balanced brackets: `grep "Nightly eval" README.md`.
- [ ] Run: `go build ./... && go test ./...` — expect all `ok` (docs-only task; catches accidental stray edits).
- [ ] Commit: `docs: link the public nightly eval scorecard (README badge, contributing, benchmarking)`

---

## Acceptance criteria

- [ ] `eval/reports/*-replay.json` written by `lore eval` now carries `at`, `model` (`provider/model`), `input_tokens`, `output_tokens`, `cost_usd` (when `pricing:` is set), and per-case `has_recall` / `expect_recall` / `recall_fired_runs` / `recall_short_circuit_runs` — verified by `go test ./internal/eval/` and `go test ./internal/app/`.
- [ ] Pre-v0.11 report keys are unchanged (superset schema); `TestCampaignJSON` passes unmodified.
- [ ] `lore eval scorecard -report <json> -dir <out>` produces `scorecard.md`, `badge.json` (shields endpoint schema with correct message and color bands), and `history.jsonl`; re-running on the same report is idempotent (deduped on `at`); history is capped at 365 entries.
- [ ] `scorecard.md` contains: the exact reproduce command, model + date + cost disclosure, a per-scenario table (PASS/FLAKY/MISS, pass-rate, median confidence, recall outcome, missing notes), a confidence-calibration section (confidently-wrong / underconfident case lists), and a history table (newest first, ≤30 rows).
- [ ] `.github/workflows/eval.yaml`: job has `permissions: contents: write`; the publish step runs on `always()` (failed gates publish too), only with the API key present and only on `refs/heads/main`; it pushes to the `eval-scorecard` branch and **never** to `main`; a missing report degrades to a `::warning::`, exit 0.
- [ ] README shows the nightly-eval badge (shields endpoint → `raw.githubusercontent.com/...eval-scorecard/badge.json`) linking to `scorecard.md` on the `eval-scorecard` branch; CONTRIBUTING and docs/benchmarking.md document the mechanism and the local render command.
- [ ] No new **required** config: `pricing:` in `eval/ci.runlore.yaml` is optional repo-side config (its absence only omits the dollar figure), user-facing `runlore.yaml` needs nothing new, and the scorecard subcommand needs no config file, model, or API key.
- [ ] `go build ./... && go test ./... && go vet ./...` all pass.
- [ ] Post-merge (maintainer): one `workflow_dispatch` of `eval` on `main` creates the `eval-scorecard` branch and the README badge goes live (shows shields "invalid" until that first run — documented, expected).
