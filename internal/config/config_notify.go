// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// Notify configures where investigation findings are delivered.
type Notify struct {
	Slack   SlackNotify   `yaml:"slack"`
	Matrix  MatrixNotify  `yaml:"matrix"`
	Thread  ThreadNotify  `yaml:"thread"`  // shared Slack/Matrix thread-capture bounds — see ThreadNotify
	Silence SilenceNotify `yaml:"silence"` // shared Slack/Matrix silence windows — see SilenceNotify
	// Extra catches notify.<name> blocks for registered (non-built-in)
	// notifiers. CONSTRAINT: a drop-in notifier must never be registered
	// under the name of any named field above — today "slack", "matrix",
	// "thread" or "silence" — because a notify.<that-name> block would land
	// there instead of here, silently shadowing the notifier's own config.
	// (Slack and Matrix themselves are registered under those exact names on
	// purpose — that pairing is the reason those two fields exist — so the
	// constraint is on every OTHER registered notifier, not on those two.)
	//
	// This list is prose and can go stale; the guard below does not read it.
	//
	// Enforced by TestRegisteredNotifierNamesDoNotCollideWithConfigFields in
	// internal/notify (package notify_test, so it can import both this
	// config package and every notifier subpackage without an import
	// cycle — internal/notify already imports internal/config, so this
	// package cannot import internal/notify back). It reflects over every
	// named field's yaml tag here and compares it against
	// notify.Registered() — the SAME enumeration notify.BuildEnabled uses at
	// runtime — so, unlike a source grep for the literal call
	// `Register(Descriptor{...})`, it catches every registered notifier
	// regardless of which form it registers with: the bare form used by
	// internal/notify/slack.go and internal/notify/matrix.go, or the
	// qualified notify.Register(notify.Descriptor{...}) form used by the
	// internal/notify/webhook and internal/notify/templated subpackages.
	//
	// notify.Registered() reports only what that test binary links in, and a
	// subpackage is linked in only by the hand-written blank-import block at
	// the top of config_collision_test.go — so the guard is exactly as
	// complete as that block. TestEveryNotifierSubpackageIsImportedHere
	// (internal/notify/notifier_imports_test.go) keeps it complete by reading
	// the internal/notify directory and requiring an import for every
	// subpackage that calls notify.Register.
	Extra map[string]yaml.Node `yaml:",inline"`
}

