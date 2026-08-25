// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"fmt"
	"time"
)

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
		user, until.Format("15:04 MST"), window)
}
