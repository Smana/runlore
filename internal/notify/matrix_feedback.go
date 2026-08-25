// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/thread"
)

// triggerKeyContentField is the custom event-content field Deliver stamps on
// investigation messages and the reaction listener reads back — the join between
// a 👍/👎 reaction and the incident it rates. Namespaced per the Matrix
// convention for custom keys.
const triggerKeyContentField = "io.runlore.trigger_key"

// FeedbackSink records a human 👍/👎 rating on a delivered investigation
// (implemented by *outcome.Ledger — the same method the Slack path records
// through, so dedup, trust weighting and the recurrence-cooldown re-arm are
// shared by construction).
type FeedbackSink interface {
	Feedback(triggerKey, rating, user string, at time.Time) error
}

// SilenceSink records a human 🔕 silence on a delivered investigation
// (implemented by *outcome.Ledger — the same ledger the Slack path records
// through, so the window cap, the dedup and the gate's read are shared by
// construction). Kept separate from FeedbackSink rather than widening it: the
// two capabilities have separate config flags and must be independently nil.
type SilenceSink interface {
	Silence(triggerKey string, window time.Duration, user string, at time.Time) error
}

// MatrixFeedback is the opt-in Matrix listener backing three INDEPENDENT
// capabilities, each gated by its own flag and its own functional option:
// recording 👍/👎 reactions into the outcome ledger
// (notify.matrix.feedback_reactions, WithFeedbackReactions), recording a 🔕
// silence into the same ledger (notify.matrix.silence_reactions,
// WithSilenceReactions), and capturing addressed thread replies into the
// knowledge base (notify.matrix.thread_capture, WithThreadCapture). One
// /sync long-poll loop serves whichever of the three are on; the /sync
// filter itself is narrowed to request only the event types the enabled
// capabilities actually need (see sync) — a deployment that enabled thread
// capture alone must never even ask the homeserver for reactions, let alone
// record one, and feedback reactions and silence reactions share the wire
// event type (m.reaction) but are told apart, and independently gated, once
// handleReaction looks at the emoji.
//
// Unlike the Slack feedback path, NOTHING is exposed: /sync is an outbound
// HTTPS long-poll authenticated by the notifier's access token — no inbound
// endpoint, no signing secret, no NetworkPolicy change. The trade is a
// long-lived poll loop, which runs leader-only (started in startWork, cancelled
// with the leadership context) so an HA deployment records each reaction once.
//
// The first /sync response is a position handshake only: historical reactions
// are deliberately skipped, so feedback counts from startup onward — replaying
// a room's history through the ledger on every restart would re-stamp old
// votes with fresh timestamps.
type MatrixFeedback struct {
	homeserver string
	roomID     string
	token      string
	sink       FeedbackSink
	log        *slog.Logger
	http       *http.Client

	// self is the bot's own Matrix user id (from /whoami at Run start). It is the
	// attribution trust anchor: a vote only counts when the REACTED-TO event was
	// sent by self. Without this check any room member could post a message
	// carrying an io.runlore.trigger_key field of their choosing and vote on it —
	// attributing ratings to an arbitrary incident (trust poisoning / vote
	// misdirection). Slack has no equivalent hole (interaction payloads only ever
	// reference buttons the app itself posted); Matrix must enforce it explicitly.
	self string

	// selfDisplayName is the bot's own Matrix profile display name, resolved
	// once via GET /_matrix/client/v3/profile/{userId}/displayname alongside
	// /whoami at Run start (see fetchDisplayName). Empty when the homeserver
	// has none set, the lookup failed, or Run has not resolved it yet — the
	// lookup is deliberately NON-FATAL (see Run), so any of those just leaves
	// stripSelfMention with one fewer candidate to match, exactly like
	// before this field existed. It is a best-effort ENHANCEMENT to
	// addressing, not the sole guard against a reserved command hiding
	// behind an unstripped mention: a display name can still be missed
	// entirely (changed after startup, a room-specific per-member override,
	// a client that renders it differently) — see handleMessage's own
	// backstop for the case this field cannot cover.
	selfDisplayName string

	// contextByEvent caches target-event → resolved thread.Context lookups
	// (contextFor) so N reactions to the same message, or a reaction followed by
	// a threaded reply to it, cost one fetch between them. Bounded crudely (reset
	// past cap): the working set is "messages still being reacted to or replied
	// to", a handful.
	//
	// Unsynchronised BY DESIGN, not oversight: today the only caller is the
	// /sync loop goroutine — handleReaction, and handleMessage's contextFor call
	// resolving the root BEFORE it dispatches the mention handler. Mention
	// HANDLING itself runs on Dispatch's worker pool, but this lookup stays on
	// the sync goroutine; if that ever changes, this map needs a mutex.
	contextByEvent map[string]eventLookup

	// feedbackReactions mirrors notify.matrix.feedback_reactions: only when true
	// does sync's /sync filter carry m.reaction, and only then does
	// handleReaction ever record one. Set via WithFeedbackReactions; false (the
	// default) means a listener started for thread capture alone never asks the
	// homeserver for reactions, and never records one even if a non-compliant
	// homeserver sends one anyway — the same wire-vs-code-guarantee reasoning
	// as threadCapture/handleMessage below.
	feedbackReactions bool

	// silenceReactions, silenceWindow and silenceSink back the 🔕 reaction
	// (notify.matrix.silence_reactions). silenceWindow is notify.silence.windows[0]:
	// a reaction is a bare emoji and can carry no duration of its own, so the
	// default is the only window this path can offer. The `silence:` thread command
	// is what offers the full choice.
	silenceReactions bool
	silenceWindow    time.Duration
	silenceSink      SilenceSink

	// threadCapture mirrors notify.matrix.thread_capture: only when true does
	// sync widen its /sync filter to also carry m.room.message, and only then
	// does handleMessage ever run. Set via WithThreadCapture; false (the
	// default) reproduces today's listener exactly.
	threadCapture bool

	// Mentions turns an addressed, RunLore-rooted thread message into a
	// knowledge-base write and posts the reply back. nil unless WithThreadCapture
	// was supplied.
	Mentions *thread.Mention

	// Dispatch runs Mentions.HandleMention detached from the sync goroutine: a
	// note that opens a pull request costs several sequential forge round-trips,
	// and doing that inline would stall /sync long-polling for minutes — pausing
	// 👍/👎 recording and making the room look dead. nil unless WithThreadCapture
	// was supplied.
	Dispatch *thread.Dispatcher

	// BusyDispatch runs Mentions.Busy detached from the sync goroutine too, under
	// its own small, separate budget — mirroring the Slack transport's
	// eventDispatcher/busyDispatcher split (internal/server/server.go). Busy
	// performs a real HTTP POST (Mention.reply → Matrix.ReplyInThread); calling
	// it inline from handleMessage would chain blocking network calls on the
	// SAME sync goroutine that must keep long-polling, for every addressed
	// message that arrives while Dispatch is saturated — up to the /sync
	// filter's limit:50 events per batch. That reproduces exactly the "/sync
	// stalls, feedback pauses" harm Dispatch's own detachment exists to
	// prevent. Separate from Dispatch, and deliberately small: telling a human
	// the mention pool is busy must never itself become the next unbounded
	// liability. nil unless WithThreadCapture was supplied.
	BusyDispatch *thread.Dispatcher

	// metrics is the optional telemetry sink for
	// MentionsDroppedOnSaturation — nil unless WithMetrics was supplied, and
	// every use is nil-checked: a deployment with no telemetry provider
	// configured must observe and reply to mentions exactly as it always did.
	metrics *telemetry.Metrics
}

