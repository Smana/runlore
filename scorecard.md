# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-10-29T12:25:04Z · model `openai/glm-4.5-air` · **5/7 scenarios reached (71%)** · n=5 runs/case, k-of-n bar 70% · est. cost $0.40 (1.6M in / 70.7k out tokens)

## Scenarios (latest run)

| scenario | result | pass-rate | median confidence | recall | notes |
|---|---|---|---|---|---|
| gitops-broken-kustomization | ✅ PASS | 80% (n=5) | 0.90 | — | — |
| harbor-chart-bump | ✅ PASS | 80% (n=5) | 0.90 | — | harbor-db |
| hpa-ceiling-saturation | ❌ MISS | 20% (n=5) | 0.90 | — | autoscal\|hpa, over-claimed: quote-engine |
| node-eviction-no-commons | ⚠️ FLAKY | 60% (n=5) | 0.60 | fired 0/5 · short-circuit 0/5 (expect: rejected) | request |
| node-eviction-with-commons | ✅ PASS | 80% (n=5) | 0.70 | fired 0/5 · short-circuit 0/5 (expect: rejected) | request |
| poisoned-recall-rejected | ✅ PASS | 80% (n=5) | 0.90 | — | investigation error: model: stream ended before finish_reason or [DONE] (truncated upstream) |
| poisoned-recall-verify | ✅ PASS | 100% (n=5) | 1.00 | fired 5/5 · short-circuit 0/5 (expect: withdrawn) | — |

## Cost per investigation

Median provider-reported tokens per case on `openai/glm-4.5-air`, priced at $0.20/MTok in · $1.10/MTok out. Replay evidence, so tool latency and live-cluster variance are excluded.

| path | cases | median in tok | median out tok | est. cost |
|---|---|---|---|---|
| full investigation | 7 | 33.4k | 1.8k | $0.009 |

## Confidence calibration

- **Confidently wrong** (missed with median confidence ≥ 0.70): 1 — hpa-ceiling-saturation
- **Underconfident** (reached with median confidence < 0.50): none

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
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
| 2026-10-19T12:13:00Z | openai/glm-4.5-air | 5/7 | 71% | $0.46 |
| 2026-10-18T10:31:08Z | openai/glm-4.5-air | 4/7 | 57% | $0.39 |
| 2026-10-17T10:28:12Z | openai/glm-4.5-air | 5/7 | 71% | $0.42 |
| 2026-10-16T10:47:37Z | openai/glm-4.5-air | 3/7 | 43% | $0.36 |
| 2026-10-15T11:02:03Z | openai/glm-4.5-air | 5/7 | 71% | $0.37 |
| 2026-10-14T11:17:28Z | openai/glm-4.5-air | 4/7 | 57% | $0.39 |
| 2026-10-13T13:46:45Z | openai/glm-4.5-air | 4/7 | 57% | $0.38 |
| 2026-10-12T14:27:29Z | openai/glm-4.5-air | 5/7 | 71% | $0.36 |
| 2026-10-11T12:16:57Z | openai/glm-4.5-air | 4/7 | 57% | $0.36 |
| 2026-10-10T12:00:58Z | openai/glm-4.5-air | 3/7 | 43% | $0.40 |
| 2026-10-09T12:47:26Z | openai/glm-4.5-air | 4/7 | 57% | $0.35 |
| 2026-10-08T12:58:20Z | openai/glm-4.5-air | 4/7 | 57% | $0.38 |
| 2026-10-07T12:48:29Z | openai/glm-4.5-air | 3/7 | 43% | $0.43 |
| 2026-10-06T12:55:42Z | openai/glm-4.5-air | 2/7 | 29% | $0.42 |
| 2026-10-05T13:41:17Z | openai/glm-4.5-air | 5/7 | 71% | $0.36 |
| 2026-10-04T12:01:11Z | openai/glm-4.5-air | 2/7 | 29% | $0.40 |
| 2026-10-03T11:19:05Z | openai/glm-4.5-air | 5/7 | 71% | $0.37 |
| 2026-10-02T12:06:28Z | openai/glm-4.5-air | 4/7 | 57% | $0.40 |
| 2026-10-01T12:34:12Z | openai/glm-4.5-air | 4/7 | 57% | $0.38 |
| 2026-09-30T12:09:09Z | openai/glm-4.5-air | 4/7 | 57% | $0.42 |
