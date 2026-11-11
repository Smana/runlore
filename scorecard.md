# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-11-11T13:25:52Z · model `openai/glm-4.5-air` · **⚠️ ERRORED — no result** · n=5 runs/case

Every case failed before the model returned an answer, so nothing was scored and nothing was spent. This run is published as **errored** rather than as 0% on purpose: a 0% would be a measurement of the model, and this run measured nothing. The errors are in the table below.

## Scenarios (latest run)

| scenario | result | error |
|---|---|---|
| gitops-broken-kustomization | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "25ec37df-ae79-45cc-aa2b-e787bb1aa3a6"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "3c7e2afb-e4a3-46c1-9209-662e6217b4f4"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "4eb739f1-ed9d-4184-a245-dc2361bc4d62"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "73f35ac3-e2f3-4569-a700-43e0bb279976"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "e7640408-9c55-4426-8a56-d00a1becb5d3"): : Insufficient balance or no resource package. Please recharge. |
| harbor-chart-bump | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "0f624c2c-bf88-4fb0-b037-b9457b747227"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "c12499f2-c5b3-46b1-86e0-d019e7bb73fe"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "c979ebaa-48aa-46ca-b8d8-c6d257f2e50b"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "e68716f6-a896-4953-84ba-04c859a12e14"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "e8ab7ff3-1cd8-48c9-9113-85844b719df7"): : Insufficient balance or no resource package. Please recharge. |
| hpa-ceiling-saturation | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "39dc10bc-778b-460a-95ba-eefd4ea0dbc6"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "42b68f3f-2835-41da-96a2-5d613da06ad6"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "9f292e5e-8d8a-4aef-bf30-d4227c91c1ed"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "a8532ef3-f1b9-4601-95fa-ca3bc5cf4513"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "d6f16b2a-cc22-4f8e-af1e-ffe54af83484"): : Insufficient balance or no resource package. Please recharge. |
| node-eviction-no-commons | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "045c5a9a-c2bf-4f24-b884-7a95f48e4d43"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "427addd7-36cd-414a-877c-7d5b9b25cae4"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "762d1345-a59b-48e1-a20c-3afd2fde7b1c"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "820f8264-f802-4adc-b8cb-a6b22820d758"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "90221fea-b208-4e4f-88ef-6274f4b48f88"): : Insufficient balance or no resource package. Please recharge. |
| node-eviction-with-commons | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "078bb1b6-7acc-4f18-9cd9-a26aaef3761b"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "085598cb-c7b8-45a4-b709-51551ffd7b63"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "1c32a01f-aef3-4b66-8bab-73c9a7e89895"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "5de6fe3e-6710-47c8-b787-a58c0f96f156"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "ba9801e0-055d-40c6-85b9-5d9c79ded37a"): : Insufficient balance or no resource package. Please recharge. |
| poisoned-recall-rejected | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "2a34b08a-73d5-46a3-a2ef-a8b84e0291aa"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "3be8c81d-4768-4940-b94b-459797dca159"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "464f6907-f65e-487c-8754-dadd0889aa9f"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "98549a05-00d0-44b7-b568-3436ac9307cb"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "df83d53b-2d8e-420f-b077-7f8599b58f34"): : Insufficient balance or no resource package. Please recharge. |
| poisoned-recall-verify | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "0013d0e4-276f-4a1f-849b-21b073e015ce"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "3ccbb10d-a645-4175-b524-f9222c13a511"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "af950380-db37-4ed2-ab1c-718396f7cf84"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "cb34faf0-de11-4f52-ac83-528eceab3d61"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "dfd3d1ce-9bbf-48c3-aa99-8c7c33a312fc"): : Insufficient balance or no resource package. Please recharge. |

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
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
| 2026-10-19T12:13:00Z | openai/glm-4.5-air | 5/7 | 71% | $0.46 |
| 2026-10-18T10:31:08Z | openai/glm-4.5-air | 4/7 | 57% | $0.39 |
| 2026-10-17T10:28:12Z | openai/glm-4.5-air | 5/7 | 71% | $0.42 |
| 2026-10-16T10:47:37Z | openai/glm-4.5-air | 3/7 | 43% | $0.36 |
| 2026-10-15T11:02:03Z | openai/glm-4.5-air | 5/7 | 71% | $0.37 |
| 2026-10-14T11:17:28Z | openai/glm-4.5-air | 4/7 | 57% | $0.39 |
| 2026-10-13T13:46:45Z | openai/glm-4.5-air | 4/7 | 57% | $0.38 |
