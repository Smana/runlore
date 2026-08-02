# S3 — Proof assets — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make RunLore's honesty positioning verifiable — a live public eval scorecard on runlore.io, a published cost-per-investigation figure, and recall claims that survive a hostile reader opening the test that produced them.

**Architecture:** Per-case token attribution is added to the eval runner by snapshotting the existing `CountingModel` around each sequential run, which lets the scorecard renderer split cost between full investigations and instant recalls. The `/eval` page is built by `docs.yml` fetching `scorecard.md` from the `eval-scorecard` orphan branch at build time, triggered by a `repository_dispatch` the nightly eval fires. The recall documentation gains the corpus and methodology caveats, pinned by a test that parses the published table and compares it to the live measurement.

**Tech Stack:** Go 1.x, GitHub Actions, Hugo/Hextra, `ossf/scorecard-action`.

**Spec:** [`docs/superpowers/specs/2026-08-02-s3-proof-assets-design.md`](../specs/2026-08-02-s3-proof-assets-design.md)

## Global Constraints

- Every new `.go` file starts with `// SPDX-License-Identifier: Apache-2.0`.
- `golangci-lint run` must pass (`.golangci.yml`).
- No new third-party Go dependencies.
- **No published number may be hand-written where it can be computed.** Every figure this plan adds to a doc is either rendered from data or pinned by a test.
- GitHub Actions must be pinned by commit SHA where the surrounding workflow already does so — match the file you are editing.
- Workflow permissions stay least-privilege; do not widen a job's `permissions` block beyond what the new step needs.
- Conventional Commits. **Never** add co-author trailers or AI attribution.

---

### Task 1: Per-case token attribution in the eval report

**Files:**
- Modify: `internal/eval/compare_run.go:17-42` (add a snapshot accessor)
- Modify: `internal/eval/score.go:12-24` (add `Usage` to `Result`)
- Modify: `internal/eval/eval.go:20-23, 25, 218-224` (Runner captures per-run usage)
- Modify: `internal/eval/eval.go:151-168` (`CaseAggregate` gains median tokens)
- Modify: `internal/eval/replay_report.go:30-45` (`ReportCase` gains tokens)
- Create: `internal/eval/usage_attribution_test.go`

**Interfaces:**
- Consumes: `providers.Usage` (`providers.go:870`), `eval.CountingModel` (`compare_run.go:17`), `eval.Runner` (`eval.go:20`).
- Produces:
  - `type UsageCounter interface { Total() providers.Usage }`
  - `Result.Usage providers.Usage`
  - `CaseAggregate.InputTokens int`, `CaseAggregate.OutputTokens int` (median over repeats)
  - `ReportCase.InputTokens int`, `ReportCase.OutputTokens int` (JSON: `input_tokens`, `output_tokens`)

- [ ] **Step 1: Write the failing test**

Create `internal/eval/usage_attribution_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestAggregateMediansUsage: per-case token counts are the MEDIAN across repeats,
// matching how confidence is already aggregated. The median is what a published
// cost figure should quote — one expensive outlier must not set the headline number.
func TestAggregateMediansUsage(t *testing.T) {
	results := []Result{
		{Name: "c", Pass: true, Confidence: 0.8, Usage: providers.Usage{InputTokens: 100, OutputTokens: 10}},
		{Name: "c", Pass: true, Confidence: 0.8, Usage: providers.Usage{InputTokens: 300, OutputTokens: 30}},
		{Name: "c", Pass: true, Confidence: 0.8, Usage: providers.Usage{InputTokens: 200, OutputTokens: 20}},
	}
	agg := aggregateResults(Case{Name: "c"}, results)
	if agg.InputTokens != 200 {
		t.Errorf("median input tokens = %d, want 200", agg.InputTokens)
	}
	if agg.OutputTokens != 20 {
		t.Errorf("median output tokens = %d, want 20", agg.OutputTokens)
	}
}

// TestAggregateUsageZeroWhenUnreported: a provider that reports no usage must yield
// zero, never a fabricated estimate. Zero renders as "unknown" downstream.
func TestAggregateUsageZeroWhenUnreported(t *testing.T) {
	agg := aggregateResults(Case{Name: "c"}, []Result{{Name: "c", Pass: true}})
	if agg.InputTokens != 0 || agg.OutputTokens != 0 {
		t.Errorf("usage = %d/%d, want 0/0 when the provider reports none", agg.InputTokens, agg.OutputTokens)
	}
}

// countingStub implements UsageCounter with a scripted running total, standing in for
// CountingModel so the snapshot arithmetic is testable without a model.
type countingStub struct{ totals []providers.Usage; i int }

func (c *countingStub) Total() providers.Usage {
	u := c.totals[c.i]
	if c.i < len(c.totals)-1 {
		c.i++
	}
	return u
}

// TestUsageDeltaIsPerRun: the runner attributes each run only the tokens that run
// spent, by differencing the cumulative counter before and after.
func TestUsageDeltaIsPerRun(t *testing.T) {
	before := providers.Usage{InputTokens: 1000, OutputTokens: 100}
	after := providers.Usage{InputTokens: 1350, OutputTokens: 140}
	got := usageDelta(before, after)
	if got.InputTokens != 350 || got.OutputTokens != 40 {
		t.Errorf("usageDelta = %+v, want 350 in / 40 out", got)
	}
}

// TestReportCarriesPerCaseTokens: the tokens must survive projection into the report,
// because that JSON is what the published scorecard renders from.
func TestReportCarriesPerCaseTokens(t *testing.T) {
	camp := Campaign{N: 1, Aggregates: []CaseAggregate{
		{Name: "c", Runs: 1, Reached: true, InputTokens: 4200, OutputTokens: 380},
	}}
	rep := camp.Report("2026-08-02T00:00:00Z", "anthropic/claude-haiku-4-5", providers.Usage{}, nil)
	if len(rep.Cases) != 1 {
		t.Fatalf("cases = %d, want 1", len(rep.Cases))
	}
	if rep.Cases[0].InputTokens != 4200 || rep.Cases[0].OutputTokens != 380 {
		t.Errorf("report case tokens = %d/%d, want 4200/380",
			rep.Cases[0].InputTokens, rep.Cases[0].OutputTokens)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/eval/ -run 'TestAggregateMediansUsage|TestUsageDelta|TestReportCarries' -v`
