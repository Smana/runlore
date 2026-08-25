---
title: Matrix
weight: 402
integration: {kind: notifier, id: matrix}
---

**What it gives you** — findings delivered to any Matrix room, with an optional zero-ingress
👍/👎 feedback loop over reactions, and a 🔕 reaction (or a `silence:` thread command) to silence a
known incident.

## Minimal config

```yaml
notify:
  matrix:
    homeserver: https://matrix.org
    room_id: "!yourroom:matrix.org"
    access_token_env: MATRIX_TOKEN
```

## Verify it locally

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=MATRIX_TOKEN='<matrix-access-token>'
```

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) and
confirm delivery:

```bash
kubectl -n runlore logs deploy/runlore | grep 'msg=findings'
```

## Notes

- Matrix delivers the same content as Slack's incoming-webhook path: a **single** message per
  finding (no threading).
- **`matrix.feedback_reactions` (opt-in, default `false`)** — react 👍/👎 to a RunLore message and the
  rating lands in the outcome ledger, with the same per-user dedup and trust weighting as Slack's
  feedback buttons.
- **Nothing is exposed to enable it** — reactions arrive over the client-server `/sync` long-poll, an
  *outbound* HTTPS request authenticated by the notifier's own access token. This is the zero-ingress
  alternative to Slack's Interactivity Request URL.
- The listener runs on the leader only, skips reactions from before startup, ignores every emoji
  except 👍/👎/🔕, and only counts votes (and silences) on messages **the bot itself sent**
  (attribution is anchored on `/whoami`).
- Startup fails loud unless `homeserver`/`room_id`/`access_token_env` and `outcome.ledger_path` are
  set.
- Use an **invite-only room** — any room member can vote, and any room member can silence.
- **`feedback_reactions`, `silence_reactions` and `thread_capture` are independently-gated
  capabilities sharing ONE listener.** Enabling any of them starts the same `/sync` long-poll and the
  same leader-only goroutine — there is one Matrix listener and one `/sync` connection per process, no
  matter how many of the three are on. What differs per flag is which event types that shared `/sync`
  filter requests and which handler actually acts on them: `feedback_reactions` **and**
  `silence_reactions` both ask for `m.reaction` (👍/👎 and 🔕 are the same event type on the wire, and
  are told apart only once `handleReaction` reads which emoji it was — each still gated on its own
  flag, so `silence_reactions` off means a 🔕 reaction is read and dropped, never recorded); enabling
  either alone is therefore enough to receive the event, but recording a silence additionally needs
  `silence_reactions` itself on. `thread_capture` alone asks for `m.room.message`. Turning one flag on
  never turns another's handling on — each is gated on its own flag in code, not just
  requested-or-not on the wire — but it does mean a homeserver hiccup or a leadership change pauses
  whichever capabilities are enabled together, since they ride the same poll loop.

### Silence a recurring incident

**`notify.matrix.silence_reactions` (opt-in, default `false`)** turns on a 🔕 reaction; a second
path, the `silence: <duration>` thread command, needs `thread_capture` too. Both land in the same
outcome ledger as Slack's `silence_button`:

- **A 🔕 reaction** on a RunLore investigation message — the zero-ingress equivalent of Slack's
  overflow menu. A bare reaction carries no duration, so it always silences for
  `notify.silence.windows[0]` (the FIRST configured preset) — there is no way to pick a longer
  window from a reaction alone. `silence_reactions` alone is enough: the `/sync` filter already
  carries `m.reaction` for feedback voting, so no other flag is required.
- **A `silence: <duration>` thread command** — reply in the investigation thread with, for example
  `silence: 4h`, and RunLore silences for exactly that duration (up to `notify.silence.max_window`).
  The `silence:` token is matched case-insensitively (`SILENCE: 4h` works) and must **start** your
  message, once the bot mention is stripped. That is stricter than `note:` and `reinvestigate:`, which
  are recognised anywhere in a message, and deliberately so: acting on this one *writes* — it records
  a ledger event and switches investigation off — so a sentence that merely mentions it (`note: we
  agreed on silence: 4h`) is answered with a request to rephrase rather than silently muting the
  incident and discarding the note. A sentence using the word without a trailing colon is not a
  command at all. This path needs
  no `model.chat` configuration and costs no model call: parsing a duration is deterministic.
  **Unlike the reaction, it also needs `notify.matrix.thread_capture: true`** — a plain message only
  reaches RunLore at all once the `/sync` filter widens to `m.room.message`, which `silence_reactions`
  on its own does not request. That in turn means the same forge/registry/replier prerequisites
  [Write knowledge back from a thread](#write-knowledge-back-from-a-thread) below documents for
  `thread_capture` apply here too, even though recording a silence never touches the forge itself:
  with any of those three missing, `thread_capture` silently degrades to off and the command is never
  parsed. Either `silence_reactions` or Slack's `silence_button` satisfies the shared
  `notify.silence` requirements (`outcome.ledger_path`, non-empty `windows`, a positive `max_window`)
  the command also depends on.

Either path suppresses re-investigating this exact `TriggerKey` — **no model call, no notification,
no ledger open** — until the window expires, the incident fires again as **CRITICAL**, a standing
**👎** re-arms it, or the incident **resolves**. Every silence, whichever path recorded it, gets the
same acknowledgement: a message naming who silenced it, until when, and restating those four escapes.

**Two of those four are alert-specific**, exactly as on the Slack side (see [Slack → Silence a
recurring incident]({{< relref "/docs/integrations/notifications/slack.md#silence-a-recurring-incident" >}})
for the full explanation): a GitOps-sourced failure sets no severity, so the CRITICAL carve-out never
fires, and its fingerprint is synthetic with no resolve channel, so the resolve escape never fires
either. **For a GitOps failure, a silence is bounded by its expiry and a 👎 — and nothing else.** The
same loss of the resolve escape applies to an Alertmanager receiver configured with
`send_resolved: false`.

```yaml
notify:
  silence:
    windows: [1h, 4h, 24h]
    max_window: 24h
  matrix:
    homeserver: https://matrix.org
    room_id: "!yourroom:matrix.org"
    access_token_env: MATRIX_TOKEN
    silence_reactions: true
