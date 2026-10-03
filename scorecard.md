# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-10-03T11:19:05Z · model `openai/glm-4.5-air` · **5/7 scenarios reached (71%)** · n=5 runs/case, k-of-n bar 70% · est. cost $0.37 (1.5M in / 65.6k out tokens)

## Scenarios (latest run)

| scenario | result | pass-rate | median confidence | recall | notes |
|---|---|---|---|---|---|
| gitops-broken-kustomization | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| harbor-chart-bump | ✅ PASS | 80% (n=5) | 0.90 | — | harbor-db |
| hpa-ceiling-saturation | ❌ MISS | 0% (n=5) | 0.90 | — | over-claimed: quote-engine |
| node-eviction-no-commons | ⚠️ FLAKY | 40% (n=5) | 0.70 | fired 0/5 · short-circuit 0/5 (expect: rejected) | report-worker, request |
| node-eviction-with-commons | ✅ PASS | 100% (n=5) | 0.85 | fired 0/5 · short-circuit 0/5 (expect: rejected) | — |
| poisoned-recall-rejected | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| poisoned-recall-verify | ✅ PASS | 80% (n=5) | 1.00 | fired 5/5 · short-circuit 0/5 (expect: withdrawn) | pull |

## Cost per investigation

Median provider-reported tokens per case on `openai/glm-4.5-air`, priced at $0.20/MTok in · $1.10/MTok out. Replay evidence, so tool latency and live-cluster variance are excluded.

| path | cases | median in tok | median out tok | est. cost |
|---|---|---|---|---|
| full investigation | 7 | 45.0k | 1.8k | $0.011 |

## Confidence calibration

- **Confidently wrong** (missed with median confidence ≥ 0.70): 2 — hpa-ceiling-saturation, node-eviction-no-commons
- **Underconfident** (reached with median confidence < 0.50): none

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
| 2026-10-03T11:19:05Z | openai/glm-4.5-air | 5/7 | 71% | $0.37 |
| 2026-10-02T12:06:28Z | openai/glm-4.5-air | 4/7 | 57% | $0.40 |
| 2026-10-01T12:34:12Z | openai/glm-4.5-air | 4/7 | 57% | $0.38 |
| 2026-09-30T12:09:09Z | openai/glm-4.5-air | 4/7 | 57% | $0.42 |
| 2026-09-29T12:16:38Z | openai/glm-4.5-air | 4/7 | 57% | $0.40 |
| 2026-09-28T13:00:09Z | openai/glm-4.5-air | 4/7 | 57% | $0.40 |
| 2026-09-27T11:35:19Z | openai/glm-4.5-air | 4/7 | 57% | $0.40 |
| 2026-09-26T11:00:52Z | openai/glm-4.5-air | 5/7 | 71% | $0.40 |
| 2026-09-25T11:17:59Z | openai/glm-4.5-air | 5/7 | 71% | $0.37 |
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
