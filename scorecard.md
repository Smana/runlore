# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-09-24T11:18:05Z · model `openai/glm-4.5-air` · **5/7 scenarios reached (71%)** · n=5 runs/case, k-of-n bar 70% · est. cost $0.34 (1.4M in / 61.9k out tokens)

## Scenarios (latest run)

| scenario | result | pass-rate | median confidence | recall | notes |
|---|---|---|---|---|---|
| gitops-broken-kustomization | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| harbor-chart-bump | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| hpa-ceiling-saturation | ⚠️ FLAKY | 40% (n=5) | 0.90 | — | autoscal, pricing-api |
| node-eviction-no-commons | ⚠️ FLAKY | 60% (n=5) | 0.60 | fired 0/5 · short-circuit 0/5 (expect: rejected) | investigation error: model: stream ended before finish_reason or [DONE] (truncated upstream), request |
| node-eviction-with-commons | ✅ PASS | 80% (n=5) | 0.90 | fired 0/5 · short-circuit 0/5 (expect: rejected) | request |
| poisoned-recall-rejected | ✅ PASS | 80% (n=5) | 0.90 | — | pull |
| poisoned-recall-verify | ✅ PASS | 100% (n=5) | 1.00 | fired 5/5 · short-circuit 0/5 (expect: withdrawn) | — |

## Cost per investigation

Median provider-reported tokens per case on `openai/glm-4.5-air`, priced at $0.20/MTok in · $1.10/MTok out. Replay evidence, so tool latency and live-cluster variance are excluded.

| path | cases | median in tok | median out tok | est. cost |
|---|---|---|---|---|
| full investigation | 7 | 36.2k | 1.5k | $0.009 |

## Confidence calibration

- **Confidently wrong** (missed with median confidence ≥ 0.70): 1 — hpa-ceiling-saturation
- **Underconfident** (reached with median confidence < 0.50): none

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
| 2026-09-24T11:18:05Z | openai/glm-4.5-air | 5/7 | 71% | $0.34 |
| 2026-09-23T10:54:28Z | openai/glm-4.5-air | 5/6 | 83% | $0.31 |
| 2026-09-22T11:06:41Z | openai/glm-4.5-air | 2/6 | 33% | $0.35 |
| 2026-09-21T12:07:05Z | openai/glm-4.5-air | 2/6 | 33% | $0.30 |
| 2026-09-20T10:43:18Z | openai/glm-4.5-air | 2/6 | 33% | $0.31 |
| 2026-09-19T10:26:39Z | openai/glm-4.5-air | 2/6 | 33% | $0.31 |
| 2026-09-18T10:40:04Z | openai/glm-4.5-air | 1/6 | 17% | $0.30 |
| 2026-09-17T11:01:41Z | openai/glm-4.5-air | 2/6 | 33% | $0.26 |
| 2026-09-16T11:00:02Z | openai/glm-4.5-air | 2/6 | 33% | $0.31 |
| 2026-09-15T11:08:25Z | openai/glm-4.5-air | 2/6 | 33% | $0.32 |
| 2026-09-14T11:49:15Z | openai/glm-4.5-air | 3/6 | 50% | $0.27 |
| 2026-09-13T11:13:48Z | openai/glm-4.5-air | 1/6 | 17% | $0.30 |
| 2026-09-12T10:11:24Z | openai/glm-4.5-air | 2/6 | 33% | $0.32 |
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
