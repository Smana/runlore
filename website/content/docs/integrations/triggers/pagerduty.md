---
title: PagerDuty
weight: 103
integration: {kind: source, id: pagerduty}
---

**What it gives you** — investigations triggered directly from PagerDuty incidents, no Alertmanager
in the loop.

## Minimal config

```yaml
sources:
  pagerduty: {}
```

Optionally verify deliveries with the subscription's signing secret:

```yaml
sources:
  pagerduty:
    secret_env: PAGERDUTY_WEBHOOK_SECRET
```

PD-side setup: create a [webhook subscription](https://developer.pagerduty.com/api-reference/b3A6MjkyNDc4NA-create-a-webhook-subscription)
with delivery URL `https://<runlore>/webhook/pagerduty`, subscribe to `incident.triggered` and
`incident.resolved`, and put the subscription's generated secret in the env var named by `secret_env`.

## Verify it locally

```bash
curl -s -o /dev/null -w 'webhook HTTP %{http_code}\n' \
  -XPOST "http://localhost:8080/webhook/pagerduty" \
  -H 'Content-Type: application/json' \
  --data '{"event":{"event_type":"incident.triggered","data":{"id":"PD123","type":"incident","title":"db latency spike","service":{"summary":"checkout-api"},"urgency":"high","html_url":"https://example.pagerduty.com/incidents/PD123"}}}'
```

With `secret_env` set, PagerDuty (and this curl) must send `X-PagerDuty-Signature: v1=<hex hmac-sha256
of the raw body>`, keyed on the shared secret — otherwise the delivery is rejected.

## Notes

- `incident.triggered` starts an investigation; `incident.resolved` closes the outcome (like an
  Alertmanager resolved alert). Every other event type is ignored.
- The incident title becomes the investigation title, priority (else urgency) the severity, and the
  service name, incident number and `html_url` ride along as labels.
- **PagerDuty carries no Kubernetes namespace or workload.** Those fields stay empty, so a PagerDuty
  incident can only recall catalog entries that are themselves resource-less (hand-written runbooks /
  curated Playbooks without a `resource` frontmatter) — the **scopeless tier**, the weakest recall
  match. It must clear `solo_floor` *and* `min_score` even with multiple candidates, recalls with
  reduced confidence, and `require_workload_match: true` disables it entirely.
- `secret_env` verifies each delivery against its `X-PagerDuty-Signature` header (`v1=`-prefixed
  HMAC-SHA256 of the raw body); multiple comma-separated signatures are accepted so a zero-downtime
  secret rotation works — any match passes, constant-time.
- `secret_env` **replaces** the shared `server.webhook_token_env` bearer token for this path (PagerDuty
  signs, it cannot send a bearer token). Leaving it unset leaves the webhook open — but it **fails
  closed** once a model is configured or `actions.mode=auto`: RunLore refuses to start with an
  unauthenticated PagerDuty webhook once a model is wired.

## Reference

- [Configuration → `sources`]({{< relref "/docs/configuration/configuration.md#sources--what-wakes-runlore" >}})
  for the full `pagerduty` key reference.
- [Custom webhook]({{< relref "custom-webhook.md" >}}) — the same scopeless-tier recall applies there.