outcome:
  ledger_path: /var/lib/runlore/outcome.jsonl
```

Like Matrix's feedback reactions, silencing is deliberately **unprivileged** — any member of the
room can silence an incident, with no allowlist — which is safe for the same reason Slack's button
is: for an alert-sourced incident the blast radius is bounded by the four escapes above (narrower for
a GitOps failure or a `send_resolved: false` receiver, per above), every silence is attributed to the
sender's Matrix id in the outcome ledger, and the window is capped by `notify.silence.max_window`.
Use an **invite-only room**, as for feedback reactions.

`silence_reactions` is gated **independently** of `feedback_reactions` — enabling it on its own
records 🔕 silences without recording 👍/👎 votes, and vice versa.

### Write knowledge back from a thread

With `thread_capture` on, replying inside an investigation thread records what you know into the
knowledge base:

    @runlore note: the real cause was the spot-node reclaim at 14:02

RunLore recognises being addressed via MSC3952 `m.mentions` when your client sends it, or — as a
fallback — the bot's full Matrix ID or its localpart (`runlore` in `@runlore:example.org`)
appearing in the message body **as a whole word**. The word boundary is not decoration: a plain
substring match would read the localpart `sre` inside "misread" as addressing the bot. A reply is attributed to its thread via the MSC3440
`m.thread` relation, falling back to `m.in_reply_to` for clients that only send the legacy
non-threaded reply fallback.

The note lands as a comment on that finding's knowledge-base PR. When the finding has no PR — an
instant recall, or a `no_action` verdict — RunLore opens a small `Concept` entry PR instead, so the
knowledge still lands somewhere. A human reviews and merges it, like every other entry.

`@runlore reinvestigate: …` is reserved and not supported yet; add the `reinvestigate` label to the
knowledge-base issue to re-run an investigation.

```yaml
notify:
  matrix:
    homeserver: https://matrix.org
    room_id: "!yourroom:matrix.org"
    access_token_env: MATRIX_TOKEN
    thread_capture: true
outcome:
  ledger_path: /var/lib/runlore/outcome.jsonl   # required: the thread registry lives beside it
```

`thread_capture: true` also requires `outcome.ledger_path` — the same registry Slack's thread
capture uses (one registry, shared by both transports): see [Configuration →
`notify`]({{< relref "/docs/configuration/configuration.md#notify--where-findings-go" >}}) for the
full persistence breakdown. **Matrix degrades more gracefully than Slack when the registry itself
is unavailable** (past its TTL, evicted past the size cap, or orphaned by a restart or leader
failover onto a replica that never saw it): every investigation message RunLore posts also carries
its own thread identity stamped directly on the Matrix event. A reply still resolves against that
stamp — fetched straight from the homeserver and re-verified as sent by RunLore itself — so a
registry miss answers the note instead of refusing it with "no context for this thread." What the
registry alone tracks (the standalone note PR a thread already opened, and how many notes it has
used against the per-thread cap) is unavailable on a fallback resolution, so a note recorded this
way opens a fresh `Concept` PR rather than commenting on one an earlier note in the same thread
opened. Slack has no per-event stamp to fall back to, so a registry miss there is unrecoverable —
see Configuration's restart/leader-failover warning for what that costs.

**Unlike Slack, this needs no exposed HTTP endpoint, no ingress change and no new permission.**
Mentions arrive over the same outbound `/sync` long-poll RunLore already runs for
`feedback_reactions` — turning `thread_capture` on only widens what that poll asks the homeserver
for. That widening is real and worth understanding before you flip the flag: see [Security model →
Matrix thread
capture]({{< relref "/docs/security/security-model.md#matrix-thread-capture-notifymatrixthread_capture--a-widened-sync-filter" >}}).

### Announce a knowledge write

A captured note is reported into the thread it came from, and nowhere else — so the
knowledge lands, but nobody outside that thread learns it did. `announce_kb_updates`
changes that: every write that reaches the forge is also announced, with its own
message naming who wrote the note and where.

```yaml
notify:
  matrix:
    homeserver: https://matrix.org
    room_id: "!yourroom:matrix.org"
    access_token_env: MATRIX_TOKEN
    thread_capture: true
  thread:
    announce_kb_updates: channel  # default false; also true (= channel), thread, both
