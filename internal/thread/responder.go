// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
	"github.com/Smana/runlore/internal/redact"
	"github.com/Smana/runlore/internal/telemetry"
)

// Forge is the write surface a thread note needs. It is providers.CurationForge
// plus two calls the curator has no use for — narrowed and restated here so the
// responder declares exactly the calls it makes and can be faked in one struct.
type Forge interface {
	CommentOnPR(ctx context.Context, number int, body string) error
	OpenPR(ctx context.Context, e providers.KBEntry) (providers.Ref, error)
	// IsPROpen reports whether the pull/merge request numbered `number` is
	// still open. write() calls this before recording a note on a linked PR: a
	// comment on a MERGED pull request is never indexed by the catalog, and a
	// commit on a merged PR's branch never reaches the base branch either, so
	// writing there would silently lose the human's knowledge while telling
	// them it was saved. github.Client and gitlab.Client both implement it.
	IsPROpen(ctx context.Context, number int) (bool, error)
	// AppendToEntryOnPR appends body to the knowledge-base ENTRY FILE the pull/
	// merge request numbered `number` carries, as a further commit on that pull
	// request's own branch. It never merges and never touches the base branch:
	// the pull request stays the proposal and a human stays the gate.
	//
	// It is on this interface — rather than left to CommentOnPR, which the
	// responder already has — because a comment and an entry are not two ways of
	// writing the same thing. Only the entry is what the catalog gains when the
	// pull request merges, so on the ONE pull request whose entry is RunLore's
	// own note (Context.NoteURL), every note after the first has to go here or
	// it never becomes knowledge at all. See write() for which pull request gets
	// which treatment, and why the curator's PR deliberately still gets a
	// comment.
	//
	// The forge locates the entry file from the pull request itself, so no path
	// is persisted here to go stale and misroute a commit later.
	//
	// key identifies this note so the write is IDEMPOTENT: an entry already
	// carrying that key is left alone and success is reported. Unlike a comment,
	// which is visibly duplicated conversation that dies at merge, a duplicated
	// append is permanent catalog content that recall then serves twice — and the
	// deliveries above this layer really do replay (see noteKey). The key is
	// passed rather than derived from body inside the forge because body carries
	// the provenance TIMESTAMP, so a retry renders different bytes; only this
	// layer knows which inputs are stable.
	//
	// It may report providers.ErrPRNotOpen, which write() treats as the same case
	// its own open-check does rather than as a failure to degrade around:
	// commenting on a finished pull request is the silent loss that check exists
	// to prevent.
	AppendToEntryOnPR(ctx context.Context, number int, body, key string) error
}

// DefaultMaxNotesPerThread bounds how many knowledge writes one thread can make.
const DefaultMaxNotesPerThread = 20

// DefaultForgeWritesPerHour bounds Responder.ForgeWrites — every forge write
// thread capture makes, globally, per hour — when notify.thread.
// forge_writes_per_hour is unset. Same value the hardcoded ratelimit.New(20,
// time.Hour) used before notify.thread existed.
const DefaultForgeWritesPerHour = 20

// ReinvestigateNotSupportedReply is what Handle answers a reserved
// "reinvestigate:" command with. Exported so any other caller that must
// answer the identical reserved command elsewhere posts identical text
// rather than a second literal that could drift from this one.
//
// The wording is deliberate on three points a reserved-command reply must
// make, all in one message, because the human is not looking at the code
// that decided this: (1) a re-run is not supported from a thread, plainly;
// (2) NOTHING was recorded — the human must never be left believing either
// that a re-run started or that their words were saved, which is the actual
// harm this whole grammar fix exists to close, worse than either outcome
// alone; (3) how to proceed either way — rephrase without the reserved word
// to record a note, or use the real `reinvestigate` label to actually
// re-run. Spelling out "without `reinvestigate:`" here avoids telling the
// human to retry something that will fail again the same way, since Parse
// now refuses that token anywhere in the message, not only at position 0.
const ReinvestigateNotSupportedReply = "Re-running an investigation from a thread is not supported yet, " +
	"and nothing was recorded. To save this as a note, rephrase without `reinvestigate:` and use `note:`. " +
	"To actually re-run, add the `reinvestigate` label to the knowledge-base issue."

// SilenceNotAnchoredReply is what Handle answers when "silence:" appeared
// mid-sentence rather than at the start of the message.
//
// It makes the same three points ReinvestigateNotSupportedReply does, for the
// same reason: (1) the incident was NOT silenced; (2) nothing was recorded
// either, so the human is never left believing their note was filed; (3) how
// to get either outcome they might have meant. The third point matters most
// here, because both readings are plausible — "note: we agreed on silence: 4h"
// is one message that could sincerely mean either — and only the human can say
// which.
//
// A refusal is the right side to err on because the two failures are not
// symmetric. Refusing costs one rephrased message. Acting wrote a durable
// ledger event, acked a suppression the human never asked for, discarded the
// note they did ask for, and left RunLore silent on the incident for the whole
// window — none of which the reply they are reading can undo.
const SilenceNotAnchoredReply = "I didn't silence anything, and I didn't record that. " +
	"`silence:` only counts as a command when it starts your message, and here it appeared mid-sentence — " +
	"so I can't tell whether you meant to mute this incident or were just writing about it. " +
	"To silence, send `silence: 4h` on its own. To save the sentence, rephrase it without `silence:` and use `note:`."

// FreeformNotRecordedReply is what Handle answers an addressed message with no
// recognised prefix (IntentFreeform) — a question, or any other prose with no
// explicit "note:" — whenever nothing was written.
//
// It is the answer in four cases: no Chat is wired (model.chat absent, which
// is the default), the message was a bare mention with nothing in it to
// answer, the model declined to propose a note, or Answer reported a failure
// of any kind. Those are every path on which the knowledge base is untouched,
// and the reply must be identical across them: a human whose words were not
// saved must never have to work out WHY they were not saved before they know
// THAT they were not.
//
// The invariant this comment used to state — "freeform text is never written
// to the knowledge base" — no longer holds, and saying so plainly matters more
// than keeping the sentence. With model.chat configured, freeform text CAN
// produce a knowledge-base write: the model reads the message and proposes
// note content, which record() files. What the original security finding
// actually objected to was the SILENCE, not the write — an on-call typing
// "anyone checked what runlore said about the CNI?" got a KB PR opened with no
// signal they had recorded anything. That is still closed, by three things
// that hold on every path: the reply always names the write and where it
// landed (see write's return values), the note is filed as model-DRAFTED
// rather than as the human's words (see ProposedNote), and the whole route is
// off unless an operator configured model.chat.
//
// The wording makes the same two points ReinvestigateNotSupportedReply does,
// for the identical reason: (1) plainly, NOTHING was recorded — the human
// must never be left believing their words were saved when they were not,
// which is worse than a refusal they can act on; (2) exactly how to record it
// instead. It does not scold: addressing the bot with a plain sentence is a
// reasonable thing to do, not a mistake.
const FreeformNotRecordedReply = "I didn't record that — nothing was saved. To save this as a note, " +
	"reply with `note: <text>`, for example: `note: the real cause was a spot-node reclaim`."

// Responder turns an addressed thread message into a knowledge-base write and
// returns the text to post back. It is transport-agnostic: every chat-system
// concern lives in the adapter that calls it.
type Responder struct {
	Forge    Forge
	Registry *Registry
	// MaxNotesPerThread caps knowledge writes per thread; <= 0 means
	// DefaultMaxNotesPerThread. A separate, narrower control than ForgeWrites:
	// it bounds one thread's share, not the total.
	MaxNotesPerThread int
	// ForgeWrites caps every forge write this responder makes — a CommentOnPR
	// exactly as much as an OpenPR — globally, per hour. It is checked once, in
	// write(), upstream of which route gets chosen, so neither route can spend
	// a budget the other does not draw from: a chatty channel must not become a
	// forge incident regardless of which route its notes happen to take. The
	// token spend a model now makes ahead of this check is bounded separately,
	// by Chat.Budget — see Chat below. nil means unlimited.
	//
	// It bounds writes that LANDED. The check reserves a token before the forge
	// call, because it has to, and every path that then returns without a write
	// refunds it — so this ceiling and the per-thread cap agree that a failure
	// costs nothing, which is the whole reason a failed write must not be able to
	// throttle the next healthy one.
	ForgeWrites *ratelimit.Window
	// MaxNoteBytes caps one human message, in bytes, before it is written to
	// the knowledge base — see NoteBody / DefaultMaxNoteBytes for why. <= 0
	// means DefaultMaxNoteBytes, mirroring MaxNotesPerThread's own <= 0
	// convention above: never "unlimited", since an unbounded note is the
	// exact gap this field exists to close.
	MaxNoteBytes int
	// ForgeRepo names the ONE repository this responder writes to, as host and
	// path together — "github.com/acme/kb", "ghe.example.com/acme/kb",
	// "gitlab.example.com/group/sub/proj". When set, write() refuses to take a
	// routing decision from a PR/MR URL that does not name exactly that
	// repository.
	//
	// It is host AND path, in one field, because anchoring on the host alone
	// bought almost nothing: PRNumber matches "/pull/<n>" anywhere in a URL, and
	// on github.com anybody owns a repository, so https://github.com/attacker/x/
	// pull/9999 passed a host check and yielded CommentOnPR(9999) — against the
	// CONFIGURED repo, which is the only repo the forge client knows how to
	// address. The number, not the URL, is what selects the pull request the
	// human's note lands on, so the number has to come from a URL naming the
	// repository that numbering belongs to. Two separate fields would let an
	// operator (or a future caller) set one and not the other and get exactly
	// the weak check back; one field cannot be half-configured.
	//
	// Empty means unanchored, exactly as it behaved before this field existed.
	// It is defence in depth rather than the primary control: the only untrusted
	// source of a CuratedURL today is a Matrix thread stamp, and
	// MatrixFeedback.contextFor already discards any stamp whose sender is not
	// the bot itself. This field exists so the defence does not depend on that
	// one control, one layer up, staying correct forever.
	ForgeRepo string
	Now       func() time.Time
	Log       *slog.Logger
	// Metrics is optional and nil-safe throughout — the same contract every
	// other *telemetry.Metrics field in RunLore follows: nil means telemetry is
	// not configured, and every call site guards it before use. Powers
	// ThreadWritesThrottled and ThreadNotesWritten (see
	// internal/telemetry/metrics.go), the counters that make this feature's
	// throttling and write volume visible to an operator instead of only ever
	// showing up in logs.
	Metrics *telemetry.Metrics
	// Chat answers a freeform message — one with no recognised command prefix —
	// with a model call. nil means the layer is off and freeform behaves exactly
	// as it does without a model configured: FreeformNotRecordedReply, and
	// nothing written. It is a strictly additive route; see freeform.
	//
	// It bounds a DIFFERENT resource than ForgeWrites above, which is why both
	// exist: Chat.Budget bounds tokens, ForgeWrites bounds forge writes. A
	// chatty channel that never produces a note spends no forge budget at all
	// and must still be bounded on tokens; a channel whose every message
	// proposes a note spends both.
	Chat *Chat
	// Announcer broadcasts a landed knowledge write to every configured
	// notifier, so a KB update reaches people who were not reading the thread
	// it came from. nil means announcements are off — the default, and the
	// same nil-safe contract Metrics, Log and Chat above follow.
	//
	// It is a distinct MESSAGE, not a second copy of the thread reply: the reply
	// acknowledges the person who typed, and this names who wrote the note and
	// which chat system it came from, for readers who have neither. Where it
	// goes — each notifier's own channel, or back into the originating thread —
	// is the announcer's own setting; see KBAnnouncer and providers.KBDelivery.
	Announcer *KBAnnouncer
	// Silence records a `silence: <duration>` command; nil when silencing is not
	// enabled, in which case the command is answered with a short explanation
	// rather than silently ignored. SilenceMax is notify.silence.max_window,
	// carried only so the reply can state the bound — the LEDGER enforces it, and
	// remains the single place that does.
	Silence    SilenceRecorder
	SilenceMax time.Duration
	// FeedbackEnabled reports whether ANY enabled transport offers a 👍/👎 control
	// (notify.slack.feedback_buttons or notify.matrix.feedback_reactions). It is a
	// deployment-wide fact, not a per-transport one — this Responder is SHARED
	// across Slack's thread capture and Matrix's, and the votes and silences share
	// one ledger, so a 👎 cast in Matrix re-arms a silence recorded in Slack.
	//
	// It exists solely so the acknowledgement does not promise an escape hatch the
	// deployment does not have; see SilenceAck. False (the zero value) is the safe
	// default: it under-promises rather than over-promises.
	FeedbackEnabled bool
}