Expected: FAIL — `unknown field Usage in struct literal of type Result`

- [ ] **Step 3: Write the implementation**

In `internal/eval/score.go`, add to `Result` (after `RecallShortCircuit`):

```go
	// Usage is the provider-reported token spend of THIS run, attributed by the
	// runner differencing its cumulative counter. Zero means the provider reported
	// nothing — treated downstream as unknown, never as free.
	Usage providers.Usage
```

In `internal/eval/compare_run.go`, add the interface beside `CountingModel`:

```go
// UsageCounter is the "how many tokens so far" capability the runner needs to
// attribute spend per case. CountingModel implements it; a plain model does not, and
// the runner simply records zero usage in that case.
type UsageCounter interface {
	Total() providers.Usage
}

// compile-time assertion: the counting wrapper satisfies the capability.
var _ UsageCounter = (*CountingModel)(nil)
```

In `internal/eval/eval.go`, add the delta helper:

```go
// usageDelta returns the spend between two cumulative snapshots. The counter only
// grows, so a negative result is impossible; clamping is unnecessary.
func usageDelta(before, after providers.Usage) providers.Usage {
	return providers.Usage{
		InputTokens:       after.InputTokens - before.InputTokens,
		OutputTokens:      after.OutputTokens - before.OutputTokens,
		CachedInputTokens: after.CachedInputTokens - before.CachedInputTokens,
		CacheWriteTokens:  after.CacheWriteTokens - before.CacheWriteTokens,
	}
}
```

Wrap `runOne` in `aggregateCase` so each run is measured. `RunN` → `aggregateCase` → `runOne` is strictly sequential (`eval.go:207-224`), which is what makes snapshot differencing correct here:

```go
func (r *Runner) aggregateCase(ctx context.Context, c Case, n int) CaseAggregate {
	results := make([]Result, 0, n)
	counter, counted := r.Model.(UsageCounter)
	for i := 0; i < n; i++ {
		var before providers.Usage
		if counted {
			before = counter.Total()
		}
		res := r.runOne(ctx, c)
		if counted {
			// Sequential by construction (RunN → aggregateCase → runOne), so the
			// delta over this window is exactly this run's spend.
			res.Usage = usageDelta(before, counter.Total())
		}
		results = append(results, res)
	}
	return aggregateResults(c, results)
}
```

In `CaseAggregate`, add:

```go
	// InputTokens / OutputTokens are the MEDIAN provider-reported spend per run for
	// this case — the basis of the published cost-per-investigation figure. Zero when
	// the model does not report usage.
	InputTokens  int
	OutputTokens int
```

In `aggregateResults`, collect and median them alongside the existing confidence median (reuse the same median helper the function already uses for `confs` — find it and use it; do not write a second one):

```go
	ins := make([]float64, 0, len(results))
	outs := make([]float64, 0, len(results))
	for _, res := range results {
		ins = append(ins, float64(res.Usage.InputTokens))
		outs = append(outs, float64(res.Usage.OutputTokens))
	}
```

