# Thread interaction — design

**Date:** 2026-08-14
**Status:** approved for planning

## Problem

RunLore delivers a finding to chat and the conversation stops there. The thread
is a dead end in both directions:

- **Knowledge flows out, never in.** The on-call reads the card at the exact
  moment they hold knowledge the agent does not — the real cause, the step that
  actually fixed it, the fact that the suggested resolution is stale since the
  Karpenter migration. Today the only channel back is 👍/👎: one bit, no prose.
  Everything else is typed into the thread for other humans and lost.
- **A wrong finding stays wrong where it matters.** The Slack card is read once
  and scrolls away, but the drafted KB entry is *served back* — with confidence,
  through the recall path — every time that incident recurs. A recall is 2 model
  calls against 7 and lands in seconds; that leverage runs in both directions. The
  highest-value write in the system is correcting an entry **before** it merges,
  and the one human who knows it is wrong is standing in the thread with no way to
  say so.
- **The correction path that exists is out-of-band.** `reinvestigate.go` re-runs
  an investigation when a human labels a forge issue. That means leaving chat,
  finding the repo, finding the issue, labelling it — during an incident.

This is the same adoption gap the KB-human-surfaces round addressed from the read
side: the humans who pay the curation cost get no way to *contribute* at the
moment they are best placed to.

