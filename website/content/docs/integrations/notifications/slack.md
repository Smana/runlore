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

### Write knowledge back from a thread

With `thread_capture` on, replying to a finding's thread records what you know
into the knowledge base:

    @runlore note: the real cause was the spot-node reclaim at 14:02

The note lands as a comment on that finding's knowledge-base PR. When the
finding has no PR — an instant recall, or a `no_action` verdict — RunLore opens
a small `Concept` entry PR instead, so the knowledge still lands somewhere. A
human reviews and merges it, like every other entry. If nobody does, the
`curate` stale sweep still applies: an untouched note PR past
`curate.stale_after` (the Helm chart ships `720h`) is closed like any other
stale draft, with a comment explaining why — it can be reopened for review at
any time.

`@runlore reinvestigate: …` is reserved and not supported yet; add the
`reinvestigate` label to the knowledge-base issue to re-run an investigation.

```yaml
notify:
  slack:
    bot_token_env: SLACK_BOT_TOKEN
    channel: C0123456789
    signing_secret_env: SLACK_SIGNING_SECRET
    thread_capture: true
outcome:
  ledger_path: /var/lib/runlore/outcome.jsonl   # required: the thread registry lives beside it
```

`thread_capture: true` also requires `outcome.ledger_path` — the thread registry
that maps a Slack thread to its investigation is stored beside the ledger.
Startup fails loud without it, the same way it fails loud without
`signing_secret_env`, `bot_token_env` and `channel` above.

That location surviving a restart or a leader failover depends on your
deployment shape, not on `outcome.ledger_path` alone: the Helm chart's default
(`persistence.enabled: false`) is an `emptyDir`, destroyed on every restart,
upgrade or failover, and a `StatefulSet` + `ReadWriteOnce` volume still gives
each replica its own copy — only `persistence.enabled: true` with
`workloadKind: Deployment` and a `ReadWriteMany` accessMode puts the registry on
the one volume every replica shares. See [Configuration →
`notify`]({{< relref "/docs/configuration/configuration.md#notify--where-findings-go" >}})
for the full breakdown. Without that combination, a restart or failover empties
the registry: a reply to a thread delivered before it gets "I don't have
context for this thread," even though the finding is still on screen.

In your Slack app:

`POST /slack/events` 404s until `thread_capture: true` is actually deployed and
running, so configure the Event Subscription only once that rollout is live —
otherwise Slack's URL verification fails and the feature looks broken before
it's even been tried.

1. **OAuth & Permissions** → add the `app_mentions:read` bot scope (`chat:write`
   is already required for delivery), then reinstall the app.
2. **Event Subscriptions** → enable, set the Request URL to
   `https://<your-runlore>/slack/events`, and subscribe to the bot event
   `app_mention`. Slack verifies the URL with a signed challenge, so the endpoint
   must be reachable before you save.

Only `app_mention` is subscribed — RunLore reads nothing in channels where it
was not directly addressed.

### Announce a knowledge write to the channel

A captured note is reported into the thread it came from, and nowhere else — so
the knowledge lands, but the channel never learns it did. `announce_kb_updates`
changes that: every write that reaches the forge is also posted to the channel
this notifier delivers findings to.

```yaml
notify:
  slack:
    bot_token_env: SLACK_BOT_TOKEN
    channel: C0123456789
    thread_capture: true
  thread:
    announce_kb_updates: true    # default false
```

The channel post names the pull request, the entry that was created, who the note
came from and which chat system they typed it in, and quotes the note itself —
capped at 512 bytes, with the pull request holding the full text. It carries the
same secret-redacted, capped text that reached the forge, and everything a human
or the model wrote in it is escaped before posting: a `<!channel>` inside a note
renders as literal text, never as a channel-wide ping.

Two things to know before turning it on:

- **One write produces two messages, and that is intended.** The thread reply
  answers the person who typed; the channel post reaches everyone who was not
  reading that thread — which is the whole point of the feature. The announcement
  never posts back into the thread, so it is a second destination rather than a
  duplicate. With Slack as your only transport you will still see both.
- **The announcement carries note content.** A note written in a thread nobody else
  was watching is broadcast to every sink you have configured — this channel, a
  Matrix room if you run both, any `webhook` endpoint. That is your decision to
  take knowingly, which is why the key is off unless you set it.

### Conversational replies

Without `model.chat`, anything you say in the thread that is not `note:` gets a
fixed reply telling you so, and nothing is recorded. Adding a `model.chat` block
changes that: RunLore answers the question instead, from the investigation's own
evidence plus the top knowledge-base hits, and files a note itself when your
message contained a durable fact.

```yaml
model:
  chat:
    model: claude-haiku-4-5      # required — never inherited from model.model
notify:
  slack:
    thread_capture: true         # the chat layer has no channel without it
  thread:
    chat_calls_per_hour: 30      # default 30
    chat_tokens_per_hour: 107720 # default 107720 — derived, not round
```

**This is a paid path anyone in the channel can trigger.** With `model.chat` set,
every message that mentions the app in an investigation thread and is not a
recognised command costs **exactly one model call** — one structurally, not on
average: the model is given a single forced tool and no search tool, so there is
no agent loop.

`@runlore note: …` still costs nothing — it is the same deterministic write it
always was — and a bare mention with nothing after it costs nothing either.

Spend is bounded by two global hourly ceilings (`chat_calls_per_hour`,
`chat_tokens_per_hour`) shared across every thread and both transports. The token
default is *derived* — what one maxed-out call can cost, times the call budget —
which is why it is not a round number and why it moves when `max_note_bytes`
moves. **It is not bounded in currency**: `model.pricing` is a reporting table,
not a limit, and nothing compares a cost to a threshold and stops. Read
[Conversational replies and what they cost]({{< relref "/docs/configuration/configuration.md#conversational-replies-and-what-they-cost" >}})
before enabling it — it states the full set of bounds and, just as plainly, what
has none.

## Reference

- [Configuration → `notify`]({{< relref "/docs/configuration/configuration.md#notify--where-findings-go" >}})
  for the full key reference.
- [Security model → the feedback channels]({{< relref "/docs/security/security-model.md#the-feedback-channels---exposure--trust-model" >}})
  for the exposure and vote trust model.
- [Learning loop]({{< relref "/docs/concepts/learning-loop.md" >}}) — how feedback weighs recall.
