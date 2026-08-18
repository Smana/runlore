---
title: Webhook
weight: 403
integration: {kind: notifier, id: webhook}
---

**What it gives you** — a generic outgoing webhook: findings POSTed as JSON to any endpoint that
takes it, no vendor-specific wiring.

## Minimal config

```yaml
notify:
  webhook:
    url_env: RUNLORE_WEBHOOK_NOTIFY_URL   # POST findings JSON to this URL
```

## Verify it locally

Run a throwaway HTTP sink and point the webhook at it:

```bash
# terminal 1: a minimal container that dumps whatever it receives
docker run --rm -p 8081:8080 mendhak/http-https-echo

# terminal 2
export RUNLORE_WEBHOOK_NOTIFY_URL=http://localhost:8081/
```

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) and
watch the echoed JSON body land in terminal 1.

## Notes

- Delivers the same content as Slack's incoming-webhook / Matrix path: a **single** message per
  finding, one POST, `Content-Type: application/json`.
- The JSON payload carries `verdict`, `severity`, `environment`, `cluster`, `tenant`, `alert_name`,
  `started_at` (RFC3339, empty when unknown), `occurrences`, `prev_curated_url`, `ruled_out` and
  `data_gaps`, alongside `title`/`confidence`/`curated_url`/`text` (all `omitempty`).
- **The three resource fields say three different things.** `resource` is the object's name,
  verbatim. `namespace` is the namespace the finding is **scoped to** — the one the object is in, or
  the namespace itself when the finding is *about* a namespace — and is **omitted when the object is
  in no namespace at all**: a `Node` is cluster-scoped and an RDS instance is not a Kubernetes object,
  so an alert's `namespace` label on either names whatever exported the series (kube-state-metrics,
  the alerting rule), not the resource. `resource_ref` is the whole scoped identity —
  `namespace/name`, or the bare name when there is nothing to qualify it — and is the field to read
  when you want one string: it is the same one the Slack card prints.
- Findings are secret-redacted **before** any notifier runs, so the webhook only ever sees redacted
  data.
- This notifier exists as much to prove RunLore's notifier extensibility (drop one self-registering
  file under `internal/notify/<name>/`) as to be useful in production — for a templated payload
  against a specific vendor (Teams, Discord, ntfy…), see [Templated]({{< relref "templated.md" >}})
  instead.

## Reference

[Configuration → `notify`]({{< relref "/docs/configuration/configuration.md#notify--where-findings-go" >}})
for the full key reference and the shared payload fields.
