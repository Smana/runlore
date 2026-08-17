// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"strings"
	"unicode/utf8"
)

// Per-transport ceilings for one posted message, in bytes of the FINAL text —
// what the transport actually receives, after every escape has been applied.
//
// They are stated here, in the notifier package, because a message limit is a
// property of the chat system and of nothing else. internal/thread composes a
// reply without knowing which transport will carry it (it may be carried by
// two at once), and the layer above it — the chat model's max_tokens — is
// operator-configurable with no upper bound at all, so nothing upstream can
// promise a reply fits.
const (
	// slackReplyBytes bounds chat.postMessage's "text". Slack documents ~40,000
	// characters; this stays under that with room for the JSON envelope, and it
	// is a BYTE count against a CHARACTER limit, which errs in the safe
	// direction — a byte count is never smaller than the character count it
	// encodes.
	slackReplyBytes = 35_000
	// matrixReplyBytes bounds an m.notice body. The Matrix spec caps a whole
	// event at 65,536 bytes — signatures, hashes and relations included — and
	// the body is one field of it, so half is the share it may claim.
	matrixReplyBytes = 32 << 10
)

// What a shortened reply says instead of stopping silently. RunLore's own
// bytes: appended AFTER escaping, so they must carry no markup of their own
// (no & < > for mrkdwn, no @room for Matrix), and they lead with the ⚠️ status
// glyph because they are a claim RunLore makes about what it did.
//
// A human who cannot tell a message was shortened reads what survived as the
// whole of it — the same failure noteTruncatedNotice prevents for the note
// quote one layer up.
const (
	replyHeadDroppedNotice = "⚠️ (this reply was too long to post — the earlier part was dropped)"
	replyTailCutNotice     = "⚠️ (this reply was too long to post — it was cut off here)"
)

// boundPostedReply caps a fully rendered thread reply at maxBytes, dropping the
// model's prose before the record of what happened.
//
// It runs on the RENDERED text — after thread.RenderReply has escaped the
// untrusted spans — because that is the string the transport receives. Bounding
// the composed reply instead would bound the wrong thing: escapeMrkdwn turns
// each "&" into "&amp;", so a reply that measures well under the budget as
// composed can arrive five times over it.
//
// WHAT it drops follows from how internal/thread composes the message. On the
// freeform route the reply is the model's answer, then RunLore's status line,
// then the recorded note quoted back (see Responder.freeform and
// recordedBlock) — the record is at the END. So whole lines are dropped from
// the FRONT: the PR link and the quote are what the human cannot reconstruct
// from anywhere else, and the tail of a long answer is what they can.
//
// Whole LINES, never part of one, and that is load-bearing rather than tidy:
// every line of model prose is blockquoted, and a cut through the middle of one
// would leave the remainder at the left margin, where RunLore's own claims sit —
// the exact forgery thread.modelVoice's blockquote exists to prevent.
//
// The one case whole lines cannot fix is a single line longer than the whole
// budget — a model answer with no newline in it. There is no record below it to
// protect (a record is several short lines, and a reply shaped like this has
// none), so its head is kept instead, "> " prefix included, and the cut lands
// on a rune boundary so no invalid UTF-8 reaches the wire.
func boundPostedReply(rendered string, maxBytes int) string {
	if maxBytes <= 0 || len(rendered) <= maxBytes {
		return rendered
	}
	lines := strings.Split(rendered, "\n")

	// Keep whole lines from the end while they fit under what the notice leaves.
	budget := maxBytes - len(replyHeadDroppedNotice) - 1 // -1: the newline joining the notice
	kept, size := 0, 0
	for i := len(lines) - 1; i >= 0; i-- {
		add := len(lines[i])
		if kept > 0 {
			add++ // the newline this line is joined by
		}
		if size+add > budget {
			break
		}
		size += add
		kept++
	}
	if kept == 0 {
		last := lines[len(lines)-1]
		return cutBytesToRuneBoundary(last, maxBytes-len(replyTailCutNotice)-1) + "\n" + replyTailCutNotice
	}
	return replyHeadDroppedNotice + "\n" + strings.Join(lines[len(lines)-kept:], "\n")
}

// cutBytesToRuneBoundary truncates s to at most n bytes without splitting a
// rune. A byte-sliced cut would hand the chat system invalid UTF-8, which is a
// rejected post — the same failure the bound exists to avoid, arrived at from
// the other side.
func cutBytesToRuneBoundary(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
