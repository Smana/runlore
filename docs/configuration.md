# Configuration reference

A navigable map of RunLore's configuration, organized by subsystem.

> [!note] `values.yaml` is the authoritative reference
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
- `incidents.debounce` — hold a firing alert this long before investigating, and skip it if a matching
  Alertmanager `resolved` webhook arrives within the window (self-resolving noise, e.g. a
  `KubeDaemonSetRolloutStuck` during a Karpenter node-churn cycle). **Default `0`** (investigate
  immediately — today's behavior); opt-in per deployment. Composes with `coalesce` (survivors are
  batched afterwards) and `dedup` (re-fires are still suppressed before the hold begins).
- `gitops_failures.debounce` — require a failure to persist this long before investigating. **Default
  60s**; explicit `0` fires immediately on every `Ready=False`.

### `investigation` — loop bounds & noise control
- `coalesce` — fold an alert storm into one investigation. Defaults when enabled: `debounce` **30s**,
  `max_wait` **2m**, `max_batch` **50**, `cooldown` **10m**; `correlation_labels` group related alerts.
- `rate_limit` — `max_per_window` (**0 = unlimited**), `window` (default **1h** when a budget is set),
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
- **Fail-closed validation:** `approve`/`auto` require `approval_token_env`; `auto` *additionally*
  requires `audit_log_path`, `server.webhook_token_env`, `auto.min_confidence > 0`,
  `auto.max_per_window > 0`, and a non-empty `allow.namespaces`. See [Security model](security-model.md).

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

> [!important] Enabling this requires exposing the agent to Slack.
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
Only `webhook_token_env` (the bearer token for the incident webhook; **required under
`actions.mode=auto`**). The listen address is the `--addr` CLI flag (`:8080` in the chart), **not** a
config key. TLS is terminated externally (ClusterIP + NetworkPolicy). If `sources.alertmanager` is
enabled and this is left unset, startup logs a warning (louder under `actions.mode=approve`) — the
webhook stays open on purpose for cluster-internal traffic, but the risk should never be silent.

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

### Other top-level keys
`gitops.engine` (`flux` default · `argocd`), `cloud` (`provider: aws`, `region`, `cluster_name`),
`network` (pluggable: `hubble` · `aws-vpc-flow-logs` · `gcp-firewall-logs`), `metrics`/`logs`
(`Endpoint{url}` for the PromQL/logs query tools), `telemetry` (`metrics_enabled`, `otlp_endpoint`),
`logging` (`format: text|json`, `level`), `leader_election` (`enabled`, `name`).