```

The post names the pull request, the entry that was created, who the note came from
and which chat system they typed it in, and quotes the note itself — capped at
512 bytes, with the pull request holding the full text. It carries the same
secret-redacted, capped text that reached the forge, and everything a human or the
model wrote in it is neutralised before sending: an `@room` inside a note is rendered
readable but inert, never a room-wide notification. That holds wherever it lands.

**Where it lands is yours to pick.** `channel` (what `true` has always meant) sends a
plain `m.notice` with no `m.thread` relation to the room this notifier delivers
findings to. `thread` sends it into the thread the note was typed in, as an
`m.notice` carrying the MSC3440 `m.thread` relation. `both` does each.

- **`channel` produces two messages for one write, and with several sinks that is the
  point.** The thread reply answers the person who typed; the room post reaches
  everyone who was not reading that thread. But **with Matrix as your only transport
  those are the same people** — the thread lives in that very room — so each write
  restates in the room what the thread just said. That is what `thread` is for: the
  announcement goes into the thread instead, keeping the provenance line the reply
  does not carry, without the echo.
- **`thread` never silences your other sinks.** A Slack channel, a `webhook`
  endpoint — neither can post into a Matrix thread, so they each receive the
  announcement in their own channel exactly as before. The echo only exists where the
  thread and the room are the same place.
- **The announcement carries note content.** A note written in a thread nobody else was
  watching is broadcast to every sink you have configured — this room, a Slack channel
  if you run both, any `webhook` endpoint. That is your decision to take knowingly,
  which is why the key is off unless you set it.

### Conversational replies

Without `model.chat`, anything you say in the thread that is not `note:` gets a fixed reply telling
you so, and nothing is recorded. Adding a `model.chat` block changes that: RunLore answers the
question instead, from the investigation's own evidence plus the top knowledge-base hits, and files a
note itself when your message contained a durable fact.

```yaml
model:
  chat:
    model: claude-haiku-4-5      # required — never inherited from model.model
notify:
  matrix:
    thread_capture: true         # the chat layer has no channel without it
  thread:
    chat_calls_per_hour: 30      # default 30
    chat_tokens_per_hour: 109480 # default 109480 — derived, not round
```

**This is a paid path any member of the room can trigger, and on Matrix "addressed" is looser than
you may expect.** RunLore treats a message as addressed to it via MSC3952 `m.mentions` — but also
when the bot's full MXID **or its bare localpart** (`runlore` in `@runlore:example.org`) appears in
the body **as a whole word**. **No mention entity is required.** So anyone in the room can trigger a
model call by typing the bot's name in a reply to an investigation thread. Keep the room invite-only.

With `model.chat` set, every addressed message rooted in one of RunLore's own messages that is not a
recognised command costs **exactly one model call** — one structurally, not on average: the model is
given a single forced tool and no search tool, so there is no agent loop.

`@runlore note: …` still costs nothing — it is the same deterministic write it always was — and a
bare mention with nothing after it costs nothing either.

Spend is bounded by two global hourly ceilings (`chat_calls_per_hour`, `chat_tokens_per_hour`) shared
across every thread and both transports. The token default is *derived* — what one maxed-out call can
cost, times the call budget — which is why it is not a round number and why it moves when
`max_note_bytes` moves. **It is not bounded in currency**: `model.pricing` is a reporting table, not
a limit, and nothing compares a cost to a threshold and stops. Read
[Conversational replies and what they cost]({{< relref "/docs/configuration/configuration.md#conversational-replies-and-what-they-cost" >}})
before enabling it — it states the full set of bounds and, just as plainly, what has none.

## Reference

- [Configuration → `notify`]({{< relref "/docs/configuration/configuration.md#notify--where-findings-go" >}})
  for the full key reference.
- [Security model → the feedback channels]({{< relref "/docs/security/security-model.md#the-feedback-channels---exposure--trust-model" >}})
  for the exposure and vote trust model.
