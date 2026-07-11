// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// LogFields names the collector's stream fields the renderer and query_logs read.
// It mirrors config.LogFields but lives here so the investigate package keeps its
// no-config-import boundary (the app layer converts). The zero value uses the
// shipped VictoriaLogs/vector convention, so callers that never set it (network_drops,
// pod_logs — sources that bake identity into the message) are unaffected.
type LogFields struct {
	ContainerField string // stream label for the container name; "" ⇒ default
	NamespaceField string // stream label for the namespace; "" ⇒ default
	PodField       string // stream label for the pod name; "" ⇒ default
	LevelField     string // severity field (after the unpack pipe); "" ⇒ default
	UnpackPipe     string // LogsQL pipe promoting JSON body to fields; "" ⇒ default
}

// Shipped log-field defaults. These MUST stay byte-identical to the strings the code
// hardcoded before logs.fields was configurable — they are the fallback that keeps
// the maintainer's test cluster working when the config is unset.
const (
	defaultContainerField = "kubernetes.container_name"
	defaultNamespaceField = "kubernetes.pod_namespace"
	defaultPodField       = "kubernetes.pod_name"
	defaultLevelField     = "log.level"
	defaultUnpackPipe     = "unpack_json"
)

// resolved fills every unset field from the shipped default so callers can use the
// result directly. An empty UnpackPipe restores the default pipe (disabling it is
// out of scope for v1).
func (f LogFields) resolved() LogFields {
	if f.ContainerField == "" {
		f.ContainerField = defaultContainerField
	}
	if f.NamespaceField == "" {
		f.NamespaceField = defaultNamespaceField
	}
	if f.PodField == "" {
		f.PodField = defaultPodField
	}
	if f.LevelField == "" {
		f.LevelField = defaultLevelField
	}
	if f.UnpackPipe == "" {
		f.UnpackPipe = defaultUnpackPipe
	}
	return f
}

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

// reNumToken matches a run of ASCII digits used as a whole numeric token — the
// volatile part of an otherwise-repeated log line ("took 12ms", "retry 3/5"). It is
// bounded by non-digit boundaries so it never splits an identifier's internal digits
// (a UUID/hash stays intact); only free-standing numbers collapse.
var reNumToken = regexp.MustCompile(`\d+`)

// collapseNums replaces every run of digits with a single "0" so log lines that
// differ only by a numeric value (latency, byte count, retry counter) share ONE
// dedup key — mirroring VictoriaLogs' collapse_nums pipe used by TopMessages, so the
// raw-line renderer and the analytics path group the same events the same way. This
// is only the GROUPING key; the representative line is displayed verbatim.
func collapseNums(msg string) string {
	return reNumToken.ReplaceAllString(msg, "0")
}

// streamIdentity derives a compact pod/container identity from a log line's
// well-known VictoriaLogs stream fields (kubernetes.pod_name, kubernetes.container_name).
// It returns "" when those fields are absent — which is the normal case for the
// two sources that already bake identity INTO the message (pod_logs prefixes
// "<pod>: "; network providers render "src -> dst") — so the renderer never
// double-prefixes. B4 (CORE-704).
func streamIdentity(fields map[string]string, conv LogFields) string {
	pod := fields[conv.PodField]
	if pod == "" {
		return ""
	}
	if m := reStreamPodHash.FindStringSubmatch(pod); m != nil {
		pod = m[1] + "…"
	}
	if c := fields[conv.ContainerField]; c != "" {
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
func groupLogLines(lines providers.LogResult, conv LogFields) []logGroup {
	byMsg := map[string]int{} // dedup key -> index into groups
	groups := make([]logGroup, 0, len(lines))
	for _, l := range lines {
		id := streamIdentity(l.Fields, conv)
		// Dedup keys on a numeric-token-collapsed form of the message (mirroring the
		// backend's collapse_nums), so "took 12ms" and "took 907ms" — the same event
		// with a volatile value — fold into ONE group instead of spending the row cap
		// on near-duplicates. The first line's ORIGINAL message is still displayed;
		// only the grouping key is collapsed.
		key := collapseNums(l.Message)
		// Distinct-pod counting keys on the FULL pod name, not the compact family
		// identity: three harbor-core pods share one truncated identity but are three
		// distinct pods ("across 3 pods"). Empty when the source carries no pod field.
		pod := l.Fields[conv.PodField]
		if i, ok := byMsg[key]; ok {
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
		byMsg[key] = len(groups)
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
	// The three sources that route through this arity — pod_logs, controller_logs,
	// network_drops — bake identity INTO the message and carry no structured stream
	// fields, so the field convention is irrelevant to them; use the default.
	renderLogLinesWith(b, lines, noun, LogFields{})
}

// renderLogLinesWith is the field-aware variant: query_logs (VictoriaLogs) carries
// structured stream fields whose names may be retargeted by config.logs.fields, so it
// passes the resolved convention. A zero LogFields yields the shipped default.
func renderLogLinesWith(b *strings.Builder, lines providers.LogResult, noun string, conv LogFields) {
	groups := groupLogLines(lines, conv.resolved())
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