// ThreadNotify configures the bounds shared by Slack and Matrix thread
// capture (the `@runlore note: …` reply loop, and — with model.chat set —
// plain questions too): how many notes one thread may write, how
// fast the shared forge-write budget refills, how long a thread stays
// answerable, how many live threads the registry holds, the largest single
// message accepted, and how much the chat model may spend — in calls and in
// tokens — per hour. Before this block existed every one of these was a
// hardcoded Go constant, so an operator who found the volume too high had
// exactly one lever: turn thread capture off entirely.
//
// Every field defaults to the constant it replaces when left at its zero
// value, so an ABSENT notify.thread block changes nothing.
//
// Every field follows gitops.mirror.max's convention, not
// ratelimit.Window's: 0 means "use the default", and Validate rejects a
// negative value. None of these bounds may mean "unlimited" — an unbounded
// note-byte cap or an unbounded per-thread note count would defeat the exact
// safety property each one exists to enforce, so — unlike ratelimit.Window,
// whose own <= 0 convention IS "unlimited" — that meaning is deliberately not
// offered here. thread.Budget, which ChatCallsPerHour/ChatTokensPerHour
// configure, DOES support <= 0 as unlimited at the type level (it mirrors
// ratelimit.Window directly), but the Effective* resolvers below never pass a
// literal 0 or negative value through to it — an absent field always
// resolves to its positive default, exactly like every sibling field here.
type ThreadNotify struct {
	// MaxNotesPerThread caps knowledge writes per thread; 0 uses
	// thread.DefaultMaxNotesPerThread.
	MaxNotesPerThread int `yaml:"max_notes_per_thread"`
	// ForgeWritesPerHour caps every forge write thread capture makes,
	// globally, per hour — a CommentOnPR spends it exactly like an OpenPR; 0
	// uses thread.DefaultForgeWritesPerHour.
	ForgeWritesPerHour int `yaml:"forge_writes_per_hour"`
	// RegistryTTL bounds how long a thread stays answerable after its
	// investigation was delivered; 0 uses thread.DefaultRegistryTTL.
	RegistryTTL Duration `yaml:"registry_ttl"`
	// RegistryMax caps how many live threads the registry holds at once — the
	// real backstop for a busy channel; 0 uses thread.DefaultRegistryMax.
	RegistryMax int `yaml:"registry_max"`
	// MaxNoteBytes caps one human message, in bytes, before it is written to
	// the knowledge base or shown to a model; 0 uses
	// thread.DefaultMaxNoteBytes. See that constant's doc comment for why
	// this bound exists.
	//
	// A positive value must be at least thread.MinMaxNoteBytes: below that the
	// truncation marker fills the whole budget and NOTHING of the note is
	// written, while the reply still reports it saved. Validate refuses it.
	//
	// It is also the ONLY term of the chat layer's assembled prompt an operator
	// can move, and it moves it exactly one-for-one: the prompt ceiling is a
	// fixed 7,192 bytes plus this value (thread.MaxChatContextBytes is that sum
	// at the default). Raising it therefore raises what a single call can cost
	// in tokens by the same amount — and ChatTokensPerHour's default is derived
	// for the DEFAULT note cap, so raise that with it.
	MaxNoteBytes int `yaml:"max_note_bytes"`
	// ChatCallsPerHour caps model calls the chat layer may make, globally,
	// per hour; 0 uses thread.DefaultChatCallsPerHour. Independent of
	// ChatTokensPerHour — see thread.Budget's doc comment for why a call cap
	// alone cannot bound spend.
	ChatCallsPerHour int `yaml:"chat_calls_per_hour"`
	// ChatTokensPerHour caps what the chat layer may spend, globally, per
	// hour, in provider-reported tokens (input + output) — plus a
	// conservative estimate whenever the provider reports none, or reports
	// output but no prompt cost, and one further input estimate for every
	// retried upstream attempt, since a request a provider accepted is
	// billed whether or not the client kept its answer. 0 uses
	// thread.DefaultChatTokensPerHour, which is DERIVED from this layer's
	// own prompt ceiling at the default MaxNoteBytes rather than chosen as a
	// round number. This is the real cumulative ceiling
	// investigation.max_tokens_per_investigation cannot provide for this
	// path, because that field compares the size of the next request against
	// the limit rather than a running total.
	ChatTokensPerHour int64 `yaml:"chat_tokens_per_hour"`
	// AnnounceKBUpdates broadcasts a landed knowledge write to the configured
	// notifiers, so a knowledge-base update reaches people who were not reading
	// the thread it came from — and says WHERE. See AnnounceMode for the
	// accepted spellings and what each one delivers.
	//
	// Default OFF. It is the one field in this block that is a switch rather
	// than a bound, and the only one whose zero value turns something OFF rather
	// than selecting a default, so the "0 means the default" convention above
	// does not apply to it. Opt-in for two reasons: it adds notification volume
	// to channels that did not ask for it, while the thread reply is already the
	// direct acknowledgement to the person who typed; and the announcement
	// carries the note's own text, so enabling it sends what someone wrote in
	// one thread to every sink configured here. That is an operator's decision
	// to take knowingly, which makes the unconfigured state the off state.
	AnnounceKBUpdates AnnounceMode `yaml:"announce_kb_updates"`
}

