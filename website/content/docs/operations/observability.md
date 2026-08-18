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
breaking `histogram_quantile`. The boundaries are defined in
[`internal/telemetry/setup.go`](../internal/telemetry/setup.go) via an
explicit-bucket-histogram view. Other histograms (scores, batch size, token
estimate) keep the SDK defaults and are read as heatmaps, not percentiles.

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
| `runlore_gitops_failures_debounced_total` | counter | — | GitOps failures dropped as transient: cleared within the debounce window before investigating |
| `runlore_mentions_dropped_on_saturation_total` | counter | — | **chat mentions LOST** — a Slack `app_mention` or addressed Matrix thread message arrived while the handler pool was saturated: accepted but never processed (Slack acked `200` and never retries; Matrix's `/sync` token has already advanced). The human gets a best-effort in-thread "retry" reply. Non-zero means humans are losing `@runlore note:` captures |
| `runlore_investigations_started_total` | counter | — | investigations actually begun |
| `runlore_investigations_completed_total` | counter | `result` | investigations finished (`resolved`/`unresolved`/`recall`/`timeout`/`error`/`max_steps`/`max_steps_degraded`/`budget_exceeded`/`inconclusive`/`recurrence_suppressed`) |
| `runlore_investigation_duration_seconds` | histogram | `result` | wall-clock per investigation |
| `runlore_investigations_throttled_total` | counter | — | starts requeued by the rate limiter |
| `runlore_investigations_dropped_total` | counter | — | dropped (rate-limiter max-requeues or token-budget kill) |
| `runlore_investigations_cancelled_total` | counter | — | queued (not yet started) investigations cancelled because the incident resolved first (`triggers.incidents.cancel_queued_on_resolve`) |

### Tools & model
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `runlore_tool_calls_total` | counter | `tool`, `result` | investigation tool calls (`ok`/`error`) |
| `runlore_tool_call_duration_seconds` | histogram | `tool` | per-tool latency |
| `runlore_model_requests_total` | counter | `provider`, `result` | LLM requests (`ok`/`error`). `provider` is the wire protocol for the main model, plus the synthetic tiers `rerank` (instant recall's LLM reranker) and `embed` (the `/embeddings` endpoint on the hybrid-recall path) |
| `runlore_model_request_duration_seconds` | histogram | `provider` | LLM request latency, same `provider` vocabulary |
| `runlore_model_input_tokens_total` | counter | `provider` | total LLM input tokens, including cached |
| `runlore_model_cached_input_tokens_total` | counter | `provider` | LLM input tokens served from cache |
| `runlore_model_responses_truncated_total` | counter | `provider` | completions cut off at the output-token ceiling (the provider's stop/finish reason indicated truncation) |
| `runlore_tool_output_truncated_bytes_total` | counter | — | bytes elided by per-call tool-output truncation (`investigation.max_tool_output_bytes`) |
| `runlore_history_compactions_total` | counter | — | mid-loop tool-output history compaction events |
| `runlore_history_elided_bytes_total` | counter | — | tool-output bytes elided by mid-loop compaction |
| `runlore_history_summarizations_total` | counter | — | compactions whose elided batch was replaced by a model-produced digest (`compaction: summarize`) |
| `runlore_history_summarize_fallbacks_total` | counter | — | summarize-mode compactions that fell back to plain elision (summarizer error, refusal or truncation). A rising share means `summarize` is paying for a digest it is not getting |
| `runlore_investigation_tokens_estimated` | histogram | — | per-investigation token estimate (pre-request `chars/4` heuristic, investigation loop only — excludes the adversarial verify phase). This is what the `RunloreInvestigationCostHigh` alert watches |
| `runlore_investigation_model_calls` | histogram | `result` | model completions per investigation (loop + verify) |
| `runlore_investigation_input_tokens` | histogram | `result` | provider-reported input tokens per investigation, including cached (loop + verify) |
| `runlore_investigation_output_tokens` | histogram | `result` | provider-reported output tokens per investigation (loop + verify) |
| `runlore_investigation_cached_input_tokens` | histogram | `result` | input tokens served from cache per investigation (loop + verify) |
| `runlore_investigation_cost_usd` | histogram | `result` | estimated per-investigation cost in USD (only when `model.pricing` is configured) |
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
| `runlore_catalog_invalid_entries_total` | counter | — | structurally-invalid entries surfaced at catalog load — they are served anyway, so this is the standing count of entries whose frontmatter a human still needs to fix |
| `runlore_catalog_embed_degraded_total` | counter | — | catalog reloads that left hybrid recall without vectors (embed failure — recall degrades to BM25-only until the next successful sync) |

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

### Thread — chat & knowledge capture

The in-thread chat layer and `@runlore note:` knowledge capture run on their own budget,
separate from `investigation.*`. These are the series that show what that costs and when it is
being refused.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `runlore_thread_chat_calls_total` | counter | — | model calls granted to the chat layer |
| `runlore_thread_chat_tokens_total` | counter | — | tokens the chat layer spent (provider-reported, or a conservative estimate when the provider reports none) |
| `runlore_thread_chat_denied_total` | counter | `ceiling` | chat calls refused by the thread budget — `ceiling` = `calls` or `tokens`. Sustained denials mean humans are asking questions the budget will not answer |
| `runlore_thread_notes_written_total` | counter | `route` | knowledge writes landed from an `@runlore note:` reply — `route` distinguishes a comment on an existing PR from a newly opened one |
| `runlore_thread_writes_throttled_total` | counter | — | note writes refused by the global per-hour forge-write window, and told to retry |

## Grafana dashboard

A portable dashboard lives at [`deploy/observability/grafana/runlore.json`](../deploy/observability/grafana/runlore.json).
It uses a single `datasource` template variable (type Prometheus), so it works with a
Prometheus **or** a VictoriaMetrics datasource with no edits. Import it via
**Dashboards → Import → Upload JSON**, or provision it. See the
[grafana README](https://github.com/Smana/runlore/blob/main/deploy/observability/grafana/README.md).

It panels the pipeline, recall, outcome, curation, cost and fleet series above — including the
output-truncation rate (`tool_output_truncated_bytes_total`), the coalesced-batch-size distribution
(`coalesce_batch_size`, heatmap), the curation dedup-score distribution (`curation_dedup_score`,
heatmap), the spend ceilings (`investigation_budget_trips_total`) and the alert-loss counter
(`incidents_dropped_on_shutdown_total`).

It is **not** exhaustive. The thread series, the history-compaction family, and the several
`*_degraded_total` / `*_debounced_total` counters have no panel — they are documented above and
queryable, but you will not see them by importing the dashboard alone. If you rely on one of those,
add a panel or an alert for it rather than assuming the dashboard covers it.

## Alerting

Alert rules ship as both a Prometheus-Operator `PrometheusRule` and a
VictoriaMetrics-Operator `VMRule` (identical rules; pick the one your stack uses):

```bash
# kube-prometheus-stack
kubectl apply -f deploy/observability/alerts/prometheusrule.yaml
# VictoriaMetrics Operator
kubectl apply -f deploy/observability/alerts/vmrule.yaml
```

All fifteen rules, named — a page names the alert, so the alert name is what you will search for:

| Alert | Fires on |
|---|---|
| `RunloreAgentDown` | liveness — no instance is reporting |
| `RunloreNoActiveLeader` | HA — no replica holds the lease, so nothing runs |
| `RunloreMultipleLeaders` | HA — more than one replica believes it leads |
| `RunlorePipelineStalled` | alerts arriving but no investigations starting |
| `RunloreInvestigationsDropped` | investigations dropped by the rate limiter or a spend ceiling |
| `RunloreInvestigationThrottlingSustained` | the rate limiter engaged and stayed engaged |
| `RunloreInvestigationErrors` | investigations ending in `result="error"` |
| `RunloreToolErrorRateHigh` | tool calls failing, **summed across every tool** |
| `RunloreModelErrorRateHigh` | LLM requests failing |
| `RunloreModelLatencyHigh` | LLM request p95 latency |
| `RunloreSlowResolution` | incident open→resolve duration |
| `RunloreInvestigationCostHigh` | per-investigation token estimate |
| `RunloreAlertRuleUnavailable` | `alert_rule` degrading **systemically** — the tool is dead for every investigation |
| `RunloreUnrecallableKBDrafts` | knowledge filed with a `resource` recall can never match |
| `RunloreResourceScopeDiscoveryFailing` | Kubernetes discovery failing, so card scope falls back to the kind list |

Thresholds are starting points — tune to your volume.

The last three watch the degradation paths rather than outright failures — each of those
subsystems is designed to fail *soft*, which means nothing breaks and nobody finds out. They alert
only on the systemic half of each counter: a routine unmatched alertname, a draft that fails its
own merge gate, or a cloud kind Kubernetes does not serve are all normal and must never page. See the [alerts README](https://github.com/Smana/runlore/blob/main/deploy/observability/alerts/README.md) for the
per-alert metric dependencies and operator discovery notes.
