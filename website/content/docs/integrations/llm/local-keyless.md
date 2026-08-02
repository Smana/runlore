---
title: Local / keyless
weight: 204
integration: {kind: llm, id: local-keyless}
---

**What it gives you** — the investigation loop driven by an **in-cluster** vLLM or Ollama endpoint,
with no API key and no egress: incident text, tool output, and findings never leave your cluster
boundary.

## Minimal config

```yaml
model:
  base_url: http://vllm.llm.svc:8000/v1   # any OpenAI-compatible endpoint, in-cluster
  model: <your-model-name>
  # api_key_env omitted — keyless
```

Ollama, same shape:

```yaml
model:
  base_url: http://ollama.llm.svc:11434/v1
  model: <your-model-name>
```

## Verify it locally

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) against
the in-cluster endpoint and confirm an investigation completes with a verdict:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'msg=findings|msg=incident'
```

## Notes

- `provider: openai` is the default — this is the same wire protocol as
  [OpenAI-compatible]({{< relref "openai-compatible.md" >}}), just pointed at a keyless in-cluster
  endpoint instead of a keyed public one. **The forced-`tool_choice` requirement is identical** — read
  that page before wiring a model here: an endpoint that only supports unforced tool calling breaks
  the verify pass, the recall reranker, the eval judge, semantic KB validation and `kb import
  --model`.
- Omitting `api_key_env` (or leaving it empty) is what makes the request keyless — there is no
  separate "keyless: true" flag.
- The cleartext-key-over-`http://` check only fires when `api_key_env` is set — with it omitted,
  plain `http://` to an in-cluster Service name is fine and expected.
- **This is the recommended posture for a public KB repo or untrusted-tenant namespaces**: self-hosting
  the model keeps redacted-but-imperfect data in-boundary regardless of what the redaction ruleset
  misses. See [Security model → secret redaction]({{< relref
  "/docs/security/security-model.md#secret-redaction-at-the-llm-and-egress-boundaries" >}}).
- `model.embeddings` (also OpenAI-compatible) can point at the same or a different in-cluster endpoint
  for hybrid catalog recall — keyless the same way.
- Not every local model supports the forced-`tool_choice` form reliably. Test the specific model
  against the paths listed above before depending on them, the same way glm-4.6 turned out to be
  broken against a hosted endpoint (see [OpenAI-compatible]({{< relref "openai-compatible.md" >}})).

## Reference

- [Configuration → `model`]({{< relref "/docs/configuration/configuration.md#model--the-llm-provider" >}})
  for the full key reference.
- [Security architecture]({{< relref "/docs/security/security-architecture.md" >}}) for the
  in-boundary posture this enables.
