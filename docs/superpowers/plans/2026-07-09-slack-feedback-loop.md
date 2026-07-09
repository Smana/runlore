# Slack 👍/👎 Feedback Loop (complete) Implementation Plan

**Goal:** Close the human-feedback loop end to end, as an **explicit opt-in**: 👍/👎 buttons on Slack
investigation messages record a human verdict in the outcome ledger, and those ratings feed the same
Beta-posterior trust weighting that recall decay already uses — so a 👎 measurably lowers the recalled
entry's confidence (and enough of them reject the recall entirely), while a 👍 builds trust.

**Why feedback matters structurally:** recall decay only learns where a ground-truth resolve signal
exists; non-resolvable sources (GitOps failures — the golden path —, reinvestigate, Alertmanager
without `send_resolved`) neither build nor erode trust today (`applyOpenLocked`). Human feedback is
the only possible ground-truth channel for those, and a direct judgment on the *diagnosis* (an alert
clearing proves nothing about whether the analysis was right).

**Opt-in contract (the user's requirement):** the feature is OFF by default. Enabling
`notify.slack.feedback_buttons: true` is what makes the buttons render AND what wires the feedback
recorder into the server. It requires exposing `POST /slack/interactions` to Slack (public Request
URL) — the same endpoint approve-mode buttons already use — and is documented as such, prominently.
Validation fails loud at startup if the option is on without `signing_secret_env` (unsigned clicks
are never accepted) or without `outcome.ledger_path` (a button whose click can't be recorded is a
lie). Disabled ⇒ bit-for-bit today's behavior: no buttons, feedback actions answer "not enabled",
and with approvals also off the endpoint stays 404.

**Architecture:** no new component, store, endpoint, dependency, or goroutine.

1. **Collect** — `Ledger.Feedback(triggerKey, rating, user, at)` appends a
   `{"event":"feedback"}` line (the replay switch already ignores unknown kinds — old binaries are
   safe, see the existing comment in `foldLocked`). The server's existing HMAC-verified
   `/slack/interactions` handler gains two cases; feedback is deliberately unprivileged (any
   signature-valid workspace member — it's an opinion, not a cluster mutation) and must NOT
   `replace_original` (that would wipe the investigation message).
2. **Attribute** — the ledger joins a feedback's TriggerKey to a catalog entry via the existing
   `byTrigger` index: `triggerAgg` gains `entry` (the newest open's `Entry`; fresh opens carry ""
   so feedback on a fresh investigation credits nothing — recorded for analytics only).
3. **Weigh** — `Aggregate` gains `FeedbackUp`/`FeedbackDown`; ratings are extra Bernoulli
   pseudo-observations in the SAME Beta posterior:
   `outcomeFactor = (resolved + up + k/2) / (recalls + up + down + k)`.
   Anti-gaming: the fold keeps ONE vote per (TriggerKey, Slack user id), latest wins — changing
   your mind moves the vote (un-credits the previous one), repeating it is idempotent.
4. **Survive restarts/compaction** — votes + attribution + counts round-trip through the
   checkpoint (`checkpointData` gains `Votes`; `triggerAggJSON` gains `Entry`; `Aggregate`'s new
   exported ints serialize for free).

**Tech Stack:** Go 1.26, stdlib only. Tests: `go test -race ./...`, table-driven + `httptest`.
Gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . &&
golangci-lint run ./...` (must be 0 issues).

## Tasks

### Task 1: Ledger — feedback events, per-user dedup, entry attribution, checkpoint round-trip
`internal/outcome/ledger.go` + `ledger_test.go`. Produces: `Event.User`,
`Aggregate.FeedbackUp/FeedbackDown`, `Ledger.Feedback(triggerKey, rating, user string, at time.Time)
error` (validates rating ∈ {up,down}; disabled ledger no-ops), `triggerAgg.entry`,
`votes map[triggerKey+"\x00"+user]feedbackVote{rating,entry}` folded on replay/live-append and
checkpointed. Tests: credit/dedup/vote-change/fresh-open-no-attribution/newest-open-wins,
replay equivalence, checkpoint survival, Episodes() untouched, invalid rating rejected.

### Task 2: Recall — feedback in the posterior
`internal/investigate/recall.go` + tests. `outcomeFactor(recalls, resolved, up, down int, k float64)`;
call site passes `agg.FeedbackUp/agg.FeedbackDown`. An entry with feedback-only history now enters
the gate (it appears in OpenCounts), so downs alone can reject a recall — intended.

### Task 3: Config — the opt-in + fail-loud validation
`internal/config/config.go` + tests. `SlackNotify.FeedbackButtons bool yaml:"feedback_buttons"`.
Validate: on ⇒ requires `signing_secret_env` AND `outcome.ledger_path`, with error messages that
name the exposure requirement.

### Task 4: Slack buttons (render only when opted in)
`internal/notify/slack.go` + tests. `Slack`/`SlackBot` gain a `FeedbackButtons` field set by the
builder from config. `summaryBlocks(inv, withFeedback bool)`; feedback actions block appended last,
`value` = `cmp.Or(inv.TriggerKey, inv.Fingerprint)`, omitted when both empty. Renders independently
of ApprovalID actions.

### Task 5: Server — record clicks
`internal/server/server.go` + tests. `FeedbackRecorder` interface + `Actions.Feedback`; endpoint
guard relaxes to `(approvals == nil && feedback == nil) || slackSecret == ""`; feedback cases are
unprivileged, respond ephemeral WITHOUT `replace_original` (updateSlack gains a `replace` flag);
approve/reject keep the allowlist and keep replacing.

### Task 6: Wiring + docs
`internal/app/serve.go`: `acts.Feedback = ledger` only when `cfg.Notify.Slack.FeedbackButtons &&
ledger.Enabled()`. Docs: `docs/configuration.md` (option + exposure requirement),
`docs/learning-loop.md` (feedback events, dedup, updated formula), `docs/getting-started.md`
(interactions Request URL note), chart values example comment.
