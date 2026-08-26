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
// in small grey type and wraps badly; and it names the user with a leading @ so
// Slack renders the mention rather than a bare string.
func SilenceMarker(user string, window time.Duration, until time.Time) string {
	return fmt.Sprintf("🔕 Silenced by @%s until %s · %s",
		user, until.Format(silenceExpiryLayout), ShortDuration(window))
}
