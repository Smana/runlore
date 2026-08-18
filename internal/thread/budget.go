// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// DefaultChatCallsPerHour bounds model calls the chat layer — the
// unprefixed-question path, Responder.freeform into Chat.Answer — may make per
// hour, when notify.thread.chat_calls_per_hour is unset.
//
// A single active incident's thread realistically sees well under a dozen
// clarifying questions over its whole live window — a handful is typical, a
// very chatty incident might reach ten. 30 gives that generous headroom
// (3x the chatty case) while still bounding a scripted loop or a compromised
// account, which would otherwise have no cap at all.
const DefaultChatCallsPerHour = 30

// DefaultChatTokensPerHour bounds the tokens (input + output) the chat layer
// may spend per hour, when notify.thread.chat_tokens_per_hour is unset. What is
// counted against it is provider-reported usage for every call that has
// returned (CompletionResponse.Usage) plus an estimate for every call still on
// the wire — see Budget.Allow on why spend in flight has to count.
//
// It is DERIVED from this layer's own prompt ceiling rather than chosen as a
// round number, because the two only work together in a stated relationship: a
// token ceiling can bind on spend the call ceiling cannot see only if a
// worst-case hour of calls costs MORE than the tokens it allows. The previous
// 200,000 was sized against an assumed ~10k input tokens per call, while the
// same branch introduced a prompt ceiling capping one call at
// maxChatCallTokens ≈ 5.4k — two constants computed against inconsistent
// assumptions, and the result was that DenyTokens could never fire at the
// defaults: only chat_calls_per_hour was live.
//
//	    5,497  maxChatCallTokens — one call with every byte cap maxed at once
//	x      30  DefaultChatCallsPerHour — a full hour of them
//	  -------
//	  164,910  what a sustained runaway costs before the call ceiling stops it
//	x     2/3
//	  -------
//	  109,940  this ceiling
//
// The 2/3 is the ordering the two limits are meant to have: 109,940 / 5,497 is
// 20, so the twentieth maximum-size call of the hour exhausts this ceiling and
// the twenty-first is refused with DenyTokens while ten call slots are still
// free — stopped by cost, ten calls before count would have stopped it.
//
// That holds under concurrency only because Allow charges a call when it
// ADMITS it, not when it returns (see attempt). Charging at Record alone made
// the ordering above false in exactly the case it was written for: 16
// concurrent mention handlers per transport mean the twenty-first call is
// checked while the first twenty are still on the wire and have recorded
// nothing, so every one of the 30 hourly calls passed the token test and the
// real bound was 164,910 tokens against this 109,940 ceiling — a runaway
// stopped by count, the inverse of what is written above.
//
// Ordinary traffic is unaffected because the estimate a call reserves is its
// own (Chat.callEstimate reads the request that is about to be sent, not the
// worst case) and is replaced by the provider's own number as soon as the call
// returns: a question and a two-sentence reply, well under 2k tokens, spends
// its 30 calls without approaching this. Averaged over the hour, this ceiling
// binds first for anything above ~3.6k tokens a call (109,940 / 30), which only
// a message near the full notify.thread.max_note_bytes cap reaches.
//
// Two things this ceiling does NOT bound, both of them real:
//
//   - A call that took several upstream attempts is charged for all of them
//     (see Chat.chargedUsage), but only at Record — the reservation covers one
//     attempt. A flaky provider therefore reaches this ceiling sooner than the
//     call count suggests, and the calls already in flight when it does can
//     push the window past it by their own retry surcharge before the next
//     Allow sees the reconciled total. The ceiling bounds what is ADMITTED, and
//     a retried request is spend that was admitted once.
//   - The window is per process. Two replicas answering the same room each
//     enforce their own 109,940, so the ceiling an operator gets is this number
//     times the number of running instances.
const DefaultChatTokensPerHour int64 = int64(2 * DefaultChatCallsPerHour * maxChatCallTokens / 3)

// callTokens is one call's charge against the token ceiling, timestamped so it
// can slide out of the window exactly like a call attempt does.
//
// It has two lives. Allow creates it pending, holding the estimate the caller
// says the call it just admitted can cost; Record turns it settled, holding
// what the call really cost. The timestamp is the one thing Record does NOT
// touch: a call occupies the ceiling from the moment it was admitted, so its
// entry expires one window after that whether or not anything ever reconciled
// it. That is the whole of the abandoned-reservation story — see Record.
type callTokens struct {
	at      time.Time
	tokens  int64
	pending bool
}

