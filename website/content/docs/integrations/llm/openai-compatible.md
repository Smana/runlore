---
title: OpenAI-compatible
weight: 201
integration: {kind: llm, id: openai-compatible}
---

**What it gives you** — the investigation loop, driven by any endpoint that speaks the OpenAI
`/chat/completions` wire format: OpenAI itself, OpenRouter, or a self-hosted vLLM/Ollama endpoint
kept entirely in-cluster.

## The endpoint must support forced `tool_choice`

RunLore is not just a chat client — five paths **force** the model to answer through one named tool
rather than free text, using the OpenAI `tool_choice: {"type":"function","function":{"name":"…"}}`
form:

- the adversarial **verify pass** (`internal/investigate/verify.go`)
- instant recall's **LLM reranker** (`internal/investigate/rerank.go`)
- the **eval judge** (`internal/eval/judge.go`)
- **semantic KB validation** (`internal/kbvalidate/semantic.go`)
- `lore kb import --model` (`internal/kbimport/enrich.go`)

An endpoint that only honours the *unforced* form (the model may call a tool, or may not) will start
fine and even complete ordinary investigations — then hang or error the moment one of the paths above
runs. **Before wiring a new endpoint, confirm it supports the named-function forced form**, not just
tool calling in general.

### Known-broken: glm-4.6 on api.z.ai

**glm-4.6**, served from `https://api.z.ai/api/paas/v4`, does **not** work with RunLore. Given a
forced `tool_choice`, it emits a single `reasoning_content` delta and then **stalls indefinitely** —
no tool call, no `finish_reason`, no `[DONE]`. Ordinary (unforced) tool calls work fine on the same
endpoint; only the forced form hangs. Tracked as
[issue #391](https://github.com/Smana/runlore/issues/391).

**glm-4.5-air**, same endpoint, same request shape, is **verified working** — the forced call
returns in ~5s. If you're on z.ai, use glm-4.5-air, not glm-4.6.

Do not take "it answered my first test message" as confirmation an endpoint works — test the forced
path (verify pass, reranker, or `kb import --model`) specifically before relying on it.

## Minimal config

```yaml
model:
  base_url: https://api.z.ai/api/paas/v4    # any OpenAI-compatible endpoint
  model: glm-4.5-air                        # verified: supports forced tool_choice
  api_key_env: ZAI_API_KEY
```

In-cluster and keyless instead (vLLM/Ollama) — see [Local / keyless]({{< relref "local-keyless.md" >}}).

## Verify it locally

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=ZAI_API_KEY='<key>'
```

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) and
confirm an investigation completes with a verdict — not a hang:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'msg=findings|msg=incident'
```

## Notes

- `provider: openai` is the default — omit it, or set it explicitly for clarity.
- `base_url` is **required** for this provider (unlike Anthropic/Gemini, there is no built-in
  default endpoint — any OpenAI-compatible host is valid).
- `api_key_env` names an env var; leave it unset for a keyless in-cluster endpoint. A cleartext key
  is rejected on a public `http://` host — use `https://` or a loopback/in-cluster address.
- `model.effort` (`minimal`/`low`/`medium`/`high`, sent as `reasoning_effort`) is supported on this
  provider; a model that doesn't understand the knob returns a 400 (classified permanent, not
  retried).
- A **different, milder** failure mode: some OpenAI-compatible local servers (vLLM/Ollama) accept a
  forced `tool_choice` but still answer in prose. The main investigation loop tolerates that — one
  nudge, then a synthetic inconclusive result rather than hanging — but this fallback only covers the
  loop's own final-step submission. It does **not** apply to verify, rerank, judge, semantic
  validation or `kb import`, and it does not help against a genuine stream stall like glm-4.6's
  (`investigation.timeout`, default 10m, is what eventually ends that kind of hang).

## Reference

- [Configuration → `model`]({{< relref "/docs/configuration/configuration.md#model--the-llm-provider" >}})
  for the full key reference.
- [Anthropic]({{< relref "anthropic.md" >}}) and [Gemini]({{< relref "gemini.md" >}}) for the other
  two providers.