// SilenceRecorder records a human 🔕 silence (implemented by *outcome.Ledger).
// Declared here rather than imported because internal/thread cannot depend on
// internal/notify; each package declaring the narrow interface it consumes is
// the idiom this codebase already follows for feedback.
type SilenceRecorder interface {
	Silence(triggerKey string, window time.Duration, user string, at time.Time) error
}

// Announcement bounds. Both are properties of the delivery, not of the write:
// a KBUpdate is a small struct handed to notifiers that each make one chat or
// HTTP round-trip, so the pool is narrow and the deadline is a network deadline.
const (
	// announceSlots bounds concurrent deliveries. Deliberately much smaller
	// than the mention pools (16, see internal/server.maxConcurrentMentions):
	// those hold a forge round-trip that a human is waiting on, and this holds
	// an announcement nobody is. It is a concurrency bound and back-pressure,
	// NOT the rate ceiling — see KBAnnouncer for where that comes from.
	announceSlots = 4
	// announceTimeout bounds one fan-out. notify.Multi delivers to every
	// configured sink in sequence, each on its own HTTP client timeout, so this
	// is sized for a handful of degraded sinks rather than for one call. Past
	// it the delivery is abandoned: the entry is already on the forge and the
	// human already has their reply, so a wedged sink must release its slot
	// rather than hold it against the next write.
	announceTimeout = 2 * time.Minute
)

// KBAnnouncer delivers KBUpdate announcements for writes that ALREADY LANDED
// on the forge.
//
// # Detached, deliberately
//
// Delivery runs on a bounded Dispatcher rather than inline on the reply path,
// and the reason is the "blocks" half of best-effort rather than the "errors"
// half. Swallowing an error would be enough to keep a failing sink from
// changing what the human is told; it would NOT keep a slow one from changing
// when they are told it. A sink here is notify.Multi fanning out to Slack,
// Matrix and any webhook in sequence, each on its own HTTP timeout — inline,
// that latency lands squarely between the forge accepting the note and the
// human learning it was accepted, on a path whose whole purpose is that the
// human is never left in silence about a write that happened. Worse, the
// mention handler that called this is itself already detached under a deadline
// (see internal/server.mentionHandlerTimeout), so a slow fan-out does not just
// delay the reply, it can consume the budget the reply needed and lose it.
//
// The Dispatcher, rather than a bare goroutine, is what makes detached
// accountable: a fixed slot count, a per-delivery deadline, panic recovery, and
// a Drain so shutdown waits for what is in flight instead of dropping it. It is
// the pattern this package already uses for exactly this shape of work.
//
// # Rate, and why there is no second ceiling
//
// Verified against the code rather than assumed. Responder.write checks
// ForgeWrites ONCE at its top, upstream of both routes; the only two *KBWrite
// values it ever returns are constructed after that check; record() is its only
// caller and announces only when that pointer is non-nil. So announcements are
// 1:1 with LANDED forge writes and inherit their ceiling — 20/hour by default —
// with no path that can announce without passing it.
//
// The inheritance is exact, including its limit: with ForgeWrites nil
// (unlimited) announcements are unbounded too, because forge writes are. That
// is the right coupling rather than a gap. A second, tighter ceiling here would
// drop announcements for writes that DID happen, which is precisely the
// "an operation that did not happen reporting as though it did" family this
// feature exists to close, reflected: an operation that DID happen going
// unreported. What a burst does hit instead is announceSlots, which refuses and
// logs rather than queueing without bound.
//
// # Payload
//
// The event carries the note at the length it was WRITTEN (up to MaxNoteBytes),
// not the 512-byte thread preview: a webhook sink wants the record, and only a
// chat sink needs a chat-sized quote. Bounding what is RENDERED is the
// transport's job, where the transport's own limit lives — the same division
// taken for the outgoing thread reply.
type KBAnnouncer struct {
	sink     providers.KBUpdateNotifier
	delivery providers.KBDelivery
	disp     *Dispatcher
	log      *slog.Logger
}

// NewKBAnnouncer returns an announcer delivering to sink, or nil when there is
// no sink.
//
// A nil sink yielding a nil announcer is what keeps "announcements are off" a
// single value: there is no state in which a Responder holds an announcer with
// nowhere to deliver, and no state in which it holds a sink with no bound
// around it. Every method below is nil-safe, so a caller never needs its own
// check before delivering or draining.
//
// delivery is where announcements go (providers.KBDelivery), held here rather
// than passed per write because it is one deployment-wide setting and an
// announcer that could route two writes from the same deployment differently
// would be a setting nobody configured. Its zero value is the channel, so an
// announcer built without a decision behaves as every announcer did before
// routing existed.
func NewKBAnnouncer(sink providers.KBUpdateNotifier, delivery providers.KBDelivery, log *slog.Logger) *KBAnnouncer {
	if sink == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &KBAnnouncer{sink: sink, delivery: delivery, disp: NewDispatcher(announceSlots, announceTimeout, log), log: log}
}

// deliver schedules one announcement and returns immediately. Every failure is
// a log line and nothing more: the write it describes is already on the forge,
// the human has already been told, and there is nothing here to roll back.
//
// The destination is stamped here, on the announcer's own copy of the event,
// rather than by the caller that describes the write: announce() knows what
// landed and where it came from, and this knows where the operator asked for it
// to go. That keeps a caller from being able to compose a KBUpdate that routes
// somewhere the configuration never selected.
func (a *KBAnnouncer) deliver(ctx context.Context, up providers.KBUpdate) {
	if a == nil {
		return
	}
	up.Delivery = a.delivery
	if !a.disp.Go(ctx, func(ctx context.Context) {
		if err := a.sink.DeliverKBUpdate(ctx, up); err != nil {
			a.log.Warn("thread: knowledge-base announcement failed (best-effort)", "url", up.URL, "route", string(up.Route), "err", err)
		}
	}) {
		// Refused, not queued: the slots are the back-pressure. Warn rather than
		// Info because a saturated pool means announcements are being dropped for
		// writes that really happened, which an operator must be able to see.
		a.log.Warn("thread: announcement pool saturated; a knowledge-base write was not announced",
			"url", up.URL, "route", string(up.Route), "root", up.Root)
	}
}

// Drain waits for in-flight announcements, bounded by ctx, so shutdown does not
// silently drop one for a write that already reached the forge.
func (a *KBAnnouncer) Drain(ctx context.Context) {
	if a == nil {
		return
	}
	a.disp.Drain(ctx)
}

// kbRoute maps this package's route names onto the providers vocabulary the
// event uses. The two spell the routes identically today, and this does not
// rely on that: a providers.KBRoute(w.Route) conversion would keep compiling —
// and start announcing a route name no consumer recognises — the moment either
// side is renamed.
//
// write() sets Route from one of the constants literally, so the default is
// unreachable. It passes the value through rather than guessing at one of the
// known routes, because an announcement describes a write that already landed
// and mislabelling it is worse than reporting an unknown label honestly.
func kbRoute(route string) providers.KBRoute {
	switch route {
	case RouteComment:
		return providers.KBRouteComment
	case RouteOpenPR:
		return providers.KBRouteOpenPR
	case RouteAppend:
		return providers.KBRouteAppend
	default:
		return providers.KBRoute(route)
	}
}

