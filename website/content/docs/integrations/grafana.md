---
title: Grafana Alerting
weight: 25
integration: {kind: source, id: grafana}
---

**What it gives you** — Grafana Alerting's firing/resolved contact-point webhook turns into
investigations, without hand-copying nine dot-path field mappings into `sources.custom`.

`grafana` is a thin wrapper: it registers `POST /webhook/grafana` and delegates every byte of
extraction — dot-path field lookup, `alerts` batching, severity mapping, resolved-event handling,
the 1MiB body cap, the per-delivery request cap and startup mapping validation — to the same
[custom webhook mapper]({{< relref "/docs/concepts/data-sources.md#custom-webhooks-any-vendor-no-code" >}})
that powers `sources.custom`, with the field paths below baked in as defaults.

## Minimal config

```yaml
sources:
  grafana:
    token_env: GRAFANA_WEBHOOK_TOKEN   # optional; falls back to server.webhook_token_env
```

That's it — `sources.grafana: {}` alone also works (no token, or the shared
`server.webhook_token_env`).

Point a Grafana webhook contact point at `https://<runlore>/webhook/grafana` with an
`Authorization: Bearer …` custom header carrying the token.

## Built-in mapping

| Field | Path | Grafana payload meaning |
|---|---|---|
| `items` | `alerts` | the contact-point payload batches alerts under `alerts` |
| `title` | `labels.alertname` | the alert rule's name |
| `message` | `annotations.summary` | the rule's summary annotation |
| `severity` | `labels.severity` | your `severity` label |
| `namespace` | `labels.namespace` | your `namespace` label |
| `workload_name` | `labels.pod` | your `pod` label |
| `fingerprint` | `fingerprint` | Grafana's per-series identity, used for dedup and resolution |
| `resolved` | `status` | `"resolved"` records a resolution instead of an investigation |
| `labels` | `labels` | every alert label rides along on the investigation's `Labels` |

**Every field stays overridable.** A rule set that doesn't label pods, or uses a different label
name, just overrides that one key — the rest of the mapping stays default:

```yaml
sources:
  grafana:
    fields:
      workload_name: labels.deployment   # this cluster labels by deployment, not pod
```

The same goes for `items`, `labels`, `defaults`, and `severity_map` — see
[Data sources → Custom webhooks]({{< relref "/docs/concepts/data-sources.md#custom-webhooks-any-vendor-no-code" >}})
for the full key reference, since it is the same mapper underneath.

## Verify it locally

```bash
curl -s -o /dev/null -w 'webhook HTTP %{http_code}\n' \
  -XPOST "http://localhost:8080/webhook/grafana" \
  -H 'Authorization: Bearer <GRAFANA_WEBHOOK_TOKEN value>' \
  -H 'Content-Type: application/json' \
  --data '{"alerts":[{"status":"firing","fingerprint":"abc123","labels":{"alertname":"HighLatency","severity":"critical","namespace":"shop","pod":"shop-api-abc"},"annotations":{"summary":"p99 latency above SLO"}}]}'
```

`hack/integration/grafana/` runs a real Grafana instance with a provisioned alert rule and contact
point wired to a local `lore serve`, for an end-to-end pass (fire the rule, confirm an
investigation starts; resolve it, confirm a resolution is recorded instead of a second
investigation).

## Notes

- **Presence enables it** — `sources.grafana: {}` is all it takes; there is no separate
  `enabled: true` flag.
- One mapping per RunLore deployment: unlike `sources.custom`, which multiplexes several named
  instances behind `/webhook/custom/{instance}`, `grafana` serves a single fixed endpoint.
- `token_env` (optional) replaces the shared `server.webhook_token_env` for this endpoint; when
  unset it falls back to the shared token, else the endpoint is open. This **fails closed** once a
  model is configured or under `actions.mode=auto`.
- Requests without a Kubernetes workload recall only resource-less entries (the scopeless tier) —
  same as PagerDuty.
- A typo'd key, an unparseable path, or a missing `fields.title` aborts startup — mappings never
  fail silently at ingest.

## Reference

- [Data sources → Custom webhooks]({{< relref "/docs/concepts/data-sources.md#custom-webhooks-any-vendor-no-code" >}})
  — the mapper `grafana` delegates to; full key reference for `items`, `fields`, `defaults`,
  `severity_map`, `resolved_value`.
- [Configuration → `sources`]({{< relref "/docs/configuration/configuration.md#sources--what-wakes-runlore" >}}).
