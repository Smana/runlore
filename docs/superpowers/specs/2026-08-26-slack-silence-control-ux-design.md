# Design — Slack silence control: a label, a visible ack, and a card that remembers

- **Date:** 2026-08-26
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Fixes:** the three defects observed on the first live silence, 2026-08-26
- **Touches:** `internal/notify/slack.go`, `internal/server/server.go`, `internal/config/config.go`

## Problem

The 🔕 silence control shipped and works — a click records the silence and the
suppression gate honours it. But the first live use exposed three UX defects, all
visible in one screenshot:

1. **The control is unlabelled.** Slack's `overflow` element takes no text, so it
   renders as a bare `···` beside 👍 Accurate / 👎 Off-base. The 🔕 appears only
   *inside* the menu, after you have already clicked something you could not
   identify. A control nobody recognises is a control nobody uses.
2. **The confirmation is private.** `slackResponseBody` sets
   `response_type: ephemeral` whenever `replace_original` is false, and silence
   goes down the feedback path. So "🔕 Silenced by @X" is visible only to the
   clicker. Nobody else learns the alert was handled, which invites a second
   person to investigate something already dealt with.
3. **The card keeps no memory.** The original message is never touched, so a
   silenced card is byte-identical to an un-silenced one. Scrolling `#sre-agent`
   tells you nothing about which findings were acknowledged, by whom, or until
   when.

Defects 2 and 3 are the same defect seen from two angles: nothing durable and
public records that a human acted.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Control element | **`static_select` with a `placeholder`** | The only actions-block element that carries both a visible label and a menu. `overflow` cannot be labelled; buttons-per-window scale badly (up to 7 controls at 5 windows); a button-plus-modal needs `views.open`, a `trigger_id`, a new interaction type and a submission handler — a new failure surface for a cosmetic win |
| Acknowledgement | **Rewrite the card in place** | Fixes 2 and 3 in one stroke. An in-place edit is public by construction and survives scrollback, which a separate message does not |
| Update transport | **existing `response_url`**, not `chat.update` | Already wired and already SSRF-guarded (https, `*.slack.com` only, bounded client). Needs no new Slack scope. The interaction payload carries `message.blocks`, so the card can be rebuilt faithfully |
| Failure mode | **Never blank the card** | `replace_original: true` today means "overwrite with plain text". Used naively it destroys the investigation. The rewrite is conditional on usable blocks; otherwise fall back to today's ephemeral note |
| Window-count bound | **Keep 2–5, rewrite the message** | A select accepts one option, so the current error's stated reason stops being true. The bound stays as a UX floor; the justification must stop citing a mechanism no longer in use |

## Non-goals

- **Listing what is currently silenced.** A `/runlore silences` command or digest
  is a second feature with its own command surface and tests. Out.
- **Un-silencing.** A silence lapses on its own, a 👎 re-arms immediately, and a
  CRITICAL breaks through. There is nothing a manual un-silence adds today.
- **Updating the marker when a silence is later broken.** The card is a historical
  record of a human action, not a live status display. Keeping it accurate as the
  silence lapses would mean tracking and re-editing arbitrarily old messages.
- **Matrix.** `silence_reactions` carries a duration-free 🔕 reaction; a reaction
  is already visible to the room, so defects 2 and 3 do not arise there.

## The control

Replace the `overflow` element with a `static_select` carrying a placeholder:

```
{"type": "static_select",
 "action_id": silenceActionID,
 "placeholder": {"type": "plain_text", "text": "🔕 Silence…", "emoji": true},
 "options": [ …one per configured window… ]}
```

`action_id` and the `block_id`-borne TriggerKey are unchanged.

**The interaction handler needs no change**, which was verified rather than
assumed: Slack sends the chosen option as `actions[0].selected_option.value` for
`static_select` and `overflow` alike, and `server.go` already reads exactly that
(`act.SelectedOption.Value`). A consequence worth stating because it is the kind
of thing a rollout usually has to worry about: **cards posted before this change
remain clickable after it**, since the payload shape is identical. No dual-shape
parsing, no migration window.

## The acknowledgement

On a silence interaction, rebuild the message from the payload's own blocks:

1. Drop the actions block carrying the silence control. 👍/👎 stay: a 👎 must
   still be able to re-arm the trigger, and that is the documented escape hatch.
2. Append a context block:
   `🔕 Silenced by @<user> until <slackDate> · <window>`
   The user id renders as a Slack mention; the time uses the existing `slackDate`
   token so it localises per reader.
3. POST `{"replace_original": true, "blocks": [...]}` to the response_url.

If the payload carries no blocks, or the rebuild produces nothing, fall back to
the current behaviour — an ephemeral note, card untouched. **A failed rebuild must
never blank the finding.** That is the one hard invariant here: the card is worth
more than the marker.

## Error handling

| Case | Behaviour |
|---|---|
| No blocks in payload | Ephemeral note, card untouched |
| Rebuild yields zero blocks | Ephemeral note, card untouched |
| response_url non-2xx | Logged best-effort, as today. The silence is already recorded in the ledger — the marker is presentation, not state |
| Silence recording fails | No rewrite. The card must not claim a silence that was not stored |

The ordering matters: **record first, then decorate.** A marker that outlives its
silence is worse than no marker.

## Testing

- The rendered card carries a `static_select` with a non-empty placeholder, and
  no `overflow`.
- The handler still parses a window from a `static_select` payload — a
  characterisation test pinning that the shape did not change, since the whole
  no-migration argument rests on it.
- A silence rebuild: control block gone, 👍/👎 retained, context block appended
  naming user and expiry.
- **Blocks absent → card untouched and the ack is ephemeral.** The regression
  test for the one hard invariant.
- Ledger-write failure → no rewrite attempted.
- `Validate` still rejects 1 window and 6 windows, with a message that no longer
  cites `overflow`.

## What this does not change

The suppression semantics are untouched: what a silence does, how long it lasts,
what breaks it. This is entirely about whether a human can see the control, see
that it worked, and see later that someone used it.
