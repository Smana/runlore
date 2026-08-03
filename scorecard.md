# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml) — the replay eval scores the model+loop over recorded incident evidence (no live cluster), so anyone can reproduce it:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-08-03T09:51:36Z · model `openai/glm-4.5-air` · **4/4 scenarios reached (100%)** · n=5 runs/case, k-of-n bar 70% · est. cost $0.22 (915.4k in / 29.6k out tokens)

## Scenarios (latest run)

| scenario | result | pass-rate | median confidence | recall | notes |
|---|---|---|---|---|---|
| gitops-broken-kustomization | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| harbor-chart-bump | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| poisoned-recall-rejected | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| poisoned-recall-verify | ✅ PASS | 100% (n=5) | 0.95 | fired 5/5 · short-circuit 0/5 (expect: withdrawn) | — |

## Cost per investigation

Median provider-reported tokens per case on `openai/glm-4.5-air`, priced at $0.20/MTok in · $1.10/MTok out. Replay evidence, so tool latency and live-cluster variance are excluded.

| path | cases | median in tok | median out tok | est. cost |
|---|---|---|---|---|
| full investigation | 4 | 37.6k | 1.4k | $0.009 |

## Confidence calibration

- **Confidently wrong** (missed with median confidence ≥ 0.70): none
- **Underconfident** (reached with median confidence < 0.50): none

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](history.jsonl). Runs below the CI gate publish here exactly like green ones.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
| 2026-08-03T09:51:36Z | openai/glm-4.5-air | 4/4 | 100% | $0.22 |
| 2026-08-02T08:35:01Z | openai/glm-4.5-air | 2/2 | 100% | $0.05 |
| 2026-08-02T08:12:29Z | anthropic/claude-haiku-4-5-20251001 | 0/2 | 0% | $0.00 |
