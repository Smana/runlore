# RunLore nightly eval scorecard

Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml). Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

**Latest run:** 2026-11-13T13:52:12Z · model `openai/glm-4.5-air` · **⚠️ ERRORED — no result** · n=5 runs/case

Every case failed before the model returned an answer, so nothing was scored and nothing was spent. This run is published as **errored** rather than as 0% on purpose: a 0% would be a measurement of the model, and this run measured nothing. The errors are in the table below.

## Scenarios (latest run)

| scenario | result | error |
|---|---|---|
| gitops-broken-kustomization | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "2d9a1d4b-ebfb-4554-8b7d-594ef2a876c7"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "3e50a151-8a91-4d8f-9d5b-93b53daa794a"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "5067f4c5-bc5a-45d4-baf1-d7d731cf2044"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "7b0b6819-7257-408d-b067-9695c8e8f007"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "fd01c0cd-a112-49b5-86e4-3d1911dec7b7"): : Insufficient balance or no resource package. Please recharge. |
| harbor-chart-bump | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "3dbcca34-7d6b-4d19-913c-e428941d9a8f"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "cbf5adfc-92a0-4db3-b484-4c1977d11a98"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "cfbf1373-c23b-4286-9b11-72ff752761f2"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "dffc7349-408d-4268-aa94-d0b928534d1f"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "f9a8579e-058d-4880-833a-932aa12d3fdf"): : Insufficient balance or no resource package. Please recharge. |
| hpa-ceiling-saturation | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "00c8e3d8-8b79-478f-aad1-f0c8c90c6ccf"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "05993833-293f-4cc3-80e0-84d883c8e45e"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "631074b3-643b-45b7-ad04-992ef1af182c"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "e8040740-664d-4c7e-b975-8db659b4545f"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "eafe5667-f108-4a42-9b37-e371f60e7490"): : Insufficient balance or no resource package. Please recharge. |
| node-eviction-no-commons | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "5ad400de-0867-4148-9a94-86433dc08533"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "a0550c58-eeab-4c92-bc0f-1de7aaa84761"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "ec021984-67b0-49e6-8a1b-39b9d26c1bd8"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "f1a596d4-bce8-4181-90de-84d8265b9a5d"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "fb09fa63-dd31-49f4-9f32-7cb5c22031e7"): : Insufficient balance or no resource package. Please recharge. |
| node-eviction-with-commons | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "978f9b10-cc12-4486-a5ec-1a4d7bd3e069"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "9d89e3d5-2d9c-409e-aa7c-41bf8eb70e79"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "a263f570-a644-47a1-be66-961695e7c92a"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "dfabbd28-fec5-4d1a-9ae0-c9c8cc115668"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "ed7346ff-0786-4931-aa14-e293954a42b4"): : Insufficient balance or no resource package. Please recharge. |
| poisoned-recall-rejected | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "6b17183c-b9cc-452d-a61f-2a42f770101e"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "8f18fbaa-cc29-49bc-85ea-b9fe5c25375a"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "9272ee9b-a95c-499f-bd1e-d9e2fe6a6048"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "b97d7b85-bbd8-4bca-b52a-86b7177f5c58"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "badd3e8c-5b32-4419-b23e-16477dd157f9"): : Insufficient balance or no resource package. Please recharge. |
| poisoned-recall-verify | 🚫 ERRORED | investigation error: model: chat status 429 (request-id "32fcf75f-982b-40d2-b464-e37e7bde6a4e"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "6ab1de0c-5dc7-4a4a-853e-564e2b9fcd91"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "83e4f009-7582-438e-93b6-86aa9d19ba52"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "9bb5b57a-990f-45c1-a8b6-e56eaff9effb"): : Insufficient balance or no resource package. Please recharge., investigation error: model: chat status 429 (request-id "f3c20417-ba82-4171-ae31-d5246db25b93"): : Insufficient balance or no resource package. Please recharge. |

## History

Newest first, last 30 shown — the full log is [`history.jsonl`](https://github.com/Smana/runlore/blob/eval-scorecard/history.jsonl). A run that reached no answer to score at all is labelled in the pass-rate column instead of scored — it is not a 0%.

| date | model | reached | pass-rate | est. cost |
|---|---|---|---|---|
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
| 2026-10-19T12:13:00Z | openai/glm-4.5-air | 5/7 | 71% | $0.46 |
| 2026-10-18T10:31:08Z | openai/glm-4.5-air | 4/7 | 57% | $0.39 |
| 2026-10-17T10:28:12Z | openai/glm-4.5-air | 5/7 | 71% | $0.42 |
| 2026-10-16T10:47:37Z | openai/glm-4.5-air | 3/7 | 43% | $0.36 |
| 2026-10-15T11:02:03Z | openai/glm-4.5-air | 5/7 | 71% | $0.37 |