// DenyReason names which Budget ceiling refused a call, reported by Allow
// atomically with its decision. It is a small typed enum rather than a bare
// string so a caller cannot invent a label the budget never emits — the
// metric label vocabulary (e.g. thread_chat_denied_total's "ceiling" label)
// is fixed at compile time to exactly these values.
//
// Reporting the reason atomically matters: the alternative — calling Allow,
// then calling Remaining() afterwards to infer which ceiling was zero —
// is racy, because another goroutine can Allow or Record between the two
// calls and change which dimension is actually exhausted. Determining the
// reason inside the same critical section that makes the decision (see
// attempt) closes that gap.
type DenyReason string

const (
	// DenyNone means Allow granted the call; no ceiling was hit.
	DenyNone DenyReason = ""
	// DenyCalls means the call-count ceiling was hit.
	DenyCalls DenyReason = "calls"
	// DenyTokens means the token ceiling was hit.
	DenyTokens DenyReason = "tokens"
)

// Budget is a cumulative spend ceiling on the chat layer: two independent
// sliding-window limits, one on the number of model calls and one on the tokens
// they cost — what returned calls really spent, plus what the calls still in
// flight are estimated to cost — over the same window.
//
// It exists because neither existing control bounds a per-chat-message model
// call: model.pricing is a reporting table with no consumer that compares a
// cost to a threshold, and investigation.max_tokens_per_investigation
// compares the size of the NEXT REQUEST against the limit rather than a
// running total, so twenty steps at 99k tokens each pass it cleanly.
//
// The two checks serve different moments, and the token dimension spans both.
// Allow is checked BEFORE a call, against the ceiling as currently known, and
// charges the call it admits an estimate of what it can cost — it cannot know
// the real number yet, and waiting for it would make every call still on the
// wire invisible to the next check. Record then REPLACES that estimate with
// what the call actually cost, from the provider's reported usage. Between the
// two, the ceiling is defended pessimistically; after them, it holds
// measurements. That is what makes the token limit bind on a burst as well as
// on a slow trickle, without leaving ordinary traffic charged a worst case it
// never spent.
//
// Neither method tracks a dimension whose ceiling is <= 0. Remaining reports
// such a dimension as unlimited without consulting its entries, so recording
// them would grow a slice nothing reads for the life of the process.
//
// Follows internal/ratelimit.Window's shape and its <= 0 convention: maxCalls
// <= 0 means calls are unlimited, and independently maxTokens <= 0 means
// tokens are unlimited — each dimension's own ceiling, not a shared one.
// Diverging from that convention here would be a trap for whoever reads both
// types side by side.
//
// It follows that shape without USING it, which is the first question a reader
// arriving from Responder.ForgeWrites (*ratelimit.Window) will ask. The calls
// dimension is Window field for field, and two things stop it being one:
//
//   - The clock. ratelimit.Window.now is package-private and New takes no clock,
//     so nothing outside internal/ratelimit can drive it. Every test here runs
//     on a fake clock (b.now), which is the only way to assert on a sliding
//     window without sleeping through it.
//   - Atomicity. Allow must report WHICH ceiling refused, decided in the same
//     critical section as the decision itself — see DenyReason on why inferring
//     it afterwards is racy. Delegating one dimension would split that decision
//     across two lock domains, Window's mutex and this one, so "calls had room,
//     tokens did not" could no longer be established atomically.
//
// A nil *Budget allows everything, the same nil-safety contract every
// optional dependency in this codebase follows: an unconfigured budget is not
// a hidden deny. Safe for concurrent use.
type Budget struct {
	maxCalls  int
	maxTokens int64
	window    time.Duration
	now       func() time.Time
	logger    *slog.Logger

	mu    sync.Mutex
	calls []time.Time
	spent []callTokens
}

// NewBudget returns a Budget allowing maxCalls calls and maxTokens tokens per
// window; either <= 0 means that dimension is unlimited (see Budget's doc
// comment). log receives a warning naming which ceiling denied a call; nil
// falls back to slog.Default().
func NewBudget(maxCalls int, maxTokens int64, window time.Duration, log *slog.Logger) *Budget {
	return &Budget{
		maxCalls:  maxCalls,
		maxTokens: maxTokens,
		window:    window,
		now:       time.Now,
		logger:    log,
	}
}

