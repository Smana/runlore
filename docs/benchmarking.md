# Benchmarking models with RunLore

RunLore is model-agnostic: you pick the model behind the investigation loop. This
page shows how to benchmark several models against RunLore's own eval harness in
**one command** and publish an honest comparison.

> [!important] Run RunLore on *your* models
> The comparison runs the **replay** eval suite (recorded incident evidence, no
> live cluster) so it is reproducible and cheap. It measures the model+loop's
> reasoning over fixed evidence — not a live cluster's flakiness. See the
> [replay-vs-live caveat](#replay-vs-live) before publishing.

## One command

```bash
lore eval --compare eval/compare.example.yaml --cases examples/eval -n 3
```

This benchmarks every model in the spec against the same replay cases, grades each
run with one fixed judge, and writes an aggregated report to
`eval/reports/<stamp>-compare.md` (human) and `.json` (machine). It needs **no
`config.model`** — the spec carries its own per-entry models and judge.

### The comparison spec

A small YAML listing the models to benchmark and (optionally) the judge. See
[`eval/compare.example.yaml`](../eval/compare.example.yaml):

```yaml
judge:                       # optional; one fixed judge for every entry (blind grading)
  provider: anthropic
  model: claude-opus-4-1
  api_key_env: RUNLORE_JUDGE_API_KEY
models:
  - name: haiku-4.5          # report label (must be unique)
    provider: anthropic      # openai (default) | anthropic | gemini
    model: claude-haiku-4-5-20251001
    api_key_env: RUNLORE_ANTHROPIC_API_KEY
    prices: {input_usd: 1.00, output_usd: 5.00}   # optional → enables the cost column
  - name: gpt-5-mini
    provider: openai
    base_url: https://api.openai.com/v1
    model: gpt-5-mini
    api_key_env: RUNLORE_OPENAI_API_KEY
    effort: medium           # reasoning_effort (openai-compatible only; not gemini)
    prices: {input_usd: 0.25, output_usd: 2.00}
  - name: local-qwen3        # keyless: a local vLLM/Ollama endpoint
    provider: openai
    base_url: http://localhost:8000/v1
    model: qwen3-30b
```

Entry fields: `name` (required, unique), `model` (required), `provider`,
`base_url`, `api_key_env` (empty = keyless), `effort` (openai/anthropic only —
gemini is rejected), `prices` (optional; omit to omit the cost column for that
entry). Unknown keys are rejected so a typo in a published spec fails loudly.

The **judge** precedence is: `--judge-*` flags → the spec's `judge:` block →
`config.model`. Keeping the judge in the spec makes a published comparison
self-describing — the judge disclosure travels with the results.

## The report

The comparison report has one row per model, in **spec order** (a comparison
should not silently reorder by score), with these columns:

| column | meaning |
|---|---|
| `model` | the entry's report label |
| `provider/model` | wire provider + model name (+ `effort=` when set) |
| `pass rate` | fraction of cases that reached the k-of-n bar (`reached/total`) |
| `reached` | cases whose pass-rate met the bar (median over N) |
| `root_cause` / `evidence` / `solution` / `description` / `calibration` | per-dimension **median** rubric score over every graded run (`—` when ungraded) |
| `coverage` | median data-source coverage ratio over all runs |
| `confident-wrong` | graded runs that stated a wrong root cause with high confidence |
| `in tok` / `out tok` | total provider-reported input/output tokens across the whole benchmark |
| `est. cost (USD)` | `in·input_usd + out·output_usd` per MTok — **only shown when at least one entry supplies `prices`** |

A second table gives the per-case pass rate (k-of-n) for each model, with `flaky`
flagged when runs disagree too much to trust. Case rows are sorted by name and the
JSON is deterministic, so two reports diff cleanly.

Rubric dimensions and the pass gate are defined in [`eval/rubric.md`](../eval/rubric.md).
Token usage is the provider-reported count per completion (see
`providers.Usage`), summed across the loop **and** the judge by the runner's
`CountingModel` wrapper.

## Publishing results honestly

RunLore's positioning is honesty about model performance. When you publish a
comparison, disclose:

1. **N runs.** State the `-n` you used (median over N; the pass gate is k-of-n at
   ≥70%). A single run is not a benchmark — one lucky pass hides a flaky model.
   Report the per-case table so flakiness is visible.
2. **The judge model.** Grading is by an LLM judge (blind — the judge never sees
   which model produced a result). The report prints the judge identity; keep it.
   Prefer a judge **stronger than any model under test**, and never let a model
   grade itself in the same run (bias).
3. **<a id="replay-vs-live"></a>Replay vs live.** These numbers are **replay**:
   fixed, recorded evidence. They isolate reasoning quality but do **not** capture
   live-cluster tool flakiness, latency, or evidence-gathering gaps. Live-fire
   (`lore eval --live`) is the harder, cluster-bound test. Label replay results as
   replay.
4. **ITBench context.** Independent work (ITBench, IBM/ICML 2025, see
   [`docs/prior-art.md`](prior-art.md)) found frontier models identify the root
   cause **< 50%** of the time and fully resolve only ~11–14% of real K8s
   incidents. Treat sub-50% as the baseline; a high replay pass-rate is a ceiling,
   not a field number. Design for failure and make honest uncertainty a feature.
5. **Versioned report.** Commit the generated `eval/reports/<stamp>-compare.md`
   and `.json` so a published claim points at a reproducible artifact (the spec,
   the case corpus at that commit, the judge, and N are all recoverable).

## Offline validation (keyless CI)

The comparison pipeline is covered by an integration test that runs it end-to-end
against a local, **keyless** OpenAI-compatible mock endpoint
(`internal/app/eval_compare_test.go`): the mock streams the tool calls the replay
loop needs (`what_changed` → `query_metrics` → `query_logs` → `submit_findings`)
and answers the judge's forced `submit_grade` tool with a fixed rubric grade plus a
token-usage block. So CI exercises load → per-entry replay → coverage → blind
grading → aggregation → report writing with no API key and no network. Run it with:

```bash
go test ./internal/app/ -run TestRunEvalCompareOffline
```
