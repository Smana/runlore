# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml) — the replay eval scores the model+loop over recorded incident evidence (no live cluster), so anyone can reproduce it:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-08-02T08:35:01Z · model `openai/glm-4.5-air` · **2/2 scenarios reached (100%)** · n=5 runs/case, k-of-n bar 70% · est. cost $0.05 (148.9k in / 15.9k out tokens)

## Scenarios (latest run)

| scenario | result | pass-rate | median confidence | recall | notes |
|---|---|---|---|---|---|
| harbor-chart-bump | ✅ PASS | 80% (n=5) | 0.90 | — | chart |
| poisoned-recall-verify | ✅ PASS | 100% (n=5) | 0.90 | fired 5/5 · short-circuit 0/5 (expect: withdrawn) | — |

## Confidence calibration

- **Confidently wrong** (missed with median confidence ≥ 0.70): none
- **Underconfident** (reached with median confidence < 0.50): none

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](history.jsonl). Runs below the CI gate publish here exactly like green ones.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
| 2026-08-02T08:35:01Z | openai/glm-4.5-air | 2/2 | 100% | $0.05 |
| 2026-08-02T08:12:29Z | anthropic/claude-haiku-4-5-20251001 | 0/2 | 0% | $0.00 |
