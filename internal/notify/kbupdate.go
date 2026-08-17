// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"errors"
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

// kbUpdateAnnouncement composes the CHANNEL form of the chat announcement for a
// knowledge-base write that already landed, marking every untrusted span with
// thread.Untrusted so the calling transport escapes it with its OWN escaper —
// the same boundary PR3 drew for thread replies, and for the same reason: only
// this side knows which bytes came from where, and only the transport knows what
// its chat system treats as markup.
//
// kbThreadAnnouncement is the shorter form used when the announcement is
// delivered into the thread the note came from; see it for why the two differ.
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
		// Not blockquoted: kbField has flattened it to one line, so it cannot
		// reach the left margin on a line of its own.
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

// kbThreadAnnouncement composes the announcement for delivery INTO the thread
// the note was typed in: the headline and the provenance line, and nothing else.
//
// It is deliberately not the same message as kbUpdateAnnouncement above, because
// the reader is not the same reader. In a channel the announcement is the only
// thing anyone sees about that write, so it carries the entry name and quotes
// the note. In the originating thread, the reply to the person who typed is
// sitting directly above it and already carries both — the same pull-request
// URL, the same "Entry: …" line, and the same 512-byte quote of the same note
// (see thread.recordedBlock). Repeating them would answer the complaint this
// destination exists for by relocating it: the operator who wrote that three
// notes produced three channel posts restating the thread would get three THREAD
// posts restating the message directly above each one, which is worse, not
// better — the two would be adjacent instead of a channel apart.
//
// What is left is exactly the delta: who wrote the note and which chat system
// they typed it in. The reply does not say either — it answers the person who
// typed, so it has no reason to name them — and the issue asking for this
// destination named that delta as the reason to want the announcement at all.
//
// A consequence worth stating: ORDER between the two is not guaranteed. The
// announcement is scheduled on the announcer's dispatcher before the reply is
// posted (see thread.Responder.record and thread.Mention.reply), so it can land
// first. That is tolerable only because this form does not restate the reply —
// two messages that each say something the other does not read correctly in
// either order, where a message and its near-duplicate do not.
func kbThreadAnnouncement(up providers.KBUpdate) string {
	lines := []string{kbHeadline(up)}
	if from := kbProvenanceLine(up); from != "" {
		lines = append(lines, from)
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

// kbField flattens and caps one untrusted single-line field, marking the cut so
// a truncated value is not read as a whole one. The ellipsis sits inside the
// span the caller wraps it in, which costs nothing: neither escaper touches it.
//
// It FLATTENS as well as capping, and that half is a safety property rather than
// tidiness. Every caller interpolates the result into a line at the LEFT MARGIN
// — "Entry: …", "By … in a slack thread", the headline's URL — where the note
// text never goes: the note is blockquoted line by line by thread.QuoteUntrusted,
// which splits on every mandatory break AND strips RunLore's status glyphs.
// These fields have neither defence, so one U+2028 in an author renders a new
// visual line whose content the sender chose, at the exact indent RunLore's own
// claims sit at — a forged "📚 Knowledge base updated — opened PR #999" directly
// under the real headline.
//
// Escaping does not cover it: neither escapeMrkdwn nor escapeMatrixReply touches
// a line separator, because on every other path the breaks were already gone.
// The producer in internal/thread does flatten these (see noteField), so this
// closes the gap for a providers.KBUpdate composed anywhere else — which is the
// premise of the field-classification guard in internal/providers: a notifier
// answers for what it renders, not for who filled the struct.
//
// It matters most on the IN-THREAD announcement, where kbThreadAnnouncement
// drops the title and the quote and the provenance line is one of only two lines
// in the message.
//
// Flattening runs BEFORE the cap so a break cannot survive by sitting past the
// ceiling, and it never changes the rune count, so it cannot push a field over.
func kbField(s string, maxBytes int) string {
	s = strings.TrimSpace(thread.FlattenLine(s))
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
//
// It ignores up.Delivery, and that is the documented fallback rather than an
// omission: an incoming webhook posts to the channel the URL was issued for and
// has no thread_ts to set, so it is one of the sinks providers.KBDelivery says
// receives a thread-routed announcement at channel level instead of not at all.
func (s *Slack) DeliverKBUpdate(ctx context.Context, up providers.KBUpdate) error {
	return s.post(ctx, slackKBUpdateMessage(up))
}

// kbAnnounceTargets decides where ONE sink delivers ONE announcement, given the
// transport that sink speaks. It is the whole of the routing rule
// providers.KBDelivery documents, written once so Slack and Matrix cannot answer
// it differently.
//
// A sink may deliver into the thread only when all three hold: the delivery asks
// for it, the sink speaks the transport the note was typed in, and BOTH thread
// handles arrived. A root without a channel is not a thread a reply can reach —
// Slack's chat.postMessage needs the channel, and Matrix's send needs the room —
// so a half-handle is treated exactly like no handle: fall back rather than post
// a threaded message into the wrong place or none at all.
//
// toChannel is true whenever the thread route was not taken, which is what makes
// the fallback total: every announcement lands somewhere. The one case where
// both are true is KBDeliverBoth on the originating transport, which is what it
// asks for.
func kbAnnounceTargets(up providers.KBUpdate, transport string) (toThread, toChannel bool) {
	toThread = up.Delivery.IntoThread() && up.Transport == transport && up.Root != "" && up.Channel != ""
	return toThread, !toThread || up.Delivery == providers.KBDeliverBoth
}

// DeliverKBUpdate announces a landed knowledge-base write
// (providers.KBUpdateNotifier), to the channel, into the originating thread, or
// to both — see kbAnnounceTargets.
//
// The default destination is still the CHANNEL and still for the original
// reason: the thread already received the direct reply to the person who typed,
// and the point of the announcement is reaching the people who were not reading
// it. What changed is that with ONE transport configured those are the same
// people, because the thread lives in that very channel — so an operator can now
// route the announcement into the thread instead of restating it beside it.
//
// The threaded post goes through ReplyInThread rather than a second local
// render: that is the method that already escapes for mrkdwn, bounds at the
// transport's ceiling and targets the passed channel, and a threaded
// announcement must be neutralised exactly as a threaded reply is.
//
// Errors from the two posts are joined rather than short-circuited. A failure to
// reach the thread must not suppress the channel half of KBDeliverBoth, and the
// announcer swallows the result either way — the write is already on the forge.
func (s *SlackBot) DeliverKBUpdate(ctx context.Context, up providers.KBUpdate) error {
	toThread, toChannel := kbAnnounceTargets(up, s.Transport())
	var errs []error
	if toThread {
		errs = append(errs, s.ReplyInThread(ctx, up.Root, up.Channel, kbThreadAnnouncement(up)))
	}
	if toChannel {
		_, err := s.post(ctx, slackKBUpdateMessage(up))
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// DeliverKBUpdate announces a landed knowledge-base write
// (providers.KBUpdateNotifier), to the configured room as a plain m.notice with
// no m.thread relation, into the originating thread via ReplyInThread's MSC3440
// relation, or both — see SlackBot.DeliverKBUpdate and kbAnnounceTargets.
//
// The room body goes through thread.RenderReply with escapeMatrixReply: a plain
// body has no markup to inject, but .m.rule.roomnotif matches "@room" in it and
// notifies every member of the room, which model-authored note text can reach
// the same way it reaches Slack's <!channel>. ReplyInThread applies the same
// escaper to the threaded copy.
func (m *Matrix) DeliverKBUpdate(ctx context.Context, up providers.KBUpdate) error {
	toThread, toRoom := kbAnnounceTargets(up, m.Transport())
	var errs []error
	if toThread {
		errs = append(errs, m.ReplyInThread(ctx, up.Root, up.Channel, kbThreadAnnouncement(up)))
	}
	if toRoom {
		_, err := m.send(ctx, m.roomID, map[string]any{
			"msgtype": "m.notice",
			"body":    boundPostedReply(thread.RenderReply(kbUpdateAnnouncement(up), escapeMatrixReply), matrixReplyBytes),
		})
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