// AnnounceMode says whether a landed knowledge write is announced, and to which
// destination. It accepts a YAML BOOLEAN as well as its own names, because
// `false` and `true` shipped first and an operator's existing file must keep
// meaning exactly what it meant: false is off, true is channel — today's
// behaviour, unchanged, down to the message and the destination.
//
// The routing values exist because "a second destination" is only a second
// destination when there are several sinks. The channel post reaches people who
// were not reading the thread, which is the whole point — but with a SINGLE
// transport the thread already lives in the channel the announcement posts to,
// so every write restated in the channel what the thread had just said. An
// operator's only remedy was turning the feature off, losing the signal along
// with the echo.
type AnnounceMode string

// The announcement destinations. "" (the absent key) is off, as is the explicit
// "off" — the same empty-plus-explicit pair Curate.Sweeps.Mode uses.
const (
	// AnnounceOff makes no announcement at all. The default: an unconfigured
	// deployment never broadcasts what someone typed in a thread.
	AnnounceOff AnnounceMode = "off"
	// AnnounceChannel posts to each configured notifier's own channel or room
	// and nowhere else. What `true` has always meant.
	AnnounceChannel AnnounceMode = "channel"
	// AnnounceThread delivers into the ORIGINATING thread on the transport the
	// note was typed in, instead of that transport's channel. Sinks on any other
	// transport — a Matrix room when the note came from Slack, a webhook, an
	// incoming-webhook Slack that cannot reply in a thread — still receive it at
	// channel level: see providers.KBDelivery for why that fallback rather than
	// a skip.
	AnnounceThread AnnounceMode = "thread"
	// AnnounceBoth delivers into the originating thread AND to every sink's
	// channel. For a deployment that wants the announcement's provenance line
	// (who wrote the note, on which chat system — which the thread reply does
	// not carry) in the thread, and the broadcast as well.
	AnnounceBoth AnnounceMode = "both"
)

// UnmarshalYAML accepts either a boolean or one of the mode names.
//
// The boolean is tried FIRST, and that ordering is the compatibility guarantee:
// every value that decodes into a Go bool today still decodes into one here, so
// `true`, `false` and YAML 1.1's `on`/`off`/`yes`/`no` — all of which yaml.v3
// resolves when the target is a bool — resolve exactly as they did when this
// field was a plain bool. Only a value that is NOT a boolean reaches the string
// branch, which is where the new names live.
//
// A null (`announce_kb_updates:` with nothing after it) never reaches this
// method at all: yaml.v3 short-circuits a null node before it looks for an
// Unmarshaler, leaving the field at its ZERO VALUE. That is why "" is a valid
// off state in its own right in Validate and On() rather than being normalised
// to AnnounceOff here — the normalisation would never run. The bool field this
// replaces resolved a null to false, so the answer is unchanged either way.
//
// An unrecognised name is stored rather than rejected here, so Validate reports
// it with the file's own spelling and the list of what is accepted — a decode
// error would name the Go type instead.
func (m *AnnounceMode) UnmarshalYAML(value *yaml.Node) error {
	var b bool
	if err := value.Decode(&b); err == nil {
		if b {
			*m = AnnounceChannel
		} else {
			*m = AnnounceOff
		}
		return nil
	}
	// Named for what it is rather than "s": internal/foldguard groups
	// case-normalised values per package BY NAME, and an unrelated `s` folded with
	// EqualFold elsewhere in this file made the two look like one value reached by
	// two normalisers — the exact defect that guard exists to catch. A distinct
	// name is the honest fix; an allowlist entry would have hidden a real one later.
	var raw string
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("notify.thread.announce_kb_updates must be a boolean or one of %s: %w", announceModeList, err)
	}
	*m = AnnounceMode(strings.ToLower(strings.TrimSpace(raw)))
	return nil
}

// announceModeList is the accepted-values list, written once so the decode
// error and the validation error cannot drift apart.
const announceModeList = "off|channel|thread|both"

// On reports whether anything is announced at all. Both spellings of off — the
// absent key and the explicit "off" — answer false.
func (m AnnounceMode) On() bool { return m != "" && m != AnnounceOff }

