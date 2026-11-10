# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-11-10T14:09:29Z · model `openai/glm-4.5-air` · **⚠️ ERRORED — no result** · n=5 runs/case

Every case failed before the model returned an answer, so nothing was scored and nothing was spent. This run is published as **errored** rather than as 0% on purpose: a 0% would be a measurement of the model, and this run measured nothing. The errors are in the table below.

## Scenarios (latest run)

| scenario | result | error |
|---|---|---|
| gitops-broken-kustomization | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "3cebb16d-54a8-401d-b06d-7580dade6c4b"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "a24ab95c-aa99-4dd4-9dd5-db76f4c421ca"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "adec46b1-915a-4e99-bf80-d60e235148d2"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "c188217c-2279-437d-bf41-df12b2e6c63f"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "d2b527a9-3e99-471f-be86-66d393b0fda5"): : Insufficient balance or no resource package. Please recharge. |
| harbor-chart-bump | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "011024a6-2024-4956-9462-8b8208c29a31"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "4d46951b-389e-4a47-a0cf-5c94274c5c74"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "9a939b51-91cb-4959-9d84-ba06f85734b3"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "c8cd7e35-5934-4af8-863d-24f43f07652c"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "db28684d-ca92-484d-92fc-d97432697832"): : Insufficient balance or no resource package. Please recharge. |
| hpa-ceiling-saturation | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "02b028b1-dabd-453c-b289-0690bcf5e078"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "039aa05f-e188-4e27-a694-50895d3a703c"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "bb164908-025f-4dcc-ac2b-2e2891118973"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "f2ef6330-90b6-4483-947d-9049f17a6ba6"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "fcb34490-d9db-4925-a184-880003cd2487"): : Insufficient balance or no resource package. Please recharge. |
| node-eviction-no-commons | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "0ade5a35-9eb0-4227-b6c2-e8b7c048b48e"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "309139d9-deea-4d31-b033-f0f42f3eb4ec"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "4fc7185b-9369-4bb2-8b47-eca7077ef206"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "5ae88378-4197-4705-b4e0-d78ff36a4d95"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "be26fea3-4f75-45da-a127-26f9ba4b3d22"): : Insufficient balance or no resource package. Please recharge. |
| node-eviction-with-commons | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "0d85f4bd-3b31-4fbf-96f1-2c3c1a5cc127"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "6b7602ad-0833-47c5-8786-64f29c997b88"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "73ee9fb2-f91a-442e-bc61-cfd756ee793d"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "8580410a-8c19-467e-b613-69b3a18f2e1b"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "8820eaea-b764-4c99-a074-401bb85d5137"): : Insufficient balance or no resource package. Please recharge. |
| poisoned-recall-rejected | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "0552ced1-dfe4-43b1-b161-b48c96e07d73"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "40fce677-badc-4e4d-a3cc-1d4258c8c771"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "6f330336-80bb-4ae5-97dd-d7df44570ea0"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "9dcdc919-5c12-4948-a985-f8e98167bcb2"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "c77fc2d5-c782-4ec2-a2c0-44174a0653fb"): : Insufficient balance or no resource package. Please recharge. |
| poisoned-recall-verify | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "13d4244d-1fb6-495e-a110-683a8b4807b1"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "226a1923-4b88-4880-a900-bde7fb09a6ca"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "820c6f3e-754f-48cf-9e33-8eb86ae43bc6"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "92e7820b-f0b5-49a5-81b1-16aea4c453c7"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "db9d4567-d23e-4e0e-a21e-02d2ac0118ca"): : Insufficient balance or no resource package. Please recharge. |

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
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
| 2026-10-12T14:27:29Z | openai/glm-4.5-air | 5/7 | 71% | $0.36 |