// log returns b.logger, defaulting to slog.Default() so a Budget built
// without an explicit logger never panics on a log call.
func (b *Budget) log() *slog.Logger {
	if b.logger != nil {
		return b.logger
	}
	return slog.Default()
}

// slideLocked drops entries — from both the calls and spent slices — older
// than the window. Caller must hold b.mu.
func (b *Budget) slideLocked() {
	cutoff := b.now().Add(-b.window)

	keptCalls := b.calls[:0]
	for _, t := range b.calls {
		if t.After(cutoff) {
			keptCalls = append(keptCalls, t)
		}
	}
	b.calls = keptCalls

	keptSpent := b.spent[:0]
	for _, e := range b.spent {
		if e.at.After(cutoff) {
			keptSpent = append(keptSpent, e)
		}
	}
	b.spent = keptSpent
}

// spentLocked sums the tokens currently in the window — reservations for calls
// still in flight alongside the reconciled cost of calls that have returned,
// since a pending charge is spend that is already committed. Caller must hold
// b.mu.
func (b *Budget) spentLocked() int64 {
	var total int64
	for _, e := range b.spent {
		total += e.tokens
	}
	return total
}

// attempt performs the sliding-window bookkeeping under lock: slide both
// dimensions, check each independently, and — only if both have headroom —
// record the attempt AND reserve estimate against the token ceiling. Returns
// the denial reason when refused (DenyNone when granted), determined in the
// same critical section as the decision, so Allow can report and log it after
// releasing the lock without a second, racy lookup.
//
// The reservation is the whole reason this method writes to b.spent at all.
// Both transports run 16 concurrent mention handlers (internal/server's
// maxConcurrentMentions, internal/app's matrixMentionConcurrency) and
// Responder.write's per-root lock sits after Chat.Answer, so nothing
// serialises the model call. Charging only at Record would leave every call
// that has not come back yet invisible here, and a burst would pass the token
// test unanimously — the ceiling would bind only on traffic slow enough to
// arrive one call at a time.
//
// Admission is still "the ceiling is not already reached", not "this call
// fits": a budget with 1 token left admits one more call, exactly as it did
// before, and so overshoots by up to one call's charge. Requiring the estimate
// to fit would instead refuse every call outright whenever one call's worst
// case exceeds the whole ceiling, which is a configuration an operator may
// reasonably choose to have absorbed rather than to be shut down by.
//
// An unlimited dimension is not tracked at all: Remaining reports math.MaxInt
// for it outright, so a call entry recorded under an unlimited call ceiling is
// one nothing ever reads. With both ceilings unlimited there is nothing to
// decide either, so this returns before taking the lock — the same shape
// ratelimit.Window.Allow uses for its single dimension.
func (b *Budget) attempt(estimate int64) (allowed bool, reason DenyReason) {
	if b.maxCalls <= 0 && b.maxTokens <= 0 {
		return true, DenyNone
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.slideLocked()
	if b.maxCalls > 0 && len(b.calls) >= b.maxCalls {
		return false, DenyCalls
	}
	if b.maxTokens > 0 && b.spentLocked() >= b.maxTokens {
		return false, DenyTokens
	}
	if b.maxCalls > 0 {
		b.calls = append(b.calls, b.now())
	}
	if b.maxTokens > 0 && estimate > 0 {
		b.spent = append(b.spent, callTokens{at: b.now(), tokens: estimate, pending: true})
	}
	return true, DenyNone
}

// Allow reports whether another call fits the budget, recording the attempt
// if so, and — atomically with that decision — which ceiling denied it when
// it did not (DenyNone when allowed). See DenyReason's doc comment for why
// that pairing matters. Non-blocking, and holds b.mu only for its own
// bookkeeping — never across the log call below. A nil *Budget always
// allows, reporting DenyNone.
//
// estimate is what the caller believes the call it is about to make can cost in
// tokens, and a granted call is charged it IMMEDIATELY rather than when it
// returns (Chat.callEstimate is what supplies it). Without that, spend in
// flight is invisible to the token check and concurrent callers all measure
// themselves against a total that only counts calls which have already
// finished. Record then replaces the estimate with the provider's own number,
// so the pessimism lasts exactly as long as the call does; an estimate <= 0
// reserves nothing, leaving the ceiling checked against recorded spend alone.
func (b *Budget) Allow(estimate int64) (bool, DenyReason) {
	if b == nil {
		return true, DenyNone
	}
	allowed, reason := b.attempt(estimate)
	if !allowed {
		b.log().Warn("thread: chat budget denied a call", "ceiling", string(reason), "window", b.window)
	}
	return allowed, reason
}

// Record reconciles a completed call: it REPLACES the reservation Allow made
// for it with what the call really cost, as providers.Usage.Total defines it
// (input + output, with neither cache field double-counted). Replacing rather
// than adding is the point — a call charged once pessimistically at Allow and
// again for real at Record would be billed twice, and a budget that
// over-charges by that much refuses the traffic it was sized to allow.
//
// It settles the NEWEST pending entry, not the one this particular call
// created, because Budget has no call identity and needs none: every pending
// entry is a reservation, so after both of two overlapping calls have recorded,
// the window holds both real costs whichever entry each landed on. Newest
// rather than oldest is what keeps an ABANDONED reservation ageing out. Every
// live call eventually settles, so an abandoned entry ends up the oldest
// pending one; settling oldest-first would hand it a live call's cost and park
// the still-pending charge on a newer timestamp instead, over and over. The
// pending charge would migrate forward through the window rather than expire,
// and one call that panicked between Allow and Record would hold a call's worth
// of ceiling for the life of the process.
// Settling newest leaves it where it was taken, and slideLocked drops it one
// window later like any other entry (TestBudgetUnreconciledReservationAgesOut).
//
// Nothing rolls a reservation back explicitly, and nothing needs to: the
// sliding window IS the rollback. The cost of a call that never reaches here is
// one window of held budget, which is real and is not zero — a burst of panics
// can refuse legitimate traffic for the rest of the hour — but it is bounded
// and it repairs itself.
//
// With no pending entry to settle — a Record with no matching Allow, or one
// whose reservation already aged out — this appends the charge as its own
// settled entry, which is exactly what this method did before reservations
// existed.
//
// u is normally the provider's own CompletionResponse.Usage, but Record does
// not require that and cannot check it: internal/providers defines a zero Usage
// as "unknown", not "zero tokens", so a caller handed one must substitute an
// estimate before calling here (Chat.chargedUsage does) rather than let this
// method bill nothing for a call that really spent tokens. Record charges
// exactly what it is given.
//
// It never denies; the call already happened. A nil
// *Budget is a no-op, and so is an unlimited token ceiling (maxTokens <= 0):
// Remaining reports it as unlimited without reading b.spent, so there is
// nothing for the entry to feed — see attempt.
func (b *Budget) Record(u providers.Usage) {
	if b == nil || b.maxTokens <= 0 {
		return
	}
	tokens := u.Total()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.slideLocked()
	for i := len(b.spent) - 1; i >= 0; i-- {
		if b.spent[i].pending {
			b.spent[i].tokens = tokens
			b.spent[i].pending = false
			return
		}
	}
	b.spent = append(b.spent, callTokens{at: b.now(), tokens: tokens})
}

// Remaining reports the calls and tokens still available in the current
// window, after sliding out expired entries (a peek; it records nothing). An
// unlimited dimension (its ceiling <= 0) reports math.MaxInt / math.MaxInt64
// rather than a number derived from a ceiling that does not exist. A nil
// *Budget — unconfigured, allows everything — reports both as unlimited.
//
// Tokens reserved for calls still in flight count as spent, so this reports the
// headroom the next Allow will actually see rather than a larger number that
// concurrent calls have already claimed. It is therefore not a total of
// provider-reported usage: read runlore_thread_chat_tokens_total for that.
//
// Chat.logLowBudget is what surfaces it: the counters report what a window
// spent, and DenyReason reports a refusal that already happened, so this is the
// only thing that can tell an operator a ceiling is coming.
func (b *Budget) Remaining() (calls int, tokens int64) {
	if b == nil {
		return math.MaxInt, math.MaxInt64
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.slideLocked()

	calls = math.MaxInt
	if b.maxCalls > 0 {
		calls = b.maxCalls - len(b.calls)
		if calls < 0 {
			calls = 0
		}
	}
	tokens = math.MaxInt64
	if b.maxTokens > 0 {
		tokens = b.maxTokens - b.spentLocked()
		if tokens < 0 {
			tokens = 0
		}
	}
	return calls, tokens
}