// Delivery maps the configured mode onto the destination the notifiers act on
// (providers.KBDelivery). Off has no delivery and resolves to the channel value
// like any other unset enum; On is what decides whether an announcer exists at
// all, so this is never reached for an off deployment.
func (m AnnounceMode) Delivery() providers.KBDelivery {
	switch m {
	case AnnounceThread:
		return providers.KBDeliverThread
	case AnnounceBoth:
		return providers.KBDeliverBoth
	default:
		return providers.KBDeliverChannel
	}
}

// EffectiveMaxNotesPerThread resolves the configured cap, falling back to
// thread.DefaultMaxNotesPerThread when unset (0). Validate rejects a negative
// value before this is ever reached from Load; this fallback also covers a
// *Config built outside Load (tests, a future embedder).
func (t ThreadNotify) EffectiveMaxNotesPerThread() int {
	if t.MaxNotesPerThread > 0 {
		return t.MaxNotesPerThread
	}
	return thread.DefaultMaxNotesPerThread
}

// EffectiveForgeWritesPerHour resolves the configured global forge-write
// budget, falling back to thread.DefaultForgeWritesPerHour when unset (0).
func (t ThreadNotify) EffectiveForgeWritesPerHour() int {
	if t.ForgeWritesPerHour > 0 {
		return t.ForgeWritesPerHour
	}
	return thread.DefaultForgeWritesPerHour
}

// EffectiveRegistryTTL resolves the configured thread-registry TTL, falling
// back to thread.DefaultRegistryTTL when unset (0).
func (t ThreadNotify) EffectiveRegistryTTL() time.Duration {
	if t.RegistryTTL > 0 {
		return t.RegistryTTL.Std()
	}
	return thread.DefaultRegistryTTL
}

// EffectiveRegistryMax resolves the configured thread-registry size cap,
// falling back to thread.DefaultRegistryMax when unset (0).
func (t ThreadNotify) EffectiveRegistryMax() int {
	if t.RegistryMax > 0 {
		return t.RegistryMax
	}
	return thread.DefaultRegistryMax
}

// EffectiveMaxNoteBytes resolves the configured per-note byte cap, falling
// back to thread.DefaultMaxNoteBytes when unset (0).
func (t ThreadNotify) EffectiveMaxNoteBytes() int {
	if t.MaxNoteBytes > 0 {
		return t.MaxNoteBytes
	}
	return thread.DefaultMaxNoteBytes
}

// EffectiveChatCallsPerHour resolves the configured chat-layer call ceiling,
// falling back to thread.DefaultChatCallsPerHour when unset (0).
func (t ThreadNotify) EffectiveChatCallsPerHour() int {
	if t.ChatCallsPerHour > 0 {
		return t.ChatCallsPerHour
	}
	return thread.DefaultChatCallsPerHour
}

// EffectiveChatTokensPerHour resolves the configured chat-layer token
// ceiling, falling back to thread.DefaultChatTokensPerHour when unset (0).
func (t ThreadNotify) EffectiveChatTokensPerHour() int64 {
	if t.ChatTokensPerHour > 0 {
		return t.ChatTokensPerHour
	}
	return thread.DefaultChatTokensPerHour
}