and set `InputTokens: int(median(ins))`, `OutputTokens: int(median(outs))` in the returned aggregate, matching the existing median function's name.

In `internal/eval/replay_report.go`, add to `ReportCase`:

```go
	// Per-case token spend (median over the repeats) — what the scorecard's
	// cost-per-investigation table is computed from.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
```

`Campaign.Report` projects with `ReportCase(a)` — a direct conversion, so the new fields must appear in **the same order and with the same names and types** in both structs for the conversion to keep compiling. Add them at the end of both.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/eval/ -v`
Expected: PASS — the new tests and every existing one.

- [ ] **Step 5: Lint**

Run: `golangci-lint run ./internal/eval/...`
Expected: no issues.

- [ ] **Step 6: Commit**

```bash
git add internal/eval/
git commit -m "feat(eval): attribute token spend per case for cost reporting"
```

---

### Task 2: Cost-per-investigation section in the scorecard

**Files:**
- Modify: `internal/eval/scorecard.go:110-165`
- Create: `internal/eval/scorecard_cost_test.go`
- Modify: `eval/ci.runlore.yaml` (add `model.pricing`)

**Interfaces:**
- Consumes: `ReportCase.InputTokens/OutputTokens` (Task 1), `ReportCase.RecallShortCircuit` (`replay_report.go:44`), `Report.CostUSD`, `EstimateCostUSD` (`replay_report.go:68`), `Report.Model`.
- Produces: `func costSection(rep Report, inUSD, cachedUSD, outUSD float64) string`, rendered inside `ScorecardMarkdown`.

- [ ] **Step 1: Write the failing test**

Create `internal/eval/scorecard_cost_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"strings"
	"testing"
)

func costReport() Report {
	return Report{
		At: "2026-08-02T06:00:00Z", Model: "anthropic/claude-haiku-4-5", N: 5,
		Reached: 2, Total: 2, PassRate: 1,
		Cases: []ReportCase{
			// A full investigation: no recall short-circuit, high token spend.
			{Name: "gitops-bad-image-tag", Runs: 5, Reached: true,
				InputTokens: 96000, OutputTokens: 3200},
			// An instant recall: short-circuited, an order of magnitude cheaper.
			{Name: "known-pattern-recall", Runs: 5, Reached: true, HasRecall: true,
				RecallShortCircuit: 5, InputTokens: 4100, OutputTokens: 260},
		},
	}
}

// TestCostSectionSplitsRecallFromFullLoop is the report's asked-for comparison: a
// full investigation next to an instant recall, priced. It is the number no
// competitor publishes.
func TestCostSectionSplitsRecallFromFullLoop(t *testing.T) {
	got := costSection(costReport(), 1.00, 0.10, 5.00)
	if !strings.Contains(got, "full investigation") {
		t.Errorf("missing the full-investigation row:\n%s", got)
	}
	if !strings.Contains(got, "instant recall") {
		t.Errorf("missing the instant-recall row:\n%s", got)
	}
	// 96000 in @ $1/MTok + 3200 out @ $5/MTok = 0.096 + 0.016 = $0.112
	if !strings.Contains(got, "0.11") {
		t.Errorf("full-investigation cost not rendered:\n%s", got)
	}
	// The model must be named next to the figure — a naked price is unfalsifiable.
	if !strings.Contains(got, "claude-haiku-4-5") {
		t.Errorf("cost figures must name the model:\n%s", got)
	}
}

// TestCostSectionOmittedWithoutPrices: no prices configured means no cost section at
// all. Rendering "$0.00" would be a lie.
func TestCostSectionOmittedWithoutPrices(t *testing.T) {
	if got := costSection(costReport(), 0, 0, 0); got != "" {
		t.Errorf("expected no cost section without prices, got:\n%s", got)
	}
}

// TestCostSectionOmittedWithoutTokens: an old report carrying no per-case tokens must
// not render an empty or zeroed table.
func TestCostSectionOmittedWithoutTokens(t *testing.T) {
	rep := costReport()
	for i := range rep.Cases {
		rep.Cases[i].InputTokens, rep.Cases[i].OutputTokens = 0, 0
	}
	if got := costSection(rep, 1.00, 0.10, 5.00); got != "" {
		t.Errorf("expected no cost section without token data, got:\n%s", got)
	}
}

