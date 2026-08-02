---
title: Templated
weight: 40
integration: {kind: notifier, id: templated}
---

**What it gives you** — delivery to **any** webhook-speaking service (Microsoft Teams, Discord, ntfy,
incident.io…) via named instances with your own Go-template payload, no Go rebuild.

## Minimal config

Worked example — Microsoft Teams (Incoming Webhook / MessageCard):

```yaml
notify:
  templated:
    - name: teams
      url_env: RUNLORE_TEAMS_WEBHOOK_URL
      template: |
        {
          "@type": "MessageCard", "@context": "https://schema.org/extensions",
          "summary": {{ toJSON .Title }},
          "themeColor": "d63333",
          "title": {{ toJSON (printf "[%s] %s (%.0f%%)" .Verdict .Title (mulPct .Confidence)) }},
          "text": {{ toJSON .Text }}
        }
```

## Verify it locally

```bash
export RUNLORE_TEAMS_WEBHOOK_URL='https://outlook.office.com/webhook/...'
```

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) and
confirm the rendered card lands in the target channel. To check the template renders before wiring a
real endpoint, point `url_env` at a local echo container (see [Webhook → Verify it
locally]({{< relref "webhook.md#verify-it-locally" >}})) and inspect the rendered body.

## Notes

- Each instance renders a Go `text/template` over the delivery payload — the same fields the
  `notify.webhook` JSON carries — and POSTs the result.
- **Two template functions**: `toJSON` (escaping-correct JSON splicing — always use it for values
  inside JSON bodies) and `mulPct` (×100 for percent display).
- Findings are secret-redacted **before** any notifier runs, so templates only ever see redacted data.
- A template that **fails to parse refuses startup**; a template that fails at **delivery time** is
  logged and skipped without blocking other channels.
- Rendered bodies are capped at **256 KiB**.
- Optional per-instance `token_env` (bearer auth to the target) and `content_type` (defaults to
  `application/json`).
- **Two config surfaces, one registry.** Built-in notifiers (`notify.slack`, `notify.matrix`) use
  typed config blocks with startup validation; drop-in notifiers (`notify.webhook`,
  `notify.templated`) live under the same `notify:` key as self-describing blocks. Both build through
  the same registry — type-checked config for the built-ins, zero-code extensibility for everything
  else.

## Reference

[Configuration → Generic templated notifier]({{< relref "/docs/configuration/configuration.md#generic-templated-notifier-notifytemplated" >}})
for the full key reference and payload field list.
