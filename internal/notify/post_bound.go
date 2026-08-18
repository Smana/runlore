// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Bounding a POSTED message — a progress ping, a delivered finding — as opposed
// to a thread reply, which reply_bound.go already handles.
//
// The two share their ceilings (a transport's limit is a property of the chat
// system, not of which message happens to be crossing it) and they share the
// drop-whole-lines rule. They do NOT share a function, because they cut opposite
// ends, and which end is not a preference:
//
//   - A thread reply is composed answer-then-record: the model's prose, then
//     RunLore's status line, then the note quoted back (thread.Responder.freeform).
//     The record of what happened is LAST, so boundPostedReply drops the front.
//   - A progress ping and a delivered card are composed headline-first:
//     FormatProgress leads with "Investigating: <title> — step 3/20", and Format
//     leads with title → verdict → confidence → top root cause and closes with
//     the provenance footer (verified / KB link / token cost). The answer is
//     FIRST, so this file drops the back.
//
// Cutting the wrong end is not a cosmetic loss in either direction: on a reply it
// drops the PR link the human cannot reconstruct, and on a card it drops the
// verdict and keeps the cost line.

// matrixDeliverBodyBytes bounds ONE of the two renderings Matrix.Deliver puts on
// a single event, in ENCODED bytes — what the rendering costs inside the JSON
// string it travels in, not how long the Go string is. See jsonBodyBytes for why
// those are different numbers and why only the first one is a bound on anything.
//
// It is not matrixReplyBytes and must not be derived from it. That constant is
// the share ONE body may claim of an event — half of the spec's hard 65,536-byte
// ceiling, with the other half left to signatures, hashes and relations. Deliver
// carries the same message TWICE, as the plaintext body and as the HTML
// formatted_body, and both count against the one event, so the two together get
// that half and each gets a quarter. Of the half left over, the stamps take
// 2 x matrixStampBytes and the envelope keeps the rest — Matrix.Deliver's doc
// comment adds it up.
const matrixDeliverBodyBytes = 16 << 10

// postTruncatedNotice is what a shortened post says instead of stopping silently.
//
// It has to be safe in three renderings at once, which constrains every byte of
// it. On the Slack path it is appended AFTER escapeMrkdwn, so it may carry no
// & < >. On the Matrix path it is appended to the SOURCE and then rendered — by
// plainFallback and by mrkdwnToHTML — so it may carry no * or backtick either,
// or the two bodies would disagree about where RunLore's own words start. And it
// must not carry "@room", which .m.rule.roomnotif would turn into a notification
// for every member of the room.
//
// It leads with the ⚠️ status glyph because it is a claim RunLore makes about
// what it did. A human who cannot tell a message was shortened reads what
// survived as the whole of it.
const postTruncatedNotice = "⚠️ (this message was too long to post — the rest was dropped)"

// boundPostedHead caps a fully rendered post at maxBytes, keeping the head.
//
// It runs on the RENDERED text — after the transport's escaper has been applied —
// because that is the string that goes on the wire. Bounding the composed message
// instead would bound the wrong thing: escapeMrkdwn turns each "&" into "&amp;",
// so a ping that measures well under budget as composed arrives five times over
// it, and the post is rejected outright.
//
// Whole LINES are dropped, never part of one. A mid-line cut leaves a fragment of
// model prose sitting at the left margin against RunLore's own status framing,
// with nothing marking where the model's words stopped — and on a rendered string
// it can also land inside an escape ("&amp;" cut to "&am") or, on the HTML side,
// inside a tag.
//
// The one case whole lines cannot fix is a first line longer than the whole
// budget — an untrusted title with no newline in it. Its head is kept, cut on a
// rune boundary so no invalid UTF-8 reaches the wire, which would be a rejected
// post arrived at from the other side.
func boundPostedHead(rendered string, maxBytes int) string {
	if maxBytes <= 0 || len(rendered) <= maxBytes {
		return rendered
	}
	// -1: the newline the notice is joined by. A ceiling too small to hold the
	// notice at all leaves nothing to say it with, so say nothing and just fit.
	budget := maxBytes - len(postTruncatedNotice) - 1
	if budget <= 0 {
		return cutBytesToRuneBoundary(rendered, maxBytes)
	}

	lines := strings.Split(rendered, "\n")
	kept, size := 0, 0
	for _, line := range lines {
		add := len(line)
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
		return cutBytesToRuneBoundary(lines[0], budget) + "\n" + postTruncatedNotice
	}
	return strings.Join(lines[:kept], "\n") + "\n" + postTruncatedNotice
}