// TestScorecardIncludesCostSection wires it end to end through the renderer.
func TestScorecardIncludesCostSection(t *testing.T) {
	rep := costReport()
	c := 0.5
	rep.CostUSD = &c
	md := ScorecardMarkdown(rep, nil)
	if !strings.Contains(md, "Cost per investigation") {
		t.Errorf("scorecard missing the cost section:\n%s", md)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/eval/ -run TestCost -v`
Expected: FAIL — `undefined: costSection`

- [ ] **Step 3: Write the implementation**

Add to `internal/eval/scorecard.go`:

```go
// costSection renders the cost-per-investigation comparison: what a full
// investigation costs against what an instant recall costs, on this run's model at
// this run's prices. It is the single most concrete claim the learning loop makes —
// recall is roughly an order of magnitude cheaper — and publishing it turns an
// assertion into a measurement.
//
// Returns "" when prices are unset or the report carries no per-case token data;
// a fabricated or zeroed cost would be worse than no cost at all.
func costSection(rep Report, inUSD, cachedUSD, outUSD float64) string {
	if inUSD == 0 && outUSD == 0 {
		return ""
	}
	var full, recall []ReportCase
	for _, c := range rep.Cases {
		if c.InputTokens == 0 && c.OutputTokens == 0 {
			continue // no usage reported for this case
		}
		if c.RecallShortCircuit > 0 {
			recall = append(recall, c)
		} else {
			full = append(full, c)
		}
	}
	if len(full) == 0 && len(recall) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n## Cost per investigation\n\n")
	fmt.Fprintf(&b, "Median provider-reported tokens per case on `%s`, priced at $%.2f/MTok in · $%.2f/MTok out. ",
		rep.Model, inUSD, outUSD)
	b.WriteString("Replay evidence, so tool latency and live-cluster variance are excluded.\n\n")
	b.WriteString("| path | cases | median in tok | median out tok | est. cost |\n|---|---|---|---|---|\n")
	row := func(label string, cs []ReportCase) {
		if len(cs) == 0 {
			return
		}
		ins := make([]float64, 0, len(cs))
		outs := make([]float64, 0, len(cs))
		for _, c := range cs {
			ins = append(ins, float64(c.InputTokens))
			outs = append(outs, float64(c.OutputTokens))
		}
		mi, mo := median(ins), median(outs)
		cost := EstimateCostUSD(
			providers.Usage{InputTokens: int(mi), OutputTokens: int(mo)},
			inUSD, cachedUSD, outUSD)
		fmt.Fprintf(&b, "| %s | %d | %s | %s | $%.3f |\n",
			label, len(cs), compactTokens(int(mi)), compactTokens(int(mo)), cost)
	}
	row("full investigation", full)
	row("instant recall", recall)
	return b.String()
}
```

Use the package's existing median helper (the one `aggregateResults` uses — check its name and signature in `internal/eval/stats.go` and call it, do not duplicate). Add `"github.com/Smana/runlore/internal/providers"` to the imports if absent.

Call it from `ScorecardMarkdown`, right after the Scenarios table and before "Confidence calibration".

The prices must reach the renderer without `internal/eval` taking a dependency on
`internal/config`, so the signature takes three floats rather than a `*config.Pricing`:

```go
func ScorecardMarkdown(rep Report, history []HistoryEntry, inUSD, cachedUSD, outUSD float64) string
```

Update both call sites: `internal/app/eval_scorecard.go:57` and `internal/eval/scorecard_test.go` (pass `0, 0, 0` in existing tests — they assert on sections the cost block does not touch).

`RunEvalScorecard` reads a report from disk and has no config, and `Report` carries only the *total* `CostUSD` — not the rates behind it. So the rates travel **in the report**. Add three fields to `Report` in `internal/eval/replay_report.go`:

```go
	// Token rates (USD per MTok) this run was priced at, carried so the published
	// scorecard can show a per-path cost breakdown without re-reading any config.
	InputUSDPerMTok       float64 `json:"input_usd_per_mtok,omitempty"`
	CachedInputUSDPerMTok float64 `json:"cached_input_usd_per_mtok,omitempty"`
	OutputUSDPerMTok      float64 `json:"output_usd_per_mtok,omitempty"`
```

Set them in `internal/app/eval.go` next to `evalCostUSD(cfg, usage)`:

```go
		rep := camp.Report(st, cfg.Model.Provider+"/"+cfg.Model.Model, usage, evalCostUSD(cfg, usage))
		if p := cfg.Model.Pricing; p != nil {
			rep.InputUSDPerMTok = p.InputUSDPerMTok
			rep.CachedInputUSDPerMTok = p.CachedInputUSDPerMTok
			rep.OutputUSDPerMTok = p.OutputUSDPerMTok
		}
```

and read them in `RunEvalScorecard`:

```go
	md := eval.ScorecardMarkdown(rep, entries, rep.InputUSDPerMTok, rep.CachedInputUSDPerMTok, rep.OutputUSDPerMTok)
```

Confirm the exact `config.Pricing` field names at `internal/config/config.go:592` before writing this — use what is there.

- [ ] **Step 4: Configure prices for the nightly**

Add to `eval/ci.runlore.yaml` under `model:` the `pricing:` block with the rates for the model the nightly runs. `config.Model.Pricing` already exists (`config.go:586`), so this is a supported key, not a schema change. Use the current published rates for that model and add a comment naming the date they were checked.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/eval/ ./internal/app/ -v`
Expected: PASS.

- [ ] **Step 6: Render a scorecard locally to eyeball it**

Run:
```bash
go run ./cmd/lore eval scorecard -report eval/reports/<any *-replay.json> -dir /tmp/sc
cat /tmp/sc/scorecard.md
```
Expected: renders. (Existing reports predate per-case tokens, so the cost section will be correctly *absent* — that is the `TestCostSectionOmittedWithoutTokens` behaviour, proven live.)

- [ ] **Step 7: Commit**

```bash
git add internal/eval/ internal/app/eval.go internal/app/eval_scorecard.go eval/ci.runlore.yaml
git commit -m "feat(eval): publish cost per investigation, split by recall vs full loop"
```

---

### Task 3: Recall-claim honesty pass and its drift guard

**Files:**
- Modify: `website/content/docs/concepts/learning-loop.md:139-146`
- Modify: `README.md` (the learning-loop recall paragraph)
- Create: `internal/investigate/recall_doc_claims_test.go`

**Interfaces:**
- Consumes: the measurement helpers `computeFire`, `computeFireReranked`, `scriptedReranker`, `rerankTargets`, `evalCases`, `writeEvalCatalog` from `internal/investigate/recalleval_test.go` (same package, test-only).
- Produces: `TestRecallDocClaimsMatchMeasurement`.

**Why this task exists:** `learning-loop.md:146` publishes `rerank on → 11/11 (1.00), precision 1.00, 0/2` as "measured on the eval harness". `recalleval_test.go:831` supplies `scriptedReranker{accept: rerankTargets(cases), conf: 0.9}` — a stand-in that accepts the correct target by construction. The row measures the gate's plumbing and the corpus's structural agreement, not a model's reranking judgment. Disclose it before a reader finds it.

- [ ] **Step 1: Write the failing test**

Create `internal/investigate/recall_doc_claims_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// docClaimsPath is the published page whose numbers this test pins.
const docClaimsPath = "../../website/content/docs/concepts/learning-loop.md"

// TestRecallDocClaimsMatchMeasurement is a DRIFT GUARD over the real parse target:
// it reads the numbers published in learning-loop.md and compares them to the values
// the measurement actually produces right now. Editing the prose without re-measuring
// (or changing the engine without updating the prose) fails here.
//
// It also asserts the METHODOLOGY caveat is present. The rerank-on row is measured
// with a scripted reranker that accepts the correct target by construction, so the
// row measures the fire gate, not model judgment — a page that quotes the number
// without saying so overstates it.
func TestRecallDocClaimsMatchMeasurement(t *testing.T) {
	raw, err := os.ReadFile(docClaimsPath)
	if err != nil {
		t.Fatalf("read %s: %v", docClaimsPath, err)
	}
	doc := string(raw)

	cat := writeEvalCatalog(t)
	cases := evalCases()
	off := computeFire(t, cat, cases)
	on := computeFireReranked(t, cat, cases, &scriptedReranker{accept: rerankTargets(cases), conf: 0.9})

	// | rerank **off** | 0/11 (0.00) | — | 0/2 |
	offRow := regexp.MustCompile(`rerank \*\*off\*\* \| (\d+)/(\d+)`)
	m := offRow.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("could not find the rerank-off row in %s — did the table change shape?", docClaimsPath)
	}
	assertInt(t, "rerank-off fired", m[1], off.fired)
	assertInt(t, "label positives", m[2], off.labelPositives)

	// | rerank **on** | **11/11 (1.00)** | **1.00** | **0/2** |
	onRow := regexp.MustCompile(`rerank \*\*on\*\* \| \*\*(\d+)/(\d+)`)
	m = onRow.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("could not find the rerank-on row in %s", docClaimsPath)
	}
	assertInt(t, "rerank-on fired", m[1], on.fired)
	assertInt(t, "label positives (on)", m[2], on.labelPositives)

	// The methodology caveat must accompany the number.
	for _, want := range []string{"scripted", "not a model"} {
		if !contains(doc, want) {
			t.Errorf("learning-loop.md must state the methodology caveat containing %q, "+
				"otherwise the rerank-on row reads as measured model accuracy", want)
		}
	}
}

func assertInt(t *testing.T, what, got string, want int) {
	t.Helper()
	n, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("%s: %q is not a number", what, got)
	}
	if n != want {
		t.Errorf("%s: doc says %d, measurement says %d — the published number is stale", what, n, want)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && regexp.MustCompile(`(?i)`+regexp.QuoteMeta(needle)).MatchString(hay)
}
```

Check the helper names against `internal/investigate/recalleval_test.go:824-841` before running — use the exact identifiers that exist (`computeFire`, `computeFireReranked`, `scriptedReranker`, `rerankTargets`, `evalCases`, `writeEvalCatalog`, and the field names `fired` / `labelPositives`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestRecallDocClaims -v`
Expected: FAIL on the methodology caveat — the words are not in the page yet.

- [ ] **Step 3: Update the documentation**

In `website/content/docs/concepts/learning-loop.md`, replace the sentence introducing the table (around line 139) and add a caveat paragraph immediately after it:

```markdown
> Measured on the eval harness at default thresholds (`recalleval_test.go`,
> `TestRecallEvalRerankFireRate`) over **11 label-derived positives and 2 negatives**:
>
> | | fire-rate (label positives) | precision | negatives fired |
> |---|---|---|---|
> | rerank **off** | 0/11 (0.00) | — | 0/2 |
> | rerank **on** | **11/11 (1.00)** | **1.00** | **0/2** |
>
> **What this does and does not measure.** The rerank-**on** row uses a *scripted*
> reranker — a stand-in that returns the correct candidate at fixed confidence. So the
> row measures the **fire gate and the corpus's structural agreement**, not a model's
> reranking accuracy: it proves that when the reranker is right, the calibrated gate
> fires at the default threshold, which BM25-magnitude gating never did. It is **not a
> model benchmark**. For what a real model achieves on real incidents, the honest
> reference point is ITBench: frontier models identify the root cause **< 50%** of the
> time (see [Benchmarking]({{< relref "/docs/reference/benchmarking.md" >}})). The
> corpus here is small and hand-built; treat these numbers as a regression guard on the
> gate, not as a field measurement.
```

Apply the same one-line caveat wherever `README.md` quotes recall performance: state the corpus size and that the fire-rate figure measures the gate.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/investigate/ -run TestRecallDocClaims -v`
Expected: PASS.

- [ ] **Step 5: Mutation-test the guard**

Edit `learning-loop.md` changing `0/11` to `1/11`. Run the test.
Expected: FAIL — `rerank-off fired: doc says 1, measurement says 0`. Revert.

Then delete the word `scripted` from the caveat. Run the test.
Expected: FAIL on the methodology assertion. Restore.

- [ ] **Step 6: Build the site and commit**

```bash
cd website && hugo --gc --minify && cd ..
git add website/content/docs/concepts/learning-loop.md README.md internal/investigate/recall_doc_claims_test.go
git commit -m "docs(recall): state the corpus and the scripted-reranker caveat, pinned by a test"
```

---

### Task 4: The `/eval` page and its publishing pipeline

**Files:**
- Modify: `.github/workflows/eval.yaml` (append a dispatch step)
- Modify: `.github/workflows/docs.yml` (trigger, fetch, render)
- Create: `website/content/eval.md` (the placeholder, overwritten at build time)
- Modify: `website/content/docs/reference/benchmarking.md:10-14`
- Modify: `README.md:16` (badge link target)
- Modify: `.gitignore` (ignore the generated page body if the render writes a temp file)

**Interfaces:**
- Consumes: the `eval-scorecard` branch layout written by `eval.yaml:80-110` (`scorecard.md`, `badge.json`, `history.jsonl`).
- Produces: `https://runlore.io/eval`.

- [ ] **Step 1: Write the placeholder page**

Create `website/content/eval.md`:

```markdown
---
title: Eval scorecard
---

# RunLore nightly eval scorecard

*No nightly scorecard has been published yet.*

The nightly replay eval publishes a per-scenario scorecard — pass/fail, recall
outcomes, confidence calibration, model, date and cost — on every run, red or green.
When a run has published, this page shows it.

Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

See [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml).
```

This file is committed so the site always builds and the link is never dead; the workflow overwrites it when a scorecard exists.

- [ ] **Step 2: Add the fetch-and-render step to `docs.yml`**

Extend the `on:` block:

```yaml
on:
  push:
    branches: [main]
    paths:
      - 'website/**'
      - '.github/workflows/docs.yml'
  workflow_dispatch:
  repository_dispatch:
    types: [scorecard-updated]
```

Insert a step after `Install Hugo Extended` and before `Build`:

```yaml
      # Pull the published scorecard from the eval-scorecard orphan branch and render
      # it as /eval. The branch is the nightly's publishing target; copying it in at
      # build time keeps main free of generated numbers. A missing branch is NOT a
      # failure — the committed placeholder stands in, so the docs deploy and the
      # link stay alive either way.
      - name: Render the eval scorecard page
        run: |
          set -euo pipefail
          if ! git fetch --depth=1 origin eval-scorecard 2>/dev/null; then
            echo "::notice title=Eval scorecard::branch not published yet — keeping the placeholder"
            exit 0
          fi
          published=$(git log -1 --format=%cI FETCH_HEAD)
          {
            echo "---"
            echo "title: Eval scorecard"
            echo "---"
            echo
            echo "*Published ${published} from the [\`eval-scorecard\`](https://github.com/Smana/runlore/tree/eval-scorecard) branch.*"
            echo
            git show FETCH_HEAD:scorecard.md
          } > website/content/eval.md
```

The checkout step already uses `fetch-depth: 0`, so the fetch works without extra configuration.

- [ ] **Step 3: Add the dispatch step to `eval.yaml`**

Append after the `publish scorecard (eval-scorecard branch)` step:

```yaml
      # Tell the docs site to rebuild so /eval shows the fresh numbers. Without this
      # the page would only refresh when someone happens to push to website/.
      - name: trigger docs rebuild
        if: always() && steps.guard.outputs.has_key == 'true' && github.ref == 'refs/heads/main'
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          gh api "repos/${GITHUB_REPOSITORY}/dispatches" \
            -f event_type=scorecard-updated
```

The job already declares `contents: write`, which is the permission `POST /repos/{owner}/{repo}/dispatches` requires — do not widen it further.

- [ ] **Step 4: Repoint the references**

- `website/content/docs/reference/benchmarking.md:10-14` — replace the blockquote's link to the branch with a link to `/eval`:
  ```markdown
  > RunLore's own nightly numbers are public: the replay eval publishes a
  > per-scenario scorecard — pass/fail, recall outcomes, confidence calibration,
  > model, date, and cost — on every run, red or green.
  > **→ [The nightly scorecard](/eval)**
  ```
- `README.md:16` — keep the badge *image* (it reads `badge.json` from the branch, which is what makes it update without a site rebuild) and change only its **link target** to `https://runlore.io/eval`.

- `website/content/_index.md` — this PR creates `/eval`, so it also owns the homepage line pointing at it. Add immediately after the hero buttons (currently `_index.md:27`):

  ```markdown
  <p class="rl-eyebrow"><a href="/eval">The only SRE agent that publishes its own nightly eval scorecard</a> — per scenario, red or green.</p>
  ```

  If the S4 positioning branch has already merged, the hero copy around it will differ — insert the line after whatever the last `hero-button` shortcode is, and do not otherwise touch the hero.

- [ ] **Step 5: Verify the workflows parse**

Run: `gh workflow view docs.yml` and `gh workflow view eval.yaml`
Expected: both render without a parse error. Alternatively run `actionlint` if available.

Test the render step's logic locally:
```bash
git fetch --depth=1 origin eval-scorecard || echo "branch absent — placeholder path taken"
```
Expected (today): the branch is absent, so the placeholder path is exercised — which is exactly the case that must not break the build.

- [ ] **Step 6: Build the site**

Run: `cd website && hugo --gc --minify`
Expected: builds, `/eval` renders the placeholder.

- [ ] **Step 7: Commit**

```bash
git add .github/workflows/docs.yml .github/workflows/eval.yaml website/content/eval.md website/content/docs/reference/benchmarking.md README.md
git commit -m "feat(eval): publish the nightly scorecard as a page on runlore.io"
```

---

### Task 5: ADOPTERS, OpenSSF Scorecard, SBOM verification

**Files:**
- Create: `ADOPTERS.md`
- Create: `.github/workflows/scorecard.yml`
- Modify: `README.md` (badge row + an adopters ask)
- Modify: `SECURITY.md` (state the supply-chain artifacts)

- [ ] **Step 1: Write `ADOPTERS.md`**

```markdown
# Adopters

Teams and organizations running RunLore. Adding yourself helps other teams judge
whether it fits their platform — and helps us prioritize what to build next.

**To add yourself:** open a PR adding a row. Anything you are comfortable sharing is
enough; the "how" column is the most useful part for other readers.

| Organization | Since | How they use RunLore | Contact |
|---|---|---|---|
| [Ogenki](https://ogenki.io) | 2026-06 | Alertmanager → investigation → curated KB PRs on a self-hosted GitOps platform | [@Smana](https://github.com/Smana) |

*Running RunLore but not ready to be listed publicly? Say hello in a
[discussion](https://github.com/Smana/runlore/discussions) — anonymized feedback is
just as valuable.*
```

- [ ] **Step 2: Add the OpenSSF Scorecard workflow**

Create `.github/workflows/scorecard.yml`:

```yaml
# OpenSSF Scorecard — a weekly automated assessment of this repo's supply-chain
# posture (branch protection, pinned dependencies, signed releases, SAST…). Results
# publish to the Security tab; the badge in the README links to the public report.
name: OpenSSF Scorecard
on:
  branch_protection_rule:
  schedule:
    - cron: "30 5 * * 1"      # Mondays 05:30 UTC
  push:
    branches: [main]

permissions: read-all

jobs:
  analysis:
    name: Scorecard analysis
    runs-on: ubuntu-latest
    permissions:
      security-events: write   # upload the SARIF result to the Security tab
      id-token: write          # publish the signed result to the public API
      contents: read
      actions: read
    steps:
      - uses: actions/checkout@<sha>   # v7.0.0 — match the SHA used in ci.yaml
        with:
          persist-credentials: false
      - uses: ossf/scorecard-action@<sha>   # pin to the latest release SHA
        with:
          results_file: results.sarif
          results_format: sarif
          publish_results: true
      - uses: github/codeql-action/upload-sarif@<sha>
        with:
          sarif_file: results.sarif
```

Resolve each `<sha>` with `gh api repos/<owner>/<repo>/commits/<tag> --jq .sha` and pin it, matching how `ci.yaml` pins its actions. Do not leave a placeholder.

- [ ] **Step 3: Add the badges and the adopters ask to the README**

In the badge block (`README.md:14-20`) add, keeping the existing style:

```markdown
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/Smana/runlore/badge)](https://securityscorecards.dev/viewer/?uri=github.com/Smana/runlore)
```

Add a short "Who's using RunLore" line near the bottom of the README linking `ADOPTERS.md` and inviting entries.

- [ ] **Step 4: Verify the SBOM actually ships**

Run:
```bash
sed -n '44,60p' .goreleaser.yaml
gh release view $(git describe --tags --abbrev=0) --json assets --jq '.assets[].name'
```
Expected: the `sboms:` config produces `*.sbom.json` (or similar) assets on the release. If they are present, document it in `SECURITY.md` under a "Supply chain" heading: cosign keyless signing of binaries, checksums bundle, SBOMs per artifact, signed OCI chart, and how to verify each. If they are **not** present, fix the goreleaser config so they are — the point of this task is that the claim is true.

- [ ] **Step 5: Commit**

```bash
git add ADOPTERS.md .github/workflows/scorecard.yml README.md SECURITY.md
git commit -m "docs: ADOPTERS.md, OpenSSF Scorecard badge, and the supply-chain posture"
```

---

### Task 6: Bootstrap the scorecard branch — maintainer step

**Files:** none in the repo.

> **This task is the owner's to perform.** Nothing in Tasks 1–5 depends on it, but the acceptance criteria do: until the secret exists, the badge stays broken and `/eval` shows the placeholder.

- [ ] **Step 1: Add the secret**

```bash
gh secret set RUNLORE_EVAL_API_KEY --body "<anthropic api key>"
gh secret list          # confirm it appears
```

- [ ] **Step 2: Run the nightly on demand**

```bash
gh workflow run eval.yaml
gh run watch
```
Expected: the run takes minutes (not 30 seconds — a 30-second "success" means the guard still sees no key), the `publish scorecard` step pushes, and the `trigger docs rebuild` step dispatches.

- [ ] **Step 3: Verify the chain end to end**

```bash
git ls-remote --heads origin | grep eval-scorecard      # branch now exists
gh run list --workflow=docs.yml -L 1                    # a repository_dispatch run fired
curl -sI https://runlore.io/eval | head -1              # 200
```
Then open the README on GitHub and confirm the **Nightly eval** badge renders numbers rather than "invalid".

- [ ] **Step 4: Read the published scorecard**

Open `https://runlore.io/eval`. Confirm the per-scenario table, the calibration section, the cost-per-investigation table (Task 2), and the history are all present and the numbers are plausible. **If the run is red, publish it anyway** — that is the design, and the value of the asset is that it publishes red runs too.

---

## Final verification

- [ ] `go build ./... && go test ./... && golangci-lint run` — all clean
- [ ] `cd website && hugo --gc --minify` builds with and without the `eval-scorecard` branch present
- [ ] The drift guard fails on an edited number and on a removed caveat (both mutations tried and reverted)
- [ ] `/eval` reachable; README badge resolves; `benchmarking.md` link resolves
- [ ] `ADOPTERS.md` linked from the README; OpenSSF badge renders; SBOM assets confirmed on the latest release
- [ ] Run `/security-review` on the branch diff — the new workflow permissions and the `repository_dispatch` call are the parts to scrutinize
- [ ] Open the PR — English title and description, no AI attribution, no co-author trailers
</content>
