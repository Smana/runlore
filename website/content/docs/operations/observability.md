---
title: Observability
weight: 10
---

RunLore self-instruments with structured logs and Prometheus-compatible metrics, and
ships a portable Grafana dashboard plus alert rules. Everything here works against
**either Prometheus or VictoriaMetrics** — RunLore is metrics-backend-agnostic.

## Logging

RunLore logs via Go's `log/slog`. Output format and verbosity are configurable:

```yaml
logging:
  format: json   # "text" (default, human-readable) | "json" (structured, for log aggregation)
  level: info    # debug | info | warn | error
```

The Helm chart defaults to **JSON** in-cluster (so logs flow cleanly into
Loki/VictoriaLogs/CloudWatch); the CLI defaults to text. Both are overridable at
startup without editing config:

```bash
RUNLORE_LOG_FORMAT=json RUNLORE_LOG_LEVEL=debug lore serve --config runlore.yaml
```

Level guidance: `error` = an operation failed; `warn` = a recoverable/degraded
condition (a backend unavailable, a provider disabled); `info` = lifecycle and
per-incident milestones; `debug` = per-step / per-tool tracing (off in production).

## Metrics

When `telemetry.metrics_enabled: true`, RunLore serves the Prometheus exposition
format at `GET /metrics` on the service port. Scrape it with a `VMServiceScrape`
(`vmServiceScrape.enabled: true` in the chart) or any `ServiceMonitor`/scrape config.

All series are prefixed `runlore_`.

The seconds-scale latency histograms (`*_duration_seconds`,
`incident_resolution_seconds`) carry explicit **SLO-aligned bucket boundaries**
(seconds): `0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300`. The OTel SDK
defaults are millisecond-scale and would collapse most calls into the first bucket,
breaking `histogram_quantile`.

### Score histogram buckets are the decision thresholds

The two BM25-score histograms — `runlore_recall_score` and
`runlore_curation_dedup_score` — carry their own explicit boundaries:
`0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4, 5, 7.5, 10`. The SDK defaults start at 5, so
an entire real corpus (an enriched BM25 score is roughly 0.1–1.2) lands in the first
bucket and the panel renders as a single bar.

The rescale is not the interesting part. **Every boundary that is also a decision
threshold is one deliberately**, so a bucket count answers "how much of the corpus
clears this gate?" with no arithmetic:

| boundary | the gate it is | reading `le` at that boundary tells you |
|---|---|---|
| `0.1` | `instant_recall.rerank_min_score` | share of recall attempts too weak to spend a rerank call on — they reject as `rerank_no_signal` |
| `1` | `instant_recall.min_score` / `margin_gap` | share below the legacy BM25 magnitude gate (live only under `instant_recall.rerank: false`) |
| `4` | `instant_recall.solo_floor` | share below the confident bar for a single-hit recall |
| `5` | `forge.dup_score` | on `curation_dedup_score`, the share **below** the dedup bar — i.e. the findings novel enough to file. At or above it, the finding is dropped as duplicating a catalog entry |

So `runlore_recall_score_bucket{le="0.1"}` over `_count` is precisely the fraction of
recall attempts that never reached the reranker, and
`runlore_curation_dedup_score_bucket{le="5"}` over `_count` the fraction of findings
that were novel. Boundaries above 5 exist for deployments that hand-tuned their
thresholds to a differently-scaled corpus; `+Inf` captures anything past 10.

> [!WARNING]
> **Existing panels over these two `*_bucket` series change shape on upgrade.**
> Histogram boundaries are not versioned and the rename-free change is invisible: the
> series keep their names and simply begin reporting a different set of `le` values.
> A panel, recording rule or `histogram_quantile` written against the old SDK ladder
> (which began at 5, so nearly everything sat in one bucket) will re-bucket silently
> and change value across the upgrade — no error, no missing series, just a different
> number. Re-check anything reading `runlore_recall_score_bucket` or
> `runlore_curation_dedup_score_bucket`, and treat comparisons spanning the upgrade as
> meaningless rather than as a real shift in corpus quality.

