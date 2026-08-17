---
title: Matrix
weight: 402
integration: {kind: notifier, id: matrix}
---

**What it gives you** — findings delivered to any Matrix room, with an optional zero-ingress
👍/👎 feedback loop over reactions.

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
  except 👍/👎, and only counts votes on messages **the bot itself sent** (attribution is anchored on
  `/whoami`).
- Startup fails loud unless `homeserver`/`room_id`/`access_token_env` and `outcome.ledger_path` are
  set.
- Use an **invite-only room** — any room member can vote.
- **`feedback_reactions` and `thread_capture` are two independently-gated capabilities sharing ONE
  listener.** Enabling either starts the same `/sync` long-poll and the same leader-only goroutine —
  there is one Matrix listener and one `/sync` connection per process, never two. What differs per
  flag is which event types that shared `/sync` filter requests and which handler actually acts on
  them: `feedback_reactions` alone asks for `m.reaction` and only ever calls the reaction handler;
  `thread_capture` alone asks for `m.room.message` and only ever calls the message handler. With both
  on, the filter requests both event types over that same connection. Turning one on never turns the
  other's handling on — each is gated on its own flag in code, not just requested-or-not on the wire —
  but it does mean a homeserver hiccup or a leadership change pauses whichever capabilities are
  enabled together, since they ride the same poll loop.

### Write knowledge back from a thread

With `thread_capture` on, replying inside an investigation thread records what you know into the
knowledge base:

    @runlore note: the real cause was the spot-node reclaim at 14:02

RunLore recognises being addressed via MSC3952 `m.mentions` when your client sends it, or — as a
fallback — the bot's full Matrix ID or its localpart (`runlore` in `@runlore:example.org`)
appearing anywhere in the message body. A reply is attributed to its thread via the MSC3440
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

## Reference

- [Configuration → `notify`]({{< relref "/docs/configuration/configuration.md#notify--where-findings-go" >}})
  for the full key reference.
- [Security model → the feedback channels]({{< relref "/docs/security/security-model.md#the-feedback-channels---exposure--trust-model" >}})
  for the exposure and vote trust model.
