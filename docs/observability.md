# Observability

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

### Pipeline & investigations
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `runlore_build_info` | gauge | `version` | constant 1; for `absent()` liveness + version display |
| `runlore_leader` | gauge | — | 1 on the elected leader, 0 on standbys |
| `runlore_alerts_received_total` | counter | — | incidents passing the trigger gate into the coalescer |
| `runlore_alerts_coalesced_total` | counter | — | incidents folded into an existing batch |
| `runlore_alerts_suppressed_total` | counter | — | incidents dropped by cooldown |
| `runlore_investigations_started_total` | counter | — | investigations actually begun |
| `runlore_investigations_completed_total` | counter | `result` | investigations finished (`resolved`/`unresolved`/`recall`/`error`/`max_steps`/`budget_exceeded`/`inconclusive`) |
| `runlore_investigation_duration_seconds` | histogram | `result` | wall-clock per investigation |
| `runlore_investigations_throttled_total` | counter | — | starts requeued by the rate limiter |
| `runlore_investigations_dropped_total` | counter | — | dropped (rate-limiter max-requeues or token-budget kill) |

### Tools & model
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `runlore_tool_calls_total` | counter | `tool`, `result` | investigation tool calls (`ok`/`error`) |
| `runlore_tool_call_duration_seconds` | histogram | `tool` | per-tool latency |
| `runlore_model_requests_total` | counter | `provider`, `result` | LLM completion requests (`ok`/`error`) |
| `runlore_model_request_duration_seconds` | histogram | `provider` | LLM completion latency |
| `runlore_investigation_tokens_estimated` | histogram | — | per-investigation token estimate |

### Recall, learning loop & curation
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `runlore_recall_hits_total` | counter | `result` | instant-recall short-circuits |
| `runlore_recall_tokens_saved_total` | counter | — | estimated tokens saved by recall |
| `runlore_recall_rejections_total` | counter | `reason` | recalls rejected before short-circuit |
| `runlore_recall_score` | histogram | — | BM25 score at the recall decision |
| `runlore_outcomes_opened_total` | counter | `kind` | investigations recorded as open |
| `runlore_incidents_resolved_total` | counter | — | resolve events matching an open investigation |
| `runlore_recall_outcome_total` | counter | `result` | resolved incidents whose answer was a recall |
| `runlore_incident_resolution_seconds` | histogram | — | open→resolve duration |
| `runlore_curations_total` | counter | `kind`, `result` | curation outcomes (`opened`/`coalesced`/`error`) |
| `runlore_curation_dedup_score` | histogram | — | catalog top-hit BM25 score at the dedup decision |

## Grafana dashboard

A portable dashboard lives at [`deploy/observability/grafana/runlore.json`](../deploy/observability/grafana/runlore.json).
It uses a single `datasource` template variable (type Prometheus), so it works with a
Prometheus **or** a VictoriaMetrics datasource with no edits. Import it via
**Dashboards → Import → Upload JSON**, or provision it. See the
[grafana README](../deploy/observability/grafana/README.md).

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
(`RunloreInvestigationCostHigh`). Thresholds are starting points — tune to your
volume. See the [alerts README](../deploy/observability/alerts/README.md) for the
per-alert metric dependencies and operator discovery notes.
