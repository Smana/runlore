---
title: Custom webhook
weight: 105
integration: {kind: source, id: custom}
---

**What it gives you** — mapping ANY vendor's alert JSON to investigations with dot-path field
extraction, config only, no code.

## Minimal config

Each named instance gets its own endpoint `POST /webhook/custom/<instance>` and its own optional
bearer token. Grafana Alerting example:

```yaml
sources:
  custom:
    instances:
      grafana:
        token_env: GRAFANA_WEBHOOK_TOKEN
        items: alerts
        fields:
          title: labels.alertname
          message: annotations.summary
          severity: labels.severity
          namespace: labels.namespace
          workload_name: labels.pod
          fingerprint: fingerprint
          resolved: status
        labels: labels
        defaults: { environment: prod }
```

Point a Grafana webhook contact point at `https://<runlore>/webhook/custom/grafana` with an
`Authorization: Bearer …` custom header.

Datadog (a flat, single-event payload) instead:

```yaml
sources:
  custom:
    instances:
      datadog:
        token_env: DATADOG_WEBHOOK_TOKEN
        fields:
          title: title
          message: text
          severity: alert_type
          fingerprint: aggreg_key
          resolved: alert_status
        resolved_value: Recovered
        severity_map: { error: critical }
```

Datadog webhooks POST a single flat JSON you define with template variables:

```json
{"title": "$EVENT_TITLE", "text": "$TEXT_ONLY_MSG", "alert_type": "$ALERT_TYPE",
 "alert_status": "$ALERT_TRANSITION", "aggreg_key": "$AGGREG_KEY"}
```

## Verify it locally

```bash
curl -s -o /dev/null -w 'webhook HTTP %{http_code}\n' \
  -XPOST "http://localhost:8080/webhook/custom/grafana" \
  -H 'Authorization: Bearer <GRAFANA_WEBHOOK_TOKEN value>' \
  -H 'Content-Type: application/json' \
  --data '{"alerts":[{"labels":{"alertname":"HighLatency","severity":"critical","namespace":"shop","pod":"shop-api-abc"},"annotations":{"summary":"p99 latency above SLO"},"fingerprint":"abc123","status":"firing"}]}'
```

## Notes

- Field paths are dot-separated with optional `[n]` indexes (`alerts[0].labels.alertname`); a missing
  path falls back to `defaults`.
- `severity_map` normalizes vendor severities to yours.
- A payload with `items` set is a batch (path to the event array); without it the whole body is one
  event.
- Events whose `resolved` path equals `resolved_value` (default `"resolved"`) record a resolution for
  the outcome ledger instead of triggering an investigation (requires `fingerprint`).
- The per-delivery request cap and 1MiB body cap apply as for every webhook source.
- **A typo'd instance key, an unparseable path, or a missing `fields.title` aborts startup** —
  mappings never fail silently at ingest.
- Requests without a Kubernetes workload recall only resource-less entries (the scopeless tier) — same
  as [PagerDuty]({{< relref "pagerduty.md" >}}).
- Each instance's token (when set) replaces the shared `server.webhook_token_env` for that instance's
  endpoint — falls back to the shared token when unset, else open.

## Reference

Full key reference: [Data sources → Custom webhooks]({{< relref "/docs/concepts/data-sources.md" >}})
and [Configuration → `sources`]({{< relref "/docs/configuration/configuration.md#sources--what-wakes-runlore" >}}).