Both ladders are defined in
[`internal/telemetry/setup.go`](../internal/telemetry/setup.go) via
explicit-bucket-histogram views. The remaining histograms — `coalesce_batch_size`,
`investigation_tokens_estimated` and the per-investigation call/token/cost
distributions — keep the SDK defaults and are read as heatmaps, not percentiles.

### Pipeline & investigations
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `runlore_build_info` | gauge | `version` | constant 1; for `absent()` liveness + version display |
| `runlore_leader` | gauge | — | 1 on the elected leader, 0 on standbys |
| `runlore_alerts_received_total` | counter | — | incidents passing the trigger gate into the coalescer |
| `runlore_alerts_coalesced_total` | counter | — | incidents folded into an existing batch |
| `runlore_alerts_suppressed_total` | counter | — | incidents dropped by cooldown |
| `runlore_incidents_debounced_total` | counter | — | firing alerts dropped as self-resolving (a matching `resolved` webhook arrived within `triggers.incidents.debounce`) |
| `runlore_incidents_dropped_on_shutdown_total` | counter | — | **alert LOSS** — firing alerts still held in the debounce window when the process shut down: accepted (`200` to Alertmanager) but never investigated, and not retried until Alertmanager's `repeat_interval`. Any non-zero value is worth a look; see [troubleshooting]({{< relref "troubleshooting.md" >}}) |
| `runlore_investigations_started_total` | counter | — | investigations actually begun |
| `runlore_investigations_completed_total` | counter | `result` | investigations finished (`resolved`/`unresolved`/`recall`/`timeout`/`error`/`max_steps`/`max_steps_degraded`/`budget_exceeded`/`inconclusive`/`recurrence_suppressed`/`silenced`) |
| `runlore_investigation_duration_seconds` | histogram | `result` | wall-clock per investigation |
| `runlore_investigations_throttled_total` | counter | — | starts requeued by the rate limiter |
| `runlore_investigations_dropped_total` | counter | — | dropped (rate-limiter max-requeues or token-budget kill) |
| `runlore_investigations_cancelled_total` | counter | — | queued (not yet started) investigations cancelled because the incident resolved first (`triggers.incidents.cancel_queued_on_resolve`) |
| `runlore_gitops_failures_debounced_total` | counter | — | GitOps failures dropped as transient: the failure cleared within the debounce window before an investigation started |
| `runlore_coalesce_batch_size` | histogram | — | incidents per flushed batch (heatmap; SDK default buckets) |

