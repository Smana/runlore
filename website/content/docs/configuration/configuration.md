---
title: Configuration Reference
weight: 10
---

A navigable map of RunLore's configuration, organized by subsystem.

> [!NOTE]
> **`values.yaml` is the authoritative reference**
>
> The single source of truth for every key is the chart's
> [`deploy/helm/runlore/values.yaml`](../deploy/helm/runlore/values.yaml) — every key carries an
> inline rationale comment, and the whole `config:` block is rendered **verbatim** into the agent's
> ConfigMap (`toYaml .Values.config`). This page is the overview and the *why*; `values.yaml` is the
> exhaustive, annotated detail. The `model` / `catalog` / `outcome` / `notify` / `forge` / `network`
> blocks ship commented-out there as copy-paste templates.

## The config model — three things to know

1. **Helm → ConfigMap → agent.** Everything under `values.config.*` becomes the agent's config file.
   Outside Kubernetes, `lore serve --config runlore.yaml` reads the same shape directly.
2. **Strict decoding — typos fail loudly.** The loader uses `KnownFields(true)`: an unknown key aborts
   startup rather than being silently ignored, so a misspelled safety-critical field can't slip
   through. **One exception:** the `sources:` map (it's a free-form map); a mistyped *source* key is
   instead caught at startup by the source builder, which errors with
   `unknown source(s) … under \`sources:\``.
3. **Secrets by indirection.** Config never holds a secret value — it names the **environment
   variable** (or Secret key) that holds it: `api_key_env`, `webhook_url_env`, `private_key_ref`, etc.
   Wire those env vars from a Kubernetes `Secret` (`envFrom`/`env` in the chart). A stray dump of the
   config therefore leaks nothing.

---

## Subsystems

Defaults below are the behaviorally-significant ones (applied in the config loader). For the full key
list and comments, follow each link to `values.yaml`.

### `sources` — what wakes RunLore
Per-source enablement map; presence enables a source. `alertmanager: {}` mounts the incident webhook;
`gitops: { enabled: true }` watches GitOps `Ready=False`; `pagerduty: {}` mounts the PagerDuty V3
incident webhook. Known keys: `alertmanager`, `gitops`, `pagerduty`, `custom`.

- **`pagerduty`** — mounts `POST /webhook/pagerduty` for [PagerDuty V3 webhook subscriptions](https://developer.pagerduty.com/docs/webhooks-overview).
  `incident.triggered` starts an investigation; `incident.resolved` closes the outcome (like an
  Alertmanager resolved alert); every other event type is ignored. The incident title becomes the
  investigation title, priority (else urgency) the severity, and the service name, incident number and
  `html_url` ride along as labels. PagerDuty carries **no Kubernetes namespace or workload**, so those
  stay empty — such workload-less incidents can recall **only entries that are themselves resource-less**
  (hand-written runbooks / curated Playbooks without a `resource` frontmatter): the scopeless tier, the
  weakest match. It must clear `solo_floor` **and** `min_score` even with multiple candidates, recalls
  with reduced confidence, and `require_workload_match: true` disables it entirely.
  - `secret_env` — names the env var holding the webhook **signing secret** (config stores the env-var
    *name*, never the value). Each delivery is verified against its `X-PagerDuty-Signature` header
    (`v1=`-prefixed HMAC-SHA256 of the raw body; multiple comma-separated signatures are accepted so a
    zero-downtime secret rotation works — any match passes, constant-time). This **replaces** the shared
    `server.webhook_token_env` bearer token for this path (PagerDuty signs, it cannot send a bearer
    token). An **unset** secret leaves the webhook open (mirrors the optional Alertmanager token) — but
    it **fails closed** when a model is configured or `actions.mode=auto`: RunLore refuses to start with
    an unauthenticated PagerDuty webhook once a model is wired.
  - PD-side setup: create a [webhook subscription](https://developer.pagerduty.com/api-reference/b3A6MjkyNDc4NA-create-a-webhook-subscription)
    (delivery URL `https://<runlore>/webhook/pagerduty`), subscribe to the `incident.triggered` /
    `incident.resolved` events, and put the subscription's generated secret in the env var named by
    `secret_env`.

### `triggers` — which incidents to investigate
- `incidents.match` / `incidents.ignore` — ANDed matchers (`severity`, `environment`, `namespaces`
  globs, `alertnames` globs, `labels`); empty fields match anything. `ignore` excludes even if `match`
  passes.
- `incidents.dedup.window` — don't re-open a still-firing alert within this window. **Code default `0`**
  (disabled — every repeat_interval re-investigates); the **chart ships `30m`** by default to bound
  LLM spend on noisy still-firing alerts (see `deploy/helm/runlore/values.yaml`).
- `incidents.debounce` — hold a **non-critical** firing alert this long before investigating, and skip
  it if a matching Alertmanager `resolved` webhook arrives within the window (self-resolving noise, e.g.
  a `KubeDaemonSetRolloutStuck` during a Karpenter node-churn cycle). **Default `60s`** (same as
  `gitops_failures.debounce`); set `0s` to investigate immediately on every fire. Beyond saving a paid
  investigation, the hold keeps a self-healed alert's `resolved` webhook **out of the outcome ledger**,
  where it would otherwise credit a recalled entry's resolve rate for a resolution the diagnosis had
  nothing to do with. Composes with `coalesce` (survivors are batched afterwards) and `dedup` (re-fires
  are still suppressed before the hold begins).

  > **A `critical` alert is never held.** A debounce must never delay the first look at a page — the
  > same invariant the coalescer enforces by flushing criticals with no batching wait. Because the
  > chart's default `match.severity` is `[critical]`, the hold is effectively **inert on a default
  > install**; it begins filtering once you widen `match.severity` (e.g. to include `warning`).
  > Self-resolving *criticals* are filtered by `cancel_queued_on_resolve` instead, at zero added latency.

  > **Operational caveat.** A held alert is lost if the process shuts down mid-hold: Alertmanager
  > already received its `200`, so it will not resend until its own `repeat_interval` (often hours).
  > The drop is logged at **WARN** (naming the alert + fingerprint) and counted in
  > `runlore_incidents_dropped_on_shutdown_total`. The hold window can exceed the drain grace period,
  > so draining does not rescue it — keep `debounce` short, or `0s`, if this matters more than the
  > noise saving.
- `incidents.cancel_queued_on_resolve` — when the matching Alertmanager `resolved` webhook arrives
  while the investigation is still **queued** (accepted, not yet started), drop it. **Default `true`.**
  This — not the hold — is what filters a self-resolving **critical**, since `debounce` never holds one:
  the investigation is dropped from the queue before it starts, so the saving costs **zero added
  latency** (nothing is waited on; the cancel merely races an investigation that has not begun). Set
  `false` if you want the post-hoc answer to "why did it fire?" even after self-resolution. Boundaries:
  an **in-flight** investigation is never cancelled (it completes and delivers), and a **coalesced
  multi-alert batch** is not cancelled on one member's resolve (partial resolution is ambiguous — the
  rest may still be firing). Cancellations count in `runlore_investigations_cancelled_total`.
- `gitops_failures.debounce` — require a failure to persist this long before investigating. **Default
  60s**; explicit `0` fires immediately on every `Ready=False`.

### `investigation` — loop bounds & noise control
- `coalesce` — fold an alert storm into one investigation. **Code default: disabled**; the **chart
  ships `enabled: true`** (see `deploy/helm/runlore/values.yaml`) — same chart-vs-code split as
  `incidents.dedup.window`. Defaults when enabled: `debounce` **30s**, `max_wait` **2m**, `max_batch`
  **50**, `cooldown` **10m**; `correlation_labels` group related alerts. Two escape hatches open the
  `cooldown`: an unseen critical alertname (a new problem — flushes immediately) and a **standing 👎
  on the trigger** (the feedback re-arm, see [learning-loop]({{< relref "learning-loop.md" >}}) — batched on the
  normal `debounce`, so a contested storm still collapses to one re-investigation). Suppressions log
  an INFO line and count in `runlore_alerts_suppressed_total`.

  **Batching *sibling* alerts takes both knobs.** Two alerts describing one physical event —
  `KubeNodeNotReady` and `KubeNodeUnreachable` on the same node, or the pod and deployment alerts for
  one crash-looping workload — only merge if you set **both** of these:

  1. **`correlation_labels`.** Without them the key is the Alertmanager `groupKey`, and a typical
     `group_by` includes `alertname` — so siblings never share a key however long you wait. Group by
     what the alerts have in common instead (e.g. `[node]`, or `[namespace, pod]`).
  2. **A `debounce` wider than the gap between them — and a `max_wait` above that.** A batch flushes
     when it has been quiet for `debounce` **or** older than `max_wait`, whichever comes first. With
     the default `debounce: 30s` a lone alert flushes ~30s after it arrives, so **raising `max_wait`
     on its own cannot batch anything**. Siblings 2 minutes apart need `debounce` above 2m — and then
     `max_wait` must be raised past it too, or the default `max_wait: 2m` flushes the batch before the
     wider `debounce` ever applies. Both knobs move together, and the latency is paid on **every**
     alert, so widen them deliberately.

  Flush decisions are made on a sweep tick of `debounce / 2`, so both deadlines are approximate — a
  batch flushes up to half a `debounce` after the deadline it crossed.
- `rate_limit` — `max_per_window` (**default 30**; an explicit **0 = unlimited**), `window` (default **1h**),
  `max_requeues`.
- `recurrence_cooldown` — **opt-in (default 0 = off)** per-trigger suppression: skip re-investigating a
  trigger that was **looked at** less than this long ago and for which **an answer stands** — some
  prior investigation of it reached a conclusion (verdict ≠ `inconclusive`), **not necessarily the most
  recent one** — with **no standing 👎 feedback**. Without it, nothing keys on the trigger before
  the paid loop: a still-firing alert re-investigates on every Alertmanager `repeat_interval` and a
  persistently-failing GitOps resource on every informer resync (**~10 minutes**) — each a full model
  run re-delivering the same answer as fresh noise. A suppressed occurrence costs nothing and says
  nothing (no model call, no notification, no ledger open — the previous notification remains *the*
  answer); the next occurrence past the cooldown re-investigates in full. Two human-deferential
  escape hatches: a trigger that has **never** concluded is re-investigated on every firing however
  often it fires (there is no answer to stand on, and silence would leave you with nothing), and a
  👎 on the previous message re-arms investigation immediately (see
  [`notify.slack.feedback_buttons`](#notify--where-findings-go)). Requires `outcome.ledger_path`
  (fails loud at startup without it). A sensible production value is `30m`–`1h`. Suppressions show up
  as `runlore_investigations_completed_total{result="recurrence_suppressed"}`; the one bypass worth
  watching — fired again inside its cooldown, but nothing conclusive to stand on — logs an INFO line
  naming the trigger.
- `timeout` — per-investigation deadline, **default 10m** (bounds a hung tool/clone so it can't starve
  the single-worker queue).
- `tool_timeout` — per-**tool**-call timeout, **default 60s** (0 = use the default). Bounds a single
  hung/slow provider (a stuck git clone, an unresponsive metrics/logs endpoint) so it can't eat the
  whole per-investigation budget; on expiry the tool result is a non-fatal "timed out" note and the
  investigation continues.
- `max_steps` (**default 20**) — model turns one investigation may take.
- `max_tool_output_bytes` (**default 32768**; `-1` = unlimited) — each tool result is truncated to
  this many bytes before it is fed back to the model.

- **Spend ceilings** — two knobs bound what one investigation may spend, and both feed the **same
  two-rung ladder**, so there is one behaviour to learn and one result code to watch. On the first
  crossing RunLore injects a nudge and forces `submit_findings`, so the run **still delivers real
  findings**; if the model has not concluded by the next step it is **hard-stopped** with
  `result="budget_exceeded"` and an unresolved stub naming the ceiling it hit. Both rungs are counted
  by `runlore_investigation_budget_trips_total{reason,stage}`.
- `max_tokens_per_investigation` (**default 400000**; `-1` = unlimited) — a **cumulative** ceiling on
  one investigation's model tokens (provider-reported input + output, loop **and** verify, including
  a recall short-circuit's reranker/verify calls, the hybrid-recall **query embeddings**, and any call
  that **failed** after the provider had already billed for it).

  **One number, two thresholds.** A quarter of it — **100 000** at the default — additionally bounds
  the estimated size of any **single request**, so one oversized request is caught before it is
  billed, and mid-loop compaction triggers at 70 % of *that* (**70 000**). The two are separate
  failures with separate fixes: "this run has spent too much in total" is answered by raising
  `max_tokens_per_investigation`; "this one request is too big" is answered by lowering
  `max_tool_output_bytes` or enabling `compaction`, and raising the run budget does not help. A run
  stopped by the per-request bound is labelled `reason="tokens_request"`, one stopped by the running
  total `reason="tokens_total"`.

  > **This ceiling used to bound one request, not the run — and its default has been raised to
  > match.** It was previously compared only against the estimated size of the next request, so
  > twenty steps of 99k each passed cleanly against a `100000` "budget". It is now a running total.
  > Read that way the old value funded only four or five steps, so the default is now **400000**: an
  > investigation whose every tool result is at the shipped `max_tool_output_bytes` costs ≈327 000
  > tokens over seven model calls, and an ordinary tool-heavy one (8 KiB results, twelve steps)
  > ≈379 000. **If you set `max_tokens_per_investigation` explicitly, your value is untouched and is
  > now read as a whole-run budget** — a config that said `100000` still says `100000`, and now means
  > roughly four steps rather than twenty. Set it to the total you are willing to pay per incident
  > (summed across all turns, not the size of one), or `-1` for the old unbounded behaviour. Watch
  > `runlore_investigation_budget_trips_total` after upgrading.

  **Budget for the ceiling plus one request.** The check runs *before* each request and compares the
  **projected** total — what the run has already spent plus the estimated size of the request about
  to be sent — so a run stops on the first request that would carry it past the number. That request
  is still sent: the nudge exists to give the model one turn to conclude. The delivered total can
  therefore exceed the ceiling **by up to the size of a single request**, and because the transcript
  grows every step that is the *largest* request of the run. That conceded request is itself bounded
  by the per-request quarter, so **≈1.25× the number you set** is the figure to budget against.
  Measured at the shipped defaults, with `max_tool_output_bytes: 32768` and a provider reporting real
  usage: **≈467 000 tokens delivered against a 400 000 ceiling**, ≈1.17×. The same applies to
  `max_cost_per_investigation` below, whose projection prices the pending request's input at
  `model.pricing` — its output length is not knowable before it is sent, so the projection errs low.

- `max_cost_per_investigation` (**no default — opt-in**) — the same ceiling denominated in **USD**,
  compared against the running estimated spend priced from `model.pricing` (and `model.verify.pricing`
  for the verify pass). Unset or `0` means no cost ceiling. There is deliberately **no `-1` opt-out**:
  `0` already means off, and a negative value is rejected at startup rather than quietly read as one.

  *Why opt-in when the token ceiling ships a default:* a token is provider-neutral — 400000 means the
  same thing on every model, so a safe value can be chosen on your behalf. A dollar is not. You pick
  your own model and supply your own rates, so any figure RunLore shipped would be generous for one
  deployment and punitive for the next, and would silently cut runs short on an upgrade nobody asked
  for.

  **It does nothing without `model.pricing`.** With no rates configured there is no cost to compare,
  so the ceiling can never fire — for any investigation, ever. RunLore says so loudly at startup
  rather than letting the limit sit inert, and warns about the same trap when `model.pricing` is
  present with **every rate at `0`**: cost is then always exactly `$0.00`, which is under any ceiling,
  while the notification footer and `runlore_investigation_cost_usd` both report a figure and make the
  deployment look instrumented for spend.

  **Fill in all three rates.** A rate left at `0` is not "this class is free" — it is a whole class of
  real spend estimated at `$0.00`. `cached_input_usd_per_mtok` is the one most often forgotten, and
  every provider RunLore speaks reports cache reads separately (Anthropic `cache_read_input_tokens`,
  OpenAI `prompt_tokens_details.cached_tokens`, Gemini `cachedContentTokenCount`), so on a cache-heavy
  run omitting it understates the input term several-fold: the ceiling still fires, but only after
  materially more real spend than the number you wrote, and the footer and metric report the same
  under-estimate. RunLore warns at startup and names each rate left at `0`. If your provider genuinely
  does not bill a class separately, set that rate equal to the one it shares (e.g.
  `cached_input_usd_per_mtok: <your input rate>`) — the corresponding token count is then always 0, so
  it costs nothing and the warning goes quiet.

  **What these ceilings do *not* cover.** Both bound ONE investigation. Stated plainly, because a
  limit whose scope is assumed is a limit that surprises someone:

  | Outside the ceilings | Why, and what does bound it |
  |---|---|
  | Anything above one investigation | There is no daily, monthly or global budget. Total exposure is `investigation.rate_limit.max_per_window` (default 30 starts/hour) × the per-investigation ceiling — a product of two knobs, not a budget |
  | The **bulk corpus embed** on catalog reload | It runs on the catalog-sync goroutine with no investigation to charge, so no investigation-scoped ceiling can reach it, and RunLore ships none of its own. What bounds it is the corpus, which is your input, not the model's: embeddings are content-hash cached and the cache is persisted across restarts (`instant_recall.vector_cache`), so steady-state cost tracks *changed* entries rather than corpus size, and there is no feedback loop to run away. A ceiling here could only fail all-or-nothing — partial vector sets are refused by design — so its one effect would be silently dropping to BM25 recall, which `catalog_embed_degraded_total` already reports. Visible as `runlore_model_requests_total{provider="embed"}` |
  | The **dollar** cost of embeddings | The hybrid-recall query embed's *tokens* now count against `max_tokens_per_investigation`, but there is no `model.embeddings.pricing`, so RunLore has no rate to price them with and refuses to borrow a completion model's — that would put a fabricated figure on the notification footer and in `runlore_investigation_cost_usd`. `max_cost_per_investigation` therefore errs **low** by the embedding spend rather than quoting a number it cannot stand behind |
  | The adversarial verify pass's own cost | It runs after the last budget check, unconditionally, because verify is the honesty guarantee. A successful run can therefore deliver a `CostUSD` slightly above `max_cost_per_investigation` with no trip recorded |
  | A `lore eval` **campaign** as a whole | Each investigation it replays runs under the operator's configured `investigation.*` ceilings and `model.pricing` — the same limits production has — but a campaign is cases × n of them, so those ceilings multiply by the corpus size instead of capping the run. `--max-total-tokens` is the run-level ceiling; **unset, a campaign has none** |
  | CLI-only model calls | `lore validate-kb --semantic`, `lore kb import --model`, and the eval judge each make completions outside any investigation. None runs under `serve` |
  | Non-model spend | Cloud API calls, git clones, metrics and logs queries are unpriced everywhere |
- `compaction` — how mid-loop history compaction treats the older tool outputs it elides once the
  estimate crosses the compaction target — 0.7× the **per-request** budget, itself a quarter of
  `max_tokens_per_investigation` (see above). **`elide`** (default)
  drops their bodies for short markers (lossy). **`summarize`** first asks a model for **one** compact
  factual digest of the elided batch (per compaction event) — "preserve identifiers, timestamps, error
  strings, counts; no speculation" — and keeps that, clearly labelled, in place of the markers.
  Routed to the `model.verify` tier when configured (cheaper), else the main model. **Fail-safe:** any
  summarizer error, refusal, or truncation falls back to plain elision — a compaction failure never
  loses the investigation. The digest is derived only from the already-redacted tool outputs, so it
  adds no new egress path. Requires `max_tokens_per_investigation > 0` (compaction is off without a
  budget). Metrics: `history_summarizations_total`, `history_summarize_fallbacks_total`. **The digest
  call is billed like any other**, so its tokens count into `model_input_tokens_total`, into the
  investigation's reported usage and cost, and against both spend ceilings — including when the
  summarizer errors or truncates, since a failed call still consumed its input.
- `progress_updates` — interim delivery for long investigations. **Off by default.** `enabled`; when
  enabled the loop delivers a progress ping (incident title, step count, tools used so far, and the
  model's latest interim text — redacted and mrkdwn-escaped like any other untrusted field) every
  `every_steps` steps (**default 5**; must be `> 0` when enabled). Pings go only to notifiers that
  implement the capability (Slack today); a delivery failure is logged and swallowed, never failing the
  investigation.
- `pod_log_namespaces` — **app-layer allowlist** of namespaces the `pod_logs` tool
  may read RAW pod logs from, *beyond* the incident's own namespace (which is always allowed; RBAC
  still gates the actual read). Pod logs carry secrets/PII and are streamed to the external LLM, so the
  model is constrained here — not by Kubernetes RBAC alone. **Empty ⇒ incident namespace only** (secure
  by default). This **must be a superset of where `pods/log` RBAC is granted** (`rbac.controllerLogNamespaces`)
  or `pod_logs` is blocked at the app layer for those namespaces. **The chart auto-defaults this to
  `rbac.controllerLogNamespaces`** when you leave `config.investigation.pod_log_namespaces` unset, so the
  app-layer allowlist and the RBAC scope stay in sync automatically — set it explicitly only to widen or
  narrow beyond the RBAC scope. See [Security model → least-privilege RBAC]({{< relref "security-model.md" >}}).

### `catalog` — the knowledge base & instant recall
- `dir` — local OKF bundle / git-sync mirror path; `git` — `url`, `branch` (default `main`), `interval`
  (default 5m), `token_env`.
- `instant_recall` — `enabled`, and when enabled the trust gates `min_score` **1.0**, `margin_gap`
  **1.0**, `solo_floor` **4.0**, `outcome_prior` **2.0**, `outcome_floor` **0.5**;
  `require_workload_match`; experimental `hybrid*` vector knobs. A workload-less incident (e.g.
  PagerDuty) can match only resource-less entries — the weakest tier, which always requires
  `solo_floor` + `min_score`, recalls with reduced confidence, and is disabled by
  `require_workload_match: true`.
- `instant_recall.stale_after` — **opt-in** age down-weighting (a Go duration, e.g. `720h`; **`0`/unset
  disables**, the default). A recalled entry whose `last_validated` (else `timestamp`) is older than
  this has its delivered confidence taken **one** step down (×0.75). It **never rejects** — `confirm`
  and `verify` remain the hard gates and the outcome floor keeps priority — it only stops an unvalidated,
  years-old runbook looking as confident as a fresh one. A dateless or unparseable-date entry is exempt
  (fail-safe). Retired/`draft` entries are filtered out of recall entirely (independent of this knob).
- `instant_recall.rerank` (**ON by default when `instant_recall` is enabled**; set `false` to fall back to the legacy gate) — replaces the corpus-dependent BM25-magnitude
  fire gate (`solo_floor`/`margin_gap`) with an **LLM reranker** that scores the top-`rerank_k`
  structurally-agreeing candidates in **one cheap call** and short-circuits only on the reranker's
  **calibrated** match confidence (`rerank_threshold`, default **0.7**). *Why:* query enrichment fixed
  retrieval *ranking* (the correct runbook ranks #1 on real BM25), but an enriched real-corpus score is
  ~0.1–1.2 — an order of magnitude below the default `solo_floor` 4.0 — so the magnitude gate only fires
  where the operator hand-tuned `solo_floor` down to their corpus. A calibrated 0–1 confidence is
  **corpus-independent**, so the same default fires across clusters. Knobs (all defaulted when `rerank`
  is on): `rerank_threshold` **0.7** (fire bar; a probability, so no per-corpus tuning), `rerank_k`
  **5** (candidates ranked per call; bounded for cost), `rerank_min_score` **0.1** (trivial
  retrieval-score floor below which the paid call is skipped — the cost guard). The call routes to
  **`model.verify`** when configured (cheaper/faster), else the main model. It costs ~1–2k tokens and
  buys back a whole investigation when it fires — the recorded demo transcript's came to 7 calls /
  ~15.6k tokens, against a `max_tokens_per_investigation` default of 400k. False-recall guards: it only
  ever ranks candidates that already passed the structural filter, ignores any `entry_id` it did not offer
  (hallucination guard), and fails **safe** on a "no match", a low confidence, or a model error (fall
  through to a full investigation). Off ⇒ the BM25-magnitude gate is unchanged. The recalled answer
  still goes through live-state confirm + the adversarial verify pass, exactly as before — the reranker
  is a *retrieval-time* "which candidate + confident enough to short-circuit" decision, not a second
  verify.

  **What it costs in steady state** (budget for this before enabling). The reranker sits *in front of*
  the short-circuit, so the call is spent on every incident that reaches it — **including the ones
  that do not fire**: a `match=false`, a confidence under `rerank_threshold`, or a model error has
  already paid for the call before falling through to the full investigation — as has a `low_outcome`
  rejection, which is decided *after* the ranking. A candidate that fails retrieval or the structural
  filter never reaches the reranker at all; past that point the last guard is `rerank_min_score`, and
  its default **0.1** sits at the *bottom* of the ~0.1–1.2 band real enriched BM25 scores occupy, so
  it skips the call only when retrieval surfaced essentially nothing. A second guard sits beside it:
  the **spend ceiling is consulted before the ranking call, not after it**, so an investigation that
  has already crossed `max_tokens_per_investigation` (or `max_cost_per_investigation`) declines to
  rank rather than spending past a limit it has demonstrably hit. That is a fall-through like any
  other — the run continues into a full investigation, where the ordinary nudge→kill ladder stops it
  — and it is counted as `runlore_recall_rejections_total{reason="rerank_over_budget"}`, not as a
  third rung on that ladder.
  In practice, then, enabling instant recall adds **one model call to the floor
  cost of nearly every investigation that has a structurally-agreeing candidate**, and buys back a
  full investigation on the subset that fires. A fired recall is consequently **two** model calls —
  rerank, then the verify pass (always on; no config key disables it — `model.verify` only *routes*
  it to a cheaper tier) — not one. The one exception is the runner-up path: when outcome decay
  rejects the ranked winner, a second (and final) ranking call runs over the remaining candidates, so
  a recall that fires from the fallback costs **three**.
  **Measure it, don't estimate it:** the reranker's traffic carries `provider="rerank"` on
  `runlore_model_requests_total` and `runlore_model_request_duration_seconds`, so
  `runlore_model_requests_total{provider="rerank"}` against `runlore_recall_hits_total` and
  `runlore_recall_rejections_total{reason=~"rerank_low_confidence|low_outcome"}` gives you calls-paid
  vs recalls-fired on your own corpus; `reason="no_resource_match"` tells you whether the reranker is
  being reached at all (see [Observability]({{< relref "/docs/operations/observability.md" >}})).
  If the ratio is bad, raise `rerank_min_score` toward your corpus's real score regime — that trades
  recall coverage for calls not made.
- `instant_recall.hybrid` (**EXPERIMENTAL**, off by default; needs `model.embeddings`) — switches recall
  to fused **BM25 + embedding** retrieval, gated on **cosine** similarity (`hybrid_min_score` default
  **0.80**, `hybrid_margin_gap` default **0.05**) instead of the BM25 magnitude. *Provenance:* the hybrid
  eval (`internal/investigate/hybrideval_test.go`) drives the whole path end-to-end with a
  **deterministic bag-of-words embedder** — the CI regime measures the *machinery and the gate
  philosophy*, not semantic quality: `SearchHybrid` + the cosine gates run, hybrid Recall@1 is **8/13**
  (`TestHybridRecallEvalRetrieval`), and at the shipped gates the fire-rate is **0/11** with **0** of 2
  negatives firing (`TestHybridRecallEvalProductionFireRate`) — the false-recall guard holds. The numeric
  cosine defaults are **conservative and not yet live-measured**: a real embedding model's cosine scale
  is model-specific, so it must be measured with the env-gated `TestHybridRecallEvalLive` (which prints
  the per-case cosine distribution and a `hybrid_min_score`/`hybrid_margin_gap` recommendation) before
  the thresholds are trusted.
- **Graduating hybrid out of `EXPERIMENTAL`** requires all of:
  1. Live-measured thresholds (`TestHybridRecallEvalLive`) for at least one recommended embedding model,
     recorded here with model + date.
  2. Hybrid Recall@1 ≥ the BM25 baseline on the same fixture set (`TestHybridRecallEvalRetrieval` vs
     `TestRecallEvalRetrieval` — today 8/13 hybrid vs 13/13 BM25 under the crude CI embedder; a real
     model is expected to close this, which is exactly what the live run must confirm).
  3. Zero negative-case fires at the shipped default gates, **live** regime included.
  4. The embedding vector cache (content-hash, chunked batches) merged so reload cost no longer scales
     with corpus size — **done** (N2, PR #328).
- `instant_recall.vector_cache` — **on by default** (only effective with `hybrid` + `model.embeddings`).
  Persists the hybrid embedding cache to disk so a **restart or HA failover re-embeds nothing** (the
  in-memory content-hash cache already spares unchanged entries within a process lifetime; this carries
  it across process lifetimes). `enabled` (**`true`**; set `false` to keep the cache in-memory only) and
  `dir` (cache directory, default `<tmp>/runlore-veccache` — **ephemeral**; point it at a PersistentVolume
  to keep it across pod restarts, the same pattern as `gitops.mirror.dir`). Fail-safe by contract: every
  failure mode — missing, corrupt, or written by a **different embedding model/dimension** — is a WARN +
  cold re-embed, never an error, so a stale cache can never serve wrong vectors. Cache files are
  **pod-local** (each replica maintains its own).
- The **"📚 Matches known runbook"** notification block (stamped when a *full* investigation's
  `kb_search` finds a pre-existing entry) uses `solo_floor` as its visibility bar, so it tracks
  the same corpus/query-dependent BM25 scale recall runs in: a cluster that tunes `solo_floor`
  **down** for sub-1.0 alert-query scores gets a correspondingly low bar instead of the signal
  silently never firing. When instant recall is disabled it falls back to the **4.0** default.
- The search index is in-memory bleve (BM25), rebuilt from `dir` at startup — not persisted.

### `outcome` — the learning ledger
`ledger_path` — append-only JSONL of investigation outcomes (empty disables). Drives outcome-weighted
recall decay; **must be on the PVC** to compound (see [Upgrade & Uninstall]({{< relref "upgrade-uninstall.md" >}})).
Also records the human 👍/👎 ratings when `notify.slack.feedback_buttons` is enabled (see
[`notify`](#notify--where-findings-go) below).

### `actions` — the autonomy ladder (off by default)
- `mode` — `off` (default) · `suggest` · `approve` · `auto` (experimental, frozen/not recommended).
- `allow` — the envelope: `reversible_only`, `namespaces` (allowlist — **empty permits nothing**),
  `protected_namespaces` (added to built-in `flux-system`/`kube-system` denies), `max_blast_radius`,
  `kinds`.
- **Per-engine op semantics** — the executable ops (`suspend` / `resume` / `reconcile`) are
  engine-neutral names; the executor for the configured `gitops.engine` translates them:

  | op | Flux (`Kustomization` / `HelmRelease`) | Argo CD (`Application`) |
  |---|---|---|
  | `suspend` | sets `spec.suspend: true` | removes `spec.syncPolicy.automated` (pauses auto-sync); the prior value is preserved in the `runlore.io/paused-sync-automated` annotation |
  | `resume` | sets `spec.suspend: false` | restores `spec.syncPolicy.automated` from that annotation (no-op if RunLore didn't pause it) |
  | `reconcile` | `reconcile.fluxcd.io/requestedAt` annotation | `argocd.argoproj.io/refresh: normal` annotation |

  All three are reversible with blast radius 1 in the server-authoritative op registry, so the same
  policy envelope gates both engines identically. **Argo CD notes:** `Application` objects usually
  live in the `argocd` namespace — add it (or your apps-in-any-namespace app namespaces) to
  `actions.allow.namespaces` **and** the chart's `rbac.actionNamespaces`. If you set `allow.kinds`,
  include `Application`. `argocd` is deliberately **not** a built-in protected namespace (unlike
  `flux-system`): it is where the reversible pause lever lives; the empty-by-default namespace
  allowlist is what bounds it.
- `require_approval` + `approval_token_env`, `audit_log_path`, and `auto.*` (`dry_run`,
  `min_confidence`, `max_per_window`, `window`).
- **Fail-closed validation:** `approve`/`auto` both require `approval_token_env` **and**
  `audit_log_path` (both executing rungs mutate the cluster, so both must be audited — the hash chain
  is verified fail-closed on open); `auto` *additionally* requires `server.webhook_token_env`,
  `auto.min_confidence > 0`, `auto.max_per_window > 0`, and a non-empty `allow.namespaces`. See
  [Security model]({{< relref "security-model.md" >}}).

### `model` — the LLM provider
`provider` — `openai` (default; any OpenAI-compatible endpoint incl. vLLM/Ollama/OpenRouter) ·
`anthropic` · `gemini`. `base_url`, `model`, `api_key_env`. Optional `verify` (a separate model for the
adversarial verify pass) and `embeddings` (for hybrid recall). Optional `effort` opts into deeper
reasoning per request — `anthropic`: `low`·`medium`·`high`·`max` (sent as `output_config.effort`);
`openai`: `minimal`·`low`·`medium`·`high` (sent as `reasoning_effort`); not supported for `gemini`
(rejected at startup); empty = omitted (default). Models that don't support the knob return a 400,
which is classified permanent (dropped, not retried). `verify.effort` overrides the parent's value,
inheriting it when empty like the other verify fields.

Optional `thinking` opts into Anthropic **adaptive extended thinking** — set `thinking: adaptive` (the
only value; sent as `thinking: {type: "adaptive"}`). **Anthropic-only**: it is rejected at startup for
any other provider, because the client must replay the model's *signed* thinking blocks verbatim across
the tool loop (a contract only the Anthropic client implements). Empty = omitted (default; today's
behavior byte-for-byte). `effort` and `thinking` are independent and may both be set — `effort` is soft
guidance for how much thinking the model does. Because thinking consumes output tokens, give `max_tokens`
headroom when you enable it (a too-low cap truncates the answer mid-thought). Caveat: on the one
budget-forced conclusion step (the loop forces `submit_findings` after the token-budget nudge), adaptive
thinking is incompatible with a forced tool choice, so the client drops the thinking param **and** strips
the replayed thinking blocks for that single request (invalidating only the message-level prompt cache
for that step). `verify.thinking` overrides the parent's value, inheriting it when empty like the other
verify fields — though the verify pass always forces a tool choice, so thinking is dropped there anyway.

Optional `pricing` turns the per-investigation token accounting into a cost estimate: `input_usd_per_mtok`,
`output_usd_per_mtok`, `cached_input_usd_per_mtok` (USD per million tokens; all must be `>= 0`). When set,
the delivered finding gains a footer line (`N model calls · X in / Y out tokens (Z% cached) · ~$C`) and the
`investigation_cost_usd` metric is populated; without it, the footer omits the cost and only token counts
show. Totals sum the investigation loop **and** the verify pass — loop tokens price at `model.pricing`,
verify tokens at `model.verify.pricing` (inheriting `model.pricing` when empty). Cost never enters the
curated KB entry, only the notification.

`pricing` is also what makes `investigation.max_cost_per_investigation` (see the `investigation`
section above) enforceable — without it there is no cost to compare, so a configured USD ceiling can
never fire and RunLore warns about it at startup. Rates are yours to supply and RunLore never fetches or updates them, so a ceiling
is only as accurate as the numbers here: check them against your provider's price list when you change
model or tier, or the ceiling drifts away from the bill it is meant to bound.

### `forge` — the Git host for curation
`provider` (`github` — the default — or `gitlab`), `kb_repo` (GitHub: `owner/name`; GitLab: the
project path, e.g. `group/project`, nested groups allowed), `base_branch` (default `main`),
`github_api_url` (default `https://api.github.com`), `git_host` (default **derived**, see below),
`dup_score` (default **5.0**), `min_confidence`
(default **0.75**, the quality bar below which a finding is chat-only), `skip_verdicts` (default
**empty** — draft every verdict). `github_app` — `app_id`, `installation_id`, and `private_key_ref`
**or** `private_key_env`. `gitlab` — `base_url` (self-managed instance root; omit for gitlab.com) and
`token_env` (a project/group access token, scope `api`); see
[Integrations → GitLab]({{< relref "/docs/integrations/forge/gitlab.md" >}}). `provider: gitlab` with no
`gitlab.token_env`, or a `kb_repo` that isn't a valid GitLab project path, fails config load closed.

`git_host` is the **one host the forge credential may be sent to** on the two clone paths whose repo
URL comes from **cluster state** — `what_changed` against your GitOps repo, and `source_diff` against
an allowlisted source repo. A clone of any other host proceeds anonymously, because a GitOps
`spec.source.repoURL` is cluster state: anyone who can create an Argo CD `Application` picks it, and
it must not be able to pick where your token goes.

> [!NOTE]
> **The catalog git-sync is not confined by `git_host`.** `catalog.Syncer` has no host field at all
> (`internal/catalog/sync.go`), so it attaches its credential to whatever `catalog.git.url` names —
> the explicit `catalog.git.token_env` if you set one, otherwise the shared forge GitHub App
> identity. Setting `git_host` does not change that; there is no mechanism on this path to change.
>
> The reason that is acceptable is a **different** reason from the one above, and it is worth knowing
> which one you are relying on: `catalog.git.url` is *operator config*, not cluster state. It is a
> value you write in `runlore.yaml`, so redirecting the token requires the ability to change your
> config — which is already game over — rather than the ability to create an `Application` in some
> namespace. What follows for you is practical: point `catalog.git.url` at a host you would hand the
> forge credential to, and give it its own read-scoped `catalog.git.token_env` when the catalog lives
> somewhere the forge App identity should not reach. Leave it empty and it is derived — `github.com`
from `api.github.com`, the API host for GitHub Enterprise, `gitlab.base_url`'s host for GitLab. Set it
only for **GitHub Enterprise with subdomain isolation** (API on `api.HOSTNAME`, git on `HOSTNAME`),
which is the one shape the derivation cannot resolve; that config **fails load** until `git_host`
names the git host, rather than guessing and silently withholding the credential from every repo.
The value is a bare hostname — no scheme, path, port or userinfo.

`skip_verdicts` is a list of investigation verdicts that must **not** draft a KB PR — the finding
still reaches chat, but no repo artifact is created. Values are validated at startup against the
verdict enum (`no_action` / `action_suggested` / `action_required` / `inconclusive`); an unknown
value fails fast. Empty (the default) preserves the original behaviour: every verdict is eligible.
Recommended production value is `skip_verdicts: ["no_action"]`, which keeps benign / self-healed /
synthetic findings out of the review queue while still notifying chat (see
[reviewing-knowledge.md]({{< relref "reviewing-knowledge.md#expected-triage-volume" >}})).

### `notify` — where findings go
`slack` (`webhook_url_env` or `bot_token_env`, `channel`, `signing_secret_env`, `approver_ids`,
`feedback_buttons`, `thread_capture`), `matrix` (`homeserver`, `room_id`, `access_token_env`,
`feedback_reactions`, `thread_capture`), plus inline blocks for any registered notifier (e.g.
`webhook` with `url_env`).

Every notifier now leads with the model's **verdict** (`no_action` / `action_suggested` /
`action_required` / `inconclusive`) and carries the trigger-time alert metadata (severity, environment,
cluster, tenant, alert name, `startsAt`), recurrence facts (occurrence count, previous KB link), the
top-cause "why", suggested next steps, **ruled-out** hypotheses and **data gaps** (tool/data
limitations, kept distinct from human-only open questions):

- **Slack, bot token (`bot_token_env`).** Posts a compact verdict-first summary to `channel`, then the
  full analysis as a **threaded reply**. When `signing_secret_env` is set and action mode is `approve`,
  the summary carries Approve/Reject buttons on any suggested remediation (see below).
- **Slack incoming webhook (`webhook_url_env`).** Delivers the same content as a **single** message
  (incoming webhooks cannot thread). Interactive buttons *do* render and dispatch on this path —
  incoming webhooks follow the same rules as `chat.postMessage`, and a click is answered through the
  interaction's `response_url`, which needs no bot token.
- **Matrix, generic webhook.** Deliver the same content as a **single** message, with no interaction
  buttons (Matrix feedback arrives as reactions instead — see `matrix.feedback_reactions` below).

**Generic webhook JSON payload** gained `verdict`, `severity`, `environment`, `cluster`, `tenant`,
`alert_name`, `started_at` (RFC3339, empty when unknown), `occurrences`, `prev_curated_url`, `ruled_out`
and `data_gaps` alongside the existing `title`/`confidence`/`curated_url`/`text` fields (all
`omitempty`).

**👍/👎 feedback buttons — `feedback_buttons` (opt-in, default `false`).** When enabled, Slack
investigation messages carry two buttons ("👍 Accurate" / "👎 Off-base") so the on-call can rate the
diagnosis in one click. Ratings land in the **outcome ledger** and weigh the recalled entry's trust
exactly like resolve signals do — enough 👎 and the entry falls below the recall floor and RunLore
re-investigates instead of reusing it (see
[learning-loop.md §6]({{< relref "learning-loop.md#6-the-feedback-edge--outcome-driven-decay-what-makes-it-learn" >}})).
This is the primary trust signal for incidents that have **no resolve channel** (GitOps failures).

> [!IMPORTANT]
> **Enabling this requires exposing the agent to Slack.**
>
> Button clicks arrive as HTTPS callbacks on **`POST /slack/interactions`**, so that endpoint must be
> reachable **from Slack's servers** (a public Interactivity *Request URL* on your Slack app — the same
> endpoint and the same exposure approve-mode buttons use; if you already run `actions.mode: approve`
> with Slack buttons, nothing new is exposed). Route it through your ingress/gateway; if you use the
> chart's `networkPolicy.ingressFrom`, allow your ingress controller, not the internet. Startup **fails
> loud** unless `signing_secret_env` (every click is HMAC-verified, ±5 min replay window),
> `outcome.ledger_path` (where ratings land) **and a Slack delivery target** — `webhook_url_env`, or
> `bot_token_env` with `channel` — are all set. A button only exists on a message RunLore actually
> delivered, so feedback with no delivery target configured could never work.

> [!NOTE]
> **Mount the credential, too.** If the env var naming the webhook URL / bot token is present but
> **empty** (an unmounted secret, a blank Helm value), the Slack notifier is skipped entirely: nothing
> is delivered, no buttons render and no rating can be recorded. Config validation cannot see runtime
> emptiness, so startup logs a **warning** instead of announcing the feature — grep the logs for
> `no slack delivery target resolved` after enabling it.

Feedback is deliberately **unprivileged**: any signature-valid member of your workspace can rate — a
rating is an opinion feeding the learning loop, not a cluster mutation (approve/reject keep their
`approver_ids` allowlist). Anti-gaming lives in the ledger: **one live vote per (trigger key, Slack
user)**, latest wins — duplicate clicks are idempotent and changing your mind moves the vote. The ack
is an ephemeral "feedback recorded" note visible only to the clicker; the investigation message is
never modified. With the option off (the default), no buttons render and the endpoint behaves exactly
as before (404 unless approve mode wired it). Exposure hardening and the vote trust model are
detailed in [security-model.md]({{< relref "security-model.md#the-feedback-channels---exposure--trust-model" >}}).

**Matrix 👍/👎 — `matrix.feedback_reactions` (opt-in, default `false`).** The same feedback loop over
Matrix **reactions**: react 👍/👎 to a RunLore investigation message and the rating lands in the
outcome ledger (same per-user dedup, same trust weighting, same recurrence-cooldown re-arm — the
ledger mechanics are shared). **Nothing is exposed**: reactions arrive over the client-server `/sync`
long-poll — an *outbound* HTTPS request authenticated by the notifier's access token — so this is the
zero-ingress alternative to Slack buttons. The listener runs on the leader only, skips reactions from
before startup, ignores every emoji except 👍/👎, and only counts votes on messages **the bot itself
sent** (attribution is anchored on `/whoami`; a member-crafted message carrying the trigger field
attributes nothing). Startup fails loud unless `homeserver`/`room_id`/`access_token_env` and
`outcome.ledger_path` are set. Use an **invite-only room** — any room member can vote (see
[security-model.md]({{< relref "security-model.md#the-feedback-channels---exposure--trust-model" >}})).

> [!NOTE]
> **`feedback_reactions` and Matrix `thread_capture` (below) are two independently-gated
> capabilities sharing ONE listener.** Turning on either starts the same leader-only `/sync` long-poll
> goroutine — there is one Matrix listener and one `/sync` connection per process, never two, no matter
> how many of the two flags are on. Each flag independently controls both what that shared `/sync`
> filter asks the homeserver for (`m.reaction` for `feedback_reactions`, `m.room.message` for
> `thread_capture`) AND, in code, whether the corresponding handler is allowed to act — enabling one
> never turns the other's handling on. The practical upshot: enabling only one still means both ride
> the same poll loop, so a homeserver hiccup or a leadership change pauses whichever of the two you
> have enabled together.

**🧵 Thread capture — `thread_capture` (opt-in, default `false`).** When enabled, replying inside a
Slack investigation thread with `@runlore note: <text>` writes what you know back into the knowledge
base as a reviewed PR — a comment on the finding's existing KB PR, or a small `Concept` entry PR when
the finding has none (an instant recall, or a `no_action` verdict), so the knowledge still lands
somewhere. A human reviews and merges it like every other entry. If nobody does, it is not exempt from
the `curate` stale sweep either: an untouched note PR past `curate.stale_after` (the Helm chart ships
`720h`) is closed like any other stale draft, with a comment explaining why — reopening it restores it
for review, nothing is discarded. `@runlore reinvestigate: …` is reserved and not supported yet — add
the `reinvestigate` label to the knowledge-base issue instead.

> [!IMPORTANT]
> **Enabling this requires exposing the agent to Slack.**
>
> Mentions arrive as HTTPS callbacks on **`POST /slack/events`**, so that endpoint must be reachable
> **from Slack's servers**: Slack **Event Subscriptions** enabled, Request URL set to
> `https://<your-runlore-host>/slack/events`, subscribed to the `app_mention` bot event (Slack verifies
> the URL with a signed challenge, so the endpoint must be reachable before you save). Route it through
> your ingress/gateway; if you use the chart's `networkPolicy.ingressFrom`, allow your ingress
> controller, not the internet. Startup **fails loud** unless `signing_secret_env` (every mention is
> HMAC-verified, same as feedback buttons), `bot_token_env` **with** `channel` (an incoming webhook
> returns no message ts, so there is no thread root to attribute a reply to) **and**
> `outcome.ledger_path` are all set — the thread registry that maps a Slack thread to its investigation
> is stored beside the ledger.

> [!WARNING]
> **That location surviving a restart or a leader failover depends on your deployment shape** — the
> Helm chart's default (`persistence.enabled: false`) is an `emptyDir`, **DESTROYED** on pod restart,
> upgrade, or leader failover, and taking the thread registry with it. Persistence alone is not enough
> either: `workloadKind: StatefulSet` with a `ReadWriteOnce` volume gives each replica its OWN copy, so
> a new leader taking over on a **different** replica still starts with an empty registry. Only
> `persistence.enabled: true` **with** `workloadKind: Deployment` **and** a `ReadWriteMany` accessMode
> puts the registry on the one volume every replica shares. `POST /slack/events` is leader-forwarded, so
> without that combination a restart or failover orphans every open thread: a human replying
> `@runlore note: …` to a thread delivered before it is told RunLore has no context for that thread,
> and the knowledge this feature exists to capture is lost.

> [!NOTE]
> **Mount the credential, too.** If `bot_token_env` names an env var that is present but **empty** (an
> unmounted secret, a blank Helm value), the Slack notifier is skipped entirely: no message is
> delivered, so no thread exists to capture knowledge in. Config validation cannot see runtime
> emptiness, so startup logs a **warning** instead of announcing the feature — grep the logs for
> `no bot-token delivery target resolved` after enabling it.

With the option off (the default), `@runlore note: …` mentions are ignored and the endpoint behaves
exactly as before (404 unless another feature wired it).

**🧵 Matrix thread capture — `thread_capture` (opt-in, default `false`).** The same feature as
Slack's thread capture, over Matrix: replying inside an investigation thread with
`@runlore note: <text>` writes what you know back into the knowledge base as a reviewed PR — a
comment on the finding's existing KB PR, or a small `Concept` entry PR when the finding has none, so
the knowledge still lands somewhere. A human reviews and merges it like every other entry.
`@runlore reinvestigate: …` is reserved and not supported yet. RunLore recognises being addressed via
MSC3952 `m.mentions`, or — as a fallback — the bot's full Matrix ID or localpart in the message body;
a reply is attributed to its thread via the MSC3440 `m.thread` relation (falling back to
`m.in_reply_to`). One thread registry backs both transports, so a thread capture note is written back
through the same responder Slack's uses (same per-hour PR-open budget, same `Concept`-entry fallback).

> [!IMPORTANT]
> **The `/sync` filter widens while this is on — said plainly, not softened.** With
> `thread_capture: false` (the default), RunLore's `/sync` filter requests only `["m.reaction"]`.
> With `thread_capture: true`, it widens to `["m.reaction","m.room.message"]`: **the process now
> receives message events** from the configured room, where before it received only reactions.
> RunLore acts only on messages that address it and are rooted in one of its own investigation
> messages — everything else is dropped immediately, its body never logged — but every message
> in the room does transit the process first. Unlike Slack's thread capture, **no exposed HTTP
> endpoint, no ingress change and no new permission is required**: this rides the same outbound
> long-poll RunLore already runs for `feedback_reactions`. See
> [security-model.md → Matrix thread
> capture]({{< relref "/docs/security/security-model.md#matrix-thread-capture-notifymatrixthread_capture--a-widened-sync-filter" >}})
> for the full trade-off against Slack's exposed endpoint.

> [!NOTE]
> **Mount the credential, too.** If `access_token_env` names an env var that is present but **empty**,
> the listener never starts: no message is delivered or received, and no knowledge can be captured.
> Config validation cannot see runtime emptiness, so startup logs a **warning** naming the empty env
> var instead of announcing the feature.

Startup fails loud unless `homeserver`/`room_id`/`access_token_env` and `outcome.ledger_path` are set
— the same requirement `matrix.feedback_reactions` has above. With the option off (the default), the
`/sync` filter is unchanged and `@runlore note: …` mentions are not read.

**🧵 Thread bounds — `notify.thread` (optional).** One block, shared by both transports, holding
every ceiling thread capture runs under. Omitting it reproduces the defaults exactly:
`max_notes_per_thread` (default **20**), `forge_writes_per_hour` (default **20**, global across both
transports), `max_note_bytes` (default **8192**, one human message's input), `registry_ttl` (default
**168h**), `registry_max` (default **2000**), plus `chat_calls_per_hour` and `chat_tokens_per_hour`
— which apply only with `model.chat` set, and are covered in full below.

One of those has a floor as well as a default: a positive `max_note_bytes` under 128 bytes is
refused at load. Below that the truncation marker — the visible mark saying a note was cut —
fills the whole budget on its own, so nothing of what the human typed is written while every
surface still reports the write as saved. Leave it at 0 to use the default.

The same block holds the one key that is a switch rather than a ceiling:
`announce_kb_updates` (default **false**). With it on, every knowledge write that lands is
also announced to your notifiers — naming the pull request, the entry, whose message produced the
note and which chat system they typed it in, and quoting the note itself. Where the note was
drafted by RunLore's chat model rather than typed after an explicit `note:`, the announcement
says so instead of attributing the words to the person who prompted it. The announcement carries note
content, so a note written in one thread reaches every sink you have configured; that is why it
is opt-in.

It takes a boolean or a destination name, and the two spellings you may already have keep their
meaning exactly:

| Value | Where the announcement lands |
|---|---|
| `false` (or omitted) | nowhere — no announcement |
| `true` | identical to `channel` |
| `channel` | each notifier's own channel or room, never the thread |
| `thread` | into the thread the note was typed in, on that transport; every other sink still gets its channel |
| `both` | the originating thread **and** every sink's channel |

`channel` is the right answer when people watch the channel but not every thread, and it is what
`true` has always done. `thread` is for a **single-transport** deployment: there the thread
already lives in the channel the announcement would post to, so a channel post restates what the
thread just said — an echo rather than a second audience. A typo (`treads`, `chanel`) fails
startup naming the accepted values, rather than quietly falling back.

**A sink that is not the originating transport falls back to channel level.** Only one sink can
reply into a given thread: a Matrix room cannot reply into a Slack thread, an incoming-webhook
Slack cannot reply into any thread, and a `webhook` endpoint has no thread at all. Those sinks are
not skipped — the echo `thread` removes exists only where the thread and the channel are the same
place, and a room that never saw the thread has no other way to learn the knowledge base moved.
The same fallback covers a thread RunLore can no longer address (a context rebuilt after a
restart): the write already landed, so it is announced at channel level rather than dropped.

See [Reviewing knowledge → Thread capture]({{< relref "/docs/concepts/reviewing-knowledge.md" >}})
and the [Slack]({{< relref "/docs/integrations/notifications/slack.md" >}}) /
[Matrix]({{< relref "/docs/integrations/notifications/matrix.md" >}}) pages.

### Conversational replies and what they cost

**Off by default.** Setting a `model.chat` block turns it on: RunLore answers an addressed thread
message that carries no recognised command prefix — a question, or a correction stated as ordinary
prose — with one model call, instead of the fixed "I can only record notes" reply. The presence of
the block *is* the switch; there is no separate boolean. It needs `thread_capture` on at least one
transport to ever fire, and startup **warns** (`… or this is dead config`) when it is set without one.

    model:
      chat:
        model: claude-haiku-4-5      # REQUIRED — never inherited
        max_tokens: 1024             # optional; default 1024, NOT model.max_tokens
        # provider, base_url, api_key_env, effort inherit from `model` when omitted.
        # thinking and pricing are accepted here but are INERT — see below.
    notify:
      thread:
        chat_calls_per_hour: 30      # default 30
        chat_tokens_per_hour: 109320 # default 109320 — derived, see below

> [!IMPORTANT]
> **`model.chat.model` must be named explicitly — startup fails without it.** It is the one field
> that does not inherit, and the exception is deliberate: `model.verify` runs once per investigation,
> on a path RunLore itself initiates, so inheriting the frontier model there is merely expensive.
> Chat runs once per addressed thread message, on a path **any channel or room member can trigger**,
> so a silently-inherited investigation model would make the cheapest way to enable the feature the
> most expensive way to run it. `max_tokens` likewise falls back to a fixed **1024**, not to
> `model.max_tokens`: a member-triggerable path staying cheap must not depend on how generously the
> investigation model happens to be capped.

> [!WARNING]
> **`thinking` and `pricing` are accepted on `model.chat` and do nothing.** Config load validates
> both — an invalid `thinking` mode or a negative rate is still rejected — so it is easy to read
> their acceptance as them taking effect. They do not:
>
> - **`model.chat.thinking`** (or a `model.thinking` inherited onto it) takes effect on **no
>   provider**. A chat reply is one call with a forced `submit_thread_reply` tool choice, which is
>   what makes it a single call instead of an agent loop; the Anthropic client gates thinking on an
>   *empty* tool choice, and the Gemini and OpenAI clients are never handed a thinking parameter at
>   all.
> - **`model.chat.pricing`** is read by **nothing**. Cost reporting — the notification footer and
>   `runlore_investigation_cost_usd` — covers the investigation and verify passes only. The chat path
>   emits `runlore_thread_chat_tokens_total` and stops there, so this is not merely "no cost ceiling":
>   there is **no cost report**. You cannot graph what conversational replies spent in dollars; do
>   that conversion yourself from the token counter.

**What costs a model call, stated plainly.** With `model.chat` set, **every addressed message that is
not a recognised command prefix costs exactly one model call** — one, structurally, not on average:
the model is offered no tool but `submit_thread_reply`, so there is no agent loop and no second call.
Knowledge-base context is pre-fetched in Go and pasted into the prompt rather than exposed as a
search tool, which is what preserves that bound.

What costs **nothing**:

- `@runlore note: <text>` — the deterministic capture path, unchanged, zero model calls.
- A bare mention with nothing after it — there is nothing to answer, so it is not a paid call.
- Any message that does not address RunLore, or is not rooted in one of its own messages.

**Who can trigger it.** On Slack, an `app_mention` event — a real mention of the app. **On Matrix,
"addressed" is looser and you should know it before enabling this:** MSC3952 `m.mentions` when the
client sends it, but *also* the bot's full MXID **or its bare localpart** (`runlore` in
`@runlore:example.org`) appearing in the message body **as a whole word**. No mention entity is
required. Any member of the room can therefore trigger a paid model call by typing the bot's name in
a reply to an investigation thread. Keep the room invite-only.

**What is capped — and the exact key that bounds each:**

| Bound | Key | Default |
|---|---|---|
| Model calls per hour, globally | `notify.thread.chat_calls_per_hour` | `30` |
| Tokens per hour, globally — reported usage, or an estimate when unreported | `notify.thread.chat_tokens_per_hour` | `109320` (derived) |
| Output tokens per call | `model.chat.max_tokens` | `1024` |
| The human's message reaching the model | `notify.thread.max_note_bytes` | `8192` bytes |
| Model calls per addressed message | *structural — always exactly 1* | — |
| Assembled prompt size | *fixed at ~15 KB (≈3.8k input tokens)*; only `max_note_bytes` moves it | — |
| Knowledge-base PR writes per hour, globally | `notify.thread.forge_writes_per_hour` | `20` |
| Notes recorded per thread | `notify.thread.max_notes_per_thread` | `20` |

Both hourly windows are global across every transport and every thread — there is one responder and
one budget for the process, not one per channel. Either ceiling refuses the call *before* it is made
and the message falls back to the deterministic reply; `runlore_thread_chat_denied_total` carries a
`ceiling` label (`calls` / `tokens`) saying which one refused.

**Why the token default is not a round number.** `chat_tokens_per_hour`'s default is *derived*, not
picked. It is the most a single call can cost — the assembled prompt's byte caps converted to
tokens, plus `model.chat.max_tokens` of output — multiplied by `chat_calls_per_hour`, then taken at
two thirds. That ordering is the point: a runaway of maximum-size calls trips the token ceiling
before it exhausts the call budget, so it is stopped by **cost** rather than by count, while ordinary
traffic (a question and a short reply) spends its calls without ever approaching it. The consequence
for you is that **the default moves when `max_note_bytes` moves**: raising the per-message cap raises
what one call can cost, and the derived hourly ceiling rises with it. Set `chat_tokens_per_hour`
explicitly if you need a ceiling that stays put.

> [!WARNING]
> **What is NOT capped.** These are real edges, not caveats to reassure you past:
>
> - **There is no ceiling in currency *on this path*.** Note the scope — it narrowed this release.
>   `investigation.max_cost_per_investigation` **is** a real ceiling now: RunLore compares an
>   investigation's accumulated estimated cost against it and stops the run (`reason="cost"` on
>   `runlore_investigation_budget_trips_total`), which is exactly what `model.pricing` makes
>   enforceable. None of that reaches the **chat** layer. `model.chat.pricing` is read by nothing at
>   all — there is no cost ceiling *and* no cost report for conversational replies, only the token
>   counter `runlore_thread_chat_tokens_total`. The chat layer's only spend ceilings are the call and
>   token counts above; translate them into money yourself at your provider's rate before enabling
>   this.
> - **The token ceiling runs partly on estimates, not only on reported usage.** Two cases. A call is
>   charged an estimate the moment it is *admitted*, before the provider has said anything, so that
>   calls running concurrently are visible to one another; when it returns, that reservation is
>   replaced by the provider's real number. And when a provider reports no usage at all, the estimate
>   stands (the request's measured input plus the full `max_tokens` output) and RunLore logs a warning
>   saying so. Estimates can only over-charge, which is the safe direction — but a ceiling running on
>   them is not a measurement, and you should be able to see it in the logs.
> - **A call that dies mid-flight holds its reservation until the window slides.** If a call is
>   admitted and then never records — a panic, a dropped context — nothing rolls the reservation back;
>   it ages out an hour later like any other entry. A burst of those can refuse legitimate traffic for
>   the rest of that hour.
> - **Retries are charged, not bounded.** The reservation covers one upstream attempt. A call the
>   provider client retried is charged for the extra attempts only once it returns, so calls already
>   in flight when the ceiling is reached can push the window past it.
> - **The window is per process.** Each replica enforces its own `chat_tokens_per_hour`, so N replicas
>   permit N times the number above.
> - **It is an hourly sliding window, not a monthly or absolute budget.** `chat_tokens_per_hour` at
>   its default permits 109320 tokens *every* hour, indefinitely. There is no cumulative cap over a
>   day, a month, or the life of the process.
> - **Any room member can trigger it** (see above), so the ceiling that actually protects you is the
>   hourly one, not the good behaviour of the person typing.

Every failure degrades rather than escalating: a model error, a refusal, a truncated response, a
malformed tool call, an exhausted budget or an empty credential all fall back to the deterministic
capture path, so a human's message is never lost to a model outage and never answered with silence.
The model can propose note *content* but never chooses where it is filed — routing stays derived from
the thread's investigation context alone, and a proposed note is written through the same per-thread
cap and the same `forge_writes_per_hour` window an explicit `note:` uses.

### Generic templated notifier (`notify.templated`)

Deliver findings to **any** webhook-speaking service — Microsoft Teams, Discord,
ntfy, incident.io — with one config block and no Go. Each instance renders a Go
`text/template` over the delivery payload (the same fields the `notify.webhook`
JSON carries) and POSTs it. Findings are secret-redacted **before** any notifier
runs, so templates only ever see redacted data. A template that fails to parse
refuses startup; a template that fails at delivery time is logged and skipped
without blocking other channels. Rendered bodies are capped at 256 KiB.

Worked example — Microsoft Teams (Incoming Webhook / MessageCard):

    notify:
      templated:
        - name: teams
          url_env: RUNLORE_TEAMS_WEBHOOK_URL
          template: |
            {
              "@type": "MessageCard", "@context": "https://schema.org/extensions",
              "summary": {{ toJSON .Title }},
              "themeColor": "d63333",
              "title": {{ toJSON (printf "[%s] %s (%.0f%%)" .Verdict .Title (mulPct .Confidence)) }},
              "text": {{ toJSON .Text }}
            }

Template functions: `toJSON` (escaping-correct JSON splicing — always use it for
values inside JSON bodies) and `mulPct` (×100 for percent display).

**Two config surfaces, one registry.** Built-in notifiers (`notify.slack`,
`notify.matrix`) use typed config blocks with startup validation; drop-in
notifiers (`notify.webhook`, `notify.templated`) live under the same `notify:`
key as self-describing blocks. Both build through the same registry — the split
is deliberate: type-checked config for the built-ins, zero-code extensibility
for everything else.

### `server` — the HTTP listener
Only `webhook_token_env` (the bearer token for the incident webhook). The listen address is the
`--addr` CLI flag (`:8080` in the chart), **not** a config key. TLS is terminated externally
(ClusterIP + NetworkPolicy).

**`webhook_token_env` is mandatory once any model is configured** — the `serve` path fails closed:
it refuses to start with an anonymous alert webhook when an LLM is wired (the webhook's
labels/annotations flow verbatim into the LLM prompt and bill the model), regardless of
`actions.mode`. It is also mandatory under `actions.mode=auto` (enforced by `config.Validate`). It
is warning-only *only* for the model-less log-only investigator (no model configured). If
`sources.alertmanager` is enabled and this is left unset with a model configured, startup fails; if
left unset without a model, startup logs a warning — the webhook stays open on purpose for
cluster-internal traffic, but the risk should never be silent.

### `rbac` — chart-only (not in the agent config)
Set under `values.rbac.*`, not `values.config`: `controllerLogNamespaces` (default `[flux-system]` —
where `pods/log` is granted, namespaced; the app-layer `config.investigation.pod_log_namespaces`
allowlist **auto-tracks this value** unless overridden, so RBAC scope and app guard never drift),
`allowActions` (gate for the patch Role), `actionNamespaces` (the patch allowlist — **must mirror
`config.actions.allow.namespaces`**). See [Security model]({{< relref "security-model.md" >}}).

### `mcp` — external MCP tool servers (opt-in)

RunLore can call tools advertised by external [Model Context Protocol](https://modelcontextprotocol.io)
servers over streamable-HTTP (JSON-RPC 2.0). MCP is **opt-in** — the default empty `servers` list
disables it entirely.

```yaml
mcp:
  servers:
    - name: mydb           # short identifier; namespaces all tools as mydb__<tool>
      url: https://mcp.example.com/mcp
      token_env: MYDB_MCP_TOKEN   # optional — env var holding a bearer token
      headers:                    # optional extra request headers
        X-Tenant: my-org
```

**Key behaviours:**

- **Namespaced names.** Every remote tool is registered as `<server>__<tool>` (e.g. `mydb__query`),
  so MCP tools never collide with RunLore's built-in tools. Built-in names always win on collision.
- **Read-only.** The MCP adapter only calls `tools/call`; it never mutates RunLore state.
- **Failure-isolated.** A server that fails the `initialize` handshake or `tools/list` is logged at
  Warn and skipped — RunLore starts the investigation loop with the remaining tools rather than
  aborting. Fix the server and restart to pick it up.
- **Secrets by indirection.** `token_env` names the environment variable — never embed the token
  value directly in config. Wire it from a Kubernetes `Secret` (`env`/`envFrom`).
- **`headers` are not secret-safe over plain HTTP.** Only `token_env` is checked at config
  validation time; custom `headers` values are not. Do not carry secrets in `headers` when `url`
  is plain `http://` to a public host — use `headers` for non-secret metadata only (e.g. `X-Tenant`).
  Use `https://` whenever the server is on a public network.

### `curate` — Phase-2 backlog groomer
- `stale_after` — close unprotected KB PRs idle longer than this; **code default `0` (disabled)** —
  the Helm chart ships `720h` (30 days). `0`/unset disables stale-close.
- `recurrence_threshold` — open a knowledge-gap issue after this many unresolved occurrences of a
  pattern; **default `3`**. A knowledge-gap issue flags patterns RunLore keeps encountering without
  resolving — a signal to write a runbook.
- `retirement` — the **opt-in** KB retirement pass: opens a human-reviewed *retire* PR that stamps
  `status: retired` into a **merged** entry whose outcome track record has sustainably decayed. It
  never merges or deletes (a human is the gate; the entry stays in git history), is idempotent, and
  respects a human veto — a retire PR closed without merging is never re-proposed. Keys:
  - `enabled` — **default `false`**. When off, the pass is not wired at all (default behavior
    unchanged); the other defaults are only applied once enabled.
  - `min_observations` — the sustained-decay bar: total observations (recalls + 👍 + 👎) an entry
    must have before retirement is even considered, so a single bad recall can't retire it;
    **default `3`**, must be `>= 1`.
  - `floor` — retire when the entry's outcome factor drops below this; **default `0.5`**, must be in
    `(0,1]`. Mirrors recall's `catalog.instant_recall.outcome_floor` so the two gates agree.
  - `prior` — Beta prior strength `k` for the decay formula; **default `2.0`**. Mirrors recall's
    `catalog.instant_recall.outcome_prior` — keep them equal unless deliberately tuning the gates apart.
- `sweeps` — the **in-server** scheduled grooming loop (leader-only; the serve pod runs the same
  passes as `lore curate` on a timer, over its live outcome ledger). Strictly additive: it only
  starts when the KB forge (`forge.kb_repo` + `forge.github_app`) is configured. Keys:
  - `mode` — `dry-run` (**default**, also when empty): log + audit every candidate action, write
    nothing to the forge; `apply`: act; `off` (quote it in YAML): disable the loop. Unknown values
    fail validation — a typo must not silently demote grooming to dry-run.
  - `interval` — sweep cadence; **default `6h`**, must be `>= 10m`. The first sweep runs one full
    interval after startup (leadership flaps never trigger immediate re-sweeps).

  Every write (or dry-run skip) is appended to the `actions.audit_log_path` hash chain as
  `actor: curate` with `op` `kb.close` / `kb.comment` / `kb.relabel` / `kb.open-issue` /
  `kb.retire-pr` and decision `executed` / `dry-run` / `failed`.

### `gitops.mirror` — persistent what_changed clone mirror
`what_changed` diffs a GitOps source repo between two revisions. By default it now keeps a
persistent **bare mirror** per repo and fetches incrementally, instead of a full clone on every
call — repeated investigations on the same (mono)repo reuse one on-disk mirror. Full history is
preserved (the history walks behind the `#239` fallback and time-window enumeration keep working).
- `enabled` — **default `true`**. Set `false` to restore the legacy clone-per-call behavior
  (the escape hatch). A mirror error at runtime already falls back to clone-per-call on its own, so
  `what_changed` never gets *worse* because a mirror misbehaved.
- `dir` — mirror root. **Default `<tmpdir>/runlore-mirrors`** (ephemeral: wiped on pod restart).
  Point it at a **PersistentVolume** to keep mirrors warm across restarts.
- `max` — maximum mirrors kept on disk; **default `10`**. When exceeded, the oldest-mtime mirror is
  evicted. Must be `>= 0` (`0` = use the default).

### `source_repos` — source-repo allowlist for `source_diff`

`what_changed` stops at the manifest layer ("image `v1.2.2 → v1.2.3`"). Listing source repos
here gives the agent a `source_diff` tool that reads the actual change behind such a bump:
commit subjects, a per-file diffstat, and the largest hunks between the two versions —
turning "the deploy correlates with the alert" into "commit `a1b2c3` raised the DB pool
size, which matches the connection exhaustion". **Unset (default) ⇒ the tool is not
registered.**

```yaml
source_repos:
  allow:
    - github.com/acme/*              # every repo directly under the org
    - gitlab.com/acme/infra-modules  # or exact host/org/repo
```

- `allow` — patterns the model may diff, `host/org/repo`-shaped with per-segment globs
  (`*` never crosses `/`). Matching is enforced server-side **before any network call** —
  the model can only make RunLore clone repos you listed, whatever it writes.
- **Auth — grant the forge GitHub App access to each private source repo.** `source_diff`
  clones with the **forge GitHub App installation token**, so for every **private GitHub**
  repo in `allow` the App must be **installed on that repo with `contents: read`** — an
  installation token is scoped to the repos the App is installed on, so a private repo that
  is not granted fails to clone (`404`/`repository not found`) even though the entry is
  correct. Add it under the App's **Repository access** (or check with
  `gh api /installation/repositories`). The token is confined to the forge's own host — a
  repo on any other host (e.g. a `gitlab.com/...` entry) is cloned **anonymously**, so the
  GitHub token is never transmitted off-host; public GitHub repos need no grant (an
  installation token reads any public repo regardless of scope) and private non-GitHub hosts
  are not supported yet. Because the model chooses which allowlisted repo to
  diff, keep the allowlist **and** the App's installation scope no broader than the source you
  intend RunLore to read — a wide `github.com/org/*` glob plus an org-wide install lets the
  agent diff any repo in that org.
- **Repo selection is done by the model.** For Terraform/module bumps the repo URL is in
  the GitOps diff, so it is exact; for images it name-matches against your allowlist (a
  wrong guess fails at ref resolution — the tag won't exist — and the error lists nearby
  tags).
- **Token cost is bounded in code:** the default response is commit subjects + diffstat +
  the biggest hunks (~8 KiB); the model zooms into specific files with `paths` (~16 KiB).
  Generated/vendored files (lockfiles, `vendor/`…) are listed in the diffstat but their
  hunks are skipped unless zoomed. Mirrors reuse the `gitops.mirror` settings (a `source/`
  subdir of the same root).
- **First-call clone latency:** the very first `source_diff` call on a large repo performs
  a full clone that must complete within `investigation.tool_timeout` (default 60 s) — raise
  it if large-repo clones exceed that budget. Subsequent calls reuse the warm mirror and
  only fetch new refs.
- **Bounded blast radius:** one investigation may clone at most 10 distinct repos, and a
  single diff is capped in memory (files past a large bound are counted but omitted, with a
  note telling the model to narrow the version range) — so a very large diff or a wandering
  loop can't exhaust the agent.
- **Local-path patterns are for dev/test only.** A pattern beginning with `/` (e.g.
  `/tmp/fixtures/*`) matches a local filesystem path and makes RunLore diff a local git
  repo. This exists for local development and the test suite; **do not** use local-path
  patterns in a deployed cluster — list only remote `host/org/repo` patterns there.

### Other top-level keys
`gitops.engine` (`flux` default · `argocd`), `cloud` (`provider: aws`, `region`, `cluster_name`),
`network` (pluggable: `hubble` · `aws-vpc-flow-logs` · `gcp-firewall-logs`),
`metrics`/`logs` — `Endpoint` for the PromQL/logs query tools: `url` (base URL), optional
`token_env` (env var name for a bearer token — `Authorization: Bearer <token>` on every request),
optional `headers` (static request headers, e.g. `X-Scope-OrgID: <tenant>` for multi-tenant
backends; **not secret-safe over plain HTTP** — use `https` for public hosts),
`telemetry` (`metrics_enabled`, `otlp_endpoint`),
`logging` (`format: text|json`, `level`), `leader_election` (`enabled`, `name`).

`model.max_tokens` — caps the model's output (generated) tokens per request; **`0` = use the 8192
default**. Streaming providers send it (`Anthropic max_tokens`, `OpenAI max_tokens`, Gemini
`generationConfig.maxOutputTokens`); a too-low value truncates. Give extra headroom when using
`thinking: adaptive` — thinking blocks consume output tokens.