// SlackNotify configures Slack delivery and (for rung-2 actions) interactive
// approve/reject buttons. Delivery uses either an incoming webhook (WebhookURLEnv)
// or a bot token posting to a channel via chat.postMessage (BotTokenEnv+Channel);
// if both are set, the bot token wins.
type SlackNotify struct {
	WebhookURLEnv    string   `yaml:"webhook_url_env"`    // env var holding the incoming-webhook URL
	BotTokenEnv      string   `yaml:"bot_token_env"`      // env var holding a bot token (xoxb-…) for chat.postMessage
	Channel          string   `yaml:"channel"`            // channel ID or name to post to (required with bot_token_env)
	SigningSecretEnv string   `yaml:"signing_secret_env"` // env var with the Slack signing secret (verifies button clicks)
	ApproverIDs      []string `yaml:"approver_ids"`       // Slack user IDs allowed to approve actions (empty = no Slack approvals)
	// FeedbackButtons (opt-in, default off) renders 👍/👎 buttons on investigation
	// messages; clicks are recorded in the outcome ledger and weigh the recalled
	// entry's trust like resolve signals do. Requires exposing POST
	// /slack/interactions to Slack (an interactivity Request URL — the same
	// endpoint approve-mode buttons use), signing_secret_env, outcome.ledger_path
	// AND a delivery target (webhook_url_env, or bot_token_env + channel — buttons
	// render and dispatch on either path); Validate fails loud when any is missing.
	FeedbackButtons bool `yaml:"feedback_buttons"`
	// SilenceButton (opt-in, notify.slack.silence_button) appends a 🔕 menu
	// offering notify.silence.windows. A click suppresses re-investigating the same
	// TriggerKey for the chosen window. Deliberately a SEPARATE flag from
	// feedback_buttons: a rating records an opinion, a silence changes what RunLore
	// does, and an operator must be able to take the first without the second.
	SilenceButton bool `yaml:"silence_button"`
	// ThreadCapture (opt-in, default off) registers each delivered investigation's
	// thread root so a later `@runlore note: …` reply in that thread can be
	// attributed to it and written into the knowledge base. Requires exposing
	// POST /slack/events to Slack (an Events API Request URL subscribed to
	// app_mention), signing_secret_env — mentions arrive on that exposed
	// endpoint and must be signature-verified, same as feedback_buttons above —
	// AND the bot-token path (bot_token_env + channel): a webhook returns no
	// message ts, so there is no thread root to attribute a reply to. Validate
	// fails loud when any is missing.
	ThreadCapture bool `yaml:"thread_capture"`
}

// MatrixNotify configures Matrix delivery.
type MatrixNotify struct {
	Homeserver     string `yaml:"homeserver"`
	RoomID         string `yaml:"room_id"`
	AccessTokenEnv string `yaml:"access_token_env"` // env var holding the access token
	// FeedbackReactions (opt-in, default off) records 👍/👎 reactions on RunLore's
	// investigation messages into the outcome ledger, where they weigh recalled-
	// entry trust exactly like Slack's feedback buttons. Unlike Slack, nothing is
	// exposed: reactions arrive over the client-server /sync long-poll — an
	// OUTBOUND request authenticated by the access token above. Requires the three
	// notifier fields and outcome.ledger_path; Validate fails loud otherwise.
	FeedbackReactions bool `yaml:"feedback_reactions"`
	// SilenceReactions (opt-in, notify.matrix.silence_reactions) records a 🔕
	// reaction as a silence at notify.silence.windows[0]. It does NOT by itself
	// enable the `silence: <duration>` thread command — that additionally needs
	// ThreadCapture on (below), since the command arrives as a message and the
	// /sync filter only requests m.room.message when ThreadCapture is set.
	// Separate from feedback_reactions for the same reason SlackNotify.SilenceButton
	// is separate from FeedbackButtons.
	SilenceReactions bool `yaml:"silence_reactions"`
	// ThreadCapture (opt-in, default off) registers each delivered investigation's
	// thread root so a later `@runlore note: …` reply in that thread can be
	// attributed to it and written into the knowledge base. Like FeedbackReactions,
	// nothing is exposed: mentions arrive over the same client-server /sync
	// long-poll, authenticated by the access token above. Requires the three
	// notifier fields (homeserver, room_id, access_token_env) and
	// outcome.ledger_path — the thread registry lives beside the ledger and must
	// survive a restart and a leader failover; Validate fails loud otherwise.
	ThreadCapture bool `yaml:"thread_capture"`
}

