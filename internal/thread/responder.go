// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
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
	// forge (or, once a model sits ahead of this same check, a token-spend)
	// incident regardless of which route its notes happen to take. nil means
	// unlimited.
	ForgeWrites *ratelimit.Window
	Now         func() time.Time
	Log         *slog.Logger
}

// prNumberRe matches the numeric id in a GitHub pull URL or a GitLab merge-request
// URL. Anchored on the path segment so an issue URL never matches.
var prNumberRe = regexp.MustCompile(`/(?:pull|merge_requests)/(\d+)`)

// PRNumber extracts the pull-request / merge-request number from a forge URL.
// providers.Ref carries only a URL, so the number the comment API needs is
// recovered here rather than widening the Ref contract.
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

func (r *Responder) maxNotes() int {
	if r.MaxNotesPerThread <= 0 {
		return DefaultMaxNotesPerThread
	}
	return r.MaxNotesPerThread
}

// Handle parses raw, writes the knowledge where it belongs, and returns the
// reply to post in the thread. The reply is returned even alongside an error —
// the human must always learn what happened to their words.
func (r *Responder) Handle(ctx context.Context, tc Context, author, raw string) (string, error) {
	p := Parse(raw)

	switch p.Intent {
	case IntentReinvestigate:
		return ReinvestigateNotSupportedReply, nil
	case IntentNote, IntentFreeform:
	}

	if p.Text == "" {
		return "Tell me what to record — for example: `note: the real cause was a spot-node reclaim`.", nil
	}

	if tc.Notes >= r.maxNotes() {
		return fmt.Sprintf("This thread has hit its note limit (%d). Add anything further directly on the pull request.", r.maxNotes()), nil
	}

	at := r.now()
	reply, landed, err := r.write(ctx, tc, author, p.Text, at, p.Intent)
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
	if p.Intent == IntentFreeform {
		reply += "\n_Tip: prefix with `note:` to record something explicitly._"
	}
	return reply, nil
}

// write routes the note to the open KB PR, to the PR this thread already opened,
// or to a new standalone Concept PR — in that order. The returned bool reports
// whether a write actually landed in the knowledge base: it is false both on
// error and on the (non-error) global-rate-limit throttle, so a caller can
// distinguish "nothing happened" from "a write happened" independently of err.
func (r *Responder) write(ctx context.Context, tc Context, author, text string, at time.Time, intent Intent) (string, bool, error) {
	// Checked once, upstream of BOTH write routes below: a CommentOnPR spends
	// this budget exactly like an OpenPR does. Gating only the OpenPR branch —
	// as an earlier version of this method did — left the comment route bounded
	// solely by the per-thread cap (20) times however many threads the registry
	// happened to be holding (up to 2000), not by this window at all. A future
	// model call on this same path belongs ahead of this check too, and nothing
	// about this placement — upstream of the route branch — needs to change to
	// put it there.
	if r.ForgeWrites != nil && !r.ForgeWrites.Allow() {
		// A throttle is not a failure — no error — but nothing landed, so the
		// caller must not charge the thread's note budget for it.
		return "⚠️ I have made too many knowledge-base writes recently and paused. Try again shortly.", false, nil
	}

	// The route is derived from the thread context alone. It is never influenced
	// by the message text.
	for _, url := range []string{tc.CuratedURL, tc.NoteURL} {
		n, ok := PRNumber(url)
		if !ok {
			continue
		}
		open, err := r.Forge.IsPROpen(ctx, n)
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
			r.log().Warn("thread: PR open-check failed; not escalating to opening a new PR", "pr", n, "root", tc.Root, "err", err)
			return fmt.Sprintf("⚠️ I could not reach the forge to check PR #%d — nothing was saved. Please try again.", n), false,
				fmt.Errorf("check PR %d open: %w", n, err)
		}
		if !open {
			// A merged/closed PR is never indexed by the catalog, so a comment
			// there would be silently lost while the human is told it was saved.
			// Fall through to the standalone-Concept path instead — the same
			// path the design doc's non-goal on amending a merged entry
			// prescribes: "v1 opens a new entry that links the one it corrects."
			r.log().Info("thread: linked PR is no longer open; opening a standalone note instead", "pr", n, "root", tc.Root)
			continue
		}
		if err := r.Forge.CommentOnPR(ctx, n, NoteBody(tc, author, text, at)); err != nil {
			return fmt.Sprintf("⚠️ I could not save that to the knowledge base: %v", err), false,
				fmt.Errorf("comment on PR %d: %w", n, err)
		}
		r.log().Info("thread: note recorded on KB PR", "pr", n, "root", tc.Root, "author", author, "intent", intent.String())
		return fmt.Sprintf("📝 Noted on the knowledge-base PR #%d — %s", n, url), true, nil
	}

	ref, err := r.Forge.OpenPR(ctx, ConceptEntry(tc, author, text, at))
	if err != nil {
		return fmt.Sprintf("⚠️ I could not save that to the knowledge base: %v", err), false,
			fmt.Errorf("open note PR: %w", err)
	}
	if uerr := r.Registry.Update(tc.Root, func(c *Context) { c.NoteURL = ref.URL }); uerr != nil {
		r.log().Warn("thread: note PR write-back failed; a later note in this thread may open a second PR",
			"root", tc.Root, "url", ref.URL, "err", uerr)
	}
	r.log().Info("thread: note opened a standalone KB PR", "url", ref.URL, "root", tc.Root, "author", author, "intent", intent.String())
	if n, ok := PRNumber(ref.URL); ok {
		return fmt.Sprintf("📝 Opened knowledge-base PR #%d with your note — %s", n, ref.URL), true, nil
	}
	return "📝 Opened a knowledge-base PR with your note — " + ref.URL, true, nil
}
