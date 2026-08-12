---
title: Slack
weight: 401
integration: {kind: notifier, id: slack}
---

**What it gives you** — findings delivered to a channel, with optional Approve/Reject buttons and
one-click 👍/👎 diagnosis feedback.

## Minimal config

Incoming webhook (simplest — single message per finding):

```yaml
notify:
  slack:
    webhook_url_env: SLACK_WEBHOOK_URL
```

Or a bot token (`chat.postMessage`) — posts a verdict-first summary then the full analysis as a
**threaded reply**; the bot must be a member of the channel (invite it / `conversations.join`):

```yaml
notify:
  slack:
    bot_token_env: SLACK_BOT_TOKEN         # xoxb-… (takes precedence over webhook_url_env)
    channel: C0123456789                   # channel ID or name to post to
```

## Verify it locally

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=SLACK_WEBHOOK_URL='https://hooks.slack.com/services/...'
```

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) and
confirm delivery:

```bash
kubectl -n runlore logs deploy/runlore | grep 'msg=findings'
```

## Notes

- A **bot token takes precedence** over `webhook_url_env` when both are set.
- An **incoming webhook** delivers the same content as a **single** message — it cannot thread. It
  *does* carry interaction buttons: incoming webhooks follow the same messaging rules as
  `chat.postMessage`, and a click is answered through the interaction's `response_url`, so neither
  Approve/Reject nor `feedback_buttons` needs a bot token.
- **Approve/Reject buttons** (`actions.mode: approve`) and **`feedback_buttons`** (opt-in 👍/👎 rating)
  both need `signing_secret_env` set **and** the same exposure: Slack **Interactivity** turned on, with
  Request URL `https://<your-runlore-host>/slack/interactions` reachable **from Slack's servers**.
  - `api.slack.com/apps` → your app → **Interactivity & Shortcuts** → toggle **On**, Request URL =
    `https://<your-runlore-host>/slack/interactions`.
  - Read-only deployments (no actions, no feedback buttons) need none of this.
  - Route it through your ingress/gateway; if you use the chart's `networkPolicy.ingressFrom`, allow
    your ingress controller, not the internet.
- `feedback_buttons: true` also requires `outcome.ledger_path` **and a delivery target**
  (`webhook_url_env`, or `bot_token_env` with `channel`) — startup fails loud otherwise, since a button
  only exists on a message RunLore actually delivered. Ratings land in the outcome ledger and weigh the
  recalled entry's trust, exactly like resolve signals do.
- Mount the credential too: an env var that is set but **empty** skips the Slack notifier altogether —
  nothing is delivered, no buttons render, no rating can be recorded. Validation cannot see that, so
  startup warns (`no slack delivery target resolved`) instead of reporting the feature enabled.
- Feedback is deliberately **unprivileged**: any signature-valid member of your workspace can rate
  (approve/reject keep their own `approver_ids` allowlist). One live vote per (trigger key, Slack user);
  latest wins.
- `signing_secret_env` HMAC-verifies every button click (±5 min replay window).

## Reference

- [Configuration → `notify`]({{< relref "/docs/configuration/configuration.md#notify--where-findings-go" >}})
  for the full key reference.
- [Security model → the feedback channels]({{< relref "/docs/security/security-model.md#the-feedback-channels---exposure--trust-model" >}})
  for the exposure and vote trust model.
- [Learning loop]({{< relref "/docs/concepts/learning-loop.md" >}}) — how feedback weighs recall.
