// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// logGroup is one distinct log message with its repeat count, the span over which
// it recurred, and the distinct pods it came from — the compact form a crash
// loop's near-identical spam reduces to.
type logGroup struct {
	first     providers.LogLine // representative line (its Message keys the group)
	identity  string            // compact stream identity (e.g. "harbor-core-6f9d…/core"); "" when the source bakes identity into Message
	count     int
	firstTime time.Time       // earliest occurrence (min Time); zero when the source carries no timestamps
	lastTime  time.Time       // latest occurrence (max Time); zero when the source carries no timestamps
	pods      map[string]bool // distinct FULL pod names contributing to this group (empty when none are derivable)
}

// reStreamPodHash truncates a Deployment/ReplicaSet pod name at its hash so distinct
// pods of the same controller read as one family while still being distinguishable:
// "harbor-core-6f9d5c8b7-abcde" → keep through the first 4 hash chars ("harbor-core-6f9d")
// so "…" can be appended. B4 (CORE-704): the model needs pod attribution but not the
// full volatile suffix, which only bloats tokens.
var reStreamPodHash = regexp.MustCompile(`^(.*-[a-f0-9]{4})[a-f0-9]{4,}-[a-z0-9]{5}$`)

// streamIdentity derives a compact pod/container identity from a log line's
// well-known VictoriaLogs stream fields (kubernetes.pod_name, kubernetes.container_name).
// It returns "" when those fields are absent — which is the normal case for the
// two sources that already bake identity INTO the message (pod_logs prefixes
// "<pod>: "; network providers render "src -> dst") — so the renderer never
// double-prefixes. B4 (CORE-704).
func streamIdentity(fields map[string]string) string {
	pod := fields["kubernetes.pod_name"]
	if pod == "" {
		return ""
	}
	if m := reStreamPodHash.FindStringSubmatch(pod); m != nil {
		pod = m[1] + "…"
	}
	if c := fields["kubernetes.container_name"]; c != "" {
		return pod + "/" + c
	}
	return pod
}

// groupLogLines collapses repeated identical messages (not just consecutive ones —
// a crash loop re-emits the same block every restart) into one group each, keeping
// first-occurrence order. This runs BEFORE the row cap, so the cap spends its
// budget on DISTINCT messages instead of 50 copies of the same panic line.
//
// The first→last span is tracked from the MIN and MAX Time explicitly, never from
// slice order: query_logs (VictoriaLogs) returns lines newest-first, so trusting
// order would report the newest occurrence as "first-seen" (B5, CORE-705). Distinct
// pods are counted per group so the same error on N pods reads as "across N pods"
// rather than being silently merged into one count (B4, CORE-704).
func groupLogLines(lines providers.LogResult) []logGroup {
	byMsg := map[string]int{} // message -> index into groups
	groups := make([]logGroup, 0, len(lines))
	for _, l := range lines {
		id := streamIdentity(l.Fields)
		// Distinct-pod counting keys on the FULL pod name, not the compact family
		// identity: three harbor-core pods share one truncated identity but are three
		// distinct pods ("across 3 pods"). Empty when the source carries no pod field.
		pod := l.Fields["kubernetes.pod_name"]
		if i, ok := byMsg[l.Message]; ok {
			g := &groups[i]
			g.count++
			if !l.Time.IsZero() && (g.lastTime.IsZero() || l.Time.After(g.lastTime)) {
				g.lastTime = l.Time
			}
			if !l.Time.IsZero() && (g.firstTime.IsZero() || l.Time.Before(g.firstTime)) {
				g.firstTime = l.Time
			}
			if pod != "" {
				g.pods[pod] = true
			}
			continue
		}
		byMsg[l.Message] = len(groups)
		g := logGroup{first: l, identity: id, count: 1, firstTime: l.Time, lastTime: l.Time, pods: map[string]bool{}}
		if pod != "" {
			g.pods[pod] = true
		}
		groups = append(groups, g)
	}
	return groups
}

// renderLogLines is the shared renderer for log-shaped tool output (pod_logs,
// controller_logs, query_logs, network_drops): each DISTINCT message renders once,
// prefixed with its stream identity when the source carries one in structured
// fields (query_logs) and not when it bakes identity into the message (pod_logs,
// network_drops — no double-prefix). The leading timestamp is the FIRST-seen time
// (min across occurrences); a repeated message adds a "(xN, first … → last …)"
// span plus "across N pods" when it spanned more than one pod — so the model sees
// when a line first appeared, that it kept repeating, and across how many pods,
// instead of 50 identical rows. Capped at maxToolRows distinct messages with the
// standard "… (N more <noun>)" note.
func renderLogLines(b *strings.Builder, lines providers.LogResult, noun string) {
	groups := groupLogLines(lines)
	renderRows(b, len(groups), noun, func(i int) {
		g := groups[i]
		if !g.firstTime.IsZero() {
			fmt.Fprintf(b, "%s ", g.firstTime.UTC().Format(time.RFC3339))
		}
		if g.identity != "" {
			fmt.Fprintf(b, "[%s] ", g.identity)
		}
		b.WriteString(g.first.Message)
		if g.count > 1 {
			b.WriteString(" (x")
			fmt.Fprintf(b, "%d", g.count)
			if len(g.pods) > 1 {
				fmt.Fprintf(b, " across %d pods", len(g.pods))
			}
			// Render the span only when timestamps exist; a source without them
			// (fake client, timestamp-less stream) still gets its count.
			if !g.firstTime.IsZero() && !g.lastTime.IsZero() {
				fmt.Fprintf(b, ", first %s → last %s", g.firstTime.UTC().Format(time.RFC3339), g.lastTime.UTC().Format(time.RFC3339))
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	})
}