// boundMatrixBodies renders msg into the two bodies Matrix.Deliver sends and
// bounds both, keeping the head.
//
// The two-body shape is what makes this its own function rather than two calls
// to boundPostedHead. body and formatted_body are two renderings of ONE message
// and a client shows whichever it supports, so they have to be cut at the same
// place. Bounding them independently would not: mrkdwnToHTML expands far more
// than plainFallback does — "&" to "&amp;", "\n" to "<br/>", "*x*" to
// "<strong>x</strong>" — so the HTML copy would lose more of the card, and two
// people in the same room would read different findings with nothing on either
// copy saying so.
//
// So the cut is chosen once, in the SOURCE, and both bodies are rendered from the
// same kept prefix. That also settles the tag question for free: mrkdwnToHTML's
// markup never spans a newline (boldRe and codeRe both exclude "\n"), so a
// whole-line prefix of the source converts to exactly the complete tags the full
// message would have produced for those lines. No cut can land inside "<strong>",
// "&amp;" or "<br/>".
func boundMatrixBodies(msg string) (body, formatted string) {
	body, formatted = renderMatrixBodies(msg)
	if jsonBodyBytes(body) <= matrixDeliverBodyBytes && jsonBodyBytes(formatted) <= matrixDeliverBodyBytes {
		return body, formatted
	}
	return renderMatrixBodies(boundMatrixSource(msg))
}

// renderMatrixBodies is the one place the two renderings of a Matrix message are
// composed, so the bound below measures precisely what Deliver sends.
func renderMatrixBodies(msg string) (body, formatted string) {
	return escapeMatrixReply(plainFallback(msg)), mrkdwnToHTML(msg)
}

// boundMatrixSource returns the head of msg — whole lines, plus the truncation
// notice — that both renderings fit their ceiling from.
//
// It accumulates the RENDERED size of each line rather than the source's, and
// that is exact rather than approximate because every transform involved is
// line-local: plainFallback's boldRe/codeRe and escapeMatrixReply's roomPingRe
// all exclude "\n", and mrkdwnToHTML's html escape is per-character with the
// newline itself rendered as the "<br/>" counted below. Rendering line by line
// therefore gives the same bytes as rendering the joined prefix.
func boundMatrixSource(msg string) string {
	plainNotice, richNotice := renderMatrixBodies("\n" + postTruncatedNotice)
	plainBudget := matrixDeliverBodyBytes - jsonBodyBytes(plainNotice)
	richBudget := matrixDeliverBodyBytes - jsonBodyBytes(richNotice)
	if plainBudget <= 0 || richBudget <= 0 {
		return ""
	}

	lines := strings.Split(msg, "\n")
	kept, plainSize, richSize := 0, 0, 0
	for _, line := range lines {
		plain, rich := renderMatrixBodies(line)
		addPlain, addRich := jsonBodyBytes(plain), jsonBodyBytes(rich)
		if kept > 0 {
			addPlain += jsonBodyBytes("\n")   // the newline this line is joined by
			addRich += jsonBodyBytes("<br/>") // which mrkdwnToHTML renders as a break
		}
		if plainSize+addPlain > plainBudget || richSize+addRich > richBudget {
			break
		}
		plainSize, richSize = plainSize+addPlain, richSize+addRich
		kept++
	}
	if kept == 0 {
		return cutMatrixLine(lines[0], plainBudget, richBudget) + "\n" + postTruncatedNotice
	}
	return strings.Join(lines[:kept], "\n") + "\n" + postTruncatedNotice
}

// cutMatrixLine handles the case dropping whole lines cannot: a single source
// line — a model title with no newline in it — longer on its own than the budget.
//
// It shrinks a candidate cut until both renderings fit, rather than dividing the
// budget by a worst-case expansion factor. There is no honest factor to divide
// by: html.EscapeString turns "&" into five bytes, and mrkdwnToHTML then turns a
// three-byte "*x*" into an eighteen-byte "<strong>x</strong>", so the two
// compound. Measuring what was actually produced is the only thing that cannot be
// wrong, and this path is reached only by a pathological message.
//
// The starting candidate is richBudget because the HTML rendering is never
// shorter than its input, so a longer prefix could not fit however it renders.
func cutMatrixLine(line string, plainBudget, richBudget int) string {
	for n := min(len(line), richBudget); n > 0; n = n * 3 / 4 {
		cut := cutBytesToRuneBoundary(line, n)
		plain, rich := renderMatrixBodies(cut)
		if jsonBodyBytes(plain) <= plainBudget && jsonBodyBytes(rich) <= richBudget {
			return cut
		}
	}
	return ""
}

// jsonBodyBytes is what s costs INSIDE a JSON string on the event: every escape
// the encoder applies, and nothing for the quotes around it, so the costs of
// consecutive lines add up exactly the way the lines themselves do.
//
// The bound above has to be counted in these bytes rather than in len(s),
// because len(s) is not a bound on anything the homeserver measures. The Matrix
// spec caps an event at 65,536 bytes "encoded as Canonical JSON", and canonical
// JSON renders every control character below 0x20 as a six-byte \u001b-style
// escape. A card whose evidence quotes a container log line carrying ANSI colour
// codes — an ordinary log line, not a crafted one — therefore measured 16,384
// bytes at the ceiling and arrived at 98,304, and the two bodies together came to
// three times the whole event budget while each sat exactly at its documented
// limit.
//
// SetEscapeHTML(false) is what makes this the homeserver's arithmetic rather
// than Go's. encoding/json escapes "<", ">" and "&" by default and canonical
// JSON does not; measuring with the default would count an HTML formatted_body
// at roughly twice its real cost and cut the card for a limit nothing enforces.
func jsonBodyBytes(s string) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string cannot fail to encode
	return buf.Len() - len(`""`) - len("\n")
}