// announce broadcasts a landed write. A nil w is the whole guard against
// announcing something that did not happen: write() returns one only when the
// forge accepted the write (see KBWrite), so there is nothing to announce on a
// throttle, a forge failure, or a note the model never proposed.
//
// n supplies the author AND the provenance, neither of which is on the KBWrite:
// the write is the same write either way, and only the caller knows whose
// message produced it and whether they wrote the words. The author goes through
// noteField — redacted and flattened, exactly as the entry itself renders it —
// because this event is a NEW egress for transport-reported text; the provenance
// is RunLore's own boolean and needs neither.
//
// Carrying the provenance is not optional decoration. Without it the event said
// {author: "alice", note: "<the model's text>"} and every sink rendered it as
// alice's own words — the same claim openedWith exists to keep out of the thread
// reply, and NoteBody's "@alice did not write it" out of the entry, arriving
// through the one surface that had no such guard.
//
// tc supplies BOTH thread handles, Root and Channel. Root alone names a thread
// only to the transport that already knows which channel it is in; a sink asked
// to reply into it needs the channel too, and one arriving without the other is
// what makes a delivery fall back to the channel (see providers.KBDelivery).
// Neither is escaped here — they are marked untrusted on the event, and the
// transport that renders them is the one that knows how.
func (r *Responder) announce(ctx context.Context, tc Context, n Note, w *KBWrite, at time.Time) {
	if r.Announcer == nil || w == nil {
		return
	}
	r.Announcer.deliver(ctx, r.kbUpdateFor(tc, n, w, at))
}

// kbUpdateFor composes the event announce delivers. Split out from announce so
// the composition can be exercised without a sink and a dispatcher — the two
// things that made the missing provenance field hard to see, since every
// announcement test asserted on what a sink RECEIVED and none of them had a
// reason to compare two notes that differed only in who wrote them.
//
// It is the ONE place a KBUpdate is built from a write, which is what lets
// TestKBUpdateCarriesEveryNoteFact assert a property over the whole mapping
// rather than over one fixture.
func (r *Responder) kbUpdateFor(tc Context, n Note, w *KBWrite, at time.Time) providers.KBUpdate {
	return providers.KBUpdate{
		Transport: tc.Transport,
		Root:      tc.Root,
		Channel:   tc.Channel,
		Route:     kbRoute(w.Route),
		PR:        w.PR,
		URL:       w.URL,
		Title:     w.Title,
		Author:    noteField(n.Author),
		// Author names whose MESSAGE produced the note; this names whether they
		// wrote its words. Read from the note's own provenance rather than from a
		// parallel flag, for the reason Note.DraftedFrom is one field: two values
		// that can disagree let model prose ship under a human's name through
		// nothing worse than a forgotten assignment.
		ModelDrafted: n.modelDrafted(),
		Note:         w.Note,
		At:           at,
	}
}

// The routes a knowledge write can take, named once. They are both the
// KBWrite.Route values a caller reads and the "route" attribute
// ThreadNotesWritten is labelled with, deliberately the same strings: an
// operator correlating a dashboard series with what a thread reported must not
// have to know that two literals happened to agree.
const (
	// RouteComment: the note was added as a comment on the CURATED pull request
	// this thread came from — a draft somebody else wrote, on which the note is
	// review feedback for a human to reconcile at merge time.
	RouteComment = "comment"
	// RouteOpenPR: the note had no open pull request to land on, so it opened a
	// standalone Concept entry of its own (see ConceptEntry).
	RouteOpenPR = "open_pr"
	// RouteAppend: the note was appended to the entry file of the standalone PR
	// an EARLIER note in this same thread opened — so the entry the catalog
	// gains on merge carries every note in the thread, not only the first.
	//
	// It is separated from RouteComment on the metric for the reason the bug it
	// closes was invisible for as long as it was: with both routes labelled
	// "comment", nothing an operator could graph distinguished a note that
	// becomes knowledge from one that is discarded when its pull request merges.
	RouteAppend = "append"
)

// KBWrite describes a knowledge-base write that LANDED. write returns one only
// when the forge accepted the write; every other outcome — a throttle, a forge
// failure, an unreachable forge — returns a nil *KBWrite instead.
//
// That it is a pointer is the point. The shape it replaced was
// (msg string, landed bool, err error), which let a caller read landed and
// ignore err, and let a zero-valued description sit next to a false: two values
// that can disagree about whether anything happened. A nil pointer cannot —
// there is no result to read unless there was a write to describe. This package
// has shipped the disagreeing-pair bug three times (GetOrCreate reporting
// success it could not record, the duplicate-PR race, the stale sweep), which
// is why the shape is worth the pointer.
type KBWrite struct {
	// Route is RouteComment, RouteAppend or RouteOpenPR.
	Route string
	// PR is the pull/merge request the note landed on or opened. Zero — and only
	// zero — when the forge returned a URL carrying no parseable number, which
	// is not an error: the write landed and URL still names it.
	PR int
	// URL is the pull request the human can open to read the note.
	URL string
	// Title is the entry title ConceptEntry generated, on RouteOpenPR only. It
	// is empty on RouteComment and RouteAppend, where the note joins an entry
	// that already has a title — someone else's on the comment route, and one an
	// earlier note in this same thread was already told about on the append
	// route — so RunLore generated no title for THIS write.
	//
	// UNTRUSTED: it is built from the thread's alert-derived title. Redacted and
	// flattened (see noteField), but not authored by RunLore.
	Title string
	// Note is the note text AS WRITTEN — redacted and capped, the same single
	// evaluation the body sent to the forge was rendered from (see
	// noteAsWritten). Never the caller's raw input: a caller quoting this back
	// into a chat message must not be able to unmask what the entry masked, nor
	// republish the bytes the cap dropped.
	//
	// What it does NOT carry is the forge-Markdown defusal noteText applies on
	// top (neutralizeImages, neutralizeHTML, escapeOKFSections). Those exist for
	// GitHub's and GitLab's renderers; a chat transport is neither, and its own
	// markup hazards are neutralised by Untrusted at the point of rendering.
	//
	// UNTRUSTED, and the most so of these fields: on the freeform route RunLore's
	// own chat model wrote it, and on the "note:" route a human did.
	Note string
}

// notePreviewBytes bounds the note text a reply quotes back into the thread
// (see recordedBlock). It is a SECOND ceiling, deliberately independent of
// MaxNoteBytes, because the two bound different things.
//
// MaxNoteBytes bounds what is WRITTEN to the knowledge base: 8 KiB by default,
// and an operator may raise it — config.Validate rejects only a negative one.
// This bounds what is SAID in a chat message. Sharing MaxNoteBytes' number
// would make a note at the default cap into an 8 KiB chat post, in a thread, on
// a path any channel member can trigger; the reply is an acknowledgement, and
// the URL on the line above is what carries the full text.
//
// 512 bytes, derived rather than picked. Every transport escapes the untrusted
// spans of a reply before posting (see RenderReply), and the widest of those
// escapes — Slack's "&" to "&amp;" — expands 5x, so this preview reaches the
// wire as at most ~2.5k characters. That leaves room inside Slack's ~4k message
// text budget for the model's own answer, which shares the SAME message on the
// freeform route (see freeform). It is also several times longer than the one
// or two sentences a real note is, so a truncated preview is the exception
// rather than the norm.
const notePreviewBytes = 512

// prNumberRe matches the numeric id in a GitHub pull URL or a GitLab merge-request
// URL. Anchored on the path segment so an issue URL never matches.
var prNumberRe = regexp.MustCompile(`/(?:pull|merge_requests)/(\d+)`)

