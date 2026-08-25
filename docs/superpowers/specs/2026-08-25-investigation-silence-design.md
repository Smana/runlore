# Design — Investigation silence (the third button)

- **Date:** 2026-08-25
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Extends:** `RecurrenceGate` (`internal/investigate/recurrence.go`) and the 👍/👎 feedback loop
- **Follow-up specs (not this one):** inconclusive-retry backoff · `recurrence_cooldown` default

## Problem

RunLore has four layers that decide not to investigate: `Deduper` (still-firing window),
`Debouncer` (60s pre-investigation hold), the coalescer, and `RecurrenceGate` (per-trigger
cooldown). Every one of them is a **machine inference from history**. None can represent the
single most common reason a human wants quiet:

> *"Right diagnosis. I know. There's a vendor ticket open until Thursday. Stop."*

That knowledge is out-of-band — it exists only in the on-call's head — and today it has no way
into the system at all. The feedback buttons come close but say the wrong things: a 👍 makes the
cooldown **more** willing to suppress, and a 👎 explicitly **re-arms** it (`TriggerRecurrence.Contested`).
There is no verdict for *accurate, and stop telling me*.

The cost dimension is real but secondary, and the spec is deliberate about not overclaiming it:

- `recurrence_cooldown` still defaults to `0` (off) — `deploy/helm/runlore/values.yaml:197` has it
  commented out. For a default install the cheapest cost win is turning the **existing** gate on,
  not adding a button.
- The largest sink is `recurrenceNoAnswer` — a trigger that keeps returning `inconclusive`
  re-runs the full paid loop on **every** firing, forever, by design (`recurrence.go`). A button
  only helps when a human happens to be looking. An automatic backoff fixes more money, more
  safely. **That is a separate spec**, agreed during brainstorming.

