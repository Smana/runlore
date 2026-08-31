# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-08-31T12:45:35Z · model `openai/glm-4.5-air` · **2/6 scenarios reached (33%)** · n=5 runs/case, k-of-n bar 70% · est. cost $0.32 (1.3M in / 54.1k out tokens)

## Scenarios (latest run)

| scenario | result | pass-rate | median confidence | recall | notes |
|---|---|---|---|---|---|
| gitops-broken-kustomization | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| harbor-chart-bump | ❌ MISS | 0% (n=5) | 0.80 | — | harbor-db |
| node-eviction-no-commons | ⚠️ FLAKY | 40% (n=5) | 0.70 | fired 0/5 · short-circuit 0/5 (expect: rejected) | request |
| node-eviction-with-commons | ✅ PASS | 100% (n=5) | 0.90 | fired 0/5 · short-circuit 0/5 (expect: rejected) | — |
| poisoned-recall-rejected | ❌ MISS | 20% (n=5) | 0.90 | — | eval-victim, pull, v9.9.9 |
| poisoned-recall-verify | ⚠️ FLAKY | 40% (n=5) | 1.00 | fired 5/5 · short-circuit 0/5 (expect: withdrawn) | v9.9.9 |

## Cost per investigation

Median provider-reported tokens per case on `openai/glm-4.5-air`, priced at $0.20/MTok in · $1.10/MTok out. Replay evidence, so tool latency and live-cluster variance are excluded.

| path | cases | median in tok | median out tok | est. cost |
|---|---|---|---|---|
| full investigation | 6 | 35.1k | 1.8k | $0.009 |

## Confidence calibration

- **Confidently wrong** (missed with median confidence ≥ 0.70): 4 — harbor-chart-bump, node-eviction-no-commons, poisoned-recall-rejected, poisoned-recall-verify
- **Underconfident** (reached with median confidence < 0.50): none

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
| 2026-08-31T12:45:35Z | openai/glm-4.5-air | 2/6 | 33% | $0.32 |
| 2026-08-30T11:21:46Z | openai/glm-4.5-air | 2/6 | 33% | $0.30 |
| 2026-08-29T12:25:38Z | openai/glm-4.5-air | 3/6 | 50% | $0.32 |
| 2026-08-28T18:23:22Z | openai/glm-4.5-air | 1/6 | 17% | $0.32 |
| 2026-08-27T17:34:55Z | openai/glm-4.5-air | 3/6 | 50% | $0.34 |
| 2026-08-26T07:04:32Z | openai/glm-4.5-air | 2/6 | 33% | $0.32 |
| 2026-08-25T06:59:22Z | openai/glm-4.5-air | 1/6 | 17% | $0.30 |
| 2026-08-24T07:07:05Z | openai/glm-4.5-air | 2/6 | 33% | $0.35 |
| 2026-08-23T06:50:15Z | openai/glm-4.5-air | 2/6 | 33% | $0.28 |
| 2026-08-22T06:54:44Z | openai/glm-4.5-air | 2/6 | 33% | $0.32 |
| 2026-08-21T07:04:23Z | openai/glm-4.5-air | 2/6 | 33% | $0.28 |
| 2026-08-20T07:02:55Z | openai/glm-4.5-air | 1/6 | 17% | $0.28 |
| 2026-08-19T06:58:02Z | openai/glm-4.5-air | 3/6 | 50% | $0.28 |
| 2026-08-18T06:58:03Z | openai/glm-4.5-air | 2/6 | 33% | $0.31 |
| 2026-08-17T07:09:28Z | openai/glm-4.5-air | 2/6 | 33% | $0.32 |
| 2026-08-16T06:48:17Z | openai/glm-4.5-air | 2/6 | 33% | $0.34 |
| 2026-08-15T06:47:46Z | openai/glm-4.5-air | 2/6 | 33% | $0.38 |
| 2026-08-09T07:04:09Z | openai/glm-4.5-air | 1/6 | 17% | $0.28 |
| 2026-08-03T09:51:36Z | openai/glm-4.5-air | 4/4 | 100% | $0.22 |
| 2026-08-02T08:35:01Z | openai/glm-4.5-air | 2/2 | 100% | $0.05 |
| 2026-08-02T08:12:29Z | anthropic/claude-haiku-4-5-20251001 | 0/2 | 0% | $0.00 |
