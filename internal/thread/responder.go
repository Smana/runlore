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

// Responder turns an addressed thread message into a knowledge-base write and
// returns the text to post back. It is transport-agnostic: every chat-system
// concern lives in the adapter that calls it.
type Responder struct {
	Forge    Forge
	Registry *Registry
	// MaxNotesPerThread caps knowledge writes per thread; <= 0 means
	// DefaultMaxNotesPerThread.
	MaxNotesPerThread int
	// OpenPRs caps how often a standalone note PR may be opened, globally. A
	// chatty channel must not become a forge incident. nil means unlimited.
	OpenPRs *ratelimit.Window
	Now     func() time.Time
	Log     *slog.Logger
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
		return "Re-running an investigation from a thread is not supported yet. " +
			"Add the `reinvestigate` label to the KB issue to re-run, or use `note:` to record what you know.", nil
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
			r.log().Warn("thread: note counter write-back failed", "root", tc.Root, "err", uerr)
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
			// outage). Two bad options were on the table: comment blindly, which
			// risks landing the note on a MERGED pull request — silently lost,
			// since a merged PR is never indexed by the catalog, which is the
			// exact failure this check exists to prevent — or refuse outright,
			// which loses the note for certain. Treating the error the same as
			// "not open" and falling through to the standalone-Concept path below
			// avoids both: the worst case is one extra small Concept PR (an
			// already-accepted cost — see the design doc's "Known cost" on
			// standalone note entries), but the human's words are never dropped.
			r.log().Warn("thread: PR open-check failed; treating as not open rather than risk commenting onto a merged PR",
				"pr", n, "root", tc.Root, "err", err)
			continue
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

	if r.OpenPRs != nil && !r.OpenPRs.Allow() {
		// A throttle is not a failure — no error — but nothing landed, so the
		// caller must not charge the thread's note budget for it.
		return "⚠️ I have opened too many knowledge-base PRs recently and paused. Try again shortly.", false, nil
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