// eventLookup is one contextByEvent cache entry: the thread.Context resolved
// from an event's content, and whether the event was one of RunLore's own
// investigation messages (ok) — cached together so a miss (wrong sender, no
// stamp) is not re-fetched on every subsequent reaction either.
type eventLookup struct {
	ctx thread.Context
	ok  bool
}

// syncTimeout is the server-side long-poll hold; the HTTP client timeout must
// comfortably exceed it or every quiet poll would surface as a client error.
const syncTimeout = 30 * time.Second

// retryBackoff is the pause after a failed sync before re-polling.
const retryBackoff = 5 * time.Second

// eventCacheCap bounds contextByEvent (reset, not evicted — simplicity over
// LRU for a cache whose working set is a handful of recent messages).
const eventCacheCap = 256

// NewMatrixFeedback builds the Matrix listener. homeserver/roomID/token are
// the notifier's own settings; sink is where ratings land (the outcome
// ledger), and — when it also implements SilenceSink, as *outcome.Ledger does
// — where silences land too. All CAPABILITIES default OFF: pass
// WithFeedbackReactions to record 👍/👎 reactions, and/or WithSilenceReactions
// to record 🔕 silences, and/or WithThreadCapture to also opt into handling
// addressed thread messages; WithMetrics is a plain optional dependency, not
// a capability, and app.BuildMatrixFeedback supplies it unconditionally.
//
// A listener with NONE of the capability options requests no event type over
// /sync and processes nothing. That is not purely hypothetical: contrary to
// an earlier version of this comment, app.BuildMatrixFeedback does not
// always avoid it — when notify.matrix.thread_capture is the only capability
// configured on and buildMatrixThreadMention degrades (no forge, no
// persisted registry, no resolvable replier), BuildMatrixFeedback still
// constructs a listener whose only option is WithMetrics, functionally
// identical to the zero-capability listener this paragraph describes, just
// with a non-empty opts slice. Its /sync loop still runs and still resolves
// /whoami; it simply never receives an event worth acting on.
func NewMatrixFeedback(homeserver, roomID, token string, sink FeedbackSink, log *slog.Logger, opts ...MatrixFeedbackOption) *MatrixFeedback {
	f := &MatrixFeedback{
		homeserver:     strings.TrimRight(homeserver, "/"),
		roomID:         roomID,
		token:          token,
		sink:           sink,
		log:            log,
		http:           httpx.SecureClient(syncTimeout + 15*time.Second),
		contextByEvent: map[string]eventLookup{},
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// MatrixFeedbackOption configures optional MatrixFeedback behaviour beyond its
// required delivery target and identity. The zero set of options reproduces
// today's listener exactly.
type MatrixFeedbackOption func(*MatrixFeedback)

// WithFeedbackReactions turns on Matrix reaction feedback
// (notify.matrix.feedback_reactions): the /sync filter carries m.reaction,
// and Run's switch actually routes those events to handleReaction. Threaded
// exactly like WithThreadCapture (see feedbackReactions): a listener started
// for thread capture alone never asks the homeserver for reactions, and one
// that was never given this option never records a reaction even if it
// somehow received one.
func WithFeedbackReactions() MatrixFeedbackOption {
	return func(f *MatrixFeedback) {
		f.feedbackReactions = true
	}
}

// WithSilenceReactions turns on Matrix 🔕 silence reactions
// (notify.matrix.silence_reactions). Like WithFeedbackReactions it is threaded
// through both the /sync filter and the handler's own guard, so a listener that
// was never given this option never records a silence even if it somehow
// received the reaction.
//
// defaultWindow is notify.silence.windows[0] — the only window a bare emoji can
// express. maxWindow is carried for the `silence:` command path and is enforced
// by the ledger regardless; it is threaded here so a misconfiguration is visible
// at construction rather than at the first click.
//
// "Visible" has to mean visible: both guards below leave the capability OFF, and
// both used to return bare. Nothing was emitted anywhere, so an operator who set
// notify.matrix.silence_reactions: true saw a clean startup and then had every
// 🔕 in the room ignored, with no line to grep for. Each branch now warns, names
// the config key the operator actually turned on, and says why it was dropped.
func WithSilenceReactions(defaultWindow, maxWindow time.Duration) MatrixFeedbackOption {
	return func(f *MatrixFeedback) {
		// A misconfigured window leaves the capability off rather than silently
		// clamping — clamping would silence for a duration nobody chose.
		if defaultWindow <= 0 || (maxWindow > 0 && defaultWindow > maxWindow) {
			f.log.Warn("matrix silence_reactions ignored: notify.silence.windows[0] is not a usable window",
				"window", defaultWindow, "max_window", maxWindow)
			return
		}
		sink, ok := f.sink.(SilenceSink)
		if !ok {
			f.log.Warn("matrix silence_reactions ignored: the configured outcome ledger cannot record silences",
				"sink", fmt.Sprintf("%T", f.sink))
			return
		}
		f.silenceReactions = true
		f.silenceWindow = defaultWindow
		f.silenceSink = sink
	}
}

// WithThreadCapture turns on Matrix thread capture
// (notify.matrix.thread_capture): the /sync filter widens to also carry
// m.room.message, and an addressed message rooted in one of RunLore's own
// investigations is handed to mentions via dispatch — detached from the sync
// goroutine, since a note that opens a pull request costs several sequential
// forge round-trips. busyDispatch runs the "I'm too busy" reply under its own,
// separate, smaller budget when dispatch is saturated — see BusyDispatch for
// why it must never share dispatch's pool or run inline.
func WithThreadCapture(mentions *thread.Mention, dispatch *thread.Dispatcher, busyDispatch *thread.Dispatcher) MatrixFeedbackOption {
	return func(f *MatrixFeedback) {
		f.threadCapture = true
		f.Mentions = mentions
		f.Dispatch = dispatch
		f.BusyDispatch = busyDispatch
	}
}

// WithMetrics wires the optional telemetry instrument set: today, solely
// MentionsDroppedOnSaturation, incremented when Dispatch is saturated and an
// addressed mention is unrecoverably lost (see handleMessage). m may be nil
// (equivalent to not calling this option at all); every use inside
// MatrixFeedback is nil-checked regardless, so a deployment with no
// telemetry provider configured is unaffected either way.
func WithMetrics(m *telemetry.Metrics) MatrixFeedbackOption {
	return func(f *MatrixFeedback) {
		f.metrics = m
	}
}

// Run long-polls /sync until ctx is cancelled (leadership loss / shutdown).
// Errors are logged and retried after a fixed backoff — a flaky homeserver
// must never crash the agent; at worst feedback pauses.
func (f *MatrixFeedback) Run(ctx context.Context) {
	// Resolve our own identity FIRST — it anchors target-event attribution (see
	// the self field). No identity, no listening: fail towards recording nothing.
	for f.self == "" {
		self, err := f.whoami(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.log.Warn("matrix feedback whoami failed; retrying", "err", err)
			select {
			case <-time.After(retryBackoff):
			case <-ctx.Done():
				return
			}
			continue
		}
		f.self = self
	}
	// Best-effort display-name resolution: an ENHANCEMENT to addressing (see
	// selfDisplayName), never a new hard dependency. One attempt, no retry
	// loop unlike whoami above — a homeserver that errors, times out, or has
	// no display name set for the account must leave the listener exactly as
	// functional as before this lookup existed, so this is logged at Debug,
	// not Warn/Error, and never blocks or delays the sync loop below.
	if dn, err := f.fetchDisplayName(ctx, f.self); err != nil {
		f.log.Debug("matrix feedback: display name lookup failed; addressing falls back to MXID/localpart only", "err", err)
	} else if dn != "" {
		f.selfDisplayName = dn
		f.log.Debug("matrix feedback: resolved own display name", "display_name", dn)
	}
	since := ""
	for ctx.Err() == nil {
		next, events, err := f.sync(ctx, since)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.log.Warn("matrix feedback sync failed; retrying", "err", err)
			select {
			case <-time.After(retryBackoff):
			case <-ctx.Done():
				return
			}
			continue
		}
		// First response = position handshake: adopt the batch token, skip the
		// (historical) events. Everything after is live.
		if since != "" {
			for _, e := range events {
				switch e.Type {
				case "m.reaction":
					f.handleReaction(ctx, e)
				case "m.room.message":
					f.handleMessage(ctx, e)
				}
			}
		}
		since = next
	}
}

// matrixEvent is the subset of a timeline event the listener needs — an
// m.reaction (RelatesTo.Key names the emoji) or an m.room.message (Body,
// Mentions, and either thread relation resolve whether, and where, to reply).
type matrixEvent struct {
	Type    string `json:"type"`
	Sender  string `json:"sender"`
	EventID string `json:"event_id"`
	Content struct {
		Body    string `json:"body"`
		MsgType string `json:"msgtype"`
		// Mentions is MSC3952's m.mentions: the authoritative "was RunLore
		// addressed" signal when the client sends it.
		Mentions struct {
			UserIDs []string `json:"user_ids"`
		} `json:"m.mentions"`
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
			Key     string `json:"key"`
			// InReplyTo is the MSC3440 fallback: a plain reply (rel_type
			// unset) or a thread client that also sends the immediate-parent
			// fallback alongside rel_type m.thread.
			InReplyTo struct {
				EventID string `json:"event_id"`
			} `json:"m.in_reply_to"`
		} `json:"m.relates_to"`
	} `json:"content"`
}

