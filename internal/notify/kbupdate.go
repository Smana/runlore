// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// What one announcement may render of each UNTRUSTED KBUpdate field.
//
// These bound what is SAID in a channel, not what was WRITTEN to the knowledge
// base, and the two are deliberately different numbers. KBUpdate.Note carries
// the note at full length — up to max_note_bytes, 8 KiB by default and
// operator-raisable — precisely so a webhook sink receives the whole record;
// rendered straight into a chat channel that is an 8 KiB post, on a path any
// channel member can trigger. KBUpdate.Author is redacted and flattened
// upstream but never length-capped at all (the 100-byte cap in
// internal/thread's chat layer bounds what a MODEL PROMPT carries, not what
// reaches a notifier), so without a bound here it arrives unbounded.
//
// The note ceiling is the same 512 bytes the thread reply's own quote uses, and
// for the same derivation: every untrusted span is escaped before it goes on
// the wire, the widest of those escapes (Slack's "&" to "&amp;") expands 5x, so
// 512 bytes reaches the transport as at most ~2.5k characters. It is restated
// here rather than shared because the two bounds answer to different owners —
// that one is the thread package's own reply, this one is a notifier's channel
// post — and because a chat message's size is a transport concern, which is the
// same division boundPostedReply draws.
const (
	kbNotePreviewBytes = 512
	kbTitleBytes       = 200
	kbAuthorBytes      = 100
	kbURLBytes         = 512
)

const (
	// kbNoteTruncatedNotice is what a shortened quote says instead of simply
	// stopping. A reader who cannot tell the quote was cut reads what survived
	// as the whole note.
	kbNoteTruncatedNotice = "(quote truncated — open the pull request for the full note)"
	// kbFieldEllipsis marks a one-line field cut to its ceiling.
	kbFieldEllipsis = "…"
)

// kbUpdateAnnouncement composes the chat announcement for a knowledge-base
// write that already landed, marking every untrusted span with thread.Untrusted
// so the calling transport escapes it with its OWN escaper — the same boundary
// PR3 drew for thread replies, and for the same reason: only this side knows
// which bytes came from where, and only the transport knows what its chat
// system treats as markup.
//
// It is a NEW egress for model-authored text, and a wider one than the reply it
// accompanies: on the freeform route RunLore's own chat model wrote the note,
// and this lands in the notifier's configured channel — where investigation
// findings are posted under the same bot identity — rather than in the thread
// the note came from.
//
// Two measures carry that, both of them the reply's:
//
//   - Every untrusted field is inside a span, so the transport neutralises it.
//     Unescaped, "<!channel>" mass-pings a Slack channel and "@room" a Matrix
//     room, and "<https://evil|https://github.com/acme/kb/pull/7>" renders as a
//     live link whose visible text is a trusted knowledge-base URL.
//   - The note is blockquoted line by line, by thread.QuoteUntrusted, so no line
//     of it can sit at the left margin where RunLore's own claims sit — and the
//     status glyphs RunLore's own lines lead with are stripped from it.
//
// It calls that helper rather than keeping a local one. This file HAD a local
// one, narrower on purpose: it left the glyph strip out on the grounds that
// duplicating the glyph list across the package boundary would create a second
// copy free to drift. The two drifted regardless, and in a way neither list
// would have shown — the copy never learned that a line is not only what "\n"
// separates (see thread.mandatoryBreaks), so a note carrying a single U+2028
// put a forged "📚 Knowledge base updated — …" at the left margin of the very
// channel investigation findings are posted to, with the real headline directly
// above it. One shared implementation is what makes the two surfaces equal.
//
// Nothing here re-reads the forge or re-derives the note: the event describes a
// write that already happened, and every value it renders was produced by the
// write itself.
func kbUpdateAnnouncement(up providers.KBUpdate) string {
	lines := []string{kbHeadline(up)}
	if title := kbField(up.Title, kbTitleBytes); title != "" {
		// Not blockquoted: it is one flattened line (see thread.noteField) that
		// cannot reach the left margin on a line of its own.
		lines = append(lines, "Entry: "+thread.Untrusted(title))
	}
	if from := kbProvenanceLine(up); from != "" {
		lines = append(lines, from)
	}
	preview := cutBytesToRuneBoundary(up.Note, kbNotePreviewBytes)
	if preview != "" {
		lines = append(lines, thread.QuoteUntrusted(preview))
	}
	if len(preview) < len(up.Note) {
		lines = append(lines, kbNoteTruncatedNotice)
	}
	return strings.Join(lines, "\n")
}