So this feature is justified primarily as a **noise/signal** feature: the only channel through
which human out-of-band knowledge can reach the suppression layer. The cost saving is a welcome
side effect, not the argument.

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| What is suppressed | **The investigation** — no model call, no notification, no ledger open | Same semantics as `recurrenceSuppressed`; anything less leaves the paid loop running |
| Framing | A **third feedback verdict**, not a new subsystem | Reuses the ledger event kind, `TriggerKey` attribution, and the gate. Suppression is a *consequence* of the verdict |
| Key | `TriggerKey` | Already the button's `value`, already the gate's key, already the ledger's index |
| Duration | Overflow menu of **configurable presets** | One visual element; works identically on the webhook and bot Slack paths |
| Dedup | **Latest wins per trigger** (not per user) | Unlike `votes`, which dedup per `(trigger, user)` because votes *aggregate*. A silence is one standing decision about the trigger |
| Re-arm: CRITICAL | A silence **never** suppresses a CRITICAL firing | Matches an established invariant — `debounce.go:89` already refuses to hold criticals |
| Re-arm: 👎 | `!prior.Contested()` | The **same** shared definition #288 established for every layer that must yield to a human |
| Re-arm: resolve | Clears the silence at fold time | The incident went away; a later firing is arguably new |
| Authorization | **Unprivileged**, like feedback | Blast radius is bounded four independent ways — see [Security](#security) |
| Matrix parity | 🔕 reaction (default window) **+** `silence:` thread command (full choice) | Reuses the reaction listener and the reserved-prefix grammar |
| Ack message | Carries an explicit **warning** | A silence is the one feedback click that changes behaviour; the reader must know what they just switched off |

## Non-goals

Stated so the spec cannot quietly grow:

- **Broader mute scopes** — per-workload, per-alert-class, per-namespace. `TriggerKey` only.
- **`lore silences` CLI.** The ack message, the log line and the metric are v1 visibility. Revisit
  once there is evidence operators lose track of live silences.
- **The inconclusive-retry backoff**, and whether `recurrence_cooldown` should default on. Both
  filed separately.
- **Modal with free-text reason capture.** A reason is genuinely valuable — "known, waiting on
  vendor" is exactly what the curator should learn — but `views.open` needs a bot token, so it
  would work on the bot path only and silently degrade for incoming-webhook deployments. Revisit
  as its own feature if the plain silence proves useful.

## Architecture

```
Slack card                          Matrix room
[👍][👎][🔕 ⋮]                       🔕 reaction  /  "@runlore silence: 4h"
     │                                   │
     ▼                                   ▼
POST /slack/interactions            MatrixFeedback (/sync, leader-only)
  (work-wrapped → forwarded              │
   single-hop to the LEADER)             │
     └───────────────┬───────────────────┘
                     ▼
          outcome.Ledger.Silence(triggerKey, window, user, at)
                     │  durable append + in-memory fold
                     ▼
             silences[triggerKey] = until
                     │
                     ▼
   TriggerRecurrence.SilencedUntil ──▶ RecurrenceGate.decide() ──▶ recurrenceSilenced
                                                                    (skip the paid loop)
```

The click lands in the **leader's** fold with no new sync mechanism, because
`POST /slack/interactions` is already `work`-wrapped (`internal/server/server.go:224`): a follower
proxies it single-hop with headers relayed byte-identically, so the HMAC verifies on the leader and
the fold happens in the same process the gate reads from. This is the single biggest reason to reuse
the ledger rather than build a silence store.

## 1. The ledger — a new event kind

A fifth kind alongside `open` / `resolve` / `feedback` / `confirm`:

```go
Event{Event: "silence", TriggerKey: key, Kind: "4h", User: userID, At: now}
```

`Kind` carries the window as a duration string, mirroring how a `feedback` line carries its rating
there. Older binaries whose fold switch has no `"silence"` case ignore the line — the same graceful
forward-compatibility `checkpoint` relies on.

New fold state on `Ledger`:

```go
// silences holds the expiry of the standing human silence per TriggerKey.
// LATEST WINS: unlike votes, which dedup per (trigger, user) because ratings
// aggregate, a silence is one standing decision ABOUT THE TRIGGER — the most
// recent human wins outright, including shortening or lifting a colleague's.
silences map[string]time.Time
```

Rebuilt on load and **checkpointed on compaction** alongside `votes`/`byTrigger`. The checkpoint is
not optional bookkeeping: without it a compaction would resurrect an expired silence, or drop a live
one, and both failures are invisible until someone asks why RunLore is quiet.

Surfaced as one field on the snapshot the gate already reads:

```go
type TriggerRecurrence struct {
    …
    SilencedUntil time.Time // zero = no standing silence
}
```

Public API mirrors `Feedback` (`ledger.go:1194`) — validate, durable-append, then fold:

```go
func (l *Ledger) Silence(triggerKey string, window time.Duration, user string, at time.Time) error
```

Rejects a non-positive window and a window above the cap held in a new exported field,
`Ledger.MaxSilenceWindow` (zero = uncapped), set by the serve wiring right after construction —
the same set-after-New pattern `cz.Outcome = ledger` already uses (`app/serve.go:281`). Keeping
the cap in the ledger rather than in each caller makes it the one place the invariant is enforced:
a Matrix `silence:` command is free text and must not be able to exceed what the Slack presets
offer.

### Expiry is read, never swept

`silences` entries are **not** garbage-collected on a timer. `decide` compares against `now`, and a
lapsed entry is inert. They are pruned opportunistically on load/compaction, exactly as
`pendingResolves` is bounded rather than swept. A background sweeper would be a goroutine, a test
seam and a shutdown concern in exchange for a handful of map entries.

## 2. Enforcement — one new branch in `decide()`

```go
const recurrenceSilenced recurrenceDecision = "silenced_by_human"

func (g *RecurrenceGate) decide(req Request, prior outcome.TriggerRecurrence, now time.Time) recurrenceDecision {
	if g == nil || req.TriggerKey == "" {
		return recurrenceOff
	}
	// Checked BEFORE the cooldown short-circuit, deliberately, for two reasons:
	// recurrence_cooldown defaults to 0 (off) and a silence must work in a default
	// install; and a silence stands regardless of prior history — INCLUDING the
	// no_conclusive_prior case the cooldown itself refuses to suppress, which is
	// exactly the case a human is most likely to want silenced.
	if now.Before(prior.SilencedUntil) && !req.IsCritical() && !prior.Contested() {
		return recurrenceSilenced
	}
	if g.Cooldown <= 0 {
		return recurrenceOff
	}
	// … existing cooldown ladder, unchanged
}
```

Three details that are easy to get wrong:

1. **`req.IsCritical()`, never `strings.EqualFold(req.Severity, …)`.** `investigate.go:50` states the
   invariant outright: `SeverityCritical` is "the ONE spelling behind `Request.IsCritical`; nothing
   else should compare Severity to a literal."
2. **The `g.Cooldown <= 0` early return moves BELOW the silence check.** Left where it is, the whole
   feature is inert on a default install — the single line most likely to ship as a silent no-op.
3. **Because the CRITICAL rule is absolute, no severity is recorded at silence time.** An earlier
   draft stored severity-at-silence to detect *relative* escalation; there is no severity ladder in
   this codebase (only `EqualFold("critical")` checks), so the absolute rule is both simpler and
   consistent with the debouncer.

A silenced firing behaves exactly like `recurrenceSuppressed`: no model call, no notification, and
**no ledger open**. That last part is load-bearing for the reason `RecurrenceGate`'s own doc gives —
an open would slide `byTrigger.last`.

### Re-arm: resolve

`applyResolveLocked` (`ledger.go:690`) already has the fingerprint. `Ledger.open` is
`map[string]Event` — fingerprint → latest unresolved open (`ledger.go:151`) — and `Event` carries
`TriggerKey`. So:

```go
if tk := l.open[fp].TriggerKey; tk != "" {
    delete(l.silences, tk)
}
```

No new index, no new state. The lookup happens before the existing pairing logic, since whether the
resolve pairs, buffers or is discarded says nothing about whether the incident ended.

## 3. Wiring — the nil-gate trap

`internal/app/investigate.go:628` builds the gate **only** when a cooldown is configured:

```go
var recurrence *investigate.RecurrenceGate
if d := cfg.Investigation.RecurrenceCooldown.Std(); d > 0 && ledger.Enabled() {
	recurrence = &investigate.RecurrenceGate{Cooldown: d}
}
```

With silence enabled and no cooldown, `recurrence` is nil, `decide` returns `recurrenceOff` on its
`g == nil` guard, and every click is durably recorded and then ignored — the worst possible failure
mode, because the UI confirms success. The condition must become:

```go
silenceOn := cfg.Notify.Silence.Enabled()   // any transport
if (d > 0 || silenceOn) && ledger.Enabled() {
	recurrence = &investigate.RecurrenceGate{Cooldown: d}   // Cooldown may legitimately be 0
}
```

The gate's doc comment broadens accordingly: it is no longer purely a cooldown gate but the one
place that decides a trigger is not worth re-investigating right now, for either a machine reason
or a human one.

## 4. Slack

`feedbackBlocks` (`slack.go:361`) gains a third element behind its **own** opt-in flag,
`notify.slack.silence_button` — separate from `feedback_buttons` because this one *changes
behaviour* rather than recording an opinion, and an operator must be able to take the ratings
without the suppression.

### The encoding constraint

Slack caps an overflow **`option.value` at 75 characters** (a `button.value` gets 2000). A GitOps
`TriggerKey` is `Workload.Ref() + ":" + Reason` = `namespace/name:Reason`
(`investigate.go:113`, `providers.go:124`). `kube-system/nginx-ingress-controller-abc123:ProgressDeadlineExceeded`
is already 68 characters and Kubernetes names run to 253. Encoding `"4h|<triggerKey>"` in the option
value would make Slack reject the **entire message** — killing the notification, not just the button.

**Resolution: the window rides in `option.value`; the `TriggerKey` rides in the actions block's
`block_id`** (255 characters, echoed back on every `payload.actions[]` entry).

```go
{"type": "actions", "block_id": "sil:" + inv.TriggerKey, "elements": [
    {…👍…}, {…👎…},
    {"type": "overflow", "action_id": silenceActionID, "options": [
        {"text": {"type": "plain_text", "text": "🔕 Silence 1h"}, "value": "1h"},
        …
    ]},
]}
```

Two alternatives were rejected: **three separate buttons** (`[🔕 1h][🔕 4h][🔕 24h]`) clutters the
row to five elements and was not the chosen UX; **a short hash plus a reverse index on the ledger**
is more principled but costs a new map, its checkpoint, and an illegible `TriggerKey` in the JSONL,
the log line and the ack message — a poor trade for a focused PR.

Guard, following the precedent `feedbackBlocks` already sets for an empty key ("there is nothing to
attribute, so no buttons render"): if `len("sil:"+triggerKey) > 255`, render 👍/👎 **without** the
silence element. A pathological resource name degrades one control, never the card.

## 5. The ack message

Posted with `replace=false` — replacing would wipe the investigation the silence is about, the same
reason the feedback ack does not replace (`server.go:499`).

```
🔕 Silenced by @smaine until 18:42 (4h).

⚠️ RunLore will NOT investigate this incident while the silence stands — no
   model call, no notification, no record. A CRITICAL firing still breaks
   through; a 👎 or a resolved alert re-arms it immediately.
```

The expiry renders through the existing `slackDate` helper so each reader sees their own local time.
`slackDate` emits a raw `<!date^…>` token that is blocks-only and must never enter escaped fallback
text — the existing constraint applies unchanged.

**The same warning text, word for word, is used on the Matrix reply.** A silence must not mean two
different things depending on which room you are standing in.

## 6. Matrix

Both paths go through `MatrixFeedback`'s existing self-authorship check — a vote only counts when
the reacted-to event was sent by the bot itself. That trust anchor is not optional here: silencing
on a forged `io.runlore.trigger_key` would be a denial-of-investigation primitive, which is
strictly worse than the vote-misdirection the check was built for.

- **🔕 reaction** → the **default** window (reactions carry no duration; the default is presets[0]).
- **`silence: 4h`** → a new `IntentSilence` in `internal/thread/grammar.go`, which already parses
  colon-anchored reserved prefixes as whole tokens (`note:`, `reinvestigate:`). It inherits the
  anywhere-match and prefix-collision behaviour already specified and tested there.
  A missing or unparseable duration falls back to the default window and says so in the reply.

### Sink wiring

The transports reach the ledger through **new, separate one-method interfaces**, not by widening
the existing feedback ones:

```go
// internal/server (beside FeedbackRecorder, server.go:107)
type SilenceRecorder interface {
	Silence(triggerKey string, window time.Duration, user string, at time.Time) error
}

// internal/notify (beside FeedbackSink, matrix_feedback.go:34)
type SilenceSink interface {
	Silence(triggerKey string, window time.Duration, user string, at time.Time) error
}
```

A third, identical one lives in `internal/thread` for the `silence:` command path (`thread.SilenceRecorder`),
because `internal/thread` cannot import `internal/notify`. `*outcome.Ledger` satisfies all three,
exactly as it satisfies `FeedbackRecorder`/`FeedbackSink` today — each package declaring the
narrow interface it consumes is the idiom this codebase already follows for feedback.
Keeping them separate preserves the existing nil-means-off pattern (`server.go:151`) at the right
granularity: a deployment with `feedback_buttons` on and `silence_button` off must have
`s.silence == nil` while `s.feedback` is live, and a widened interface could not express that.

## 7. Configuration

```yaml
notify:
  silence:
    windows: [1h, 4h, 24h]   # presets offered; [0] is the reaction-path default
    max_window: 24h          # hard cap, enforced in the ledger
  slack:
    silence_button: true
  matrix:
    silence_reactions: true
```

Validation mirrors the existing `feedback_buttons` rules (`config.go:2070`) — fail loud at startup
rather than render a control whose clicks vanish:

- `notify.slack.silence_button` requires `notify.slack.signing_secret_env` (clicks arrive on the
  exposed endpoint and must be signature-verified).
- Any silence transport requires `outcome.ledger_path` — without it the gate would silently never
  suppress, the same failure `recurrence_cooldown` already fails loud on (`config.go:2064`).
- `windows` must be non-empty, every entry `> 0` and `<= max_window`.

## 8. Observability

- **Metric:** `investigations_completed_total{result="silenced"}`, spelled as a **literal** at the
  call site in `loop.go` — per the standing warning in `recurrenceDecision`'s doc comment that the
  internal decision name must never become the metric label, because a rename for clarity here
  would silently break a dashboard there.
- **Log (INFO, on suppression):** `trigger_key`, `user`, `until`, `occurrences`. INFO rather than
  DEBUG for the same reason `recurrenceNoAnswer` is INFO: a human deliberately switched something
  off, and the operator asking "why was RunLore quiet?" must find it without raising log levels.
- **Log (INFO, on the click):** who silenced what, for how long, on which transport.

## Security

Silencing is unprivileged — the Slack signature proves the workspace, which is the bar feedback
already meets. The justification is that the blast radius is bounded four **independent** ways, and
an attacker would have to defeat all of them:

1. It **expires** on its own, capped by `max_window`.
2. It **never** suppresses a CRITICAL firing.
3. Any colleague's 👎 **immediately** undoes it (and is one click).
4. Every silence is attributed to a user id in the durable ledger and in the logs.

An approver allowlist was considered and rejected: it buys little against that, and adds friction
exactly when the on-call is busiest. Should the posture need tightening later, a
`notify.silence.require_approver` toggle reusing `s.approvers` is a contained follow-up.

The Matrix self-authorship check (§6) is the one control that is **not** optional.

## Testing

TDD throughout — failing test first, table-driven where the shape allows.

| Area | Test |
|---|---|
| `decide()` | Matrix: silenced · expired · CRITICAL · contested · cooldown-off-but-silenced · no trigger key · nil gate |
| Nil-gate trap (§3) | Silence configured, cooldown `0` ⇒ a firing IS suppressed. Pins the bug the wiring change exists to prevent |
| Ledger fold | `Silence` → `Recurrence().SilencedUntil`; latest-wins on a second click; window cap rejected |
| Compaction | Round-trip **through a checkpoint**: a live silence survives, an expired one does not resurrect |
| Resolve re-arm | open → silence → resolve ⇒ the next firing is investigated |
| Slack blocks | Golden card test (`card_golden_test.go`) for the new element; `block_id` carries the key; over-255 key degrades to 👍/👎 only |
| Encoding | A realistic long GitOps `TriggerKey` produces a payload Slack would accept (option values ≤ 75) |
| Matrix | 🔕 reaction on a **non-self** event records nothing; `silence: 4h` parses; bad duration falls back |
| Config | Each validation rule fails loud with its own message |
| Integration | Silence, then a CRITICAL firing ⇒ investigated in full |

## Documentation

- `website/content/docs/integrations/notifications/slack.md` — the third button, the warning, the
  four re-arms.
- The troubleshooting table's "why was RunLore quiet?" section gains a row. It will then have five
  entries, which is itself the argument against adding a sixth suppression layer later.
- `website/content/docs/concepts/learning-loop.md` — silence as the third feedback verdict.
- `deploy/helm/runlore/values.yaml` (commented, opt-in) and `values-full.yaml`.

## Open questions deferred

- **Should a silence feed the curator?** "This trigger is known-noisy" is a genuine signal about an
  entry, independent of suppression. Deliberately not wired in v1: the weighting would be a guess,
  and a wrong weight silently distorts recall trust. Revisit with data from real silences.
- **Reason capture.** See Non-goals. The strongest argument for the modal, and worth revisiting
  once there is evidence people want to explain their silences.