// sync performs one filtered /sync long-poll and returns the next batch token
// plus the room's new timeline events. The inline filter requests ONLY the
// event types the enabled capabilities actually need — no presence, state, or
// other rooms' traffic ever crosses the wire, and neither does an event type
// belonging to a capability this listener was not opted into: m.reaction only
// when feedbackReactions or silenceReactions is on (both ride the same
// m.reaction event type — 👍/👎 and 🔕 are only told apart once handleReaction
// looks at the key), m.room.message only when threadCapture is on. A
// deployment that has not opted into a capability must keep receiving exactly
// what it receives today for that capability — including "nothing", so it
// never even asks the homeserver for events it would just discard.
func (f *MatrixFeedback) sync(ctx context.Context, since string) (string, []matrixEvent, error) {
	var types []string
	if f.feedbackReactions || f.silenceReactions {
		types = append(types, `"m.reaction"`)
	}
	if f.threadCapture {
		types = append(types, `"m.room.message"`)
	}
	timelineTypes := "[" + strings.Join(types, ",") + "]"
	filter := fmt.Sprintf(`{"presence":{"types":[]},"account_data":{"types":[]},"room":{"rooms":[%q],"state":{"types":[]},"ephemeral":{"types":[]},"account_data":{"types":[]},"timeline":{"types":%s,"limit":50}}}`, f.roomID, timelineTypes)
	q := url.Values{"filter": {filter}, "timeout": {fmt.Sprintf("%d", syncTimeout.Milliseconds())}}
	if since != "" {
		q.Set("since", since)
	}
	endpoint := f.homeserver + "/_matrix/client/v3/sync?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("matrix sync: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", nil, fmt.Errorf("matrix sync status %d", resp.StatusCode)
	}
	var res struct {
		NextBatch string `json:"next_batch"`
		Rooms     struct {
			Join map[string]struct {
				Timeline struct {
					Events []matrixEvent `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&res); err != nil {
		return "", nil, fmt.Errorf("matrix sync decode: %w", err)
	}
	if res.NextBatch == "" {
		return "", nil, fmt.Errorf("matrix sync: empty next_batch")
	}
	return res.NextBatch, res.Rooms.Join[f.roomID].Timeline.Events, nil
}

// handleReaction maps one m.reaction event to either a ledger rating (👍→up,
// 👎→down) or a ledger silence (🔕, at f.silenceWindow) — every other emoji is
// ignored, reactions being a general mechanism: 🎉 on a resolved incident must
// not count as an endorsement of the diagnosis, nor as a request to silence
// it. The reaction names its target event; the trigger identity is read back
// from the target's content via triggerKeyFor — a target without the field,
// or not sent by the bot (see triggerKeyFor/contextFor's self-authorship
// check), is not one of RunLore's investigation messages and is skipped.
func (f *MatrixFeedback) handleReaction(ctx context.Context, e matrixEvent) {
	if e.Type != "m.reaction" || e.Content.RelatesTo.RelType != "m.annotation" {
		return
	}
	// The opt-in check is PER CAPABILITY, not per function: a deployment with
	// silence_reactions on and feedback_reactions off must record 🔕 and must not
	// record 👍/👎. A single early return at the top of this function would
	// silently grant one capability to anyone who enabled the other — the exact
	// class of bug the original guard was written to prevent. Each case's own
	// guard is a code-level backstop behind the /sync filter's wire-level one
	// (see sync): a non-compliant homeserver can still deliver an event type
	// this listener never asked for.
	kind := ""
	// Clients commonly append the emoji variation selector (U+FE0F); strip it so
	// "👍"/"🔕" and "👍️"/"🔕️" are the same action.
	switch strings.ReplaceAll(e.Content.RelatesTo.Key, "️", "") {
	case "👍":
		if !f.feedbackReactions {
			return
		}
		kind = "up"
	case "👎":
		if !f.feedbackReactions {
			return
		}
		kind = "down"
	case "🔕":
		if !f.silenceReactions {
			return
		}
		kind = "silence"
	default:
		return
	}
	key, err := f.triggerKeyFor(ctx, e.Content.RelatesTo.EventID)
	if err != nil {
		f.log.Warn("matrix feedback: target event lookup failed", "event_id", e.Content.RelatesTo.EventID, "err", err)
		return
	}
	if key == "" {
		return // not one of our investigation messages
	}
	if kind == "silence" {
		if err := f.silenceSink.Silence(key, f.silenceWindow, e.Sender, time.Now()); err != nil {
			f.log.Warn("matrix silence recording failed", "key", key, "err", err)
			return
		}
		f.log.Info("matrix silence recorded", "key", key, "window", f.silenceWindow, "user", e.Sender)
		return
	}
	if err := f.sink.Feedback(key, kind, e.Sender, time.Now()); err != nil {
		f.log.Warn("matrix feedback recording failed", "key", key, "err", err)
		return
	}
	f.log.Info("matrix feedback recorded", "key", key, "rating", kind, "user", e.Sender)
}

// handleMessage reacts to one m.room.message event: when it addresses RunLore
// and is rooted in one of RunLore's own investigation messages, the actual
// knowledge-base write is handed to Dispatch — detached, because a note that
// opens a pull request costs several sequential forge round-trips, and doing
// that inline would stall this /sync long-poll for minutes, pausing 👍/👎
// recording and making the room look dead. When Dispatch is saturated, the
// "I'm too busy" reply is ALSO detached, through BusyDispatch's own separate,
// smaller budget — never run inline here, or the same stall this function
// exists to avoid would reappear one call later, for every addressed message
// arriving in the same 50-event /sync batch.
//
// contextFor — the trust-anchored lookup deciding whether the root belongs to
// RunLore — is resolved HERE, on the sync goroutine, before Dispatch ever
// runs: its cache (contextByEvent) is deliberately unsynchronised, so calling
// it from the dispatched closure would race with the next sync iteration's own
// cache access. Its result is also handed to HandleMention as the
// registry-miss fallback (see HandleMention's fallback parameter): the
// thread.Registry can miss (TTL, the size cap, a leader failover onto a
// replica without its JSONL) in cases where the root event's own stamp still
// carries everything needed to answer the mention — the advantage a Matrix
// event has over a Slack thread_ts, which carries nothing on its own.
//
// There is deliberately no transport-side backstop here for a reserved
// command (e.g. "reinvestigate:") sitting behind a mention token
// stripSelfMention could not identify and remove — an earlier version of
// this function had one, gated on stripSelfMention's own success. That gate
// was the bug: it only fired when stripping FAILED, so a message where
// stripping SUCCEEDED but a filler word still separated the mention from the
// command ("@runlore please reinvestigate: …" → stripped to "please
// reinvestigate: …") sailed straight past it. The actual fix belongs in
// thread.Parse itself — matching a reserved prefix as a whole, colon-anchored
// token ANYWHERE in the text, not only at position 0 (see Parse's doc) — so
// every caller of Responder.Handle is covered by construction, on both
// transports, regardless of whether or how a mention token got stripped
// first. That makes the old backstop here fully redundant, not merely
// narrower: HandleMention below already routes every addressed message
// through Parse via Responder.Handle, so the reserved-anywhere match applies
// identically whether text still carries an unstripped mention token or not.
func (f *MatrixFeedback) handleMessage(ctx context.Context, e matrixEvent) {
	// Guard FIRST, before the self-guard below: the /sync filter only requests
	// m.room.message when threadCapture is on (see sync), but that is a
	// wire-level guarantee, not a code-level one — a non-compliant homeserver,
	// or a future filter-construction regression, can still deliver one. Without
	// this, a root that happens to resolve against RunLore's own history (very
	// plausible: investigation messages persist in the room regardless of the
	// option) would reach f.Dispatch.Go on a nil *thread.Dispatcher and panic,
	// contradicting Run's own documented invariant that a flaky homeserver must
	// never crash the agent.
	if !f.threadCapture || f.Dispatch == nil || f.Mentions == nil || f.BusyDispatch == nil {
		return
	}
	if e.Sender == f.self {
		return // loop guard: never answer our own messages
	}
	if !f.addressed(e) {
		return
	}
	root := threadRootOf(e)
	if root == "" {
		return
	}
	tc, ok, err := f.contextFor(ctx, root)
	if err != nil {
		f.log.Warn("matrix thread capture: root event lookup failed", "event_id", root, "err", err)
		return
	}
	if !ok {
		return // not rooted in one of RunLore's own investigation messages
	}
	channel, author := f.roomID, e.Sender
	// stripped is unused beyond stripSelfMention's own doc contract (which text
	// callers get, stripped or not): whether a reserved command sitting behind
	// an unstripped mention token gets refused no longer depends on it — see
	// this function's doc comment above for why that used to matter and no
	// longer does. text always reaches Parse (via Responder.Handle, below)
	// exactly as stripSelfMention returns it, stripped or not.
	text, _ := stripSelfMention(e.Content.Body, f.self, f.selfDisplayName)
	if f.Dispatch.Go(ctx, func(wctx context.Context) {
		// tc is the context resolved HERE, on the sync goroutine, off the root
		// event's own stamp — passed through as HandleMention's registry-miss
		// fallback rather than re-resolved inside this closure. contextByEvent is
		// deliberately unsynchronised (see its doc): only the sync goroutine may
		// touch it, and this closure runs on Dispatch's worker pool.
		f.Mentions.HandleMention(wctx, channel, root, author, text, &tc)
	}) {
		return
	}
	// Dispatch is saturated: the mention itself is unrecoverably lost.
	// /sync's `since` will have advanced past this event by the time this
	// batch finishes, so — unlike a queued retry — the homeserver never
	// redelivers it. Error, not Warn, mirroring the Slack mention path
	// (internal/server/server.go's own "mention dropped, handler pool
	// saturated" log): this loss is permanent, for a feature whose entire
	// premise is not losing the operator's knowledge.
	f.log.Error("matrix thread capture: mention dropped, dispatcher saturated", "channel", channel, "root", root)
	if f.metrics != nil {
		f.metrics.MentionsDroppedOnSaturation.Add(ctx, 1)
	}
	// Telling the human is itself another network call
	// (Mention.Busy → Matrix.ReplyInThread), so it gets the same treatment as
	// the mention handler itself: run through BusyDispatch's own small budget,
	// detached from this sync goroutine — never called inline here. If
	// BusyDispatch is ALSO saturated, log and move on: this /sync loop must
	// keep long-polling no matter what, at worst leaving one human with no
	// "try again" notice this round.
	if !f.BusyDispatch.Go(ctx, func(wctx context.Context) {
		f.Mentions.Busy(wctx, channel, root)
	}) {
		f.log.Error("matrix thread capture: busy notice dropped, busy dispatcher saturated", "channel", channel, "root", root)
	}
}

// addressed reports whether e addresses RunLore. MSC3952's m.mentions is
// authoritative when a client sends it; clients that don't are matched by the
// message body containing the bot's full MXID or its localpart (the
// "runlore" in "@runlore:example.org") as a WHOLE WORD — see containsWord for
// why a plain substring match is not enough. Both fallbacks apply the same
// boundary rule: a full MXID is just as embeddable in a longer token (e.g.
// "x@runlore:hs" or "@runlore:hs2") as a bare localpart is.
func (f *MatrixFeedback) addressed(e matrixEvent) bool {
	for _, id := range e.Content.Mentions.UserIDs {
		if id == f.self {
			return true
		}
	}
	if f.self == "" {
		return false
	}
	if containsWord(e.Content.Body, f.self) {
		return true
	}
	lp := selfLocalpart(f.self)
	return lp != "" && containsWord(e.Content.Body, lp)
}

// selfLocalpart returns a Matrix user id's localpart — "runlore" from
// "@runlore:example.org" — used as addressed's body-match fallback.
func selfLocalpart(mxid string) string {
	mxid = strings.TrimPrefix(mxid, "@")
	if i := strings.IndexByte(mxid, ':'); i >= 0 {
		return mxid[:i]
	}
	return mxid
}

// containsWord reports whether body contains word bounded on both sides by a
// non-word RUNE (anything that is not a Unicode letter, digit, or combining
// mark — see isWordRune) or the start/end of body. A plain strings.Contains
// would treat the localpart "sre" as addressing RunLore inside "misread", or
// "ops" inside "oops" — not cosmetic, since anything this decides is addressed
// reaches thread.Responder.Handle. An unprefixed message (IntentFreeform) no
// longer writes on its own, but with model.chat configured it costs exactly
// one paid model call, and the model may propose note content that is then
// written to a real knowledge-base PR. A false positive here therefore spends
// money and can record something nobody intended to record.
func containsWord(body, word string) bool {
	if word == "" {
		return false
	}
	for i := 0; i+len(word) <= len(body); i++ {
		if body[i:i+len(word)] != word {
			continue
		}
		if i > 0 {
			if r, _ := utf8.DecodeLastRuneInString(body[:i]); isWordRune(r) {
				continue
			}
		}
		if j := i + len(word); j < len(body) {
			if r, _ := utf8.DecodeRuneInString(body[j:]); isWordRune(r) {
				continue
			}
		}
		return true
	}
	return false
}

// isWordRune reports whether r is a Unicode letter, digit, or combining
// mark — the boundary condition containsWord and stripSelfMention both
// require immediately before and/or after a match. RUNE-aware, not
// byte-aware: the byte-only predicate this replaced (isAlphanumericByte,
// ASCII a-z/A-Z/0-9 only) never recognised a UTF-8 CONTINUATION byte as
// alphanumeric, because a continuation byte in isolation never is — so a
// non-ASCII letter or digit sitting immediately next to a match (e.g.
// "runlore" inside "runloreé", or next to a fullwidth digit) was silently
// treated as a word boundary, a false positive containsWord's own doc warns
// can write unintended text into the knowledge base.
// DecodeLastRuneInString/DecodeRuneInString decode the one full rune
// adjacent to the match instead of a single byte of it; utf8.RuneError (an
// empty string, or invalid encoding) is neither a letter, digit, nor mark,
// so it is correctly treated as a boundary like any other non-word
// character.
//
// unicode.IsMark(r) — true for the Mn/Mc/Me categories — covers a residual
// gap IsLetter/IsDigit alone leave open: an NFC (precomposed) accented
// letter like 'é' (U+00E9) is one rune and IsLetter already reports true for
// it, but an NFD (decomposed) accent is TWO runes — the base letter followed
// by a standalone COMBINING mark such as U+0301 COMBINING ACUTE ACCENT — and
// a combining mark on its own is neither a letter nor a digit. A combining
// mark does not start a new character; it continues the grapheme of the
// rune immediately before it. So without IsMark here, matching "runlore"
// against NFD "runloré" (== "runlore" + U+0301) would decode that trailing
// U+0301 as the neighbour rune, find it is neither a letter nor a digit, and
// misread the middle of a single accented character as a word boundary —
// the exact same false-positive class the rune-awareness above fixed for
// NFC, just one normalization form later. Real Matrix clients send NFD in
// practice (it's the default output of macOS input methods), so this is not
// a hypothetical input shape.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// selfMentionPrefixes returns the leading-mention tokens stripSelfMention
// matches against, longest first: self's full MXID with the "@" sigil, the
// full MXID without it, "@"+localpart, the bare localpart, and — when known —
// the resolved display name. Longest-first matters — for self "@runlore:hs"
// the bare localpart "runlore" is itself a prefix of the full MXID, so trying
// it before the full-MXID candidates would strip only "runlore" and leave
// ":hs" sitting in front of the command; sorting by length rather than
// hand-ordering also keeps this correct regardless of how displayName's
// length happens to compare to the MXID/localpart candidates.
func selfMentionPrefixes(self, displayName string) []string {
	lp := selfLocalpart(self)
	mxid := strings.TrimPrefix(self, "@")
	var out []string
	if mxid != "" {
		out = append(out, "@"+mxid, mxid)
	}
	if lp != "" {
		out = append(out, "@"+lp, lp)
	}
	if displayName != "" {
		out = append(out, displayName)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// stripSelfMention removes ONE leading token addressing RunLore from a Matrix
// message body — a bare MXID ("@runlore:hs"), a localpart alone ("runlore"),
// the resolved display name when one is known, or any of those immediately
// followed by ":" or "," — before thread.Parse ever sees the text. The second
// return reports whether a token was actually found and removed: false means
// body is returned completely unchanged, which callers use to tell "the
// mention really wasn't there in a form we recognise" apart from "we
// recognised and stripped it, and what's left just happens to look the same"
// (impossible here, since a match always consumes at least one byte, but the
// bool makes that guarantee explicit rather than inferred by callers
// re-deriving it from string equality).
//
// This exists because thread.Parse strips only Slack's "<@U…>" mention
// encoding, which real Matrix clients never send: a client puts a bare MXID,
// a localpart, or a display name directly in body instead. Without stripping
// it here, the mention token sits in front of "note:"/"reinvestigate:" and
// Parse's prefix match never fires — every addressed message falls through to
// IntentFreeform, silently including the RESERVED "reinvestigate:" prefix
// (see thread.IntentReinvestigate's doc), with the mention text still
// embedded in what gets written to the knowledge base.
//
// Matching is case-insensitive and whole-token only (the same rune-aware
// boundary rule as containsWord/addressed — see isWordRune): "runlore" must
// not strip the front of a longer word like "runlored" or "runloreé". A
// client-configured DISPLAY NAME unrelated to the localpart (e.g. an
// operator-set "Ops Bot") is stripped only when
// displayName carries it — MatrixFeedback.Run resolves it once via a
// best-effort profile lookup (see fetchDisplayName/selfDisplayName); pass ""
// when it is unknown. Even then, a display name can still go unrecognised
// (never resolved, changed after startup, a room-specific per-member
// override) — handleMessage's own backstop covers that residual case rather
// than this function trying to. A display name that happens to equal the
// localpart case-insensitively — the common case, since Matrix's own
// suggested display name IS the localpart — is stripped regardless, via the
// bare-localpart candidate.
func stripSelfMention(body, self, displayName string) (string, bool) {
	body = strings.TrimSpace(body)
	for _, cand := range selfMentionPrefixes(self, displayName) {
		if cand == "" || len(body) < len(cand) || !strings.EqualFold(body[:len(cand)], cand) {
			continue
		}
		rest := body[len(cand):]
		if rest != "" {
			if r, _ := utf8.DecodeRuneInString(rest); isWordRune(r) {
				continue // not a whole-token match — e.g. "runlored" must not strip as "runlore"
			}
		}
		return strings.TrimSpace(strings.TrimLeft(rest, ":,")), true
	}
	return body, false
}

// threadRootOf resolves e's thread root: an m.thread relation's event_id names
// the root directly (MSC3440), so a reply nested anywhere in the thread still
// resolves to the same root. Otherwise the m.in_reply_to fallback names the
// immediate parent, which for a fallback-only (non-threaded) reply IS the
// root. "" means e carries no thread relation at all.
func threadRootOf(e matrixEvent) string {
	rel := e.Content.RelatesTo
	if rel.RelType == "m.thread" && rel.EventID != "" {
		return rel.EventID
	}
	return rel.InReplyTo.EventID
}

// whoami resolves the access token's own user id — the sender every legitimate
// investigation message carries.
func (f *MatrixFeedback) whoami(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.homeserver+"/_matrix/client/v3/account/whoami", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("matrix whoami status %d", resp.StatusCode)
	}
	var res struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&res); err != nil {
		return "", err
	}
	if res.UserID == "" {
		return "", fmt.Errorf("matrix whoami: empty user_id")
	}
	return res.UserID, nil
}

// fetchDisplayName resolves userID's Matrix profile display name via
// GET /_matrix/client/v3/profile/{userId}/displayname. A 404 is a VALID
// answer, not a failure: per the Client-Server spec (and every homeserver
// implementation checked — Synapse, Dendrite), an account with no display
// name set responds 404 M_NOT_FOUND rather than 200 with an empty field, so
// it is reported here as ("", nil) rather than an error a caller would log.
func (f *MatrixFeedback) fetchDisplayName(ctx context.Context, userID string) (string, error) {
	endpoint := f.homeserver + "/_matrix/client/v3/profile/" + url.PathEscape(userID) + "/displayname"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil // no display name set for this account
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("matrix displayname status %d", resp.StatusCode)
	}
	var res struct {
		DisplayName string `json:"displayname"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&res); err != nil {
		return "", err
	}
	return res.DisplayName, nil
}

// fetchEvent fetches one event by id from the configured room and returns its
// sender and raw content — the single HTTP call shared by contextFor (and,
// through it, triggerKeyFor).
func (f *MatrixFeedback) fetchEvent(ctx context.Context, eventID string) (sender string, content map[string]any, err error) {
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/event/%s",
		f.homeserver, url.PathEscape(f.roomID), url.PathEscape(eventID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.http.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", nil, fmt.Errorf("matrix event fetch status %d", resp.StatusCode)
	}
	var ev struct {
		Sender  string         `json:"sender"`
		Content map[string]any `json:"content"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ev); err != nil {
		return "", nil, err
	}
	return ev.Sender, ev.Content, nil
}

// contextFor fetches eventID and rebuilds the thread.Context RunLore stamped
// on it. The second return is false when the event is not one of RunLore's
// own investigation messages: no recognised field in its content (unstamped,
// or one of ours from before the field existed), or — the trust anchor —
// sent by anyone OTHER than the bot itself. Without that check, any room
// member could stamp io.runlore.thread (or the legacy io.runlore.trigger_key)
// onto their own message and redirect where a human's note gets written;
// Slack has no equivalent hole (interaction payloads only ever reference
// content the app itself posted), so Matrix must enforce it explicitly.
//
// Results are cached per event id; see contextByEvent for the cache's
// concurrency contract.
func (f *MatrixFeedback) contextFor(ctx context.Context, eventID string) (thread.Context, bool, error) {
	if eventID == "" {
		return thread.Context{}, false, nil
	}
	if cached, hit := f.contextByEvent[eventID]; hit {
		return cached.ctx, cached.ok, nil
	}
	sender, content, err := f.fetchEvent(ctx, eventID)
	if err != nil {
		return thread.Context{}, false, err
	}
	tc, ok := contextFromContent(content, eventID, f.roomID)
	if sender != f.self {
		tc, ok = thread.Context{}, false // not our message: whatever it claims attributes nothing
	}
	if len(f.contextByEvent) >= eventCacheCap {
		f.contextByEvent = map[string]eventLookup{} // crude bound; the live working set is tiny
	}
	f.contextByEvent[eventID] = eventLookup{ctx: tc, ok: ok}
	return tc, ok, nil
}

// triggerKeyFor returns the trigger identity embedded in eventID's event, or
// "" when contextFor found none (absent, or not sent by the bot — see
// contextFor for the trust anchor this enforces). Built on the identical
// fetch, self-check and cache contextFor uses.
func (f *MatrixFeedback) triggerKeyFor(ctx context.Context, eventID string) (string, error) {
	tc, ok, err := f.contextFor(ctx, eventID)
	if err != nil || !ok {
		return "", err
	}
	return tc.TriggerKey, nil
}
