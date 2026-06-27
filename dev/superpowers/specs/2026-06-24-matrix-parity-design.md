# Matrix parity — HTML formatting + txn durability (R16)

Date: 2026-06-24
Status: accepted
Scope: `internal/notify/matrix.go` (+ tests)

## Problem

The Matrix notifier (`internal/notify/matrix.go`) lags the Slack notifier in
three respects flagged as item R16:

1. **Markdown sent as plaintext.** `Deliver` sends `Format(inv)` — which is
   Slack-mrkdwn-flavoured (`*bold*`, `•`, `→`, plain URLs) — as the Matrix
   `body` only (matrix.go:45). Matrix `body` is rendered verbatim, so users see
   raw `*asterisks*` instead of bold. Slack renders the same string because its
   `text`/`mrkdwn` fields parse `*…*`; Matrix does not.
2. **Txn counter resets on restart.** `m.txn` is an `atomic.Int64` starting at 0
   (matrix.go:23,41). The transaction id is `runlore-<n>`. After a process
   restart the counter restarts at 1, so `runlore-1`, `runlore-2`, … repeat.
   Matrix homeservers dedupe by `(access_token, txnId)` within a window, so a
   post-restart message can collide with a pre-restart one and be silently
   dropped.
3. **No interactive approval.** Slack renders Approve/Reject Block Kit buttons
   for actions carrying an `ApprovalID` and processes clicks via a signed
   `POST /slack/interactions` handler (server.go:236) gated by an approver
   allowlist. Matrix has no native buttons and no inbound interaction path.

## CHALLENGE — findings vs current code

### Finding 1 (HTML formatting) — VALID. file:line `internal/notify/matrix.go:45`
`json.Marshal(map[string]string{"msgtype": "m.notice", "body": Format(inv)})`
sends only `body`. The Matrix spec renders `body` literally; rich text requires
`format: "org.matrix.custom.html"` + `formatted_body`. So Matrix users do see
raw markup. **Verdict: fix.** Send `formatted_body` (HTML) alongside the plain
`body`. `body` stays the plaintext fallback for clients that ignore the format.

### Finding 2 (txn durability) — VALID but narrower than "persist". file:line `internal/notify/matrix.go:23,41`
The counter genuinely restarts at 0, and the spec's claimed homeserver dedup is
real. **Verdict: fix — but the cheapest correct fix is to *seed* the counter,
not persist it.** Persisting an int to disk introduces state, a path config, and
failure modes for a value whose only requirement is "don't repeat across a
restart." Seeding `m.txn` from `time.Now().UnixNano()` at construction makes ids
strictly increasing across restarts (wall clock advances) without any new state.
`time.Now()` is used freely in this codebase (cmd/lore/main.go:577,656,1209) and
there is no clock-injection policy, so the spec's "if that's forbidden" clause
does not apply. We keep `atomic.Int64.Add` for in-process monotonicity; the seed
only sets the floor. Documented constraint: a backwards wall-clock jump across a
restart could still collide; acceptable given the homeserver dedup window is
minutes and the prior fix was *zero* protection.

### Finding 3 (Matrix approval) — VALID gap, **scoped OUT** with documented follow-up.
Slack approval is a non-trivial machine: an inbound signed webhook
(`verifySlack`, HMAC over `v0:ts:body`), an approver allowlist
(`s.approvers[userID]`), `approvals.Approve/Reject`, and a `response_url`
message edit (server.go:236-335). Matrix has **none** of these primitives:
no buttons, no signed inbound webhook, no `/matrix/interactions` route. A Matrix
approval path would require (a) a long-running `/sync` client to read replies,
(b) a reply/command convention parser (`!approve <id>`), (c) a Matrix-user →
approver mapping and authorization, and (d) wiring a new long-lived component
into the server. That is its own feature slice, not a formatting fix, and the
prompt explicitly sanctions deferring it. **Verdict: out of scope here.** This
slice does HTML + txn only. Follow-up tracked below.

## Decision

Implement, in `internal/notify/matrix.go`:

1. A minimal markdown→HTML converter (`mrkdwnToHTML`) covering exactly the
   constructs the message stream uses plus `code`: `*bold*` → `<strong>`,
   `` `code` `` → `<code>`, bare `http(s)://` URLs → `<a href>`, `\n` → `<br/>`,
   with HTML-escaping applied first so user content can't inject markup.
2. `Deliver` sends `{msgtype, body, format, formatted_body}` where `body` is the
   plaintext fallback (`Format(inv)` with the `*` stripped so the fallback is
   clean) and `formatted_body` is the HTML.
3. Seed `m.txn` from `time.Now().UnixNano()` in `NewMatrix`.

### Out of scope (follow-up)
- **Matrix interactive approval** — needs a `/sync` reader, a `!approve <id>`
  command convention, Matrix-user authorization, and server wiring. File a
  follow-up; Matrix operators approve via the existing
  `POST /actions/{id}/approve` HTTP endpoint (already surfaced in the action
  description) in the meantime.

## Tests (test-first, stdlib + httptest, table-driven, no testify)
- Sent event carries `format == "org.matrix.custom.html"`, a non-empty
  `formatted_body` containing `<strong>`/`<a href`, and a plaintext `body` with
  no raw `*`.
- HTML is escaped: a root cause containing `<script>` appears escaped, not raw.
- `mrkdwnToHTML` table cases: bold, code, link, newline, escaping, plain text.
- Two notifiers constructed "across a restart" produce non-colliding txn ids
  (the second's first id > the first's last id), proving the seed advances.