// SilenceNotify configures the human 🔕 silence verdict — the presets offered on
// the Slack 🔕 menu and the hard cap every transport is held to. Shared
// across transports on purpose: a Matrix `silence: 999h` command is free text,
// and must not be able to exceed what the Slack presets offer.
type SilenceNotify struct {
	// Windows are the durations offered, in the order they are rendered. The FIRST
	// entry is the default used where no duration can be carried — notably a Matrix
	// 🔕 reaction, which is a bare emoji.
	//
	// Non-empty always; and 2 to 5 entries whenever notify.slack.silence_button is
	// on. Both bounds began as Slack's — they were the overflow element's limits,
	// and breaching either made Slack reject the WHOLE message (invalid_blocks)
	// rather than just the control. The 🔕 control is a static_select since
	// 2026-08-26 and Slack now accepts either count, so the bounds survive purely
	// as UX: one option is a button wearing a menu, six is a form. Enforced in
	// Validate.
	Windows []Duration `yaml:"windows"`
	// MaxWindow is the hard cap. Zero means uncapped, which Validate rejects
	// whenever a silence transport is on — an uncapped silence is indistinguishable
	// from a permanent one, and nothing else in the system would ever lift it.
	MaxWindow Duration `yaml:"max_window"`
}

// Std returns the presets as standard-library durations, in render order.
func (s SilenceNotify) Std() []time.Duration {
	out := make([]time.Duration, 0, len(s.Windows))
	for _, w := range s.Windows {
		out = append(out, w.Std())
	}
	return out
}

// Default is the window used where none can be carried (a Matrix reaction).
// Zero when no presets are configured — callers treat that as "silencing off".
func (s SilenceNotify) Default() time.Duration {
	if len(s.Windows) == 0 {
		return 0
	}
	return s.Windows[0].Std()
}

// hasDeliveryTarget reports whether a Slack notifier could be built at all: an
// incoming-webhook URL, or a bot token together with a channel. It is the ONE
// definition of "a Slack delivery target", shared by every control that only
// exists on a message the notifier delivered (feedback buttons, the silence
// menu). Kept as a method rather than repeated inline so a new delivery path is
// added in one place — two copies of this predicate drifted apart is exactly
// the failure it exists to prevent.
//
// It answers the STATIC question only. Runtime emptiness (an env var set but
// unmounted) is not knowable here; app.SlackFeedbackDeliverable warns for that.
func (s SlackNotify) hasDeliveryTarget() bool {
	return s.WebhookURLEnv != "" || (s.BotTokenEnv != "" && s.Channel != "")
}

// hasListenerFields reports whether the Matrix /sync listener could run at all.
// Every Matrix capability that long-polls the room needs the same three fields,
// so they are checked through one predicate rather than three inline copies.
func (m MatrixNotify) hasListenerFields() bool {
	return m.Homeserver != "" && m.RoomID != "" && m.AccessTokenEnv != ""
}

// SilenceEnabled reports whether ANY transport can record a silence. It is the
// condition the investigation gate must be constructed on: without it, a
// deployment with silencing on but no recurrence cooldown would record every
// click durably and then ignore it, while the UI confirmed success.
func (n Notify) SilenceEnabled() bool {
	return n.Slack.SilenceButton || n.Matrix.SilenceReactions
}

// FeedbackEnabled reports whether ANY transport offers a 👍/👎 control. It is a
// DEPLOYMENT-WIDE fact rather than a per-transport one on purpose: votes and
// silences share one outcome ledger and are keyed by TriggerKey alone, so a 👎
// cast in Matrix re-arms a silence clicked in Slack.
//
// It exists because one of the four documented escapes from a 🔕 — "a standing
// 👎 re-arms it" — simply does not exist when no such control is enabled, and
// the deployment where that is true is the one the Slack docs RECOMMEND
// (feedback_buttons off, silence_button on). Read by the silence acknowledgement
// so it never promises an escape this deployment lacks, and by the startup
// warning that tells an operator the bound is narrower than four.
func (n Notify) FeedbackEnabled() bool {
	return n.Slack.FeedbackButtons || n.Matrix.FeedbackReactions
}
