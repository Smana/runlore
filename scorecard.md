# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-09-11T10:42:36Z · model `openai/glm-4.5-air` · **3/6 scenarios reached (50%)** · n=5 runs/case, k-of-n bar 70% · est. cost $0.29 (1.1M in / 56.6k out tokens)

## Scenarios (latest run)

| scenario | result | pass-rate | median confidence | recall | notes |
|---|---|---|---|---|---|
| gitops-broken-kustomization | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| harbor-chart-bump | ⚠️ FLAKY | 40% (n=5) | 0.90 | — | harbor-db |
| node-eviction-no-commons | ❌ MISS | 20% (n=5) | 0.70 | fired 0/5 · short-circuit 0/5 (expect: rejected) | request |
| node-eviction-with-commons | ✅ PASS | 80% (n=5) | 0.90 | fired 0/5 · short-circuit 0/5 (expect: rejected) | request |
| poisoned-recall-rejected | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| poisoned-recall-verify | ❌ MISS | 20% (n=5) | 0.95 | fired 5/5 · short-circuit 0/5 (expect: withdrawn) | pull, v9.9.9 |

## Cost per investigation

Median provider-reported tokens per case on `openai/glm-4.5-air`, priced at $0.20/MTok in · $1.10/MTok out. Replay evidence, so tool latency and live-cluster variance are excluded.

| path | cases | median in tok | median out tok | est. cost |
|---|---|---|---|---|
| full investigation | 6 | 33.8k | 1.9k | $0.009 |

## Confidence calibration

- **Confidently wrong** (missed with median confidence ≥ 0.70): 3 — harbor-chart-bump, node-eviction-no-commons, poisoned-recall-verify
- **Underconfident** (reached with median confidence < 0.50): none

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
| 2026-09-11T10:42:36Z | openai/glm-4.5-air | 3/6 | 50% | $0.29 |
| 2026-09-10T10:41:10Z | openai/glm-4.5-air | 2/6 | 33% | $0.34 |
| 2026-09-09T11:04:57Z | openai/glm-4.5-air | 2/6 | 33% | $0.31 |
| 2026-09-08T10:47:50Z | openai/glm-4.5-air | 3/6 | 50% | $0.34 |
| 2026-09-07T11:40:44Z | openai/glm-4.5-air | 2/6 | 33% | $0.31 |
| 2026-09-06T10:30:32Z | openai/glm-4.5-air | 2/6 | 33% | $0.35 |
| 2026-09-05T10:11:28Z | openai/glm-4.5-air | 2/6 | 33% | $0.33 |
| 2026-09-04T10:23:29Z | openai/glm-4.5-air | — | ⚠️ errored | — |
| 2026-09-03T10:33:32Z | openai/glm-4.5-air | — | ⚠️ errored | — |
| 2026-09-02T10:25:45Z | openai/glm-4.5-air | — | ⚠️ errored | — |
| 2026-09-01T11:13:35Z | openai/glm-4.5-air | 2/6 | 33% | $0.35 |
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
