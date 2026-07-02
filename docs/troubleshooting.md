# Troubleshooting

How to diagnose the most common *"why didn't RunLore do X?"* situations. RunLore exposes
two diagnostic channels:

- **Structured logs** — every decision is a one-line `slog` event. Set `logging.format: json`
  (or `text`) and `logging.level: debug` for the most detail. Each section below quotes the exact
  `msg=` value to grep for.
- **Metrics** — `runlore_*` Prometheus series, exposed when `telemetry.metrics_enabled: true`. See
  [Observability](observability.md) for the full catalog and a Grafana dashboard.

> [!note] Leader-only by design
> With `leader_election.enabled` (the chart default, 2 replicas) **only the leader investigates**. A
> standby logs `msg="standby; another replica leads"` and stays at `/readyz` 503 for its whole life —
> that is intentional, not a fault. `runlore_leader == 1` marks the elected pod.

---

## An alert fired but no investigation started

By far the most common case. Work from ingress inward.

**1. Is there a per-incident decision line?** Every *admitted* alert produces exactly one log event:

```
msg=incident alert=<name> severity=<sev> namespace=<ns> investigate=<bool> reason="<reason>"
```

| `reason` | meaning | what to do |
|---|---|---|
| `matched trigger policy` | admitted → investigating | nothing — this is the happy path |
| `filtered by trigger policy` | didn't match `triggers.incidents.match`, or hit `triggers.incidents.ignore` | widen `match` (check `severity`, `environment`, `namespaces` globs, `labels`); check the alert isn't in `ignore.alertnames` |
| `deduplicated (still-firing)` | the same alert is already under investigation within `triggers.incidents.dedup.window` | expected — wait for the window to pass or the alert to resolve |

> Trigger filtering and dedup are **not** counted by any metric — the `incident` log line is the only
> place they surface. Grep it first.

**2. No `incident` line at all?** The alert never reached the trigger pipeline:

- **Source not enabled.** The webhook only mounts when `sources.alertmanager: {}` is set. A typo such
  as `alertmanagr:` now **fails startup** with
  `unknown source(s) [alertmanagr] under \`sources:\` — known sources are [alertmanager gitops]`
  (older builds silently did nothing). Check the startup logs.
- **Webhook rejected at ingress.** If `server.webhook_token_env` is set (mandatory under
  `actions.mode=auto`), Alertmanager must send `Authorization: Bearer <token>`. A `401` means the
  token is missing or wrong. The request body is also capped at 1 MiB.
- **Metric cross-check.** `runlore_alerts_received_total` counts alerts that passed initial decoding
  and `Decide`. Flat while alerts are firing ⇒ they're being rejected at ingress (auth/parse) or the
  source isn't mounted.

**3. Admitted, but still nothing ran?** Compare `runlore_investigations_started_total` against
`runlore_alerts_received_total`, then:

| signal | meaning | fix |
|---|---|---|
| `runlore_leader == 0` on all pods | no leader elected → nothing runs | check the `leases` RBAC and `leader_election` config; look for `msg="acquired leadership"` |
| `runlore_investigations_throttled_total` rising | rate limiter engaged (`investigation.rate_limit`); `msg="investigation rate limit engaged; throttling…"` | raise `rate_limit.max_per_window` / `window`, or accept the budget |
| `runlore_investigations_dropped_total` rising | dropped by `rate_limit.max_requeues` or the token-budget hard-stop | see the timeout/budget section below |
| `runlore_alerts_coalesced_total` rising | folded into an existing batch (`investigation.coalesce`) | expected noise control — one investigation covers the batch |
| `runlore_alerts_suppressed_total` rising | dropped by the coalescer **cooldown** | expected — a recently-investigated correlation is in cooldown |
| `runlore_incidents_debounced_total` rising | the alert self-resolved within `triggers.incidents.debounce` and was dropped before investigating; log: `msg="alert resolved within debounce window; dropping self-resolving incident"` | expected noise control — lower `incidents.debounce` if you want faster (but noisier) reactions, or set `0` to disable |

---

## A GitOps failure didn't trigger an investigation

The GitOps-failure watcher (`sources.gitops: { enabled: true }`) **debounces** before firing, to
filter reconcile-churn transients:

- `runlore_gitops_failures_debounced_total` rising ⇒ the failure cleared within the debounce window
  and was dropped as transient. Log: `msg="gitops-failure cleared within debounce window; dropping transient"`.
- Tune with `triggers.gitops_failures.debounce` (default **60s**; explicit `0` fires immediately on
  every `Ready=False`).

---

## The investigation ran but timed out / came back empty

Check `runlore_investigations_completed_total{result=…}` — the `result` label tells you how it ended:

| `result` | meaning | log line | lever |
|---|---|---|---|
| `resolved` / `unresolved` | finished; `unresolved` = honest "couldn't determine" | `msg="investigation complete"` | — |
| `recall` | answered instantly from the catalog | `msg="instant recall (catalog hit; skipping the loop)"` | — |
| `timeout` | hit `investigation.timeout` (default **10m**) | `msg="investigation hit per-investigation deadline"` | raise `investigation.timeout`; check for a hung tool/provider |
| `budget_exceeded` | hit `investigation.max_tokens_per_investigation` | `msg="investigation hard-stopped at token budget"` | raise the budget, or accept the cap |
| `max_steps` | hit `investigation.max_steps` (default **20**) | `msg="investigation hit max steps"` | raise `max_steps`, or the loop is looping — inspect tool calls |
| `inconclusive` | model never called `submit_findings` after a nudge | `msg="investigation inconclusive (no submit_findings after nudge)"` | often a weak/over-quantized model; try a stronger one |
| `error` | a tool or model call failed | `msg="investigation failed; retrying"` | inspect the `err=` field |

