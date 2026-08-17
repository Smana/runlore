// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
	"github.com/Smana/runlore/internal/telemetry"
)

// Forge is the write surface a thread note needs. It is a subset of
// providers.CurationForge, which satisfies it — narrowed here so the responder
// declares exactly the two calls it makes and can be faked in one struct.
type Forge interface {
	CommentOnPR(ctx context.Context, number int, body string) error
	OpenPR(ctx context.Context, e providers.KBEntry) (providers.Ref, error)
	// IsPROpen reports whether the pull/merge request numbered `number` is
	// still open. write() calls this before commenting on a linked PR: a
	// comment on a MERGED pull request is never indexed by the catalog, so
	// commenting there would silently lose the human's knowledge while telling
	// them it was saved. github.Client and gitlab.Client both implement it.
	IsPROpen(ctx context.Context, number int) (bool, error)
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
}

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

// Handle parses raw, writes the knowledge where it belongs, and returns the
// reply to post in the thread. The reply is returned even alongside an error —
// the human must always learn what happened to their words.
func (r *Responder) Handle(ctx context.Context, tc Context, author, raw string) (string, error) {
	p := Parse(raw)

	switch p.Intent {
	case IntentReinvestigate:
		return ReinvestigateNotSupportedReply, nil
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
// A stray unpaired mark could therefore only widen what gets escaped, never
// narrow it, which is the safe direction to fail in.
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
// Three independent measures, in order of how much they carry. The blockquote
// is load-bearing: EVERY line is prefixed, so no line of model prose can sit
// at the left margin where RunLore's own lines sit, and there is no way to
// escape a quote from inside it. Every line's CONTENT is also wrapped in an
// Untrusted span, which is what stops the model composing the transport's own
// markup — a Slack mrkdwn "<https://evil.example|https://github.com/acme/kb/
// pull/7>" renders as a clickable link whose visible text is a trusted KB URL,
// and "<!channel>" pings everyone; the blockquote neutralises neither, because
// quoting is not escaping. The span ends at the newline rather than covering
// the whole block so the "> " markers stay RunLore's own bytes and keep
// rendering as a blockquote instead of coming back as literal "&gt; ".
// Stripping the status glyphs is belt-and-braces against a renderer that
// flattens quoting, so a glyph missing from the list above degrades the
// marking rather than reopening the gap.
//
// The model's words are never censored, only marked: a human must still be
// able to read the answer they asked for, including one that turns out to be
// an injected instruction, which they can only judge by seeing it. Escaping
// preserves that — an escaped "<!channel>" is still readable as "<!channel>",
// it simply stops being a command to the chat system.
func modelVoice(s string) string {
	lines := strings.Split(stripStatusGlyphs(s), "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight("> "+Untrusted(strings.TrimRight(ln, " ")), " ")
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
func (r *Responder) record(ctx context.Context, tc Context, n Note) (string, error) {
	if tc.Notes >= r.maxNotes() {
		return fmt.Sprintf("This thread has hit its note limit (%d). Add anything further directly on the pull request.", r.maxNotes()), nil
	}

	at := r.now()
	reply, landed, err := r.write(ctx, tc, n, at)
	if err != nil {
		return reply, err
	}

	// The budget is consumed only by a write that landed: a forge outage — or a
	// global-rate-limit throttle, which also returns with no error — must not
	// burn the thread's allowance.
	if landed {
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

// write routes the note to the open KB PR, to the PR this thread already opened,
// or to a new standalone Concept PR — in that order. The returned bool reports
// whether a write actually landed in the knowledge base: it is false both on
// error and on the (non-error) global-rate-limit throttle, so a caller can
// distinguish "nothing happened" from "a write happened" independently of err.
//
// Reached from exactly two callers, both through record(): an explicit
// "note:", and the note content a chat answer proposed for a freeform message
// (see freeform). IntentReinvestigate never reaches it at all. Both callers
// supply note CONTENT only — the route below is derived from the thread
// context, never from the text and never from the model.
func (r *Responder) write(ctx context.Context, tc Context, n Note, at time.Time) (string, bool, error) {
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
		return "⚠️ I have made too many knowledge-base writes recently and paused. Try again shortly.", false, nil
	}

	// Serialize concurrent writes for THIS root — not the registry's own
	// mutex, which is never held across the forge round-trip below, so this
	// only blocks another write for the SAME thread, never Get/Put/Update or
	// a write for a different thread. Deferred immediately after acquiring,
	// so any return path below — success, a forge error, or a panic — always
	// releases it; a write that never released this would wedge every later
	// note in the thread behind it forever.
	release := r.Registry.lockRoot(tc.Root)
	defer release()

	// Re-read the registry now that the guard is held: another write for
	// this same root may have landed and updated NoteURL while this call was
	// waiting for the lock, and the routing decision below must see THAT,
	// not the possibly-stale tc captured before the wait. This is what turns
	// "two callers can both observe NoteURL == '' and both open a PR" (the
	// residual race GetOrCreate's doc comment describes) into "the second
	// caller sees the first caller's PR and comments on it instead." A miss
	// here (disabled registry, or the entry aged out mid-request) leaves tc
	// exactly as the caller passed it, same as before this guard existed.
	if fresh, ok := r.Registry.Get(tc.Root); ok {
		tc = fresh
	}

	// The route is derived from the thread context alone. It is never influenced
	// by the message text, and — through prNumberOn — never by a URL that names
	// a forge RunLore does not write to.
	for _, candidate := range []string{tc.CuratedURL, tc.NoteURL} {
		num, ok := r.prNumberOn(candidate)
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
			return fmt.Sprintf("⚠️ I could not reach the forge to check PR #%d — nothing was saved. Please try again.", num), false,
				fmt.Errorf("check PR %d open: %w", num, err)
		}
		if !open {
			// A merged/closed PR is never indexed by the catalog, so a comment
			// there would be silently lost while the human is told it was saved.
			// Fall through to the standalone-Concept path instead — the same
			// path the design doc's non-goal on amending a merged entry
			// prescribes: "v1 opens a new entry that links the one it corrects."
			r.log().Info("thread: linked PR is no longer open; opening a standalone note instead", "pr", num, "root", tc.Root)
			continue
		}
		if err := r.Forge.CommentOnPR(ctx, num, NoteBody(tc, n, at, r.maxNoteBytes())); err != nil {
			return fmt.Sprintf("⚠️ I could not save that to the knowledge base: %s", Untrusted(err.Error())), false,
				fmt.Errorf("comment on PR %d: %w", num, err)
		}
		r.log().Info("thread: note recorded on KB PR", "pr", num, "root", tc.Root, "author", n.Author)
		r.recordWrite(ctx, "comment")
		return fmt.Sprintf("📝 Noted on the knowledge-base PR #%d — %s", num, Untrusted(candidate)), true, nil
	}

	ref, err := r.Forge.OpenPR(ctx, ConceptEntry(tc, n, at, r.maxNoteBytes()))
	if err != nil {
		return fmt.Sprintf("⚠️ I could not save that to the knowledge base: %s", Untrusted(err.Error())), false,
			fmt.Errorf("open note PR: %w", err)
	}
	if uerr := r.Registry.Update(tc.Root, func(c *Context) { c.NoteURL = ref.URL }); uerr != nil {
		r.log().Warn("thread: note PR write-back failed; a later note in this thread may open a second PR",
			"root", tc.Root, "url", ref.URL, "err", uerr)
	}
	r.log().Info("thread: note opened a standalone KB PR", "url", ref.URL, "root", tc.Root, "author", n.Author)
	r.recordWrite(ctx, "open_pr")
	if num, ok := PRNumber(ref.URL); ok {
		return fmt.Sprintf("📝 Opened knowledge-base PR #%d with your note — %s", num, Untrusted(ref.URL)), true, nil
	}
	return "📝 Opened a knowledge-base PR with your note — " + Untrusted(ref.URL), true, nil
}