// PRNumber extracts the pull-request / merge-request number from a forge URL.
// providers.Ref carries only a URL, so the number the comment API needs is
// recovered here rather than widening the Ref contract.
//
// It is UNANCHORED — it does not care which forge, or which repository, the
// URL names — and must only be used on a URL RunLore's own forge client just
// returned. Anything that decides WHERE a note is written has to go through
// Responder.prNumberOn instead, which anchors the same parse to the configured
// repository — see Responder.ForgeRepo for what that closes.
func PRNumber(rawURL string) (int, bool) {
	m := prNumberRe.FindStringSubmatch(rawURL)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// prNumberOn is PRNumber anchored to r.ForgeRepo: the number is only returned
// when rawURL names a pull/merge request IN the one repository RunLore writes
// to. Every routing decision in write() goes through this rather than through
// PRNumber, because the number alone says nothing about where it came from —
// and the number is what selects the pull request the note lands on.
//
// What is actually enforced, when ForgeRepo is set: the URL's host equals the
// configured host, the path begins with the configured repository path, and
// the pull/merge-request segment is the one IMMEDIATELY after that path. So
// neither another host, nor another repository on the same host, nor a
// "/pull/<n>" buried somewhere else in the path can supply the number.
//
// PRNumber is still called first, and only as a gate: it decides whether
// rawURL is a pull-request URL at all, so an empty or unrelated candidate (the
// common case — most thread contexts carry neither a CuratedURL nor a NoteURL)
// returns quietly instead of logging a refusal on every write.
//
// A refusal is not a failure: write() simply moves on to the next candidate
// URL and, finding none, opens a standalone note PR. The human's knowledge is
// never dropped for a URL this rejects. It is logged all the same — an
// operator whose ForgeRepo is mis-derived would otherwise see notes quietly
// stop landing on the curated PR with nothing saying why.
func (r *Responder) prNumberOn(rawURL string) (int, bool) {
	n, ok := PRNumber(rawURL)
	if !ok || r.ForgeRepo == "" {
		return n, ok
	}
	n, ok = prNumberInRepo(rawURL, r.ForgeRepo)
	if !ok {
		r.log().Warn("thread: ignoring a pull-request URL that is not on the configured knowledge-base repository",
			"url", rawURL, "forge_repo", r.ForgeRepo)
		return 0, false
	}
	return n, true
}

// repoPRNumberRe matches the pull-request / merge-request number in the path
// REMAINDER that follows a repository path: "/pull/7" (GitHub), and both
// "/-/merge_requests/9" and the older "/merge_requests/9" (GitLab). It stays
// shape-agnostic across the two forges for the same reason prNumberRe does:
// the responder is handed one forge client and never learns which.
//
// The start anchor is what makes prNumberInRepo need no separate boundary
// check of its own: the number can only come from the segment right after the
// repository path, never from a "/pull/<n>" buried deeper in it, and a sibling
// repository whose name merely BEGINS with the configured one ("acme/kb-
// staging" against "acme/kb") leaves "-staging/pull/9", which no alternation
// here matches. The leading "/" states that same segment boundary a second
// time; it is redundant against the alternation rather than independently
// load-bearing, and is kept because a reader should not have to derive the
// boundary from what "pull" happens not to start with.
var repoPRNumberRe = regexp.MustCompile(`^/(?:-/)?(?:pull|merge_requests)/(\d+)`)

// prNumberInRepo extracts the pull/merge-request number from rawURL only when
// rawURL names that request inside repo, given as "host/path" (see
// Responder.ForgeRepo). Reported false whenever anything about that does not
// line up — a malformed repo, an unparseable URL, a different host, a
// different repository, or a path that does not continue into a
// pull/merge-request segment.
func prNumberInRepo(rawURL, repo string) (int, bool) {
	host, repoPath, ok := strings.Cut(strings.Trim(repo, "/"), "/")
	if !ok || host == "" || repoPath == "" {
		return 0, false
	}
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Hostname(), host) {
		return 0, false
	}
	// The two slices compared are the same LENGTH, so a repo path holding
	// non-ASCII cannot desynchronise the comparison the way scanning a
	// separately case-folded copy could (see commandTokenIndex for the same
	// hazard stated in full): a cut that lands mid-rune simply fails to match.
	prefix := "/" + repoPath
	if len(u.Path) < len(prefix) || !strings.EqualFold(u.Path[:len(prefix)], prefix) {
		return 0, false
	}
	m := repoPRNumberRe.FindStringSubmatch(u.Path[len(prefix):])
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func (r *Responder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// log returns r.Log, defaulting to slog.Default() so a zero-value Responder
// (or one built without wiring a logger explicitly) never panics on a log
// call.
func (r *Responder) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// recordWrite increments ThreadNotesWritten for a write that just landed,
// labelled by which route landed it ("comment" or "open_pr"). Nil-safe: a
// no-op whenever Metrics is not wired, exactly like every other optional
// *telemetry.Metrics use in RunLore.
func (r *Responder) recordWrite(ctx context.Context, route string) {
	if r.Metrics == nil {
		return
	}
	r.Metrics.ThreadNotesWritten.Add(ctx, 1, metric.WithAttributes(attribute.String("route", route)))
}

func (r *Responder) maxNotes() int {
	if r.MaxNotesPerThread <= 0 {
		return DefaultMaxNotesPerThread
	}
	return r.MaxNotesPerThread
}

// maxNoteBytes resolves the effective per-note byte cap, falling back to
// DefaultMaxNoteBytes exactly like maxNotes falls back to
// DefaultMaxNotesPerThread. Passed into both NoteBody (the comment-on-PR
// route) and ConceptEntry (the standalone-PR route) below, so a single
// resolved value governs whichever route a given write takes.
func (r *Responder) maxNoteBytes() int {
	if r.MaxNoteBytes <= 0 {
		return DefaultMaxNoteBytes
	}
	return r.MaxNoteBytes
}

// noteAsWritten evaluates the redact-then-cap half of the note pipeline ONCE
// and returns both things that one evaluation is for: the note to hand to the
// forge, its Text replaced by the result, and that same string to report as
// KBWrite.Note.
//
// One evaluation, two consumers, because two evaluations drift. The reply a
// human reads and the entry a reviewer reads have to describe the same bytes:
// a reply built from a separately-derived string would keep agreeing with the
// entry only for as long as nobody edits one pipeline without the other, and
// the first thing to break that way is redaction — the reply would quote the
// secret the entry masked.
//
// The forge body is therefore a FUNCTION of the reported string rather than a
// sibling of it: NoteBody renders what this returned. Passing already-redacted,
// already-capped text back through noteText cannot change it — redact.Secrets
// is idempotent over already-masked text (pinned in internal/redact's own
// tests, over its whole secret manifest), and capNoteText returns at most
// maxBytes bytes, so a second cap at the same bound is a no-op. What noteText
// still adds on top is the forge-Markdown defusal, which is deliberately NOT in
// the reported string — see KBWrite.Note.
//
// Note is a value type, so the caller's own note is untouched.
func (r *Responder) noteAsWritten(n Note) (Note, string) {
	n.Text = capNoteText(redact.Secrets(n.Text), r.maxNoteBytes())
	return n, n.Text
}

// Handle parses raw, writes the knowledge where it belongs, and returns the
// reply to post in the thread. The reply is returned even alongside an error —
// the human must always learn what happened to their words.
func (r *Responder) Handle(ctx context.Context, tc Context, author, raw string) (string, error) {
	p := Parse(raw)

	switch p.Intent {
	case IntentReinvestigate:
		return ReinvestigateNotSupportedReply, nil
	case IntentSilence:
		// The same guard IntentNote applies below, and for a STRONGER reason. Parse
		// matches "silence:" as a whole token anywhere in the message and scans it
		// ahead of "note:", so "note: we agreed on silence: 4h" lands here with Text
		// "4h". Parse justifies the anywhere-match with "the outcome is a refusal
		// that writes nothing and spends nothing" — true for reinvestigate:, false
		// for this one: r.silence writes a ledger event and switches investigation
		// off. Unguarded, a sentence meant as prose silenced the incident for four
		// hours AND discarded the note the human was actually writing.
		//
		// Refused rather than routed to freeform, and unconditionally rather than
		// only with no Chat wired: freeform with a Chat can itself write a
		// model-drafted note, and a write is not a refusal — the human reading the
		// reply cannot undo it. A refusal comes back with wording they can act on.
		if !p.Anchored {
			return SilenceNotAnchoredReply, nil
		}
		return r.silence(tc, author, p.Text)
	case IntentFreeform:
		return r.freeform(ctx, tc, author, p.Text)
	case IntentNote:
		// Parse matches "note:" as a whole token anywhere in the message, and
		// the argument for that is a COST one that only holds with a chat layer
		// wired: there, a mid-sentence "note:" would otherwise reach the model
		// and be billed. With no chat layer there is nothing to save and a real
		// write to lose — "the runbook note: link is stale" would open a
		// knowledge-base PR containing "link is stale" — so an unanchored match
		// is prose, exactly as it was before the chat layer existed. See Parse.
		//
		// Routed through freeform rather than answering FreeformNotRecordedReply
		// directly so there is one place that decides what an unrecognised
		// message gets; the branch is only reachable with a nil Chat, on which
		// freeform ignores the text it is handed and writes nothing.
		if !p.Anchored && r.Chat == nil {
			return r.freeform(ctx, tc, author, p.Text)
		}
	}

	if p.Text == "" {
		return "Tell me what to record — for example: `note: the real cause was a spot-node reclaim`.", nil
	}
	return r.record(ctx, tc, HumanNote(author, p.Text))
}

// silence answers a `silence: <duration>` command. Every failure path REPLIES —
// a command that changes behaviour must never fail quietly, or the human walks
// away believing the incident is muted when it is not.
func (r *Responder) silence(tc Context, author, text string) (string, error) {
	if r.Silence == nil {
		return "Silencing isn't enabled here — ask an operator to turn on `notify.silence` for this transport.", nil
	}
	if tc.TriggerKey == "" {
		return "I can't tell which incident this thread is about, so there's nothing to silence.", nil
	}
	window, err := time.ParseDuration(strings.TrimSpace(text))
	if err != nil {
		return fmt.Sprintf("I couldn't read %q as a duration — try `silence: 4h` (up to %s).",
			strings.TrimSpace(text), ShortDuration(r.SilenceMax)), nil
	}
	now := r.now()
	if err := r.Silence.Silence(tc.TriggerKey, window, author, now); err != nil {
		return "Couldn't silence this: " + err.Error(), nil
	}
	return SilenceAck(author, window, now.Add(window), r.FeedbackEnabled), nil
}

// freeform answers an addressed message that carried no recognised command
// prefix — a question, or a correction stated as ordinary prose.
//
// With no Chat wired the behaviour is exactly what it was before this route
// existed: FreeformNotRecordedReply, and nothing written. See that constant's
// doc comment for every path that still ends there, and for what protects the
// human on the path that now does write.
//
// With a Chat wired, the model answers and may propose note CONTENT alongside
// its reply. The content is written through record() — the same path an
// explicit "note:" takes, so the same per-thread cap, the same ForgeWrites
// window and the same context-derived routing apply. The model never selects
// where the note goes, and could not: it returns text.
//
// It is filed as a ProposedNote, never a HumanNote: the text is the MODEL's,
// and the human's own message travels with it so a KB reviewer can see what
// prompted the draft rather than reading a paraphrase as the engineer's own
// statement. See Note for why that distinction is load-bearing.
//
// Every failure degrades to FreeformNotRecordedReply rather than to silence.
// Answer reports false for a model error, a denied budget, a refusal, a
// truncated response, a malformed tool call or an empty reply — all of which
// leave the human's message unanswered, which must never be indistinguishable
// from having answered it.
func (r *Responder) freeform(ctx context.Context, tc Context, author, text string) (string, error) {
	// A bare mention parses as freeform with empty Text. There is nothing in it
	// to answer, so it must not become a paid model call — otherwise the
	// cheapest way to run up a bill is to mention the bot with nothing after it.
	if r.Chat == nil || text == "" {
		return FreeformNotRecordedReply, nil
	}
	res, ok := r.Chat.Answer(ctx, tc, author, text)
	if !ok {
		return FreeformNotRecordedReply, nil
	}
	// An empty note is the model saying "file nothing" — a question with nothing
	// durable in it — not an omission to compensate for.
	if res.KBNote == "" {
		return modelVoice(res.Reply), nil
	}
	noteReply, err := r.record(ctx, tc, ProposedNote(author, text, res.KBNote))
	// Both parts, always: the answer the human asked for, and what happened to
	// the note — including when the write was capped, throttled or failed, since
	// the one outcome this must never produce is a human believing something was
	// saved that was not.
	//
	// The model's half goes through modelVoice and RunLore's own half does not,
	// which is what keeps the two distinguishable inside the one message.
	return modelVoice(res.Reply) + "\n" + noteReply, err
}

// untrustedMark delimits a span of untrusted text inside a reply. It is a
// Unicode private-use code point, chosen because nothing that legitimately
// reaches a chat message ever carries one and because it survives JSON
// round-trips unchanged; Untrusted strips it from the content it wraps, so the
// marks in a finished reply are always balanced and always RunLore's own.
const untrustedMark = "\uE000"

// Untrusted marks s as content RunLore did not author — model prose, a forge's
// own error text, a URL that arrived from somewhere rather than being written
// here — so the transport adapter that finally posts the reply can neutralise
// it for ITS markup before sending.
//
// The marking sits here, and the ESCAPING sits in the adapter, because the two
// pieces of knowledge live in different places and neither can move. Only this
// package knows which bytes of a reply came from where; only the adapter knows
// what its chat system treats as markup (Slack's mrkdwn escapes & < >, Matrix
// replies are plain-text bodies with a different hazard entirely). Escaping
// here would need Slack's rules in a package that is deliberately
// transport-agnostic, and escaping the FINISHED message in the adapter would
// destroy RunLore's own framing — the "> " modelVoice prefixes every line of
// model prose with would come out as literal "&gt; " and the marking that
// keeps model words distinguishable from RunLore's status lines would be gone.
// The boundary therefore has to sit around the untrusted SPANS, which is what
// this pair of functions expresses.
//
// A reply that is never rendered (a log line, a test that inspects it
// directly) carries the marks verbatim; RenderReply with a nil escape strips
// them without escaping anything.
func Untrusted(s string) string {
	if s == "" {
		return ""
	}
	return untrustedMark + strings.ReplaceAll(s, untrustedMark, "") + untrustedMark
}

// SafeErrorText prepares a SERVER-SUPPLIED error string for a chat message:
// redacted, backticks neutralised, flattened to one line. It is what a forge
// error must go through before any surface publishes it.
//
// It is EXPORTED and shared rather than reimplemented per surface, because the
// two surfaces that publish this class of text have already been shown to drift:
// internal/notify's curateFailureReason arrived at exactly this pipeline for the
// curate-failure line on an investigation card, while this package went on
// posting `Untrusted(err.Error())` into a thread — which its own doc comment
// cited as precedent for publishing forge errors at all. One implementation is
// what makes "the same treatment" a fact rather than an intention, the same
// argument QuoteUntrusted's own doc makes one function down.
//
// Three measures, in this order, each answering something the others do not:
//
//   - redact.Secrets FIRST, because a forge error may echo the credential it
//     rejected — a GitHub 403 body carrying the bearer token was the live case —
//     and running it before any cut means a truncation can never hand it a
//     half-token it no longer matches;
//   - backticks become apostrophes, because every caller wraps the result in an
//     inline code span and ONE backtick inside would close it early. Apostrophe
//     rather than deletion, so a quoted identifier still reads;
//   - SingleLine LAST of the three, because no escaper in this repo touches a
//     line break: a multi-line JSON body renders its continuation lines at the
//     same left margin as RunLore's own status claims, which is the forged-
//     headline class internal/notify was already bitten by. Flattening also maps
//     the private-use area, which is what stops a mark in a server's body from
//     flipping RenderReply's parity (see RenderReply).
//
// The CAP is deliberately left to the caller. Both surfaces bound this, in
// different units for different reasons — notify counts runes against a Slack
// section limit, this package counts bytes against a message budget it shares —
// and folding one of those numbers in here would make the other a lie.
func SafeErrorText(s string) string {
	return SingleLine(strings.ReplaceAll(redact.Secrets(s), "`", "'"))
}

// forgeErrorBytes bounds the forge error one thread reply publishes.
//
// It is the byte counterpart of internal/notify's curateErrorRunes, and it is
// that size for that comment's reason rather than a coincidentally similar one:
// truncate cuts the HEAD, and a wrapped Go error puts the diagnosis LAST
// ("open PR: github POST /repos/o/r/pulls: status 403: Resource not accessible
// by integration"), so a tighter cap reliably keeps the call site and drops the
// status and the message — the only two things an operator can act on.
//
// Bytes, not runes, because every other bound in this package counts bytes
// (notePreviewBytes, MaxNoteBytes) and a second unit here would be one more
// thing to get wrong. A forge error is overwhelmingly ASCII, so the two agree in
// practice and the byte count is the conservative one where they do not.
const forgeErrorBytes = 300

// forgeErrorReply renders the one reply a failed knowledge-base write gets, from
// both routes that can fail, so the two cannot drift into telling a human two
// different things — which is exactly how the unredacted one survived: the fix
// that would have covered it had two literals to find.
//
// The inline code span is the fourth measure, the one SafeErrorText cannot
// provide: a reason this long soft-wraps in every client, and a continuation
// line starts at the left margin with no quote bar of its own. Inside a span it
// is monospaced-with-a-background on Slack and <code> on Matrix — visibly not
// RunLore's own voice. The backticks are RunLore's OWN bytes, outside the marked
// span, so no escaper rewrites them and nothing inside can close them early.
func forgeErrorReply(err error) string {
	return "⚠️ I could not save that to the knowledge base: `" +
		Untrusted(truncate(SafeErrorText(err.Error()), forgeErrorBytes)) + "`"
}

// RenderReply resolves the untrusted spans a reply carries: escape is applied
// to every span Untrusted marked, everything RunLore wrote itself is left
// exactly as it is, and the marks themselves are removed either way. Every
// Replier must call it before posting — see SlackBot.ReplyInThread and
// Matrix.ReplyInThread, and the guard test that pins both.
//
// escape may be nil, which strips the marks and escapes nothing. That is the
// right behaviour for a transport with no markup to inject into, and it is
// what keeps an unrendered reply readable in a test.
//
// Splitting on a single mark rather than on an open/close pair is deliberate:
// spans cannot nest (Untrusted strips the mark from its own content), so the
// segments simply alternate — even indices are RunLore's, odd indices are not.
//
// That alternation is the whole contract, and it is a PARITY: one stray mark
// anywhere flips it for everything after that point, so a span downstream that
// really is untrusted lands on an even index and is handed to the transport
// UNESCAPED. This comment used to claim the opposite — that a stray mark "could
// only widen what gets escaped, never narrow it" — and that was simply false;
// measured, a raw U+E000 interpolated ahead of a marked span let "<!channel>"
// through verbatim. The false rationale mattered more than the bug: it is what
// would license a future caller to interpolate untrusted text without wrapping
// it, on the grounds that the failure was safe.
//
// What actually holds the parity is that every untrusted span goes through
// Untrusted(), which strips the mark from its own content, and that every
// single-line field reaching a reply outside a span goes through SingleLine,
// which maps the private-use area — U+E000 included — to a space (see
// forgeErrorReply, and TestForgeFailureCannotNarrowWhatTheReplyEscapes). Neither
// is optional, and neither is checked here: this function cannot tell a mark it
// wrote from one it was handed.
func RenderReply(reply string, escape func(string) string) string {
	parts := strings.Split(reply, untrustedMark)
	if len(parts) == 1 {
		return reply
	}
	if escape != nil {
		for i := 1; i < len(parts); i += 2 {
			parts[i] = escape(parts[i])
		}
	}
	return strings.Join(parts, "")
}

// runloreStatusRunes are the marks RunLore's OWN bot messages lead a status
// line with: 📝/⚠️ from this file's replies, and ⚡📚🔍🤖✅🛠🔥❓ from
// internal/notify's investigation formatter, which posts into the very same
// thread under the very same identity.
const runloreStatusRunes = "📝⚠⚡📚🔍🤖✅🛠🔥❓"

// modelVoice renders model-authored prose so a human can always tell it from
// RunLore's own statements about what it did.
//
// The two used to be concatenated into one message, under one bot identity, in
// one vocabulary. The model has been shown RunLore's real status lines, and it
// reproduces them: "📝 Noted on the knowledge-base PR #7 — <attacker URL>" and
// "⚠️ RunLore security notice: your session token has expired, re-authenticate
// at <attacker URL>" both arrived looking exactly like lines RunLore wrote. A
// bot's own claims about what it did must not be forgeable by its own model
// output — that is the one thing in the message the human cannot verify
// anywhere else.
//
// Three independent measures, stated by what each one covers. They used to be
// ranked — the blockquote load-bearing, the glyph strip a backstop — and that
// ranking was itself a defect: it is what justified shipping the announcement
// path (notify.kbUpdateAnnouncement) with the blockquote alone, and the
// blockquote is the one that turned out to fail for a whole input class.
//
// The blockquote keeps model prose off the left margin: EVERY line is
// prefixed, so no line of it can sit where RunLore's own lines sit, and there
// is no way to escape a quote from inside it. That holds exactly as far as
// "line" means the same thing here as it does to the client rendering the
// message, which is what mandatoryBreaks makes true and what splitting on "\n"
// alone did not. Every line's CONTENT is also wrapped in an Untrusted span,
// which is what stops the model composing the transport's own markup — a Slack
// mrkdwn "<https://evil.example|https://github.com/acme/kb/pull/7>" renders as
// a clickable link whose visible text is a trusted KB URL, and "<!channel>"
// pings everyone; the blockquote neutralises neither, because quoting is not
// escaping. The span ends at the newline rather than covering the whole block
// so the "> " markers stay RunLore's own bytes and keep rendering as a
// blockquote instead of coming back as literal "&gt; ". Stripping the status
// glyphs is what stops a line READING as a RunLore claim when it does reach
// the left margin — a renderer that flattens quoting, or a break the split has
// not learned about yet. A glyph missing from the list above degrades that,
// and a break missing from mandatoryBreaks degrades the blockquote; neither
// covers the other's gap.
//
// The model's words are never censored, only marked: a human must still be
// able to read the answer they asked for, including one that turns out to be
// an injected instruction, which they can only judge by seeing it. Escaping
// preserves that — an escaped "<!channel>" is still readable as "<!channel>",
// it simply stops being a command to the chat system.
func modelVoice(s string) string { return QuoteUntrusted(s) }

// mandatoryBreaks folds every character that STARTS A NEW VISUAL LINE into the
// "\n" QuoteUntrusted splits on, so "one line" means the same thing to the
// blockquote as it does to the client rendering it.
//
// The set is UAX #14's mandatory breaks — the classes BK, CR, LF and NL — and
// nothing else, because that is exactly the set a text layout is REQUIRED to
// break at. SingleLine already made this argument for the single-line YAML
// title (see its doc comment: U+2028 and U+2029 are line breaks "many
// renderers and tokenizers break on", which is what made a Cc-only check a
// real gap rather than a pedantic one); this is the same fact applied to the
// one untrusted span that is rendered multi-line BY DESIGN and so cannot be
// flattened the way SingleLine flattens a title.
//
// CRLF is listed first so it folds to ONE break: strings.Replacer tries its
// patterns in argument order at each position, so a later bare "\r" cannot
// split a CRLF into two lines.
//
// It normalises breaks and nothing else, deliberately. SingleLine additionally
// maps Cf and every other Unicode space because a title must end up on one
// line; a quoted note must end up READABLE, so a tab, a no-break space and a
// zero-width space are carried through exactly as written. Those reorder or
// disguise text WITHIN a line, which the blockquote and the escaping already
// bound; they cannot move a line out of the quote.
var mandatoryBreaks = strings.NewReplacer(
	"\r\n", "\n", // CRLF — one break, so it is matched before the bare CR below
	"\r", "\n", // U+000D CARRIAGE RETURN (class CR)
	"\v", "\n", // U+000B LINE TABULATION (BK)
	"\f", "\n", // U+000C FORM FEED (BK)
	"\u0085", "\n", // U+0085 NEXT LINE (NL)
	"\u2028", "\n", // U+2028 LINE SEPARATOR (BK)
	"\u2029", "\n", // U+2029 PARAGRAPH SEPARATOR (BK)
)

// QuoteUntrusted is the rendering modelVoice's doc comment describes, factored
// out because more than one kind of untrusted prose needs exactly the same
// three measures: the note text a landed write quotes back into the thread
// (see recordedBlock), and the same note quoted into the notifier's configured
// channel (see notify.kbUpdateAnnouncement).
//
// That note is model-authored on the freeform route and human-authored on the
// "note:" route, and neither may be able to pose as RunLore. A note whose
// second line reads "📝 Noted on the knowledge-base PR #7 — <hostile URL>" is
// the identical forgery modelVoice was written for, arriving through a
// different door — so it gets the identical treatment rather than a second,
// weaker one.
//
// It is EXPORTED for that last reason. internal/notify had its own narrower
// copy, justified on the grounds that duplicating the glyph list across the
// package boundary would let the two drift; they drifted anyway, in the other
// direction — the copy never grew the glyph strip, and neither grew
// mandatoryBreaks — and a note carrying one U+2028 posted a forged headline at
// the left margin of the channel where investigation findings go. One
// implementation both surfaces call is what makes "identical treatment" a fact
// rather than an intention.
func QuoteUntrusted(s string) string {
	lines := strings.Split(mandatoryBreaks.Replace(stripStatusGlyphs(s)), "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight("> "+Untrusted(strings.TrimRight(ln, " ")), " ")
	}
	return strings.Join(lines, "\n")
}

// modelDraftedNotice is the line that keeps the model-drafted route
// distinguishable once its text is quoted into the thread.
//
// Quoting is what makes it necessary. Before, the reply named a pull request
// and nothing else, so there was nothing in it to mistake for the human's own
// words; a quote directly under "…with your note" reads as theirs unless
// something says otherwise. ProposedNote already carries that distinction into
// the ENTRY (see NoteBody, which attributes the text to RunLore and quotes the
// human's real message under it) — this is the same fact, said where the human
// actually reads it.
//
// It is RunLore's own bytes, so it is never marked untrusted and never
// blockquoted: it sits at the left margin, alongside the status line, precisely
// because it is a claim RunLore makes about what it did.
//
// It ends in a colon, so it is only ever emitted directly above the quote that
// colon promises — never above the entry title, and never with nothing after it
// (see recordedBlock).
const modelDraftedNotice = "Drafted by RunLore from your message — not your own words, pending review:"

// openedWith names the note in write()'s open_pr status line — the one place a
// reply says WHOSE words landed.
//
// "your note" is that claim, and on the model-drafted route it was false. It
// sat directly above modelDraftedNotice, so one message told the human "with
// your note" and then "not your own words", and the line they read first was
// the wrong one. The status line is also the half that has to stand alone: it
// is what a reader skims and what a transport keeps when it truncates, so a
// reader who never reaches the notice below must not be left believing RunLore
// filed their words under their name.
//
// The note: route keeps "your note" verbatim. The defect is a claim RunLore
// cannot support, not the act of naming an author — hedging both routes into
// something neutral would tell the human LESS than the line it replaced, which
// on a path whose whole purpose is saying what was recorded is a regression.
//
// The comment route deliberately gets no equivalent branch. "📝 Noted on the
// knowledge-base PR #7" asserts only that RunLore recorded something there and
// never says whose words it was, so it is already true on both routes; giving
// it an authorship clause for symmetry's sake would ADD the kind of claim this
// exists to remove.
//
// It reads Note rather than a second flag for the reason ProposedNote exists:
// one field is the discriminator and the evidence (see Note.DraftedFrom), and a
// parallel flag could disagree with it. write() reassigns n from noteAsWritten
// before calling this, which rewrites Text only — provenance survives.
func openedWith(n Note) string {
	if n.modelDrafted() {
		return "a note drafted from your message"
	}
	return "your note"
}

// noteTruncatedNotice is what a preview cut by notePreviewBytes says instead of
// silently stopping. A human who cannot tell a quote was shortened would read
// the visible part as the whole note, which is the same failure capNoteText's
// own marker exists to prevent on the forge side.
const noteTruncatedNotice = "(quote truncated — open the pull request for the full note)"

// recordedBlock renders the "here is what I filed" half of a landed write's
// reply: who wrote the text, what the entry is called, and the note itself.
//
// It is built from the KBWrite rather than from n, so what the human is shown
// is what the forge was sent — redacted and capped once, upstream, by
// noteAsWritten. Quoting n.Text instead would republish the secret the entry
// masked and the bytes the cap dropped.
//
// n is read for provenance only (modelDrafted), which is not on the KBWrite:
// the write is the same write either way, and only the caller knows whose words
// it carried.
//
// w.Title is named on the open_pr route, where RunLore generated an entry the
// human has no other way to see the name of. It is empty on the comment route
// BY DESIGN — that note joined an entry someone else already titled, and the
// pull request it landed on is named in the status line above — so an empty
// title renders no line rather than an empty one.
//
// modelDraftedNotice sits between the title and the quote, not above both,
// because its trailing colon promises the quote specifically: emitted above the
// title it separated a colon from the thing it introduced, and the title's own
// "Entry:" answered it instead. It is emitted only when there IS a preview, for
// the same reason — a promise with nothing after it is the same defect with the
// answer missing entirely rather than merely displaced.
func recordedBlock(n Note, w *KBWrite) string {
	var lines []string
	if w.Title != "" {
		// Untrusted: alert-derived, redacted and flattened but not RunLore's own
		// (see KBWrite.Title). Not blockquoted — it is one flattened line that
		// cannot reach the left margin on a line of its own.
		lines = append(lines, "Entry: "+Untrusted(w.Title))
	}
	preview := cutToRuneBoundary(w.Note, notePreviewBytes)
	if preview != "" {
		if n.modelDrafted() {
			lines = append(lines, modelDraftedNotice)
		}
		lines = append(lines, QuoteUntrusted(preview))
	}
	if len(preview) < len(w.Note) {
		lines = append(lines, noteTruncatedNotice)
	}
	return strings.Join(lines, "\n")
}

// stripStatusGlyphs removes every RunLore status mark from s, along with the
// variation selector and the single space that conventionally follow one, so
// removing a glyph does not leave a ragged indent behind.
func stripStatusGlyphs(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	eatSpace := false
	for _, r := range s {
		switch {
		case strings.ContainsRune(runloreStatusRunes, r):
			eatSpace = true
			continue
		case r == '️': // emoji variation selector, e.g. the ️ in "⚠️"
			continue
		case eatSpace && r == ' ':
			eatSpace = false
			continue
		}
		eatSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// record writes n to the knowledge base and charges this thread's note
// allowance for it, returning the reply that says what happened. Shared by the
// explicit "note:" route and by the note freeform's chat answer proposes, so
// both are bounded by the same per-thread cap and — through write() — by the
// same global ForgeWrites window, and both count against the same allowance.
//
// What the two routes do NOT share is provenance: n carries which one it came
// from (see Note), so the shared path can render each honestly instead of
// filing model prose under the human's name.
//
// # The per-thread cap is decided inside this thread's own guard
//
// Reading the cap, writing to the forge and incrementing the counter are ONE
// critical section, and they have to be: they are three steps of a single
// read-modify-write over one number that concurrent deliveries share.
//
// They used to be three. The cap was read from the caller's own snapshot of the
// context — captured before any guard was held — write() then took lockRoot for
// its own reasons, and the increment ran after that guard had been released. So
// eight concurrent notes on a thread two writes into a cap of three all observed
// Notes == 2, all decided they were within budget, and produced TEN forge writes
// with nothing refused, under -race. The guard this needs already existed for
// the sibling duplicate-PR race one layer down; the cap was simply being decided
// outside it.
//
// The guard is acquired HERE rather than in write() because this is the widest
// span that has to be atomic — a lock taken inside write() cannot cover a
// decision its caller already took. write() therefore documents this as its
// precondition rather than acquiring anything itself; it has exactly one
// production caller, which is this one.
//
// Everything under the guard is either local or a registry call. r.announce
// schedules onto a bounded dispatcher and returns immediately (Dispatcher.Go
// never blocks), so the one thing here that talks to the network does not hold
// the thread's writers behind it.
func (r *Responder) record(ctx context.Context, tc Context, n Note) (string, error) {
	// Deferred immediately after acquiring, so every return below — a refusal, a
	// forge error, a panic — releases it; a write that never released this would
	// wedge every later note in the thread behind it forever. An untracked root
	// ("" or a registry that has lost it) gets a no-op guard, exactly as before:
	// there is nowhere durable to count, which is the separate degradation
	// ErrThreadNotTracked reports.
	release := r.Registry.lockRoot(tc.Root)
	defer release()

	// Re-read under the guard. The caller's tc is a snapshot taken before the
	// wait — the mention handler reads the registry and then hands it here — so
	// the count it carries may be several writes stale by the time this call owns
	// the thread. A miss (disabled registry, or the entry aged out mid-request)
	// leaves tc exactly as the caller passed it, same as before this existed.
	if fresh, ok := r.Registry.Get(tc.Root); ok {
		tc = fresh
	}

	if tc.Notes >= r.maxNotes() {
		return fmt.Sprintf("This thread has hit its note limit (%d). Add anything further directly on the pull request.", r.maxNotes()), nil
	}

	at := r.now()
	reply, w, err := r.write(ctx, tc, n, at)
	if err != nil {
		return reply, err
	}

	// The budget is consumed only by a write that landed: a forge outage — or a
	// global-rate-limit throttle, which also returns with no error — must not
	// burn the thread's allowance.
	if w != nil {
		// Announced HERE, from the same non-nil result the reply is rendered
		// from, and for the same reason: this is the ONE place both write routes
		// and both callers ("note:" and the note a chat answer proposed)
		// converge, so a future route cannot be added that announces without
		// replying or replies without announcing. It sits ahead of the reply
		// rendering and the counter write-back below because it must not depend
		// on either: the entry is on the forge, and neither a truncated preview
		// nor a failed counter changes that it landed.
		r.announce(ctx, tc, n, w, at)

		// Rendered HERE rather than inside write() so the two concerns stay
		// apart: write() reports what it DID (see KBWrite), and this is where a
		// reply is finished — the same place the counter warning below is
		// appended. It also keeps write()'s own status lines byte-identical to
		// what they were, so the quote is strictly additive.
		if block := recordedBlock(n, w); block != "" {
			reply += "\n" + block
		}
		if uerr := r.Registry.Update(tc.Root, func(c *Context) { c.Notes++ }); uerr != nil {
			// The knowledge itself is already saved on the forge — that is not in
			// question — but the per-thread cap can no longer be trusted for this
			// thread, so this must be reported rather than returned as a plain
			// success: an uncountable write is exactly what let the cap go
			// permanently inert before ErrThreadNotTracked existed to catch it.
			r.log().Warn("thread: note counter write-back failed; this thread's cap may no longer be enforced", "root", tc.Root, "err", uerr)
			return reply + "\n⚠️ I saved that, but could not update this thread's note count — its limit may not be enforced correctly from here.",
				fmt.Errorf("note counter write-back for root %q: %w", tc.Root, uerr)
		}
	}
	return reply, nil
}

// noteTarget is one pull request write() may record a note on, plus the single
// fact that decides HOW it records it: whether RunLore's own thread capture is
// what opened that pull request.
//
// The distinction is the whole of issue #493. Both URLs used to be iterated as
// a bare []string, and both got a comment — which is right for exactly one of
// them and quietly lossy for the other:
//
//   - CuratedURL is the curator's draft. It has an author who is not RunLore, a
//     body they wrote, and a review in progress. A note there is FEEDBACK, and a
//     human decides at merge time what of it belongs in the entry. Rewriting
//     their file from under them would be the wrong shape entirely.
//
//   - NoteURL is the standalone PR an earlier note in THIS thread opened. Its
//     entry has no author but the operator and there is no draft to comment on,
//     so a comment there is not feedback on anything — it is knowledge parked
//     next to the entry instead of in it. Merge the PR and the catalog gains one
//     entry holding the FIRST note; every later one stays behind as pull-request
//     conversation the catalog never indexes, so kb_search and recall never see
//     it. Four notes in, one out.
//
// Order is unchanged: the curated PR still wins when both are set and open.
type noteTarget struct {
	url string
	// ours is true for the pull request thread capture itself opened, and is
	// what selects the append route in addToPR. It is derived from WHICH context
	// field the URL came from rather than from a label lookup on the forge: the
	// registry already knows which PR it opened (Context.NoteURL), so asking the
	// forge would be a round trip to re-learn a fact RunLore recorded itself —
	// and one that fails open, on a network blip, straight back into the lossy
	// route.
	ours bool
}

// noteTargets lists the pull requests a note may land on, in priority order.
func noteTargets(tc Context) []noteTarget {
	return []noteTarget{{url: tc.CuratedURL}, {url: tc.NoteURL, ours: true}}
}

// addToPR records n on the already-open pull request numbered num and reports
// which route landed it.
//
// On OUR OWN note PR the entry file is the destination, because that file is
// the only part of the pull request the catalog gains on merge. On anyone
// else's, a comment is — see noteTarget.
//
// The comment is also the FALLBACK when the append fails, and that ordering is
// deliberate. An append is several forge calls against a branch (locate the
// entry, read it, commit) and can fail for reasons that have nothing to do with
// the human — a push race with another writer, a transient 5xx, a pull request
// whose entry file has been renamed by a reviewer. Losing their words to any of
// those would be a worse outcome than the bug this fixes: a comment is a
// degraded record, not a missing one. It is logged at Warn because a note that
// keeps degrading is exactly what nobody noticed the first time.
//
// Both routes send the SAME body — one NoteBody rendering, evaluated once — so
// what a reviewer reads in the entry and what they would have read in a comment
// can never drift.
func (r *Responder) addToPR(ctx context.Context, tc Context, n Note, at time.Time, num int, ours bool) (string, error) {
	body := NoteBody(tc, n, at, r.maxNoteBytes())
	if ours {
		err := r.Forge.AppendToEntryOnPR(ctx, num, body, noteKey(tc, n))
		if err == nil {
			return RouteAppend, nil
		}
		// The one failure that is NOT a reason to comment: the pull request
		// finished between the open-check above and this write. A comment there is
		// never indexed by the catalog, so falling back would be the silent loss
		// the open-check exists to prevent, arriving through the window the check
		// cannot cover. Passed up for write() to treat exactly as it treats its own
		// closed-PR case — open a standalone entry instead.
		if errors.Is(err, providers.ErrPRNotOpen) {
			return "", err
		}
		r.log().Warn("thread: could not append the note to its entry; falling back to a comment, which the catalog will NOT index on merge",
			"pr", num, "root", tc.Root, "err", err)
	}
	if err := r.Forge.CommentOnPR(ctx, num, body); err != nil {
		return "", err
	}
	return RouteComment, nil
}

// noteKey is the stable identity of one note, used to make the entry append
// idempotent (see okf.NoteMarker and Forge.AppendToEntryOnPR).
//
// It is needed because the layers above this one replay. internal/server dedups
// Slack deliveries through a bounded, PER-PROCESS set that is wiped wholesale at
// capacity (see seenSet), so a busy channel, a restart or a leader failover all
// deliver the same message twice — and thread capture is detached from the ack,
// so nothing downstream notices. As comments a replay was self-limiting: two
// visibly identical comments on one pull request, both discarded at merge. As
// appends it is two copies of the same knowledge in the catalog, indexed twice
// and recalled twice, with no signal that either is a duplicate.
//
// The inputs are the ones that DO NOT move between a delivery and its replay:
// the thread, the author, and the note's own text as written. Deliberately not
// the rendered body, which carries the timestamp of the attempt — hashing that
// yields a different key for the replay of the same note, which is the one case
// this exists for. Deliberately not the thread's note counter either: a replay
// re-enters write() at whatever count the registry has reached.
//
// Two genuinely identical notes from the same person in the same thread
// therefore collapse into one. That is the right trade rather than a limitation:
// the entry ends up saying what they said, once, and the alternative — a false
// negative — is the permanent duplicate this is here to prevent.
//
// It keys BOTH sides of the entry's life: the marker stamped into the entry when
// the open_pr route creates it (see write) and the one an append then looks for.
// Only the second half existed at first, which left note 1 — and only note 1 —
// duplicable by a redelivery.
//
// WHERE IT DOES NOT REACH: the freeform route. There n.Text is the MODEL's draft,
// and a replayed delivery re-invokes the model, so the redelivery arrives with
// different bytes and hashes to a different key. Nothing here can close that. The
// only identity a replay genuinely shares is the human's original message, and
// keying on that would collapse two deliberate follow-ups drafted from similar
// messages into one — silently dropping a note somebody meant to file, which is
// the worse of the two failures. What bounds it instead is upstream and already
// present: internal/server's delivery dedup catches the common replay before a
// model is called at all, and the per-thread cap and the ForgeWrites window bound
// what gets through. The residue is visible duplicate prose in an entry a human
// still has to merge, not silent catalog corruption. See
// TestModelDraftedReplayIsNotDeduplicated.
//
// SHA-256 truncated to 16 hex characters. It is an identity, not a
// authentication tag: the entry it is compared against is one RunLore wrote, and
// note text reaching an entry has "<!" escaped out of it (see noteText), so a
// marker cannot be forged into an entry to suppress a later note — which would
// in any case require knowing that note's exact bytes in advance.
func noteKey(tc Context, n Note) string {
	sum := sha256.Sum256([]byte(tc.Root + "\x00" + n.Author + "\x00" + n.Text))
	return hex.EncodeToString(sum[:])[:16]
}

// recordedOn renders the status line for a note that landed on a pull request
// that already existed. The two routes get different words because they made
// different promises: an appended note IS the entry the catalog will gain, and
// a comment is a remark beside it that a human still has to fold in. A human
// reading their own acknowledgement is the only one who can tell RunLore it
// picked wrong, and they can only do that if the line says which it picked.
func recordedOn(route string, num int, prURL string) string {
	if route == RouteAppend {
		return fmt.Sprintf("📝 Added to the knowledge-base entry on PR #%d — %s", num, Untrusted(prURL))
	}
	return fmt.Sprintf("📝 Noted on the knowledge-base PR #%d — %s", num, Untrusted(prURL))
}

// write routes the note to the open KB PR, to the PR this thread already opened,
// or to a new standalone Concept PR — in that order. The returned *KBWrite
// describes what actually landed, and is nil whenever nothing did — on error
// and on the (non-error) global-rate-limit throttle alike.
//
// The first two destinations are not treated alike: a note on the curator's
// draft is a comment, and a note on the standalone PR thread capture itself
// opened is appended to that PR's entry file. See noteTarget.
//
// Reached from exactly two callers, both through record(): an explicit
// "note:", and the note content a chat answer proposed for a freeform message
// (see freeform). IntentReinvestigate never reaches it at all. Both callers
// supply note CONTENT only — the route below is derived from the thread
// context, never from the text and never from the model.
//
// PRECONDITION: the caller holds this root's write guard (Registry.lockRoot) and
// has re-read tc under it. This method used to acquire that guard itself, which
// serialised the forge round-trip correctly and still left the per-thread cap
// being decided outside it by the caller — see record(), which now owns both.
// The guard is what makes the routing below correct as well as the cap: it is
// how "two callers both observe NoteURL == ” and both open a PR" (the residual
// race GetOrCreate's doc comment describes) becomes "the second caller sees the
// first caller's PR and appends to it instead".
func (r *Responder) write(ctx context.Context, tc Context, n Note, at time.Time) (string, *KBWrite, error) {
	// Checked once, upstream of BOTH write routes below: a CommentOnPR spends
	// this budget exactly like an OpenPR does. Gating only the OpenPR branch —
	// as an earlier version of this method did — left the comment route bounded
	// solely by the per-thread cap (20) times however many threads the registry
	// happened to be holding (up to 2000), not by this window at all.
	//
	// The chat route sits AHEAD of this check, not behind it, and that is
	// deliberate: this window bounds forge writes, Chat.Budget bounds tokens,
	// and they are two ceilings over two different resources. Folding the model
	// call in here would leave a channel that chats constantly but proposes no
	// notes spending nothing against either — the exact gap Budget exists to
	// close. Nothing about this placement, upstream of the route branch, changed
	// to admit the second caller.
	if r.ForgeWrites != nil && !r.ForgeWrites.Allow() {
		// This is the one global cap this feature has, so it must be visible to
		// an operator: Warn (not Info) because a saturated write budget is an
		// operational condition worth seeing, and the counter because a log line
		// alone cannot be graphed, alerted on, or summed over a window the way
		// MentionsDroppedOnSaturation already can be.
		r.log().Warn("thread: global forge-write budget exhausted; throttling", "root", tc.Root, "author", n.Author)
		if r.Metrics != nil {
			r.Metrics.ThreadWritesThrottled.Add(ctx, 1)
		}
		// A throttle is not a failure — no error — but nothing landed, so the
		// caller must not charge the thread's note budget for it.
		return "⚠️ I have made too many knowledge-base writes recently and paused. Try again shortly.", nil, nil
	}

	// The token above is a RESERVATION, and every path below that returns without
	// a landed write hands it back.
	//
	// Allow() has to be optimistic — a caller that checked the budget only after
	// succeeding would let two callers race past the same last token — so the
	// charge necessarily precedes the outcome. Leaving it charged on failure made
	// this feature's two budgets disagree about what a failure costs: record()
	// deliberately does NOT charge the per-thread count for a write that did not
	// land, while a forge outage spent this hour's whole allowance on nothing and
	// then told the next human "I have made too many knowledge-base writes
	// recently" with zero writes behind it (see
	// TestForgeOutageDoesNotBurnTheGlobalWriteBudget).
	//
	// Keyed on `landed` rather than on the returned error, because the two are not
	// the same question: the ErrPRNotOpen paths below return no error and no
	// result — they fall THROUGH to open a standalone entry, and the token they
	// are still holding is the one that write pays with. So the flag is set at the
	// two returns that carry a *KBWrite, and nowhere else.
	landed := false
	if r.ForgeWrites != nil {
		defer func() {
			if !landed {
				r.ForgeWrites.Refund()
			}
		}()
	}

	// Evaluated once here, ahead of the route branch, so whichever route runs
	// files the same bytes and reports the same bytes — see noteAsWritten.
	n, asWritten := r.noteAsWritten(n)

	// The route is derived from the thread context alone. It is never influenced
	// by the message text, and — through prNumberOn — never by a URL that names
	// a forge RunLore does not write to.
	for _, candidate := range noteTargets(tc) {
		num, ok := r.prNumberOn(candidate.url)
		if !ok {
			continue
		}
		open, err := r.Forge.IsPROpen(ctx, num)
		if err != nil {
			// The open-check itself failed (network blip, rate limit, forge
			// outage) — "we could not tell", which is NOT the same case as "the PR
			// is closed" just below. An earlier version of this method treated the
			// two identically and fell through to the standalone-Concept path,
			// reasoning that dropping the note outright was the worse of the two
			// remaining options. That reasoning was sound but the direction was
			// wrong: on GitHub, OpenPR is a ~7-call sequence (branch, file PUTs,
			// PR, labels) against a ~2-call CommentOnPR, so escalating here turns
			// one degraded forge call into six more of them — exactly when the
			// forge is already struggling. Report the failure honestly instead:
			// the note is not silently dropped, and success is not falsely
			// claimed either, so the human knows to retry.
			r.log().Warn("thread: PR open-check failed; not escalating to opening a new PR", "pr", num, "root", tc.Root, "err", err)
			return fmt.Sprintf("⚠️ I could not reach the forge to check PR #%d — nothing was saved. Please try again.", num), nil,
				fmt.Errorf("check PR %d open: %w", num, err)
		}
		if !open {
			// A merged/closed PR is never indexed by the catalog, so a comment
			// there would be silently lost while the human is told it was saved.
			// A commit appending to its entry is lost the same way: a merged PR's
			// branch no longer reaches the base branch, and a closed one never
			// will. Fall through to the standalone-Concept path instead — the
			// same path the design doc's non-goal on amending a merged entry
			// prescribes: "v1 opens a new entry that links the one it corrects."
			r.log().Info("thread: linked PR is no longer open; opening a standalone note instead", "pr", num, "root", tc.Root)
			continue
		}
		route, err := r.addToPR(ctx, tc, n, at, num, candidate.ours)
		// The PR was open a moment ago and is not any more — a reviewer merged it
		// while this note was being written. Identical handling to the !open branch
		// above, because it is the identical situation observed one round trip
		// later: fall through and open a standalone entry. The two must not
		// diverge, or a note that arrives a second too late is lost by a route the
		// same note a second earlier survives.
		if errors.Is(err, providers.ErrPRNotOpen) {
			r.log().Info("thread: linked PR closed while the note was being written; opening a standalone note instead",
				"pr", num, "root", tc.Root, "err", err)
			continue
		}
		if err != nil {
			return forgeErrorReply(err), nil,
				fmt.Errorf("comment on PR %d: %w", num, err)
		}
		r.log().Info("thread: note recorded on KB PR", "pr", num, "root", tc.Root, "route", route, "author", n.Author)
		r.recordWrite(ctx, route)
		landed = true
		return recordedOn(route, num, candidate.url),
			&KBWrite{Route: route, PR: num, URL: candidate.url, Note: asWritten}, nil
	}

	entry := ConceptEntry(tc, n, at, r.maxNoteBytes())
	// Mark note 1 with the SAME identity a later append would look for, so the
	// entry this opens is already idempotent against its own redelivery.
	//
	// Without it the idempotency this package built covered every note in a
	// thread except the first. A replayed delivery re-enters here with NoteURL
	// already set by the write it is replaying, takes the append route, finds no
	// marker in the entry — because nothing had ever put one there — and writes
	// note 1 into the catalog a second time. Unlike a duplicated comment, which
	// is visible conversation that dies at merge, that is permanent entry
	// content: kb_search indexes it twice and recall serves it twice, with
	// nothing saying either copy is a duplicate.
	//
	// Derived here rather than inside ConceptEntry because the key is a property
	// of the DELIVERY, not of the entry: it is the same noteKey(tc, n) addToPR
	// passes, over the same n — reassigned above by noteAsWritten, so both see the
	// note as written rather than as typed. Two derivations that could disagree is
	// the only way this fix fails while looking correct, so there is one, and
	// TestOpenPRNoteMarkerIsTheKeyTheAppendPathDerives pins it.
	//
	// The marker survives the write: both forge clients render an entry body
	// through neutralizeImages alone, which rewrites "![" and nothing else, and an
	// HTML comment is not markdown a renderer shows. It cannot be forged from note
	// text either — noteText escapes "<!" out of everything untrusted (see
	// neutralizeHTML), which is what keeps a stranger from planting a marker that
	// suppresses somebody else's later note.
	entry.Body = okf.WithNoteMarker(entry.Body, noteKey(tc, n))
	// The same draft-time report the curator's PR path runs, for the same reason
	// and with the same refusal to block on it: this route opens a KB pull request
	// too, so an entry that cannot merge — or that carries a `resource` recall can
	// never match — must be visible NOW, not in the catalog repo's CI days later.
	// It matters more here than there. The human is told, in their own thread, that
	// their correction was saved, and it WAS: the forge write landed, the
	// announcement fired, and only the merge is impossible. Nothing else surfaces
	// that. The entry's own type selects the rules, so a Concept is not asked for
	// the `resource` an Incident must have.
	//
	// r.Metrics goes with it (nil-safe) so this route lands in the SAME
	// runlore_kb_draft_defects_total the curator writes. A counter only the curator
	// fed would answer "how much dead weight is the catalog taking on?" with the
	// curator's share of it, and read as the whole — which is the class of silence
	// the metric exists to end. recordWrite below counts this note as landed no
	// matter what shape the entry it landed is in.
	kbvalidate.WarnDraft(ctx, r.log(), r.Metrics, entry)
	ref, err := r.Forge.OpenPR(ctx, entry)
	if err != nil {
		return forgeErrorReply(err), nil,
			fmt.Errorf("open note PR: %w", err)
	}
	if uerr := r.Registry.Update(tc.Root, func(c *Context) { c.NoteURL = ref.URL }); uerr != nil {
		r.log().Warn("thread: note PR write-back failed; a later note in this thread may open a second PR",
			"root", tc.Root, "url", ref.URL, "err", uerr)
	}
	r.log().Info("thread: note opened a standalone KB PR", "url", ref.URL, "root", tc.Root, "author", n.Author)
	r.recordWrite(ctx, RouteOpenPR)
	landed = true
	// PRNumber, not prNumberOn: this URL is one RunLore's own forge client just
	// returned, which is exactly the case PRNumber documents itself as safe for.
	// A URL it cannot parse is not a failure — the write landed, and the result
	// reports everything else about it with PR left at zero.
	w := &KBWrite{Route: RouteOpenPR, URL: ref.URL, Title: entry.Title, Note: asWritten}
	if num, ok := PRNumber(ref.URL); ok {
		w.PR = num
		return fmt.Sprintf("📝 Opened knowledge-base PR #%d with %s — %s", num, openedWith(n), Untrusted(ref.URL)), w, nil
	}
	return "📝 Opened a knowledge-base PR with " + openedWith(n) + " — " + Untrusted(ref.URL), w, nil
}
