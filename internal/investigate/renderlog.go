// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"fmt"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// logGroup is one distinct log message with its repeat count and the time of its
// last occurrence — the compact form a crash loop's near-identical spam reduces to.
type logGroup struct {
	first providers.LogLine // first occurrence (its Time is the first-seen time)
	count int
	last  time.Time // last occurrence's time; zero when the source carries no timestamps
}

// groupLogLines collapses repeated identical messages (not just consecutive ones —
// a crash loop re-emits the same block every restart) into one group each, keeping
// first-occurrence order. This runs BEFORE the row cap, so the cap spends its
// budget on DISTINCT messages instead of 50 copies of the same panic line.
func groupLogLines(lines providers.LogResult) []logGroup {
	byMsg := map[string]int{} // message -> index into groups
	groups := make([]logGroup, 0, len(lines))
	for _, l := range lines {
		if i, ok := byMsg[l.Message]; ok {
			groups[i].count++
			if l.Time.After(groups[i].last) {
				groups[i].last = l.Time
			}
			continue
		}
		byMsg[l.Message] = len(groups)
		groups = append(groups, logGroup{first: l, count: 1, last: l.Time})
	}
	return groups
}

// renderLogLines is the shared renderer for log-shaped tool output (pod_logs,
// controller_logs, query_logs, network_drops): each DISTINCT message renders once
// with its first-seen timestamp (when the source has one) and, when repeated, a
// "(xN, last <time>)" suffix — so the model sees when a line first appeared and
// that it kept repeating, instead of 50 identical rows. Capped at maxToolRows
// distinct messages with the standard "… (N more <noun>)" note.
func renderLogLines(b *strings.Builder, lines providers.LogResult, noun string) {
	groups := groupLogLines(lines)
	renderRows(b, len(groups), noun, func(i int) {
		g := groups[i]
		if !g.first.Time.IsZero() {
			fmt.Fprintf(b, "%s ", g.first.Time.UTC().Format(time.RFC3339))
		}
		b.WriteString(g.first.Message)
		if g.count > 1 {
			if !g.last.IsZero() {
				fmt.Fprintf(b, " (x%d, last %s)", g.count, g.last.UTC().Format(time.RFC3339))
			} else {
				fmt.Fprintf(b, " (x%d)", g.count)
			}
		}
		b.WriteString("\n")
	})
}