Supporting metrics: `runlore_tool_calls_total{tool,result}` and `runlore_model_requests_total{provider,result}`
(watch the `result="error"` slice), `runlore_model_responses_truncated_total` (completions cut off at the
output-token ceiling — a frequent cause of `inconclusive`), and `runlore_investigation_duration_seconds{result}`.

---

## The curator didn't open a PR

RunLore files a KB pull request only for **novel, confident** findings — by design it does *not* file
for everything. Check `runlore_curations_total{kind="pr",result=…}` and the curator log:

| situation | log line | this is… |
|---|---|---|
| recalled answer (cache hit) | `msg="skipping curation of a recalled finding (cache hit, not novel)"` | expected — not novel |
| below the quality bar | `msg="finding below the quality bar; chat-only, no KB artifact"` | expected — `confidence < forge.min_confidence` (default 0.75) |
| duplicates a catalog entry | `msg="finding duplicates a catalog entry; not filing"` | expected — within `forge.dup_score` |
| coalesced onto an open PR | `msg="finding coalesced onto an open PR"` (`result="coalesced"`) | expected — added to an existing PR |
| **opened** | `msg="curated as PR"` with the `url` | success |
| **error** | `msg="curate findings"` with `err=` (`result="error"`) | a forge/GitHub-App problem — check App scopes & `forge.kb_repo` |

If you expected a PR and got `chat-only` or `duplicates`, the finding simply wasn't novel/confident
enough — tune `forge.min_confidence` / `forge.dup_score` if the thresholds are wrong for you.

> Knowledge-gap **issues** (opened by the separate `lore curate` recurrence agent, not the live
> curator) are **not** counted by any metric — they only log `msg="opened knowledge-gap issue"`.

---

## Recall never fires (every incident runs the full loop)

Instant recall requires `catalog.instant_recall.enabled: true` **and** a confident catalog hit. Check:

- `runlore_recall_hits_total` is zero, and `runlore_recall_rejections_total{reason=…}` shows why
  candidates were rejected:
  | `reason` | meaning | lever |
  |---|---|---|
  | `no_resource_match` | top hit didn't match the incident's workload | `instant_recall.require_workload_match` |
  | `low_margin` | top hit too close to the runner-up (ambiguous) | `instant_recall.margin_gap` (default 1.0) |
  | `low_outcome` | the entry's real-world resolve-rate decayed below the floor | `instant_recall.outcome_floor` (default 0.5) |
- `runlore_recall_score` (BM25 at the decision point) sitting below `instant_recall.min_score`
  (default 1.0) ⇒ the catalog has no strong match yet. Recall **compounds** — it improves as merged
  PRs accrete. A cold catalog legitimately won't recall.
- Decision detail is logged at `msg="instant recall decision"` with `score`, `margin`, `confidence`.
- A recalled answer that fails the adversarial verify pass falls through to a full investigation:
  `msg="instant recall rejected by verify; running full investigation"`.

---

## Findings were investigated but never delivered to chat

> [!warning] Delivery has no metric — logs only
> There is currently **no** `runlore_*` counter for notifier delivery. A failed send logs
> `msg="delivery failed" err=…` (the Slack/Matrix/webhook fan-out is best-effort and joins errors).
> Successful sends are **not** logged. Grep `msg="delivery failed"` and `msg="deliver findings"`.

Common causes: wrong `notify.slack.channel` / bot-token scope, an invalid Matrix `room_id` /
`access_token`, or a generic-webhook endpoint returning non-2xx. At startup, `msg="delivery notifiers"`
with `count=` confirms how many sinks were wired — `count=0` means none are configured.

---

## `/readyz` never goes green

`/readyz` is gated by **leadership AND catalog warmth** (`internal/app/runtime.go`). It returns `503`
("`not ready`") until the pod both leads *and* has completed its first catalog index/sync.

- A **standby** is `503` for its entire life — by design (probes target `/healthz`, not `/readyz`,
  so a standby isn't killed). Only the leader should be `Ready`.
- The **leader** stuck at `503` ⇒ the catalog never warmed: check `catalog.dir` / `catalog.git`
  (clone failing? token wrong?) and the startup logs. `runlore_catalog_invalid_entries_total` rising
  ⇒ malformed OKF entries at load.
- The `startupProbe` allows ~60s of warm-up; a slow first clone can exceed it — raise the chart's
  `startupProbe.failureThreshold` if needed.

---

## Quick reference — metric → meaning

The most useful series for triage (full list in [Observability](observability.md)):

| metric | use it to see… |
|---|---|
| `runlore_alerts_received_total` | alerts that passed ingress + `Decide` |
| `runlore_investigations_started_total` | investigations actually begun |
| `runlore_investigations_throttled_total` / `_dropped_total` | rate-limit / budget pressure |
| `runlore_alerts_coalesced_total` / `_suppressed_total` | storm-coalescing / cooldown drops |
| `runlore_investigations_completed_total{result}` | how investigations ended (incl. `timeout`, `error`) |
| `runlore_recall_hits_total{result}` / `_rejections_total{reason}` | whether instant recall is working |
| `runlore_curations_total{kind,result}` | KB PRs opened / coalesced / errored |
| `runlore_leader` | which replica is the active leader |
| `runlore_model_requests_total{provider,result}` | LLM call success vs error |
