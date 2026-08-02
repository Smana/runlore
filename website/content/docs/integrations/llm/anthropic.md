---
title: Anthropic
weight: 202
integration: {kind: llm, id: anthropic}
---

**What it gives you** — the investigation loop driven by native Claude, over the Messages API rather
than an OpenAI-compatible shim — keyed against `api.anthropic.com` or a compatible in-cluster gateway.

## Minimal config

```yaml
model:
  provider: anthropic
  model: claude-sonnet-5
  api_key_env: ANTHROPIC_API_KEY   # base_url defaults to https://api.anthropic.com
```

Pointed at an in-cluster gateway instead of the public API:

```yaml
model:
  provider: anthropic
  base_url: http://claude-gateway.llm.svc:8080
  model: claude-sonnet-5
  # api_key_env omitted — keyless behind the gateway
```

## Verify it locally

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=ANTHROPIC_API_KEY='<key>'
```

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) and
confirm an investigation completes with a verdict:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'msg=findings|msg=incident'
```

## Notes

- **`base_url` is optional** — unset, it defaults to `https://api.anthropic.com`; set it to point at
  a compatible in-cluster gateway instead.
- Requests carry `anthropic-version: 2023-06-01`.
- Every forced-tool path RunLore relies on (the adversarial verify pass, the recall reranker, the eval
  judge, semantic KB validation, `kb import --model`) sends Anthropic's native forced-`tool_choice`
  form (`{"type":"tool","name":"…"}`) — this is the reference-quality provider for those paths; see
  [OpenAI-compatible]({{< relref "openai-compatible.md" >}}) for what happens on an endpoint that
  doesn't honour it correctly.
- **`effort`** (`low`/`medium`/`high`/`max`, sent as `output_config.effort`) and **`thinking:
  adaptive`** (sent as `thinking: {type: "adaptive"}`) are both supported, Anthropic-only. The client
  replays the model's *signed* thinking blocks verbatim across the tool loop — give `max_tokens`
  headroom when `thinking: adaptive` is on, since thinking consumes output tokens. On the loop's
  forced final-submission step, and for the always-forced verify pass, the client drops `thinking`
  and strips replayed thinking blocks for that one request (a forced `tool_choice` is incompatible
  with adaptive thinking in the API).
- `model.verify` (a separate, optionally cheaper model for the adversarial verify pass) and
  `model.embeddings` (OpenAI-compatible `/embeddings` endpoint for hybrid recall — Anthropic has no
  embeddings API of its own) both work alongside this provider.
- A cleartext API key is rejected on a public `http://` host — use `https://` or a loopback/in-cluster
  address.

## Reference

[Configuration → `model`]({{< relref "/docs/configuration/configuration.md#model--the-llm-provider" >}})
for the full key reference.
