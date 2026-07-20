# Configuration reference

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
incident webhook. Known keys: `alertmanager`, `gitops`, `pagerduty`.

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
  on the trigger** (the feedback re-arm, see [learning-loop](learning-loop.md) — batched on the
  normal `debounce`, so a contested storm still collapses to one re-investigation). Suppressions log
  an INFO line and count in `runlore_alerts_suppressed_total`.
- `rate_limit` — `max_per_window` (**default 30**; an explicit **0 = unlimited**), `window` (default **1h**),
  `max_requeues`.
- `recurrence_cooldown` — **opt-in (default 0 = off)** per-trigger suppression: skip re-investigating a
  trigger whose previous investigation completed less than this long ago, **concluded** (verdict ≠
  `inconclusive`), and has **no standing 👎 feedback**. Without it, nothing keys on the trigger before
  the paid loop: a still-firing alert re-investigates on every Alertmanager `repeat_interval` and a
  persistently-failing GitOps resource on every informer resync (**~10 minutes**) — each a full model
  run re-delivering the same answer as fresh noise. A suppressed occurrence costs nothing and says
  nothing (no model call, no notification, no ledger open — the previous notification remains *the*
  answer); the next occurrence past the cooldown re-investigates in full. Two human-deferential
  escape hatches: an inconclusive prior never suppresses (there is no answer worth repeating), and a
  👎 on the previous message re-arms investigation immediately (see
  [`notify.slack.feedback_buttons`](#notify--where-findings-go)). Requires `outcome.ledger_path`
  (fails loud at startup without it). A sensible production value is `30m`–`1h`. Suppressions show up
  as `runlore_investigations_completed_total{result="recurrence_suppressed"}`.
- `timeout` — per-investigation deadline, **default 10m** (bounds a hung tool/clone so it can't starve
  the single-worker queue).
- `tool_timeout` — per-**tool**-call timeout, **default 60s** (0 = use the default). Bounds a single
  hung/slow provider (a stuck git clone, an unresponsive metrics/logs endpoint) so it can't eat the
  whole per-investigation budget; on expiry the tool result is a non-fatal "timed out" note and the
  investigation continues.
- `max_steps` (**default 20**), `max_tool_output_bytes` (0 = unlimited), `max_tokens_per_investigation`
  (0 = unlimited, else a hard token ceiling).
- `compaction` — how mid-loop history compaction treats the older tool outputs it elides once the
  estimate crosses the compaction target (0.7× `max_tokens_per_investigation`). **`elide`** (default)
  drops their bodies for short markers (lossy). **`summarize`** first asks a model for **one** compact
  factual digest of the elided batch (per compaction event) — "preserve identifiers, timestamps, error
  strings, counts; no speculation" — and keeps that, clearly labelled, in place of the markers.
  Routed to the `model.verify` tier when configured (cheaper), else the main model. **Fail-safe:** any
  summarizer error, refusal, or truncation falls back to plain elision — a compaction failure never
  loses the investigation. The digest is derived only from the already-redacted tool outputs, so it
  adds no new egress path. Requires `max_tokens_per_investigation > 0` (compaction is off without a
  budget). Metrics: `history_summarizations_total`, `history_summarize_fallbacks_total`; the digest
  call's token usage is counted into `model_input_tokens_total`.
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
  narrow beyond the RBAC scope. See [Security model → least-privilege RBAC](security-model.md).

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
  saves the ~100k of a full investigation when it fires. False-recall guards: it only ever ranks
  candidates that already passed the structural filter, ignores any `entry_id` it did not offer
  (hallucination guard), and fails **safe** on a "no match", a low confidence, or a model error (fall
  through to a full investigation). Off ⇒ the BM25-magnitude gate is unchanged. The recalled answer
  still goes through live-state confirm + the adversarial verify pass, exactly as before — the reranker
  is a *retrieval-time* "which candidate + confident enough to short-circuit" decision, not a second
  verify.
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
recall decay; **must be on the PVC** to compound (see [Upgrade & Uninstall](upgrade-uninstall.md)).
Also records the human 👍/👎 ratings when `notify.slack.feedback_buttons` is enabled (see
[`notify`](#notify--where-findings-go) below).

### `actions` — the autonomy ladder (off by default)
- `mode` — `off` (default) · `suggest` · `approve` · `auto` (experimental, frozen/not recommended).
- `allow` — the envelope: `reversible_only`, `namespaces` (allowlist — **empty permits nothing**),
  `protected_namespaces` (added to built-in `flux-system`/`kube-system` denies), `max_blast_radius`,
  `kinds`.
- `require_approval` + `approval_token_env`, `audit_log_path`, and `auto.*` (`dry_run`,
  `min_confidence`, `max_per_window`, `window`).
- **Fail-closed validation:** `approve`/`auto` both require `approval_token_env` **and**
  `audit_log_path` (both executing rungs mutate the cluster, so both must be audited — the hash chain
  is verified fail-closed on open); `auto` *additionally* requires `server.webhook_token_env`,
  `auto.min_confidence > 0`, `auto.max_per_window > 0`, and a non-empty `allow.namespaces`. See
  [Security model](security-model.md).

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

### `forge` — the Git host for curation
`kb_repo` (`owner/name`), `base_branch` (default `main`), `github_api_url` (default
`https://api.github.com`), `dup_score` (default **5.0**), `min_confidence` (default **0.75**, the
quality bar below which a finding is chat-only), `skip_verdicts` (default **empty** — draft every
verdict). `github_app` — `app_id`, `installation_id`, and `private_key_ref` **or** `private_key_env`.

`skip_verdicts` is a list of investigation verdicts that must **not** draft a KB PR — the finding
still reaches chat, but no repo artifact is created. Values are validated at startup against the
verdict enum (`no_action` / `action_suggested` / `action_required` / `inconclusive`); an unknown
value fails fast. Empty (the default) preserves the original behaviour: every verdict is eligible.
Recommended production value is `skip_verdicts: ["no_action"]`, which keeps benign / self-healed /
synthetic findings out of the review queue while still notifying chat (see
[reviewing-knowledge.md](reviewing-knowledge.md#expected-triage-volume)).

### `notify` — where findings go
`slack` (`webhook_url_env` or `bot_token_env`, `channel`, `signing_secret_env`, `approver_ids`,
`feedback_buttons`), `matrix` (`homeserver`, `room_id`, `access_token_env`), plus inline blocks for any
registered notifier (e.g. `webhook` with `url_env`).

Every notifier now leads with the model's **verdict** (`no_action` / `action_suggested` /
`action_required` / `inconclusive`) and carries the trigger-time alert metadata (severity, environment,
cluster, tenant, alert name, `startsAt`), recurrence facts (occurrence count, previous KB link), the
top-cause "why", suggested next steps, **ruled-out** hypotheses and **data gaps** (tool/data
limitations, kept distinct from human-only open questions):

- **Slack, bot token (`bot_token_env`).** Posts a compact verdict-first summary to `channel`, then the
  full analysis as a **threaded reply**. When `signing_secret_env` is set and action mode is `approve`,
  the summary carries Approve/Reject buttons on any suggested remediation (see below).
- **Slack incoming webhook (`webhook_url_env`), Matrix, generic webhook.** Deliver the same content as a
  **single** message (incoming webhooks cannot thread and expose no interaction buttons).

**Generic webhook JSON payload** gained `verdict`, `severity`, `environment`, `cluster`, `tenant`,
`alert_name`, `started_at` (RFC3339, empty when unknown), `occurrences`, `prev_curated_url`, `ruled_out`
and `data_gaps` alongside the existing `title`/`confidence`/`curated_url`/`text` fields (all
`omitempty`).

**👍/👎 feedback buttons — `feedback_buttons` (opt-in, default `false`).** When enabled, Slack
investigation messages carry two buttons ("👍 Accurate" / "👎 Off-base") so the on-call can rate the
diagnosis in one click. Ratings land in the **outcome ledger** and weigh the recalled entry's trust
exactly like resolve signals do — enough 👎 and the entry falls below the recall floor and RunLore
re-investigates instead of reusing it (see
[learning-loop.md §6](learning-loop.md#6-the-feedback-edge--outcome-driven-decay-what-makes-it-learn)).
This is the primary trust signal for incidents that have **no resolve channel** (GitOps failures).

> [!IMPORTANT]
> **Enabling this requires exposing the agent to Slack.**
>
> Button clicks arrive as HTTPS callbacks on **`POST /slack/interactions`**, so that endpoint must be
> reachable **from Slack's servers** (a public Interactivity *Request URL* on your Slack app — the same
> endpoint and the same exposure approve-mode buttons use; if you already run `actions.mode: approve`
> with Slack buttons, nothing new is exposed). Route it through your ingress/gateway; if you use the
> chart's `networkPolicy.ingressFrom`, allow your ingress controller, not the internet. Startup **fails
> loud** unless both `signing_secret_env` (every click is HMAC-verified, ±5 min replay window) and
> `outcome.ledger_path` (where ratings land) are set.

Feedback is deliberately **unprivileged**: any signature-valid member of your workspace can rate — a
rating is an opinion feeding the learning loop, not a cluster mutation (approve/reject keep their
`approver_ids` allowlist). Anti-gaming lives in the ledger: **one live vote per (trigger key, Slack
user)**, latest wins — duplicate clicks are idempotent and changing your mind moves the vote. The ack
is an ephemeral "feedback recorded" note visible only to the clicker; the investigation message is
never modified. With the option off (the default), no buttons render and the endpoint behaves exactly
as before (404 unless approve mode wired it). Exposure hardening and the vote trust model are
detailed in [security-model.md](security-model.md#the-feedback-channels--exposure--trust-model).

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
[security-model.md](security-model.md#the-feedback-channels--exposure--trust-model)).

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
`config.actions.allow.namespaces`**). See [Security model](security-model.md).

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
- `stale_after` — close unprotected KB PRs idle longer than this; **default `720h`** (30 days);
  `0` disables stale-close.
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