### Tools & model
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `runlore_tool_calls_total` | counter | `tool`, `result` | investigation tool calls (`ok`/`error`) |
| `runlore_tool_call_duration_seconds` | histogram | `tool` | per-tool latency |
| `runlore_alert_rule_degraded_total` | counter | `reason`, `class` | `alert_rule` calls that answered with an "unavailable" note instead of the firing rule's expression. `reason` = `no_capability` (the metrics backend serves no alerting-rule endpoint), `backend_error` (the read failed — 404/500/unparseable body), `no_rules` (the endpoint answered, with an empty ruleset), `unmatched_alert` (the ruleset defines no rule under this alertname). `class` = `systemic` (the first three) or `routine` (`unmatched_alert`) |
| `runlore_model_requests_total` | counter | `provider`, `result` | LLM requests (`ok`/`error`). `provider` is the wire protocol for the main model, plus the synthetic tiers `rerank` (instant recall's LLM reranker) and `embed` (the `/embeddings` endpoint on the hybrid-recall path) |
| `runlore_model_request_duration_seconds` | histogram | `provider` | LLM request latency, same `provider` vocabulary |
| `runlore_investigation_tokens_estimated` | histogram | — | per-investigation token estimate (pre-request `chars/4` heuristic, investigation loop only — excludes the adversarial verify phase). This is what the `RunloreInvestigationCostHigh` alert watches |
| `runlore_investigation_model_calls` | histogram | `result` | model completions per investigation (loop + verify) |
| `runlore_investigation_input_tokens` | histogram | `result` | provider-reported input tokens per investigation, including cached (loop + verify) |
| `runlore_investigation_output_tokens` | histogram | `result` | provider-reported output tokens per investigation (loop + verify) |
| `runlore_investigation_cached_input_tokens` | histogram | `result` | input tokens served from cache per investigation (loop + verify) |
| `runlore_investigation_cost_usd` | histogram | `result` | estimated per-investigation cost in USD (only when `model.pricing` is configured) |
| `runlore_model_responses_truncated_total` | counter | `provider` | completions cut off at the output-token ceiling (the provider's stop/finish reason indicated truncation) — a truncated answer is a degraded one, so a rising rate means `model.max_tokens` is too tight |
| `runlore_model_input_tokens_total` | counter | `provider` | total LLM input tokens, including cached. Fleet-wide spend, as opposed to the per-investigation `investigation_input_tokens` distribution |
| `runlore_model_cached_input_tokens_total` | counter | `provider` | LLM input tokens served from cache. Over `model_input_tokens_total` this is the cache hit rate, which is the single biggest lever on the bill |
| `runlore_tool_output_truncated_bytes_total` | counter | — | bytes elided by per-call tool-output truncation (`investigation.max_tool_output_bytes`) |
| `runlore_history_compactions_total` | counter | — | mid-loop tool-output history compaction events |
| `runlore_history_elided_bytes_total` | counter | — | tool-output bytes elided by mid-loop compaction |
| `runlore_history_summarizations_total` | counter | — | compaction events whose elided batch was replaced by a model-produced digest (`compaction: summarize`) |
| `runlore_history_summarize_fallbacks_total` | counter | — | summarize-mode compactions that fell back to plain elision after a summariser error, refusal or truncation — the digest was not produced and the bodies were dropped instead |
| `runlore_investigation_budget_trips_total` | counter | `reason`, `stage` | spend ceilings crossed during an investigation. `reason` = `tokens_request` (the next request alone exceeded `max_tokens_per_investigation`), `tokens_total` (the run's projected cumulative tokens did), or `cost` (`max_cost_per_investigation`). `stage` = `nudge` (forced to conclude early; findings still delivered) or `kill` (hard-stopped, `result="budget_exceeded"`) |

**One run reports one `reason`.** The ceiling that first engaged the ladder is latched at the nudge
and carried to the kill, so the two rungs of a single stop always agree. Without that, the nudged
turn's own spend could push a *second* ceiling over the line and the kill would name that one
instead — telling you to raise a knob that never stopped anything, and splitting one stop across two
series in the `sum by (reason)` recipe below.

`budget_trips_total{stage="nudge"}` is the one to alert on. A nudged investigation completes and
records `result="resolved"` like any other, so it is invisible in every other series — only this
counter distinguishes "the ceiling is comfortable" from "the ceiling has been silently truncating
investigations for a week":

```promql
# share of investigations cut short by a ceiling, whether or not they died
sum(rate(runlore_investigation_budget_trips_total{stage="nudge"}[1h]))
  / sum(rate(runlore_investigations_completed_total[1h]))

# which ceiling to raise
sum by (reason) (rate(runlore_investigation_budget_trips_total[1h]))
```

The five usage histograms carry the same `result` values as
`runlore_investigations_completed_total`, so `{result="recall"}` selects exactly the
investigations a recall short-circuited and `{result!="recall"}` exactly those that ran
the full loop. That split is what makes the recall saving measurable — see below.

### Thread capture & conversational replies

Emitted by the `@runlore …` thread path (`notify.*.thread_capture`, plus `model.chat`
for the conversational half). Both halves are **member-triggerable** — anyone in the
channel or room can cause a forge write or a model call — so these are the series that
say what that path is costing and where it is being refused.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `runlore_mentions_dropped_on_saturation_total` | counter | — | **note LOSS** — a Slack `app_mention` or an addressed Matrix thread message that arrived when the handler pool was saturated. Accepted and never processed (Slack was already acked and will not retry; Matrix's `/sync` token has advanced), so the human's note is gone. RunLore tells them to retry with a best-effort in-thread reply |
| `runlore_thread_notes_written_total` | counter | `route` | knowledge writes that **landed** from an `@runlore note:` reply. `route` = `comment` (added to the curated PR this thread came from), `open_pr` (no PR to land on, so a standalone `Concept` entry PR was opened), or `append` (appended to the entry file of the standalone PR an earlier note in this same thread opened). `comment` and `append` are split deliberately: a comment is discarded when its PR merges, an appended entry becomes catalog knowledge, and with both labelled the same nothing distinguished them |
| `runlore_thread_writes_throttled_total` | counter | — | note writes **denied** by the global per-hour forge-write window and told to retry. This is the feature's one global cap and is otherwise invisible to an operator — nothing else reports it |
| `runlore_thread_chat_calls_total` | counter | — | model calls the chat layer made (granted by the per-hour budget) |
| `runlore_thread_chat_tokens_total` | counter | — | tokens the chat layer spent, from provider-reported usage — or a conservative estimate when the provider reported none |
| `runlore_thread_chat_denied_total` | counter | `ceiling` | chat calls the budget refused. `ceiling` = `calls` (`notify.thread.chat_calls_per_hour`) or `tokens` (`chat_tokens_per_hour`). The message falls back to the deterministic reply |

> [!IMPORTANT]
> **There is no cost report for the chat path.** `thread_chat_tokens_total` is a token
> count and nothing anywhere converts it to currency: per-investigation cost reporting
> (`runlore_investigation_cost_usd`, the notification footer) covers the main and verify
> passes only, and `model.chat.pricing` is read by nothing at all. To know what
> conversational replies cost, multiply this counter by your provider's rate yourself.

`thread_chat_denied_total{ceiling="tokens"}` firing while call slots remain is the
budget working as designed — the token ceiling is derived so that a runaway of
maximum-size calls is stopped by **cost** before it exhausts the call count. Sustained
denials mean the ceiling is genuinely too low for your traffic, not that something
broke:

```promql
# share of chat attempts refused, by which ceiling bound first
sum by (ceiling) (rate(runlore_thread_chat_denied_total[1h]))
  / scalar(sum(rate(runlore_thread_chat_calls_total[1h]))
         + sum(rate(runlore_thread_chat_denied_total[1h])))

# notes that became durable knowledge vs. notes that will vanish at merge
sum by (route) (rate(runlore_thread_notes_written_total[24h]))
```

**`class="systemic"` is the one to alert on.** `alert_rule` degrades gracefully by design — a
rule definition is corroborating context, never the primary evidence, so it must never fail an
investigation — and every degraded outcome is therefore a plain string, which the loop records as
`tool_calls_total{tool="alert_rule",result="ok"}`. A backend serving no alerting-rule endpoint is
metrically identical to one where the tool works, while every investigation on it silently loses
the feature that keeps a threshold alert from being judged against the wrong series. This counter
is the only series that separates them:

```promql
# the tool is delivering NO rule expressions on this deployment — a config or backend fact
sum(rate(runlore_alert_rule_degraded_total{class="systemic"}[1h]))

# which path fired: a missing capability, a failing read, or an empty ruleset
sum by (reason) (rate(runlore_alert_rule_degraded_total{class="systemic"}[1h]))

# share of alert_rule calls that came back without an expression
sum(rate(runlore_alert_rule_degraded_total{class="systemic"}[1h]))
  / sum(rate(runlore_tool_calls_total{tool="alert_rule"}[1h]))
```

`class="routine"` (`unmatched_alert`) is deliberately excluded from all three: the backend answered
with its rules and this incident's alertname simply is not among them — evaluated elsewhere, or
renamed. That is per-incident and normal, and folding it in with the systemic paths would bury the
signal an operator must act on under traffic nobody should be paged for. Watch it as a rate if it
climbs, though: a sudden step change means alertnames and rules have drifted apart.

### Recall, learning loop & curation
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `runlore_recall_hits_total` | counter | `result` | instant-recall short-circuits |
| `runlore_recall_tokens_spent_total` | counter | — | tokens a **delivered** recall short-circuit actually cost (LLM reranker + adversarial verify). A measurement, not a saving estimate — see [Measuring the recall saving](#measuring-the-recall-saving) |
| `runlore_recall_rejections_total` | counter | `reason` | recalls rejected before short-circuit |
| `runlore_recall_score` | histogram | — | BM25 score at the recall decision |
| `runlore_outcomes_opened_total` | counter | `kind` | investigations recorded as open |
| `runlore_incidents_resolved_total` | counter | — | resolve events matching an open investigation |
| `runlore_recall_outcome_total` | counter | `result` | resolved incidents whose answer was a recall |
| `runlore_incident_resolution_seconds` | histogram | — | open→resolve duration |
| `runlore_curations_total` | counter | `kind`, `result` | curation outcomes (`opened`/`coalesced`/`error`) |
| `runlore_curation_dedup_score` | histogram | — | catalog top-hit BM25 score at the dedup decision |
| `runlore_catalog_invalid_entries_total` | counter | — | structurally-invalid (but parseable) entries surfaced at catalog load. The entry is still indexed and served — one bad entry never empties the catalog — so this is the only signal that the CI merge gate let something through |
| `runlore_kb_draft_defects_total` | counter | `defect` | KB entries filed with a defect found **before** the PR was opened: `unrecallable_resource` (the `resource`/`alert_resource` recall indexes on is not shaped `namespace/name`, so recall's structural match can never agree with it) or `merge_gate` (the draft fails `lore validate-kb`, so its PR cannot merge). Counted once per entry per defect, for **both** entry writers — the curator and the `@runlore note:` standalone-note route |
| `runlore_catalog_embed_degraded_total` | counter | — | catalog reloads that left hybrid recall without vectors (embed failure — recall degrades to BM25-only until the next successful sync) |

`runlore_kb_draft_defects_total` is the one series that separates a healthy catalog from one
silently filling with dead weight. `runlore_curations_total{kind="pr",result="opened"}` scores
the pull request a success whether or not the entry inside it can ever be recalled, so without
this the two deployments look identical:

```promql
# share of filed entries recall will never be able to match — alert on this one
sum(increase(runlore_kb_draft_defects_total{defect="unrecallable_resource"}[$__range]))
  / sum(increase(runlore_curations_total{kind="pr",result="opened"}[$__range]))
```

Keep the two `defect` values apart rather than summing them. `merge_gate` blocks its own pull
request, so a human meets it at review time regardless; `unrecallable_resource` merges cleanly and
is silent forever, and every such entry is permanent catalog weight that never repays its cost.
The log lines behind both, and what to do about them, are in
[The PR opened, but the entry will never be recalled]({{< relref "troubleshooting.md#the-pr-opened-but-the-entry-will-never-be-recalled" >}}).

### Measuring the recall saving

RunLore never asserts a saving — it measures both sides and lets you subtract them.
Compare two means, both provider-reported, both per investigation:

```promql
# what a delivered recall short-circuit costs
sum(increase(runlore_recall_tokens_spent_total[$__range]))
  / sum(increase(runlore_investigations_completed_total{result="recall"}[$__range]))

# what an investigation that ran the full loop costs
(
    sum(increase(runlore_investigation_input_tokens_sum{result!="recall"}[$__range]))
  + sum(increase(runlore_investigation_output_tokens_sum{result!="recall"}[$__range]))
)
  / sum(increase(runlore_investigation_input_tokens_count{result!="recall"}[$__range]))
```

The **gap between the two is the saving**. `$__range` is the Grafana range variable —
substitute a window (`[24h]`) to run these in Prometheus directly. The dashboard plots
exactly these two series in **💸 Cost & efficiency**, and the **🧠 Learning loop** row
reduces them to one number, `1 - recall/full-loop`, as the "Recall token savings" stat.

> [!WARNING]
> **The `{result!="recall"}` filter is load-bearing.**
>
> Unfiltered, these histograms cover recall short-circuits too — so the "full loop" term
> would contain the very runs you are subtracting from it, and the measured saving would
> *shrink* as recall got better. Never difference `recall_tokens_spent_total` out of an
> unfiltered per-investigation total.

## Grafana dashboard

A portable dashboard lives at [`deploy/observability/grafana/runlore.json`](../deploy/observability/grafana/runlore.json).
It uses a single `datasource` template variable (type Prometheus), so it works with a
Prometheus **or** a VictoriaMetrics datasource with no edits. Import it via
**Dashboards → Import → Upload JSON**, or provision it. See the
[grafana README](https://github.com/Smana/runlore/blob/main/deploy/observability/grafana/README.md).

It panels most of the `runlore_` series above — the learning loop, recall, curation,
investigations, tools and model, cost, thread capture and fleet health. Notably it
includes the spend-ceiling trips broken out **by `stage`** (a nudged investigation
completes and records `result="resolved"`, so no other panel shows it) and **by
`reason`** (which ceiling to raise), the thread knowledge-write routes and the
throttle/denial counters, the output-truncation rate
(`tool_output_truncated_bytes_total`), the coalesced-batch-size distribution
(`coalesce_batch_size`, heatmap), and the curation dedup-score distribution
(`curation_dedup_score`, heatmap).

It is not exhaustive: the debounce counters, the history-compaction family and a few
per-investigation usage histograms are documented above but have no panel. Query them
ad hoc, or add a panel — the metric table is the authority on what exists, the
dashboard on what is worth a glance.

## Alerting

Alert rules ship as both a Prometheus-Operator `PrometheusRule` and a
VictoriaMetrics-Operator `VMRule` (identical rules; pick the one your stack uses):

```bash
# kube-prometheus-stack
kubectl apply -f deploy/observability/alerts/prometheusrule.yaml
# VictoriaMetrics Operator
kubectl apply -f deploy/observability/alerts/vmrule.yaml
```

The rule set covers liveness (`RunloreAgentDown`), HA (`RunloreNoActiveLeader`,
`RunloreMultipleLeaders`), pipeline health (`RunlorePipelineStalled`,
`RunloreInvestigationsDropped`, throttling), quality (tool/model error rates,
investigation errors), latency (model p95, slow resolution), and cost
(`RunloreInvestigationCostHigh`).

Three of them exist to catch failures that are **silent in every other series**, which
is what makes them worth the alert slot:

| Alert | What would otherwise hide it |
|---|---|
| `RunloreInvestigationsNudgedByCeiling` | a nudged run completes and records `result="resolved"`, so completion rate, error rate and duration all stay healthy while findings get shallower |
| `RunloreThreadMentionsDropped` | the transport was acked before the work was queued, so a lost note leaves no retry, no error result, and no trace beyond this counter |
| `RunloreCurationWriteErrors` | investigations keep succeeding; only the *write* fails, so the learning loop stops compounding with every upstream metric looking fine |

Thresholds are starting points — tune to your volume. See the [alerts README](https://github.com/Smana/runlore/blob/main/deploy/observability/alerts/README.md) for the
per-alert metric dependencies and operator discovery notes.

## Alert runbooks

Every shipped rule's `runbook_url` points at its heading below. Each says what the
alert means, what to look at first, and what actually fixes it — thresholds are
starting points, so "the threshold is wrong for us" is a legitimate outcome for most
of them.

### RunloreAgentDown

`absent(runlore_build_info)` for 5m: the series vanished from the TSDB entirely. Either
the process is gone or the scrape broke — those need different fixes, so tell them
apart first. Check the pod (`kubectl get pods -l app.kubernetes.io/name=runlore`) and
then the scrape target's health in Prometheus/VMAgent. A `VMServiceScrape`/`ServiceMonitor`
that stopped matching the Service produces this with a perfectly healthy agent.

### RunloreNoActiveLeader

`max(runlore_leader) == 0` for 10m: no replica holds the lease, so nothing is
investigating and incoming alerts are accumulating or being dropped. Check the lease
(`kubectl get lease -n <ns>`) and the logs of every replica for lease-renewal failures —
usually RBAC on `coordination.k8s.io/leases`, or API-server latency causing repeated
lease loss. Standbys are warm, so recovery is fast once one can acquire.

### RunloreMultipleLeaders

`sum(runlore_leader) > 1` for 5m: two replicas both believe they lead, so incidents can
be investigated twice — double model spend and duplicate curation PRs. Brief overlap
during failover is normal; five minutes is not. Check for clock skew and for a lease
duration tuned shorter than your API-server latency.

### RunloreInvestigationsDropped

`runlore_investigations_dropped_total` increased: an incident was discarded without
being investigated — the rate limiter exhausted its requeues, or a spend ceiling
hard-killed the run. Split the two before reacting: a token-budget kill also shows on
`runlore_investigation_budget_trips_total{stage="kill"}` and on
`runlore_investigations_completed_total{result="budget_exceeded"}`. Rate-limiter drops
mean `investigation.rate_limit` is too tight for your alert volume; budget kills mean
`max_tokens_per_investigation` is.

### RunloreInvestigationThrottlingSustained

Throttling continuously for 30m. Brief throttling under a burst is the rate limiter
working; half an hour of it means RunLore is structurally under-provisioned for its
load. Either raise `investigation.rate_limit.max_per_window` and accept the spend, or
reduce intake at the trigger gate. Check `RunloreInvestigationsDropped` alongside — a
throttle that never becomes a drop is only latency.

### RunlorePipelineStalled

Alerts are arriving and zero investigations are starting: the intake-to-investigation
handoff is broken, which no amount of alert volume will fix. Check leadership first
(this fires whenever there is no leader, so rule that out via `RunloreNoActiveLeader`),
then the coalescer — incidents sitting in a debounce or cooldown window that never
flushes look exactly like this. `runlore_alerts_suppressed_total` and
`runlore_incidents_debounced_total` tell you if intake is being filtered rather than
stalled.

### RunloreInvestigationErrors

Investigations finishing with `result="error"` — the loop is failing mid-run rather than
concluding. Read the investigation logs for the failing incidents; the common causes are
a tool integration that is down (cross-check `RunloreToolErrorRateHigh` and
`runlore_tool_calls_total{result="error"}` by `tool`) and provider errors
(`RunloreModelErrorRateHigh`). A low background rate can be normal; a step change is
not.

### RunloreToolErrorRateHigh

More than 20% of tool calls are failing over 15m, gated on a minimum call rate so the
ratio is not noise at idle. Break down by tool: `sum by (tool) (rate(runlore_tool_calls_total{result="error"}[15m]))`.
This is almost always one integration — expired credentials, a datasource that moved, or
RBAC narrowed on the cluster reader — not a general fault.

### RunloreModelErrorRateHigh

More than 10% of model requests are failing. Model errors stop investigations cold,
hence critical. Break down by `provider`, which distinguishes the main model from the
synthetic `rerank` and `embed` tiers — an `embed` failure degrades recall to BM25-only
(also visible as `runlore_catalog_embed_degraded_total`) but leaves investigations
working, which is a much smaller problem than the main tier failing. Check quota,
credentials, and provider status.

### RunloreModelLatencyHigh

p95 model latency above 30s over 15m. Every investigation is dragged out by this, and
slow calls interact badly with `investigation.timeout`. Confirm against your provider's
status and the `provider` label; if this is your model's genuine profile at your prompt
sizes, raise the threshold rather than living with a permanently firing alert.

### RunloreSlowResolution

p95 open-to-resolve above 1h. Informational, not a paging condition — it measures your
incident lifecycle, not RunLore's health, since the resolve event comes from
Alertmanager. Useful as an SLO signal; align the threshold with your own target.

### RunloreInvestigationsNudgedByCeiling

More than 10% of investigations in the last hour were nudged to conclude early by a
spend ceiling. This is the one budget signal worth alerting on, for the reason given
under [the budget-trip metric](#tools--model): a nudged investigation still delivers
findings and records `result="resolved"`, so completion rate, error rate and duration
all look healthy while the answers quietly get shallower. Nothing else distinguishes a
comfortable ceiling from one that has been truncating investigations for a week.

Find the binding ceiling with `sum by (reason) (rate(runlore_investigation_budget_trips_total[1h]))`.
One run reports one `reason` — the ceiling that first engaged the ladder is latched from
the nudge through to the kill — so the label names the knob to raise:
`tokens_request` and `tokens_total` both point at
`investigation.max_tokens_per_investigation`, `cost` at `max_cost_per_investigation`.
Before raising either, check whether prompts are being filled with tool output
(`investigation.max_tool_output_bytes`, the compaction settings) — that is the cheaper
fix, and `RunloreInvestigationCostHigh` is the corroborating signal.

### RunloreThreadMentionsDropped

An `@runlore` mention arrived while the concurrent handler pool was saturated. This is
**data loss, not backpressure**: the transport was acked before the work was queued
(Slack got its `200`, Matrix's `/sync` position token advanced), so nothing will be
redelivered. Whatever a human was trying to record is gone, and their only notice is a
best-effort in-thread reply asking them to send it again.

Any occurrence deserves a look, because it lands during incidents — the moment thread
traffic peaks is exactly when someone is recording what actually fixed it. If it recurs,
the handler pool is undersized for your channel volume. Cross-check
`runlore_thread_writes_throttled_total`: throttled writes are refused politely and can
be retried, dropped mentions cannot, and confusing the two leads to tuning the wrong
limit.

### RunloreCurationWriteErrors

`runlore_curations_total{result="error"}` is non-zero: investigations reached findings
and the forge write failed. The learning loop stops compounding here, and it does so
invisibly — every upstream metric (investigations completed, recall rate, curation
*attempts*) still looks healthy, and recall degrades only slowly as the catalog stops
growing.

Almost always a forge credential or scope problem: GitHub App permissions narrowed,
`forge.kb_repo` pointing somewhere the installation cannot reach, or the App uninstalled
from the KB repo. Grep `msg="curate findings"` with `err=` for the underlying error. Note
that `result="error"` counts only genuine write failures — a finding that was chat-only
(below `forge.min_confidence`) or deduplicated against an existing entry is a different
`result` and is not an error.

### RunloreAlertRuleUnavailable

`runlore_alert_rule_degraded_total{class="systemic"}` is non-zero: the `alert_rule` tool
cannot read the firing alert's own rule definition, for every investigation, until the
backend or the config changes.

This is the one alert on this page for a tool that is *working as designed* when it
fails. `alert_rule` degrades to a plain string rather than an error, because a rule
definition is corroborating context and must never fail an investigation — which means
`runlore_tool_calls_total` records every degradation as `result="ok"`. A deployment whose
backend serves no rules endpoint is therefore metrically identical to one where the tool
works, and this counter is the only series that separates them.

`class="routine"` (`unmatched_alert`) is excluded on purpose: an alert with no matching
rule is normal and must never page. Check the backend serves `/api/v1/rules` and that
your metrics provider implements it.

### RunloreUnrecallableKBDrafts

`runlore_kb_draft_defects_total{defect="unrecallable_resource"}` is non-zero: entries are
being filed with a `resource` recall's structural match can never agree with.

These merge cleanly and then never fire — worse than an entry with no `resource` at all,
because a non-empty one also disables the scopeless fallback. Every such entry is
permanent catalog weight that never repays its cost, and nothing else surfaces it:
`runlore_curations_total{kind="pr",result="opened"}` scores the pull request a success
either way.

`defect="merge_gate"` is excluded deliberately — that draft fails `lore validate-kb`, so
its pull request cannot merge and a human meets it at review time regardless. Fix the
frontmatter on the open PRs; the two log lines that name the entry are on the
[troubleshooting page]({{< relref "troubleshooting.md#the-pr-opened-but-the-entry-will-never-be-recalled" >}}).

### RunloreInvestigationCostHigh

p95 of `runlore_investigation_tokens_estimated` above 100000 over 30m. Read the metric
carefully: it records the size of the **last request** an investigation made, not its
cumulative spend, and 100000 is the per-request bound the agent itself enforces (a
quarter of the 400000 `max_tokens_per_investigation` default). p95 at that bound means
typical runs are ending on a maximum-size request — evidence that prompts are being
filled with tool output. Look at `investigation.max_tool_output_bytes` and the
compaction settings before raising any ceiling, and scale this threshold if you retune
`max_tokens_per_investigation`.
