---
title: Slack
weight: 401
integration: {kind: notifier, id: slack}
---

**What it gives you** — findings delivered to a channel, with optional Approve/Reject buttons,
one-click 👍/👎 diagnosis feedback, and a 🔕 control to silence a known incident.

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
- **Approve/Reject buttons** (`actions.mode: approve`), **`feedback_buttons`** (opt-in 👍/👎 rating)
  and **`silence_button`** (opt-in 🔕 control — see [Silence a recurring
  incident](#silence-a-recurring-incident) below) all need `signing_secret_env` set **and** the same
  exposure: Slack **Interactivity** turned on, with Request URL
  `https://<your-runlore-host>/slack/interactions` reachable **from Slack's servers**.
  - `api.slack.com/apps` → your app → **Interactivity & Shortcuts** → toggle **On**, Request URL =
    `https://<your-runlore-host>/slack/interactions`.
  - Read-only deployments (no actions, no feedback buttons, no silence button) need none of this.
  - Route it through your ingress/gateway; if you use the chart's `networkPolicy.ingressFrom`, allow
    your ingress controller, not the internet.
- `feedback_buttons: true` also requires `outcome.ledger_path` **and a delivery target**
  (`webhook_url_env`, or `bot_token_env` with `channel`) — startup fails loud otherwise, since a button
  only exists on a message RunLore actually delivered. Ratings land in the outcome ledger and weigh the
  recalled entry's trust, exactly like resolve signals do. `silence_button` has the identical
  requirement, for the identical reason — its control lives on the same message.
- Mount the credential too: an env var that is set but **empty** skips the Slack notifier altogether —
  nothing is delivered, no buttons render, no rating can be recorded. Validation cannot see that, so
  startup warns (`no slack delivery target resolved`) instead of reporting the feature enabled.
- Feedback is deliberately **unprivileged**: any signature-valid member of your workspace can rate
  (approve/reject keep their own `approver_ids` allowlist). One live vote per (trigger key, Slack user);
  latest wins.
- `signing_secret_env` HMAC-verifies every button click (±5 min replay window).

### Silence a recurring incident

**`notify.slack.silence_button` (opt-in, default `false`)** adds a third control to investigation
messages, beside 👍/👎: a 🔕 overflow menu offering the durations listed in `notify.silence.windows`
(e.g. `1h`, `4h`, `24h`). Where a 👍/👎 records an *opinion* about the diagnosis, picking a window
from the 🔕 menu changes what RunLore **does**: it suppresses re-investigating this exact
`TriggerKey` for the chosen window — **no model call, no notification, no ledger open** — enforced in
`RecurrenceGate.decide` before the paid investigation loop even starts. It works even with
`investigation.recurrence_cooldown` left at its default of `0` (off): the human silence check and the
machine cooldown are independent, and the silence is checked first.

The silence stands until one of **four** things happens:

- the window **expires**;
- the incident fires again as **CRITICAL** — a silence never mutes a page, the same carve-out the
  debouncer makes for criticals;
- a colleague casts a standing **👎** on the trigger — a human contesting the diagnosis outranks an
  earlier human silencing it, and re-arms investigation immediately;
- the incident **resolves** — a resolve clears the silence outright, so the next occurrence gets a
  fresh look.

**Two of those four are alert-specific and do not apply to a GitOps-sourced failure.**
`FromFailureEvent` never sets a severity, so the CRITICAL carve-out can never fire; and a GitOps
failure's fingerprint is synthetic, so it has no resolve channel at all — nothing can ever clear the
silence early. **For a GitOps failure, a silence is bounded by its expiry and a colleague's 👎 — and
nothing else.** The same loss of the resolve escape (but not the CRITICAL one, since a real severity
is still present) applies to an Alertmanager receiver configured with `send_resolved: false`.

The click is acknowledged with a message naming who silenced it, until when, and restating the escapes
above, so nobody has to guess why the channel went quiet.

Like `feedback_buttons`, silencing is deliberately **unprivileged**: any signature-valid member of
your workspace can silence an incident — there is no `approver_ids`-style allowlist. That is safe for
an alert-sourced incident, where the blast radius is bounded four independent ways (the escapes
above); for a GitOps failure, or a `send_resolved: false` receiver, it is narrower — per above — but
still bounded by expiry and a 👎. Every silence is attributed to the clicking Slack user's id in the
outcome ledger (so a bad click is auditable, not anonymous), and the window itself is capped by
`notify.silence.max_window` — nothing can be silenced indefinitely.

```yaml
notify:
  silence:
    windows: [1h, 4h, 24h]
    max_window: 24h
  slack:
    webhook_url_env: SLACK_WEBHOOK_URL   # or bot_token_env + channel — either delivery target works
    silence_button: true
    signing_secret_env: SLACK_SIGNING_SECRET
outcome:
  ledger_path: /data/outcome.jsonl
```

`silence_button` is gated **independently** of `feedback_buttons` — a deployment can enable either
without the other, and each renders only its own control on the card. With `feedback_buttons: false`
and `silence_button: true`, an investigation message shows the 🔕 menu and nothing else.

**The button is not the only way in.** With `thread_capture` also on (see below), replying
`@runlore silence: 4h` in the investigation thread does exactly what the button does — same parser,
same ledger write, same acknowledgement — because Slack's thread mentions and the 🔕 button both
route through the one shared `thread.Responder`. That command path is gated on
`notify.silence` being enabled at all (either `silence_button` here **or** Matrix's
`silence_reactions` — whichever transport you turned it on for) plus Slack's own `thread_capture`,
**not** specifically on `silence_button` — so a deployment running `thread_capture: true` with
`silence_button: false` still accepts `silence: 4h` typed in a thread, as long as silencing is
enabled somewhere. See [Matrix → Silence a recurring
incident]({{< relref "matrix.md#silence-a-recurring-incident" >}}) for the same command in detail —
the grammar and the ledger write are identical across both transports.

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

#### Pre-flight: prove the endpoint answers

`POST /slack/events` 404s until `thread_capture: true` is actually deployed and
running, so configure the Event Subscription only once that rollout is live —
otherwise Slack's URL verification fails and the feature looks broken before
it's even been tried. Send one unsigned POST from outside the cluster, against
the hostname Slack will use, and **read the body, not just the code** — the body
is what tells you whether RunLore answered or something in front of it did:

```console
$ curl -sS -w '\n%{http_code}\n' -X POST https://<your-runlore>/slack/events
unauthorized
401
```

| code | body | what it means | do next |
|---|---|---|---|
| **401** | `unauthorized` | the request reached RunLore and failed signature verification: the path routes and the HMAC is enforced | configure Slack |
| **404** | `slack events not enabled` | **RunLore answered** — the path routes, but the handler is not wired | see *not wired* below |
| **404** | anything else (HTML, `default backend`) | your ingress answered — the path is **not routed** to RunLore | see *not routed* below |
| **200** | anything | nothing is verifying signatures on that URL | stop — do not point Slack at it |
| **405** | `Method Not Allowed` | you sent a `GET`; the route is `POST`-only | resend with `-X POST` |
| **503** | `no leader known; retry` / `leader unreachable; retry` | leader-forwarded, and no leader is routable right now | retry once leader election settles |
| **421** | `not the leader (request already forwarded once)` | mid-failover, on a stale holder view | retry against the Service |
| anything else | — | the ingress or a proxy answered, not RunLore (`502`, `504`, a redirect to an SSO page…) | read the body; fix routing first |

**Not wired.** A handler that is actually serving announces itself at startup:

```
msg="slack thread capture enabled" endpoint=/slack/events
```

No such line ⇒ grep the warning beside it, which names the reason: `no forge is
configured`, `no bot-token delivery target resolved`, or `no thread-capable
notifier resolved`. **The endpoint is also unserved when `signing_secret_env`
names a variable that is present but empty** (an unmounted secret, a blank Helm
value) — an endpoint that cannot verify a signature is not exposed at all. That
one is the trap: the startup line still prints, and nothing warns. So treat the
pre-flight as authoritative for *routing*, and the startup log as authoritative
for *wiring*; check both.

**Not routed.** `/slack/events` is a **separate route** from
`/slack/interactions`. On an ingress that lists paths individually rather than
forwarding the whole service, that is the failure this check exists to catch:
Approve/Reject and the feedback buttons keep working, so the Slack integration
looks healthy, while every mention silently never arrives. Route both.

#### Configure the Slack app

> [!WARNING]
> **Reinstalling can issue a new bot token — verify, don't assume.** If it does
> and your secret store still holds the previous value, finding delivery stops
> outright, which is loud. The quiet half is threads posted *before* the
> rotation: a note against one still reaches the knowledge base and only the
> acknowledgement fails, which reads as broken capture. Verify against the value
> the cluster actually holds, not whatever is in your shell:
>
> ```console
> $ TOKEN=$(kubectl -n runlore get secret runlore-secrets \
>     -o jsonpath='{.data.SLACK_BOT_TOKEN}' | base64 -d)
> $ curl -sS -X POST -H "Authorization: Bearer $TOKEN" https://slack.com/api/auth.test
> {"ok":true,…}
> ```
>
> `{"ok":false,"error":"not_authed"}` means the token was empty, not that it was
> rejected. `invalid_auth`, `token_revoked`, `token_expired` and
> `account_inactive` all mean the same remedy: copy the current **Bot User OAuth
> Token** from **OAuth & Permissions** into the secret store, then **restart the
> pod** — the token is read from the environment once, at startup, so until you
> restart, the pod is still using whatever the Secret held when it started.
>
> The **signing secret is per-app, not per-install**, so a reinstall never
> touches it: mentions keep arriving, and `/slack/events` keeps answering `401`
> to the pre-flight, while replies fail.

1. **OAuth & Permissions** → add the `app_mentions:read` bot scope (`chat:write`
   is already required for delivery), then reinstall the app.
2. **Event Subscriptions** → enable, set the Request URL to
   `https://<your-runlore>/slack/events`, and subscribe to the bot event
   `app_mention`. Slack verifies the URL with a signed challenge, so the endpoint
   must be reachable before you save.

The bot must be **a member of the channel**: Slack only sends `app_mention` from
conversations the app is in. Delivery already requires this, so if findings are
arriving, it is satisfied.

Saving the Request URL **is** the live test of `signing_secret_env`: Slack signs
the `url_verification` challenge like every other delivery, and RunLore verifies
it before echoing it back. **Success is silence** — an accepted challenge logs
nothing. A mismatched secret logs
`msg="rejected slack event: bad signature"` and Slack reports the URL as
unverified.

Only `app_mention` is subscribed — RunLore reads nothing in channels where it
was not directly addressed.

### Announce a knowledge write

A captured note is reported into the thread it came from, and nowhere else — so
the knowledge lands, but nobody outside that thread learns it did.
`announce_kb_updates` changes that: every write that reaches the forge is also
announced, with its own message naming who wrote the note and where.

```yaml
notify:
  slack:
    bot_token_env: SLACK_BOT_TOKEN
    channel: C0123456789
    thread_capture: true
  thread:
    announce_kb_updates: channel  # default false; also true (= channel), thread, both
```

The post names the pull request, the entry that was created, who the note came
from and which chat system they typed it in, and quotes the note itself — capped
at 512 bytes, with the pull request holding the full text. It carries the same
secret-redacted, capped text that reached the forge, and everything a human or the
model wrote in it is escaped before posting: a `<!channel>` inside a note renders
as literal text, never as a channel-wide ping. That holds wherever it lands.

**Where it lands is yours to pick.** `channel` (what `true` has always meant) posts
to the channel this notifier delivers findings to. `thread` posts into the thread
the note was typed in. `both` does each.

- **`channel` produces two messages for one write, and with several sinks that is
  the point.** The thread reply answers the person who typed; the channel post
  reaches everyone who was not reading that thread. But **with Slack as your only
  transport those are the same people** — the thread lives in that very channel —
  so each write restates in the channel what the thread just said. That is what
  `thread` is for: the announcement goes into the thread instead, keeping the
  provenance line the reply does not carry, without the echo.
- **`thread` never silences your other sinks.** A Matrix room, an incoming-webhook
  Slack, a `webhook` endpoint — none of them can post into a Slack thread, so they
  each receive the announcement in their own channel exactly as before. The echo
  only exists where the thread and the channel are the same place.
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
    chat_tokens_per_hour: 109480 # default 109480 — derived, not round
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
- [Troubleshooting → A Slack mention in a thread does nothing]({{< relref "/docs/operations/troubleshooting.md#a-slack-mention-in-a-thread-does-nothing" >}})
  — the log lines to grep once it is deployed and a mention lands nowhere.
