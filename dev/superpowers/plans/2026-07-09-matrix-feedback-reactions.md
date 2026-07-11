# Matrix Feedback via Reactions Implementation Plan

**Goal:** Give Matrix users the same one-click 👍/👎 feedback channel Slack got (#267) — reactions on
RunLore's investigation messages are recorded in the outcome ledger and weigh recalled-entry trust
(and re-arm the recurrence cooldown) through the exact same notifier-agnostic mechanics.

**The headline property — no inbound exposure.** Slack feedback requires exposing
`POST /slack/interactions` to Slack. Matrix needs nothing exposed: reactions arrive over the
client-server **`/sync` long-poll** — an *outbound* HTTPS request authenticated by the access token
the notifier already holds. No Request URL, no signing secret, no NetworkPolicy change.

**Mechanism:**
1. **Embed** — `Matrix.Deliver` adds a custom content field `io.runlore.trigger_key`
   (`cmp.Or(TriggerKey, Fingerprint)`) to every investigation message. Custom keys are legal in
   Matrix events and invisible in clients; embedding is unconditional (harmless), the *listener* is
   the opt-in.
2. **Listen** — a leader-only goroutine (`MatrixFeedback.Run`, started in `startWork` like the
   reinvestigate poller) long-polls `/sync` filtered to the configured room + `m.reaction` timeline
   events. The first response is a position handshake only (historical reactions are skipped —
   feedback counts from startup onward); errors back off 5s and retry; leadership loss cancels.
3. **Attribute** — a reaction (`m.annotation`) names its target event id; the listener fetches that
   event (`/rooms/{room}/event/{id}`, small bounded cache) and reads `io.runlore.trigger_key` back.
   No key ⇒ not one of ours ⇒ ignored. Stateless across restarts by construction.
4. **Record** — 👍→`up`, 👎→`down` (variation selector U+FE0F stripped; every other emoji ignored),
   `sink.Feedback(key, rating, sender, now)` with the Matrix user id (`@alice:hs`) as the dedup
   identity. Ledger-side per-user latest-wins dedup, posterior weighting, and the 👎-breaks-cooldown
   escape hatch (#270) all apply unchanged — zero ledger changes in this PR.

**Opt-in contract:** `notify.matrix.feedback_reactions` (default off). Validate fails loud when on
without `outcome.ledger_path` or without the Matrix notifier fields (homeserver/room_id/token env).
Off ⇒ bit-for-bit today's behavior (the embedded content key is additive and inert).

**Tasks:**
1. `internal/config` — `MatrixNotify.FeedbackReactions` + fail-loud Validate + tests.
2. `internal/notify/matrix.go` — embed `io.runlore.trigger_key` in Deliver (+ tests).
3. `internal/notify/matrix_feedback.go` (new) — `FeedbackSink` interface (satisfied by
   `*outcome.Ledger`), `MatrixFeedback` listener: filtered long-poll, handshake-then-listen, target
   fetch + key readback, emoji mapping, backoff. httptest-scripted tests: handshake skips history,
   👍/👎 recorded with sender, foreign emoji + keyless targets ignored, ctx cancel stops Run.
4. `internal/app` — `BuildMatrixFeedback` (nil unless opted in + ledger enabled + token present),
   started leader-only in `startWork`.
5. Docs: configuration.md (option + "no exposure" contrast with Slack), learning-loop.md (second
   feedback channel), getting-started.md + chart values comments.

**Gate:** `go build && go vet && go test -race ./... && gofmt -l . && golangci-lint run` — 0 issues.
