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
func SilenceAck(user string, window time.Duration, until time.Time) string {
	return fmt.Sprintf("🔕 Silenced by @%s until %s (%s).\n\n"+
		"⚠️ RunLore will NOT investigate this incident while the silence stands — "+
		"no model call, no notification, no record. A CRITICAL firing still breaks "+
		"through; a 👎 or a resolved alert re-arms it immediately.",
		user, until.Format("15:04 MST"), ShortDuration(window))
}