`2026-07-07-kb-human-surfaces-design.md` deferred this deliberately ("Out of
scope: Slack slash command… revisit after the three features above land"). They
landed.

## Scope

One capability — **the thread becomes a write path into the KB** — delivered
across two transports and one optional model layer.

In scope:

1. Shared, transport-agnostic core: parse an addressed message, decide where the
   knowledge lands, write it, return a reply.
2. Slack transport (inbound Events API).
3. Matrix transport (inbound over the existing `/sync` long-poll).
4. Optional conversational replies via a cheap model.

### Non-goals

- **Re-running an investigation from the thread.** `@runlore reinvestigate: …`
  is a *reserved* prefix in the grammar; v1 does not implement it. Rationale:
  the capability already exists behind the `reinvestigate` label, a re-run costs
  ~7 model calls and is trivially spammable by anyone in the channel during the
  worst possible moment, and — the decisive point — the chat layer answers most
  "you're wrong" challenges without one, because it already holds the
  investigation's ruled-out hypotheses, open questions and data gaps. Revisit if
  dogfooding shows thread traffic is mostly *"go look again"* rather than
  *"here is what it actually was"*.
- **Amending an already-merged entry in place.** Needs a new forge method (fetch
  blob SHA, PUT with `sha`) on both GitHub and GitLab. v1 opens a new entry that
  *links* the one it corrects and lets the reviewer fold it in.
- **One-way notifiers** (`webhook`, `templated`). See below — stated, not silently
  skipped.
- Editing the delivered card after the fact. It is history; the KB is the record.

## Architecture — one responder, two transports

The knowledge-writing logic is transport-agnostic and lives in one place. Each
transport is a thin adapter that answers three questions its own way: *did a human
address us?*, *which investigation is this thread about?*, *how do we reply?*

```
   Slack: POST /slack/events            Matrix: existing /sync long-poll
   (exposed, signature-verified)        (outbound, nothing exposed)
              │                                       │
              └──────────────┬────────────────────────┘
                             ▼
                    thread.Responder            ← knows nothing about Slack/Matrix
                             │
              ┌──────────────┼──────────────┐
              ▼              ▼              ▼
      CommentOnPR       OpenPR(Concept)   reply text
      (existing)        (existing)        → transport posts it
```

`internal/thread` depends on `providers` and `internal/catalog` only. Both
transports are tested against the same responder with fake forges.

## Shared core — `internal/thread` (new package)

### `thread.Context`

The transport-agnostic answer to "which investigation is this thread about":

```go
type Context struct {
    Transport      string    // "slack" | "matrix" — for logs and metrics only
    Root           string    // opaque transport handle: Slack thread_ts, Matrix event id
    Channel        string    // Slack channel id; Matrix room id
    TriggerKey     string
    DupFingerprint string
    Title          string
    Resource       string
    Verdict        providers.Verdict
    CuratedURL     string    // KB PR opened for this finding; "" when none
    RecalledEntry  string    // bundle-relative entry path when the answer was a recall
    NoteURL        string    // set after this thread opens a standalone PR (see routing)
    At             time.Time
}
```

### `thread.Responder`

```go
func (r *Responder) Handle(ctx context.Context, tc Context, text, author string) (reply string, err error)
```

Steps, in order:

1. **Strip the mention** — the transport hands over raw body text; the responder
   removes a leading `<@U…>` / MXID / display-name mention and trims.
2. **Parse the grammar** (below). Unknown reserved prefix → a reply naming the
   prefixes it understands; never a silent drop.
3. **Compose the note body** — deterministic path: the human's text verbatim,
   wrapped in a provenance header (`author`, transport, timestamp, thread link).
   Chat path: the model's `kb_note`, same header, *plus* the human's verbatim text
   below it — the raw words are never replaced by a paraphrase.
4. **Route and write** (below).
5. **Return the reply text.** The transport posts it.

The responder never chooses the write target from model output; the route is
derived from `Context` alone.

### Grammar

| Input | Behaviour | Model calls |
|---|---|---|
| `@runlore note: <text>` | Verbatim capture. Works with no model configured. | 0 |
| `@runlore <anything else>` | Chat layer if `model.chat` is set; otherwise treated as `note:` with a reply saying so. | 0 or 1 |
| `@runlore reinvestigate: …` | Reserved. Replies that it is not supported yet. | 0 |

Reserving `reinvestigate:` in v1 — even unimplemented — is what makes adding it
later a handler case rather than a grammar migration.

### Routing

| Thread state | Route | Why |
|---|---|---|
| `CuratedURL` set (open KB PR) | `CurationForge.CommentOnPR` | The knowledge belongs on the artifact a human is about to review. Zero new forge code. |
| `NoteURL` set (this thread already opened one) | `CommentOnPR` on that PR | Prevents five notes becoming five PRs. |
| `RecalledEntry` set, no PR | `OpenPR` — new `Concept`, body links the recalled entry | Recalls skip curation entirely, so there is nothing to comment on. |
| Neither (`no_action` verdict, below the confidence gate, coalesced) | `OpenPR` — new `Concept` | The knowledge still has to land somewhere. |

After a successful `OpenPR`, its URL is written back into the `Context` as
`NoteURL` so subsequent notes in the same thread comment instead of opening again.

**Why `Concept`.** `kbvalidate.ValidateStructural` requires the
`Symptom`/`Cause`/`Resolution` body sections and a `resource` **only for
`Incident`**; `Playbook`/`Concept` are deliberately relaxed
(`kbvalidate.go:145,186`). A bare operator note has no evidence sections and
often no single resource, so typing it `Concept` clears the merge gate honestly
instead of fabricating sections. `OpenPR` already handles branch → OKF file →
`maintainBundle` → PR → labels in one call (`github.go:117`), so a standalone
note PR needs no new forge surface at all.

**Known cost:** this creates a new stream of small `Concept` entries that enter
the BM25 corpus once merged, which can dilute recall. The mitigations are the
ones already in the system — a human merges every one, and `curate`'s lifecycle
sweep can retire them — plus a per-thread cap. Accepted deliberately; revisit if
the corpus shows drift.

### Rate limits

`internal/ratelimit.Window` on two axes, both leader-local:

- Per thread: 20 notes total (further mentions get a reply, no write).
- Per hour, global: caps `OpenPR` calls and (when the chat layer is on) model
  calls, so a chatty channel cannot become a forge- or token-spend incident.

## Slack transport

### Inbound: `POST /slack/events`

Chosen over Socket Mode. Socket Mode needs no public endpoint, but costs a
hand-rolled WebSocket client plus a reconnect loop in the leader and reuses none
of the existing verified-request machinery. The Events API reuses `verifySlack`,
the `work()` leader-forwarding wrapper and the ingress that any deployment with
`feedback_buttons` already exposes — which is every deployment where the KB loop
is running.

The handler:

- 404s unless `notify.slack.thread_capture` is on, mirroring
  `handleSlackInteraction`'s gating.
- `type: url_verification` → echo `challenge` (still signature-verified).
- `type: event_callback` with `event.type: app_mention` → **ack 200 immediately**,
  process asynchronously on a bounded worker. Slack's 3s deadline cannot
  accommodate a model call plus a forge round-trip.
- Dedup on `event_id` (bounded seen-set): Slack retries anything it does not see
  acked, and a retry must not file the note twice.
- Drop events carrying `bot_id`, or authored by the app's own user id
  (loop guard).
- `event.thread_ts` absent → not a thread reply; reply pointing at the thread.

Subscribing to `app_mention` **only**, never `message.channels`: explicit consent,
no firehose, and nothing is read in channels where RunLore was not addressed.

New Slack scope: `app_mentions:read`. `chat:write` is already held.

### Context resolution: the registry

*(Lives in `internal/thread` and is transport-neutral — it is described here
because Slack is the transport that depends on it for lookup. Matrix uses it for
one narrow purpose only; see that section.)*

Slack's `app_mention` payload carries the thread id and the text — not the parent
message. Resolving "which investigation" therefore needs either a local record or
a fetch of the root message, and the fetch (`conversations.replies`) requires
`channels:history`, i.e. read access to the entire channel's history. That is a
disproportionate grant for a cluster-reading agent, so Slack keeps a record:

- `notify.ThreadSink` — a nil-safe optional interface (same shape as
  `curator.ConfirmationSink`), called by `SlackBot.Deliver` after the summary post
  returns its `ts`. The notifier stays ignorant of the registry.
- Storage: bounded LRU with a TTL (default 7 days), appended as JSONL in the
  outcome ledger's state dir and replayed on startup — the mechanism
  `outcome.Ledger` already uses, for the same reason: `/slack/events` is
  `work()`-forwarded to the leader, so a failover with an in-memory-only registry
  would orphan every live thread.
- Unknown thread → a reply saying so, never a silent drop.

**Rejected alternative:** Slack `message_metadata` on the root message. Stateless
and restart-proof, and it would delete this component — but it is readable only
via `conversations.replies`, so it buys simplicity with the `channels:history`
scope. Not worth it.

### Reply

`chat.postMessage` with `thread_ts` — the same call `Deliver` already makes for
the detail reply.

## Matrix transport

Matrix needs **no new inbound surface and no new permission**, and — because the
mechanism already exists for 👍/👎 — no registry lookup to answer *"which
investigation is this thread about"*. The context rides on the event.

### Context resolution: stamped on the event

`Matrix.Deliver` already stamps `io.runlore.trigger_key` into the event content
unconditionally (`matrix.go:90` — "the field is inert data; the LISTENER is the
opt-in"), and `MatrixFeedback.triggerKeyFor` (`matrix_feedback.go:269`) already
fetches an event by id and reads that field back through a bounded cache.

Widen both sides:

- Stamp a second custom field, `io.runlore.thread`, carrying the `Context`
  identifiers (trigger key, dup fingerprint, curated URL, recalled entry,
  resource, verdict). Keep `io.runlore.trigger_key` exactly as it is — the
  reaction listener reads it and must not break, and old events must keep working.
- Generalise `triggerKeyFor` into `contextFor(eventID) (thread.Context, bool)`,
  reusing the same fetch and the same `keyByEvent` cache (renamed).

**Where Matrix does use the registry.** Reading context off the event covers
lookup, but not the `NoteURL` write-back: a Matrix event is immutable, so "this
thread already opened a standalone PR" cannot be stamped back onto the root after
the fact. `OpenPR` offers no protection of its own — its branch name carries a
unix timestamp (`github.go:120`), so a second call opens a second PR rather than
no-oping.

The registry is therefore transport-neutral, and Matrix writes to it too — but
only this one field, and only when a standalone PR is opened. It is never on the
Matrix read path, so a registry miss there (restart, TTL expiry) costs at most one
duplicate PR in a thread, never a lost note.

### Inbound: the existing `/sync` loop

`MatrixFeedback.Run` already long-polls with a server-side filter scoped to one
room and `timeline.types: ["m.reaction"]`. When `notify.matrix.thread_capture` is
on, widen that filter to `["m.reaction", "m.room.message"]` and handle the new
type:

- Addressed to us: `m.mentions.user_ids` contains `self` (MSC3952, set by modern
  clients), falling back to the body containing the bot's MXID or localpart.
- Threaded: `m.relates_to.rel_type == "m.thread"` → root is `event_id`. Fallback
  for clients without thread support: `m.relates_to["m.in_reply_to"].event_id`,
  treated as the root.
- **Trust anchor:** the root event must have been sent by `self` — the identical
  check `handleReaction` already enforces, and for the identical reason. Without
  it any room member could post a message carrying an `io.runlore.thread` field
  of their choosing and file notes against an arbitrary incident.
- Ignore our own messages (`sender == self`) to prevent a reply loop.
- First `/sync` is a position handshake; history is skipped, exactly as today.

**Honest disclosure:** widening the filter means RunLore's sync loop receives
message events in the configured room, where today it receives only reactions. It
*acts* only on events that mention it and are rooted in its own message, and
non-matching events are dropped immediately without their bodies being logged —
but they do transit the process. The filter is widened only when
`thread_capture` is on. This belongs in the docs and in `SECURITY.md`'s data-flow
section, not just in a code comment.

### Reply

`m.room.message` with `m.relates_to: {rel_type: "m.thread", event_id: <root>}`.

## One-way notifiers — stated non-goal

`webhook` and `templated` are fire-and-forget HTTP POSTs with no inbound channel
and no message identity to thread against. They carry no interaction and none is
faked for them.

The extension point is an optional capability interface alongside the existing
`providers.ProgressNotifier`:

```go
// ThreadNotifier is implemented by notifiers that can carry a conversation back.
type ThreadNotifier interface {
    providers.Notifier
    ReplyInThread(ctx context.Context, root, channel, text string) error
}
```

A notifier that does not implement it simply has no thread interaction — the same
"an unset source disables its tool" contract the rest of the system uses. Both
built-in one-way notifiers are unaffected and untouched.

## Chat layer (optional)

Enabled by the **presence of `model.chat`** — no separate boolean. This mirrors
`BuildVerifyModel` (`app/model.go:71`), which returns `nil` when `model.verify` is
unset, and the project-wide contract that an unset thing disables its feature.
`model.chat` is a `config.ModelOverride` with the same inherit-when-empty `or()`
semantics, so `{model: claude-haiku-4-5}` inherits provider, base URL and key from
`model.*`.

One call per mention, no agent loop:

- **Context assembled deterministically before the call:** the `thread.Context`,
  the investigation's summary, root causes, ruled-out hypotheses, open questions
  and data gaps, plus the top 3 hits from a BM25 `kb_search` run on the human's
  text. Pre-fetching the search rather than exposing it as a tool is what keeps
  "cheap model" actually cheap — a tool loop is an unbounded number of calls.
- **Forced structured output:** `ToolChoice: "submit_thread_reply"`
  (`providers.go:811`) returning `{reply string, kb_note string}`. `kb_note` empty
  ⇒ the message was a question, nothing is filed. Non-empty ⇒ filed through the
  same routing as `note:`.
- Failure — model error, timeout, refusal, budget — degrades to the deterministic
  path: the note is filed verbatim and the reply says the answer is unavailable.
  A model outage must never lose the human's knowledge.

Because the chat layer lives in the shared responder, it lands once and both
transports get it.

## Security

- **Origin.** Slack: HMAC signature, unchanged from `verifySlack`. Matrix: the
  authenticated outbound `/sync`, plus the sender/self checks above.
- **Authorisation: deliberately unprivileged**, no approver allowlist — the same
  rationale as feedback (`server.go:353`). A note is an opinion that a human
  merges; it is not a cluster mutation. Anyone who can post in the room can
  already tell the on-call the same thing out loud.
- **The route is never model-chosen.** The model produces prose; the target is
  derived from `thread.Context`.
- **Prompt injection is in scope but bounded.** Human text reaches the model, and
  model output reaches a PR body / entry file — but nothing merges without human
  review, which is the same gate every curated entry passes. The GitHub forge's
  existing image-markdown neutralisation applies to note bodies on the `OpenPR`
  path; the `CommentOnPR` path gets the same treatment.
- **Loop guards** on both transports (bot_id / self-sender).
- **Cost guards** — the rate limits above, and no tool loop in the chat call.

## Config

```yaml
notify:
  slack:
    thread_capture: false     # requires signing_secret_env + bot_token_env
  matrix:
    thread_capture: false     # requires feedback-listener credentials (access_token_env)

model:
  chat:                       # ModelOverride; PRESENT ⇒ conversational replies,
    model: claude-haiku-4-5   # ABSENT ⇒ note-capture only. Inherits from model.*
```

Both capture flags default `false`, matching `feedback_buttons` /
`feedback_reactions`: the Slack side needs an Events subscription configured in
the Slack app anyway, so an on-by-default flag would be a lie.

`Validate` additions, following the `feedback_buttons` precedent
(`config.go:1446`):

- `notify.slack.thread_capture` requires `signing_secret_env` **and** a resolved
  bot-token delivery target — a webhook-only Slack notifier has no `ts` to thread
  against and no way to reply.
- `notify.matrix.thread_capture` requires `homeserver`, `room_id` and
  `access_token_env`.
- `model.chat` inherits the existing `validateEffort` / `validateThinking` /
  `checkSecureKeyEndpoint` treatment already applied to `model.verify`
  (`config.go:1272,1346`).

## Error handling

Every layer is best-effort relative to its host path; none may block delivery or
the investigation loop.

| Failure | Behaviour |
|---|---|
| Registry miss on Slack (unknown thread, TTL expiry, thread predating the upgrade) | Reply naming the limitation. Never a silent drop. |
| Registry miss on Matrix | Lookup is unaffected (context is on the event). Only the `NoteURL` write-back is lost, so a second note may open a second PR. |
| `CommentOnPR` / `OpenPR` fails | Reply reports the failure with the error; the human still has their text on screen and can retry. Logged at warn. |
| Model call fails | Degrade to verbatim capture; reply notes the answer is unavailable. |
| Reply post fails | Logged at warn. The KB write already succeeded and is not rolled back. |
| Rate limit hit | Reply stating the cap; no write, no model call. |
| `ThreadSink` registration fails at delivery time | Logged at warn; **delivery is unaffected** — the same contract as the detail-thread post. |

## Testing

- **Responder (transport-agnostic, the bulk of the value):** table-driven over the
  grammar; the full routing matrix against a fake `CurationForge`; the
  `NoteURL` write-back preventing a second `OpenPR`; provenance-header rendering;
  rate-limit exhaustion.
- **`Concept` entries pass the gate:** every entry the routing can emit runs
  through `kbvalidate.ValidateStructural` in the test, so a drafted note can never
  produce a PR that fails the KB repo's own merge gate.
- **Slack handler:** signature rejection; `url_verification` echo; `event_id`
  retry dedup files once; bot-loop guard; non-thread mention; 404 when the
  feature is off; the async ack returns within the deadline.
- **Registry:** TTL expiry, LRU bound, JSONL replay across a simulated restart,
  concurrent access.
- **Matrix:** `io.runlore.trigger_key` still readable by the existing reaction
  listener after `io.runlore.thread` is added (backward-compat pin); root-event
  self-check rejects a forged `io.runlore.thread` from a room member;
  `m.thread` and `m.in_reply_to` both resolve; the widened filter is only sent
  when the feature is on.
- **Chat layer:** forced-tool-call contract; empty `kb_note` files nothing; model
  failure degrades to verbatim capture; the pre-fetched `kb_search` context is
  bounded.
- **Config:** each `Validate` rule above, both directions.

## Sequencing

Three PRs. PR2 and PR3 are independent of each other and belong in parallel
worktrees once PR1 lands.

| PR | Contents | Notes |
|---|---|---|
| 1 | `internal/thread` core (Context, Responder, grammar, routing, rate limits) + Slack transport + registry | **No model anywhere in it.** Complete and useful alone — worth dogfooding before PR3 is written. |
| 2 | Matrix transport | Widens the stamped field and the `/sync` filter; reuses the responder untouched. |
| 3 | Chat layer + `model.chat` | Lands in the shared responder, so both transports gain it at once. |

Docs land with their PR: `notify.slack.thread_capture` / the Slack app scope in
the Slack setup page, the Matrix filter-widening disclosure in `SECURITY.md`, and
the thread grammar in the user-facing docs on the first PR.
