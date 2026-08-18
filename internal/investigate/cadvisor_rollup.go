// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"regexp"
	"strings"
)

// cadvisor exports every cgroup-accounted metric TWICE per pod: once per container,
// and again as a pod-level rollup series carrying container="". Any aggregation that
// SUMS across series therefore counts each pod twice. Measured live on one workspace
// pod: container="dev" 17_100_390_400 + pause 229_376 + pod rollup 15_384_018_944 =
// 32_484_638_720 — exactly the "32.48 GB" a verdict card reported for a pod whose
// real peak was 17.1 GB. The same unguarded shape over a whole node returned 50.2 GB
// of pods on a 33 GB node, and it shipped as "verified · high confidence".
//
// The detector below is ADVISORY ONLY: it never rewrites the query. RunLore hands the
// model guidance and lets it choose (see metricsQLGuidance in query_tools.go); the
// note is prepended to the rendered result so the model can re-run a corrected query
// mid-loop, with the original rows still in front of it.

var (
	// cadvisorRollupMetric matches the cadvisor metric families that carry a
	// pod-level rollup series. container_network_* is deliberately ABSENT: those
	// counters are reported per pod sandbox only, with no per-container series to add
	// to, so summing them is correct and an advisory there would be a false positive.
	cadvisorRollupMetric = regexp.MustCompile(`\bcontainer_(?:memory|cpu|fs|blkio)_[a-z0-9_]+`)

	// summingAggregation matches sum/count used as PromQL AGGREGATION operators:
	// `sum(`, `sum by (…)`, `count without (…)`. The trailing \b on the keyword keeps
	// sum_over_time / count_over_time / count_values out — they aggregate within a
	// single series, so they cannot fold the rollup into a total. max/min/avg/topk are
	// excluded by construction: `max by (namespace,pod) (…)` is the shape to prefer.
	summingAggregation = regexp.MustCompile(`\b(?:sum|count)\b\s*(?:\(|by\b|without\b)`)

	// aggregationByContainer matches a `by (…container…)` grouping, which keeps the
	// rollup in an output series of its own instead of folding it into the pod's
	// total — so such a query does not double-count. `without (container)` is the
	// opposite (it merges the rollup in) and is deliberately not matched.
	aggregationByContainer = regexp.MustCompile(`\bby\b\s*\([^)]*\bcontainer\b[^)]*\)`)

	// containerMatcher matches any label matcher that constrains `container`
	// (container!="", container="dev", container=~"…"), which is what makes a
	// selector unambiguous about the rollup.
	containerMatcher = regexp.MustCompile(`\bcontainer\b\s*(?:=~|!~|!=|=)`)
)

// cadvisorRollupNote is the advisory prepended to a metrics tool result whose query
// would double-count. It names the concrete fix and a falsification step, because
// the failure mode is a confidently wrong number rather than an error: an inflated
// total looks plausible until it is compared with the node's real capacity.
const cadvisorRollupNote = `warning: possible DOUBLE-COUNT in this query. cadvisor exports every cgroup metric twice per pod — once per container, and again as a pod-level rollup series carrying container="" — so sum()/count() over container_memory_*/container_cpu_*/container_fs_*/container_blkio_* counts each pod twice (a real 17.1 GB pod reads as 32.5 GB, and a node's pods can appear to exceed its physical memory).
fix: add container!="" to the selector, or aggregate with max by (namespace,pod) (…) instead of sum. Before reporting any total, cross-check it against real capacity — node_memory_MemTotal_bytes, or kube_node_status_capacity{resource="memory"} — a pod total above capacity means the rollup is still in the sum.
The rows below are the UNCHANGED result of the query as written, so they may be inflated.`

// cadvisorRollupAdvisory returns cadvisorRollupNote when query sums cadvisor series
// across a pod's containers without excluding the pod-level rollup, and "" when it
// does not. It is a deliberately conservative regex check, not a PromQL parser: an
// advisory on every metrics call would train the model to ignore it, so ambiguous
// shapes stay silent (false negatives are preferred over false positives).
//
// The name deliberately does NOT end in "Warning": that suffix is reserved for the
// operator-facing STARTUP guards whose message must reach log.Warn from exactly one
// call site, a property internal/app/warnings_wired_test.go enforces by scanning for
// the suffix. This is a per-tool-call advisory to the MODEL, raised from both metrics
// tools and never logged, so it is a different shape — do not rename it back.
func cadvisorRollupAdvisory(query string) string {
	if query == "" {
		return ""
	}
	// Mask string-literal contents first so only real syntax is matched:
	// {agg="sum"} is not an aggregation, and {__name__="container_memory_…"} is not a
	// selector this detector can reason about.
	q := maskStringLiterals(query)
	if !summingAggregation.MatchString(q) || aggregationByContainer.MatchString(q) {
		return ""
	}
	for _, m := range cadvisorRollupMetric.FindAllStringIndex(q, -1) {
		if !selectorConstrainsContainer(q, m[1]) {
			return cadvisorRollupNote
		}
	}
	return ""
}

// selectorConstrainsContainer reports whether the label matcher block starting at
// from (the end of a metric name) constrains the container label. A metric with no
// braces at all — `sum(container_memory_working_set_bytes)` — constrains nothing and
// so returns false.
func selectorConstrainsContainer(q string, from int) bool {
	rest := strings.TrimLeft(q[from:], " \t\n")
	if !strings.HasPrefix(rest, "{") {
		return false
	}
	end := strings.IndexByte(rest, '}')
	if end < 0 {
		return false
	}
	return containerMatcher.MatchString(rest[1:end])
}

// maskStringLiterals blanks the CONTENTS of every PromQL string literal (double,
// single or back quoted) while preserving the query's byte length, so offsets taken
// from the masked copy still line up with the original query.
func maskStringLiterals(q string) string {
	out := []byte(q)
	i := 0
	for i < len(out) {
		quote := out[i]
		if quote != '"' && quote != '\'' && quote != '`' {
			i++
			continue
		}
		i++ // step past the opening quote
		for i < len(out) && out[i] != quote {
			// PromQL escapes with a backslash inside "" and '' (but not inside ``), so an
			// escaped quote must not be mistaken for the terminator.
			if out[i] == '\\' && quote != '`' && i+1 < len(out) {
				out[i] = ' '
				i++
			}
			out[i] = ' '
			i++
		}
		i++ // step past the closing quote (or past the end of an unterminated literal)
	}
	return string(out)
}
