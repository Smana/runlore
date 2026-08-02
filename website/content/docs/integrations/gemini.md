---
title: Gemini
weight: 30
integration: {kind: llm, id: gemini}
---

**What it gives you** — the investigation loop driven by Google's Gemini API (native
`streamGenerateContent` function calling, not an OpenAI-compatible shim) — **version-gated**, see
below before you wire it.

## Gemini 3.x does not work

**Do not configure a Gemini 3.x model.** RunLore's Gemini client does not capture or replay the
`thought_signature` that Gemini 3 requires on `functionCall` parts. An investigation gets through the
first turn, then fails on the **second** turn with:

```
400 INVALID_ARGUMENT: Function call is missing a thought_signature
```

This was reproduced, not inferred — every multi-turn investigation on a Gemini 3.x model fails this
way. Tracked as [issue #392](https://github.com/Smana/runlore/issues/392).

**Gemini 2.5 works.** Verified with `gemini-2.5-flash-lite`.

## Minimal config

```yaml
model:
  provider: gemini
  model: gemini-2.5-flash-lite      # verified working — do NOT use a gemini-3.x model
  api_key_env: GEMINI_API_KEY       # base_url defaults to https://generativelanguage.googleapis.com
```

## Verify it locally

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=GEMINI_API_KEY='<key>'
```

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) and
watch a **multi-turn** investigation (more than one tool call) complete rather than fail on its second
turn:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'msg=findings|msg=incident|INVALID_ARGUMENT'
```

## Notes

- **`base_url` is optional** — unset, it defaults to `https://generativelanguage.googleapis.com`.
- Forced tool calls (verify pass, recall reranker, eval judge, semantic KB validation, `kb import
  --model`) use Gemini's `functionCallingConfig: {mode: "ANY", allowedFunctionNames: […]}` — the
  provider-native equivalent of OpenAI's forced `tool_choice`. This works correctly on Gemini 2.5;
  the thought-signature issue above is what breaks Gemini 3.x specifically, not forced calling in
  general.
- **`effort` is rejected at startup for this provider.** Gemini's `thinkingConfig` has replay
  semantics (thought signatures) the provider-agnostic message history can't carry — the same class of
  problem behind the 3.x incompatibility above, just enforced up front instead of surfacing as a
  runtime 400.
- **`thinking: adaptive` is Anthropic-only** and is likewise rejected for this provider.
- RunLore relies on Gemini's automatic **implicit prefix caching** (enabled on Gemini 2.5+): the
  request prefix (system instruction + tools + earlier turns) must stay byte-stable and append-only
  across the loop's steps, which the client guarantees.
- A common non-fatal ReAct pattern: Gemini sometimes concludes an investigation in prose instead of
  calling `submit_findings`. The loop nudges it once, then falls back to a synthetic inconclusive
  result rather than hanging — same fallback described on [OpenAI-compatible]({{< relref
  "openai-compatible.md" >}}).
- A cleartext API key is rejected on a public `http://` host — use `https://` or a loopback/in-cluster
  address.

## Reference

[Configuration → `model`]({{< relref "/docs/configuration/configuration.md#model--the-llm-provider" >}})
for the full key reference.
