---
title: Gemini
weight: 203
integration: {kind: llm, id: gemini}
---

**What it gives you** — the investigation loop driven by Google's Gemini API (native
`streamGenerateContent` function calling, not an OpenAI-compatible shim), supporting **Gemini 4**
(`gemini-4-neon`, `gemini-4-carbon`), **Gemini 3**, and **Gemini 2.5**.

## Minimal config

```yaml
model:
  provider: gemini
  model: gemini-4-neon              # or gemini-4-carbon, gemini-2.5-flash-lite
  api_key_env: GEMINI_API_KEY       # base_url defaults to https://generativelanguage.googleapis.com
  # effort: high                    # optional: minimal|low|medium|high|xhigh (omit for model default)
```

## Reasoning depth (`model.effort`)

RunLore maps `model.effort` uppercase to `generationConfig.thinkingConfig.thinkingLevel`:

| Value | Wire `thinkingLevel` | Notes |
|---|---|---|
| *(empty)* | *(omitted)* | Preserves each model's native default (`high` on `gemini-4-neon`, `medium` on `gemini-4-carbon`) |
| `minimal` | `MINIMAL` | Gemini 3 Flash only — rejected with a permanent `400` by Gemini 4 |
| `low` | `LOW` | Faster, lower thinking-token spend |
| `medium` | `MEDIUM` | Balanced reasoning depth |
| `high` | `HIGH` | Deep reasoning |
| `xhigh` | `XHIGH` | Maximum reasoning depth on Gemini 4 |

`model.verify.effort` and `model.chat.effort` inherit `model.effort` when unset and can override it per pass.

## Output-token headroom (`max_tokens`)

Gemini counts internal reasoning tokens (`usageMetadata.thoughtsTokenCount`) against
`generationConfig.maxOutputTokens` alongside visible output, and bills both at the output token rate.
RunLore folds `thoughtsTokenCount` into `Usage.OutputTokens` so `max_tokens_per_investigation`,
`max_cost_per_investigation`, and Prometheus metrics reflect actual billed output.

- **Investigations** default to `model.max_tokens: 8192`. Raise `model.max_tokens` (for example
  `16384` or `32768`) when running `gemini-4-neon` or `effort: high`/`xhigh` so long tool calls or
  final verdicts are not truncated mid-thought (`MAX_TOKENS`).
- **Thread chat** (`model.chat`) defaults to `model.chat.max_tokens: 1024`. Raise
  `model.chat.max_tokens` (or set `model.chat.effort: low`) when pointing `model.chat` at Gemini 4
  models so thinking tokens do not exhaust the 1024-token chat ceiling.

## Verify it locally

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=GEMINI_API_KEY='<key>'
```

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) and
watch a **multi-turn** investigation (more than one tool call) complete:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'msg=findings|msg=incident|INVALID_ARGUMENT'
```

## Notes

- **`base_url` is optional** — unset, it defaults to `https://generativelanguage.googleapis.com`.
- **Automatic `thoughtSignature` replay** — Gemini 3 and Gemini 4 attach opaque `thoughtSignature`
  fields to response parts. The client captures and replays them verbatim via `Message.Opaque` across
  multi-turn tool loops (including parallel tool calls and forced tool turns), while keeping
  `thought: true` summary parts out of visible text.
- Forced tool calls (verify pass, recall reranker, eval judge, semantic KB validation, `kb import
  --model`) use Gemini's `functionCallingConfig: {mode: "ANY", allowedFunctionNames: […]}` — the
  provider-native equivalent of OpenAI's forced `tool_choice`.
- **`thinking: adaptive` is Anthropic-only** and is rejected at startup for this provider; use
  `model.effort` instead.
- RunLore relies on Gemini's automatic **implicit prefix caching** (enabled on Gemini 2.5+): the
  request prefix (system instruction + tools + earlier turns, including replayed thought signatures)
  stays byte-stable and append-only across the loop's steps.
- A common non-fatal ReAct pattern: Gemini sometimes concludes an investigation in prose instead of
  calling `submit_findings`. The loop nudges it once, then falls back to a synthetic inconclusive
  result rather than hanging — same fallback described on [OpenAI-compatible]({{< relref
  "openai-compatible.md" >}}).
- A cleartext API key is rejected on a public `http://` host — use `https://` or a loopback/in-cluster
  address.

## Reference

[Configuration → `model`]({{< relref "/docs/configuration/configuration.md#model--the-llm-provider" >}})
for the full key reference.
