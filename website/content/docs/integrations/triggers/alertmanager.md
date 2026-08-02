---
title: Alertmanager
weight: 101
integration: {kind: source, id: alertmanager}
---

**What it gives you** — the primary trigger: Prometheus/VMAlert `firing`/`resolved` webhooks turn into
investigations, filtered by your trigger policy.

## Minimal config

```yaml
sources:
  alertmanager: {}           # enable the Alertmanager/VMAlert webhook source
triggers:
  incidents:
    match:
      severity: [critical, warning]   # match against the alert's labels
    dedup: { window: 30m }
```

Point Alertmanager at the mounted webhook:

```yaml
# alertmanager config
receivers:
  - name: runlore
    webhook_configs:
      - url: http://runlore.runlore.svc:8080/webhook/alertmanager
route:
  routes:
    - receiver: runlore
      continue: true        # keep your existing routing too
```

## Verify it locally

`hack/demo.sh` runs `lore serve` locally with a keyless config and fires the mocked alerts in
`examples/alertmanager-webhook.json` through the trigger policy — no cluster, no LLM, no credentials
(just Go + `curl`):

```bash
hack/demo.sh
```

Or by hand, against a running server:

```bash
curl -s -o /dev/null -w 'webhook HTTP %{http_code}\n' \
  -XPOST "http://localhost:8080/webhook/alertmanager" \
  --data @examples/alertmanager-webhook.json
```

## Notes

- **Presence enables it** — `sources.alertmanager: {}` is all it takes; there is no separate
  `enabled: true` flag.
- The **trigger policy** (`triggers.incidents.match`/`.ignore`/`.dedup`/`.debounce`) is the real filter —
  Alertmanager routing is just the firehose.
- **Auth is server-level**, not per-source: set `server.webhook_token_env` to a bearer token and send it
  as `Authorization: Bearer …` from Alertmanager. This is **mandatory once a model is configured** — the
  `serve` path fails closed rather than start with an anonymous, LLM-billing webhook open.
- With 2+ replicas, every warm replica is `Ready` and may receive the webhook; a non-leader replica
  transparently proxies it to the elected leader, so only the leader's queue investigates.

## Reference

- [Configuration → `sources`]({{< relref "/docs/configuration/configuration.md#sources--what-wakes-runlore" >}})
  and [`triggers`]({{< relref "/docs/configuration/configuration.md#triggers--which-incidents-to-investigate" >}})
  for the full key reference.
- [Security model]({{< relref "/docs/security/security-model.md" >}}) for the webhook auth posture.
