# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-11-18T15:11:54Z · model `openai/glm-4.5-air` · **3/7 scenarios reached (43%)** · n=5 runs/case, k-of-n bar 70% · est. cost $0.39 (1.6M in / 62.0k out tokens)

## Scenarios (latest run)

| scenario | result | pass-rate | median confidence | recall | notes |
|---|---|---|---|---|---|
| gitops-broken-kustomization | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| harbor-chart-bump | ⚠️ FLAKY | 60% (n=5) | 0.80 | — | harbor-db |
| hpa-ceiling-saturation | ⚠️ FLAKY | 40% (n=5) | 0.90 | — | over-claimed: quote-engine |
| node-eviction-no-commons | ❌ MISS | 20% (n=5) | 0.60 | fired 0/5 · short-circuit 0/5 (expect: rejected) | report-worker, request |
| node-eviction-with-commons | ⚠️ FLAKY | 60% (n=5) | 0.80 | fired 0/5 · short-circuit 0/5 (expect: rejected) | report-worker, request |
| poisoned-recall-rejected | ✅ PASS | 100% (n=5) | 0.90 | — | — |
| poisoned-recall-verify | ✅ PASS | 100% (n=5) | 1.00 | fired 5/5 · short-circuit 0/5 (expect: withdrawn) | — |

## Cost per investigation

Median provider-reported tokens per case on `openai/glm-4.5-air`, priced at $0.20/MTok in · $1.10/MTok out. Replay evidence, so tool latency and live-cluster variance are excluded.

| path | cases | median in tok | median out tok | est. cost |
|---|---|---|---|---|
| full investigation | 7 | 32.8k | 1.4k | $0.008 |

## Confidence calibration

- **Confidently wrong** (missed with median confidence ≥ 0.70): 3 — harbor-chart-bump, hpa-ceiling-saturation, node-eviction-with-commons
- **Underconfident** (reached with median confidence < 0.50): none

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
| 2026-11-18T15:11:54Z | openai/glm-4.5-air | 3/7 | 43% | $0.39 |
| 2026-11-17T15:39:15Z | openai/glm-4.5-air | 5/7 | 71% | $0.40 |
| 2026-11-16T14:47:38Z | openai/glm-4.5-air | 5/7 | 71% | $0.41 |
| 2026-11-15T13:35:05Z | openai/glm-4.5-air | 4/7 | 57% | $0.43 |
| 2026-11-14T13:07:49Z | openai/glm-4.5-air | 5/7 | 71% | $0.48 |
| 2026-11-13T13:52:12Z | openai/glm-4.5-air | — | ⚠️ errored | — |
| 2026-11-12T14:30:26Z | openai/glm-4.5-air | — | ⚠️ errored | — |
| 2026-11-11T13:25:52Z | openai/glm-4.5-air | — | ⚠️ errored | — |
| 2026-11-10T14:09:29Z | openai/glm-4.5-air | — | ⚠️ errored | — |
| 2026-11-09T15:10:51Z | openai/glm-4.5-air | 4/7 | 57% | $0.44 |
| 2026-11-08T12:51:47Z | openai/glm-4.5-air | 5/7 | 71% | $0.41 |
| 2026-11-07T12:30:22Z | openai/glm-4.5-air | 3/7 | 43% | $0.38 |
| 2026-11-06T13:51:14Z | openai/glm-4.5-air | 5/7 | 71% | $0.41 |
| 2026-11-05T14:09:39Z | openai/glm-4.5-air | 6/7 | 86% | $0.44 |
| 2026-11-04T13:55:37Z | openai/glm-4.5-air | 3/7 | 43% | $0.39 |
| 2026-11-03T13:46:22Z | openai/glm-4.5-air | 6/7 | 86% | $0.41 |
| 2026-11-02T14:45:51Z | openai/glm-4.5-air | 5/7 | 71% | $0.37 |
| 2026-11-01T12:47:13Z | openai/glm-4.5-air | 5/7 | 71% | $0.38 |
| 2026-10-31T11:48:14Z | openai/glm-4.5-air | 5/7 | 71% | $0.44 |
| 2026-10-30T12:15:29Z | openai/glm-4.5-air | 4/7 | 57% | $0.42 |
| 2026-10-29T12:25:04Z | openai/glm-4.5-air | 5/7 | 71% | $0.40 |
| 2026-10-28T12:12:14Z | openai/glm-4.5-air | 4/7 | 57% | $0.37 |
| 2026-10-27T11:43:35Z | openai/glm-4.5-air | 4/7 | 57% | $0.41 |
| 2026-10-26T12:24:26Z | openai/glm-4.5-air | 5/7 | 71% | $0.37 |
| 2026-10-25T10:51:12Z | openai/glm-4.5-air | 4/7 | 57% | $0.39 |
| 2026-10-24T10:25:11Z | openai/glm-4.5-air | 5/7 | 71% | $0.38 |
| 2026-10-23T11:12:34Z | openai/glm-4.5-air | 4/7 | 57% | $0.39 |
| 2026-10-22T11:16:19Z | openai/glm-4.5-air | 5/7 | 71% | $0.34 |
| 2026-10-21T11:17:31Z | openai/glm-4.5-air | 4/7 | 57% | $0.37 |
| 2026-10-20T11:07:32Z | openai/glm-4.5-air | 5/7 | 71% | $0.38 |