// kbHeadline is RunLore's own claim about what it did, so every byte of it
// except the forge URL is RunLore's and stays outside any span. It names the
// route in the words a human uses rather than the enum value a metric label
// uses, and drops the "#n" when the forge returned a URL carrying no parseable
// number — which is not an error, and must not suppress the announcement.
func kbHeadline(up providers.KBUpdate) string {
	what := "noted on"
	if up.Route == providers.KBRouteOpenPR {
		what = "opened"
	}
	head := "📚 Knowledge base updated — " + what + " PR"
	if up.PR > 0 {
		head = fmt.Sprintf("%s #%d", head, up.PR)
	}
	if url := kbField(up.URL, kbURLBytes); url != "" {
		head += " — " + thread.Untrusted(url)
	}
	return head
}

// kbProvenanceLine names who the note came from and which chat system it was
// typed in. Transport is RunLore's own value (it is the notifier's Transport(),
// not anything a message carried), so it is never marked; Author is whatever
// the chat transport reported and always is.
func kbProvenanceLine(up providers.KBUpdate) string {
	author := kbField(up.Author, kbAuthorBytes)
	switch {
	case author != "" && up.Transport != "":
		return "By " + thread.Untrusted(author) + " in a " + up.Transport + " thread"
	case author != "":
		return "By " + thread.Untrusted(author)
	case up.Transport != "":
		return "From a " + up.Transport + " thread"
	}
	return ""
}

// kbField caps one untrusted single-line field, marking the cut so a truncated
// value is not read as a whole one. The ellipsis sits inside the span the
// caller wraps it in, which costs nothing: neither escaper touches it.
func kbField(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes {
		return s
	}
	return cutBytesToRuneBoundary(s, maxBytes-len(kbFieldEllipsis)) + kbFieldEllipsis
}

// slackKBUpdateMessage renders the announcement for both Slack delivery paths:
// escaped for mrkdwn, then bounded at the transport's own ceiling, exactly as a
// thread reply is. Plain text rather than Block Kit — the message is a short
// status line plus a quote, and a section block would only add a second, lower
// ceiling to keep in step with this one.
func slackKBUpdateMessage(up providers.KBUpdate) map[string]any {
	return map[string]any{
		"text": boundPostedReply(thread.RenderReply(kbUpdateAnnouncement(up), escapeMrkdwn), slackReplyBytes),
	}
}

// DeliverKBUpdate announces a landed knowledge-base write to the configured
// incoming webhook (providers.KBUpdateNotifier).
//
// The incoming-webhook path implements this for the same reason the bot path
// does: Multi SKIPS a notifier that does not implement the capability, so a
// supported delivery target that lacked the method would turn
// notify.thread.announce_kb_updates into a switch that silently does nothing
// for every operator on it.
func (s *Slack) DeliverKBUpdate(ctx context.Context, up providers.KBUpdate) error {
	return s.post(ctx, slackKBUpdateMessage(up))
}

// DeliverKBUpdate announces a landed knowledge-base write to the configured
// channel (providers.KBUpdateNotifier).
//
// It posts to the CHANNEL, never into the originating thread: the thread
// already received the direct reply to the person who typed, and the whole
// point of the announcement is reaching the people who were not reading it. So
// no thread_ts is set here, deliberately — see ReplyInThread for the other
// destination.
func (s *SlackBot) DeliverKBUpdate(ctx context.Context, up providers.KBUpdate) error {
	_, err := s.post(ctx, slackKBUpdateMessage(up))
	return err
}

// DeliverKBUpdate announces a landed knowledge-base write to the configured
// room (providers.KBUpdateNotifier), as a plain m.notice with no m.thread
// relation — see SlackBot.DeliverKBUpdate for why the destination is the room
// rather than the thread.
//
// The body goes through thread.RenderReply with escapeMatrixReply: a plain body
// has no markup to inject, but .m.rule.roomnotif matches "@room" in it and
// notifies every member of the room, which model-authored note text can reach
// the same way it reaches Slack's <!channel>.
func (m *Matrix) DeliverKBUpdate(ctx context.Context, up providers.KBUpdate) error {
	_, err := m.send(ctx, m.roomID, map[string]any{
		"msgtype": "m.notice",
		"body":    boundPostedReply(thread.RenderReply(kbUpdateAnnouncement(up), escapeMatrixReply), matrixReplyBytes),
	})
	return err
}
