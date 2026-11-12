# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-11-12T14:30:26Z · model `openai/glm-4.5-air` · **⚠️ ERRORED — no result** · n=5 runs/case

Every case failed before the model returned an answer, so nothing was scored and nothing was spent. This run is published as **errored** rather than as 0% on purpose: a 0% would be a measurement of the model, and this run measured nothing. The errors are in the table below.

## Scenarios (latest run)

| scenario | result | error |
|---|---|---|
| gitops-broken-kustomization | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "3a69292c-1cff-469a-b9f7-e7065f22a95c"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "49398de9-5299-4ee0-9b48-127afa07b914"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "86dfd194-6c12-4e2b-9168-a935fbab4a0f"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "ce6ebf73-9da9-45bf-afac-be843ccf8f35"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "ce9e0645-b4bd-4ced-ad74-4e3f29c99bae"): : Insufficient balance or no resource package. Please recharge. |
| harbor-chart-bump | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "085094a5-e46a-42df-b942-30286af5215a"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "2a49ebc8-3d92-4593-80f6-e4b4fecad982"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "3aac01f2-a6b3-4e40-9b86-cdb9d47e9405"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "41ca82b8-22c0-4e5a-9176-aa1e275a81de"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "d4773d8f-43df-4c21-b582-65c0a4a3a246"): : Insufficient balance or no resource package. Please recharge. |
| hpa-ceiling-saturation | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "06224c23-2289-4190-900f-5c1c8715d755"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "30e4f38a-8168-46b3-a205-fcbea0d52032"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "a3e460d8-6bb4-4f71-95a4-b605ab022c24"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "b1c74c33-e74e-45b9-bdd3-f331d0a9fd3c"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "e2faa1ce-8c40-46b6-8464-b9781c269b79"): : Insufficient balance or no resource package. Please recharge. |
| node-eviction-no-commons | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "3aeb6828-a49c-4e33-9578-5765a5db4315"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "53c554fe-9cf1-4386-8e49-4ad8e7645764"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "a61488a6-1f8e-4caa-8980-abab721a3d13"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "c7e6b108-36a0-483b-aeb4-40ff49874a61"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "e5778e55-e5b7-451d-921e-ca8414351479"): : Insufficient balance or no resource package. Please recharge. |
| node-eviction-with-commons | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "125386e8-1be2-44cb-9b15-ad21f93c5bcc"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "5c76d437-db14-4f7b-b508-0897a548a963"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "a5b5b5cf-b750-4d8b-b8aa-57f6b58181f3"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "d730b0a6-278f-4562-8104-918014377c68"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "d9f2e1fe-b8e2-4169-985d-0e91af4b0276"): : Insufficient balance or no resource package. Please recharge. |
| poisoned-recall-rejected | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "b140938f-035e-4ab3-89a2-11fcad2aa6d5"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "c6fbad5c-c0e0-4a58-a738-7537c91dd677"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "d46d82af-68bf-4675-9cc9-3d0cd5e41832"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "f1bead13-50a6-4f5b-9e15-92b7d1cd0bed"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "fb52c2ce-ca78-496f-9c09-2b294cb65484"): : Insufficient balance or no resource package. Please recharge. |
| poisoned-recall-verify | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "56332e75-13aa-45b0-accd-58a7e080e3de"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "7ee8036b-3506-4e8c-b252-0c0e63e63fda"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "978717b0-82ee-45b3-bc71-93400fdd5e07"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "d4845c56-3179-443b-9c83-6a71f179925a"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "dc5890d0-50b1-4a1b-8d26-c0e2f93bc21d"): : Insufficient balance or no resource package. Please recharge. |

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
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
| 2026-10-19T12:13:00Z | openai/glm-4.5-air | 5/7 | 71% | $0.46 |
| 2026-10-18T10:31:08Z | openai/glm-4.5-air | 4/7 | 57% | $0.39 |
| 2026-10-17T10:28:12Z | openai/glm-4.5-air | 5/7 | 71% | $0.42 |
| 2026-10-16T10:47:37Z | openai/glm-4.5-air | 3/7 | 43% | $0.36 |
| 2026-10-15T11:02:03Z | openai/glm-4.5-air | 5/7 | 71% | $0.37 |
| 2026-10-14T11:17:28Z | openai/glm-4.5-air | 4/7 | 57% | $0.39 |
