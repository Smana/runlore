// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"fmt"
	"strings"
	"time"
)

// ShortDuration renders a silence window the way an operator writes it — the way
// notify.silence.windows spells it in YAML, and the way every docs page quotes it
// ("1h", "4h", "24h").
//
// time.Duration.String() always appends the zero minute/second tail, so a preset
// documented as `1h` reached humans as "🔕 Silence 1h0m0s" on the Slack card and
// "(1h0m0s)" in the acknowledgement, and the thread command's error advertised a
// cap of "24h0m0s" nobody would type. That is a small ugliness with a real cost:
// the control is supposed to look like the config it came from.
//
// It lives here, beside SilenceAck, for the same reason that does: it is used on
// every transport (internal/notify renders the Slack labels through it) and a
// second copy would drift into two spellings of the same window.
//
// Both trims are SUFFIX-anchored on the unit letter that must precede them, so a
// value merely ending in those digits is untouched: "10s" keeps its zero, and
// "1m10s" is not mistaken for a tail to strip. The result always still parses
// through time.ParseDuration.
func ShortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// silenceExpiryLayout dates the expiry rather than printing a bare clock time.
//
// "15:04 MST" made the SHIPPED DEFAULT the ambiguous case, not an edge one: 24h
// is the window values-full.yaml and both docs pages configure, so a 🔕 clicked
// at 11:58 acked as "until 11:58 UTC" — the same clock time it was clicked at,
// which reads as "expires now" — and any window past 12h reads as a time in the
// PAST (a 20h silence at 18:00 acked "until 14:00"). An operator cannot act on
// that, and the ack's whole job is to say when the channel goes quiet until.
//
// ISO-ordered rather than a localised day name: the reader is an on-call
// engineer, the zone is whatever the process runs in, and yyyy-mm-dd is the one
// ordering nobody has to guess at.
const silenceExpiryLayout = "2006-01-02 15:04 MST"

// SilenceAck is the message posted back after a human silences an
// investigation, on EVERY transport. It carries an explicit WARNING, because a
// silence is the one feedback verdict that changes what RunLore does: a reader
// who clicked expecting "note my opinion" has in fact switched off
// investigation for this incident, and the escape hatches are only reassuring
// if they are stated at the point of the click.
//
// It lives here, shared, rather than being spelled once per transport: two
// copies would drift, and a silence meaning something subtly different in Slack
// than in Matrix is exactly the confusion the warning exists to prevent.
//
// feedbackEnabled reports whether ANY enabled transport offers a 👍/👎 control
// (notify.slack.feedback_buttons or notify.matrix.feedback_reactions). It is a
// deployment-wide fact rather than a per-transport one because the votes and the
// silences share one ledger: a 👎 cast in Matrix re-arms a silence clicked in
// Slack.
//
// It exists because the ack promised "a 👎 … re-arms it immediately"
// unconditionally, while the Slack docs RECOMMEND the deployment where that is
// false — feedback_buttons off, silence_button on, which the same page describes
// as showing "the 🔕 menu and nothing else". There is then no way to cast a 👎 at
// all; the Matrix equivalent (silence_reactions on, feedback_reactions off) drops
// a 👎 in handleReaction. Pair that with a GitOps-sourced trigger — no severity,
// so the CRITICAL carve-out never fires; a synthetic fingerprint, so no resolve
// can ever arrive — and the real bound is EXPIRY ALONE, while the ack named
// three escapes and the security argument for leaving silencing unprivileged
// rested on four. The escape is named only where it exists, and its absence is
// stated rather than quietly dropped: a reader who was told three bounds and has
// one is worse off than one who was told the truth.
func SilenceAck(user string, window time.Duration, until time.Time, feedbackEnabled bool) string {
	escapes := "A CRITICAL firing still breaks through; a 👎 re-arms it immediately, " +
		"and so does the alert resolving."
	if !feedbackEnabled {
		escapes = "A CRITICAL firing still breaks through, and the alert resolving re-arms it. " +
			"👍/👎 feedback is not enabled here, so no 👎 can lift this silence early."
	}
	return fmt.Sprintf("🔕 Silenced by @%s until %s (%s).\n\n"+
		"⚠️ RunLore will NOT investigate this incident while the silence stands — "+
		"no model call, no notification, no record. %s",
		user, until.Format(silenceExpiryLayout), ShortDuration(window), escapes)
}

// SilenceCardUnmarked is appended to the acknowledgement when the silence was
// recorded but the card could NOT be stamped — Slack sent no blocks to rebuild
// from, or refused the rewrite.
//
// It is disclosure, not an error: the suppression is in the ledger and is real.
// What failed is the announcement, and the person who clicked is the only one who
// can now make it. Left unsaid, they would reasonably assume the channel had been
// told — which is precisely the "a colleague starts investigating something
// already handled" failure the marker exists to prevent, restored silently.
const SilenceCardUnmarked = "⚠️ The silence is recorded, but the card itself could not be marked, " +
	"so the channel cannot tell this finding is handled. Say so in the thread if someone else might pick it up."

// SilenceCardStale is appended when the click DID change the ledger but the card
// already carried a marker from an earlier silence, so it could not be re-marked.
//
// Distinct from SilenceCardUnmarked, and the distinction is the whole point: there
// the channel can see nothing, here it can see the WRONG thing. Telling someone
// "the channel cannot tell this finding is handled" about a card that plainly says
// it is handled sends them to write a redundant note, while the real problem — the
// card advertises the earlier window and the ledger now holds theirs — goes unsaid.
//
// It does not repeat the new window because the acknowledgement it is appended to
// has just stated it in full; "the one above" is that line.
const SilenceCardStale = "⚠️ This card already carried a silence marker, so it could not be re-marked: " +
	"the window shown on it is the earlier one, not the one above. Say so in the thread if the difference matters."

// SilenceMarker is the one-line stamp left ON THE CARD after a silence, as
// distinct from SilenceAck's full explanation to the person who clicked.
//
// Two different readers, which is why it is a second function and not a shorter
// SilenceAck: the ack answers "what did I just do?" for the clicker and is
// allowed to spend three sentences on the consequences, while the marker answers
// "has anyone already dealt with this?" for a colleague scrolling the channel a
// day later. That reader is scanning, not reading, so the marker states only who
// and until when.
//
// It is a single line because it renders in a Slack context block, which is set
// in small grey type and wraps badly.
//
// userRef and untilText are both inserted VERBATIM: this function owns the word
// order, the 🔕 and the window, and the TRANSPORT owns how a person and a time
// are drawn. That split is the same one RenderReply makes by taking an escaper,
// and it is here because both halves were got wrong while thread rendered them
// itself. A bare "@bob" is not a mention — only "<@U9>" is linkified inside a
// Block Kit mrkdwn element, since link_names is a chat.postMessage option with no
// effect on blocks. And a time formatted here is formatted in the RunLore
// process's timezone, while every other time on the card is a Slack date token
// that localises per reader. Neither failure is visible to anyone reading the
// string in the zone that wrote it, which is why the caller must state the shape.
func SilenceMarker(userRef, untilText string, window time.Duration) string {
	return fmt.Sprintf("🔕 Silenced by %s until %s · %s",
		userRef, untilText, ShortDuration(window))
}
