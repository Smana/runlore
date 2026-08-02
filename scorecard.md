# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml) — the replay eval scores the model+loop over recorded incident evidence (no live cluster), so anyone can reproduce it:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-08-02T08:12:29Z · model `anthropic/claude-haiku-4-5-20251001` · **0/2 scenarios reached (0%)** · n=5 runs/case, k-of-n bar 70% · est. cost $0.00 (0 in / 0 out tokens)

## Scenarios (latest run)

| scenario | result | pass-rate | median confidence | recall | notes |
|---|---|---|---|---|---|
| harbor-chart-bump | ❌ MISS | 0% (n=5) | 0.00 | — | investigation error: model: messages status 401 (request-id "req_011CddauL2bPuaDpR5ukLH7S"): authentication_error: invalid x-api-key, investigation error: model: messages status 401 (request-id "req_011CddauLSQPkbYcW4hDeJXN"): authentication_error: invalid x-api-key, investigation error: model: messages status 401 (request-id "req_011CddauLuBg3xNFeCXoxMJh"): authentication_error: invalid x-api-key, investigation error: model: messages status 401 (request-id "req_011CddauMXdjBT5vowUgwCyc"): authentication_error: invalid x-api-key, investigation error: model: messages status 401 (request-id "req_011CddauMxBdWUqxfpgQ4Xrw"): authentication_error: invalid x-api-key |
| poisoned-recall-verify | ❌ MISS | 0% (n=5) | 0.90 | fired 5/5 · short-circuit 5/5 (expect: withdrawn) | expect_recall=withdrawn but recall short_circuit |

## Confidence calibration

- **Confidently wrong** (missed with median confidence ≥ 0.70): 1 — poisoned-recall-verify
- **Underconfident** (reached with median confidence < 0.50): none

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](history.jsonl). Runs below the CI gate publish here exactly like green ones.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
| 2026-08-02T08:12:29Z | anthropic/claude-haiku-4-5-20251001 | 0/2 | 0% | $0.00 |
