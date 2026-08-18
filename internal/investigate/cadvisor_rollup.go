// SPDX-License-Identifier: Apache-2.0

package investigate

import "regexp"

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
	// cadvisorRollupMetric matches a cadvisor metric family that carries a pod-level
	// rollup series, together with the label matcher block that follows it (capture
	// group 2, empty when the metric is written bare).
	//
	// container_network_* is deliberately ABSENT: those counters are reported per pod
	// sandbox only, with no per-container series to add to, so summing them is correct
	// and an advisory there would be a false positive.
	//
	// The leading (?:^|[^\w:]) rejects RECORDING RULE names, and the (:?) capture
	// rejects them from the other side. A plain \b would match inside
	// node_namespace_pod_container:container_memory_working_set_bytes:sum_irate,
	// because ':' is a word boundary — and since a recording rule can never carry a
	// {container!=""} matcher on the raw family, that would be a permanent false
	// positive on a metric name most stock dashboards use. Recording rules are
	// pre-aggregated by whoever defined them, so this detector cannot judge them.
	cadvisorRollupMetric = regexp.MustCompile(
		`(?:^|[^\w:])container_(?:memory|cpu|fs|blkio|spec)_[a-z0-9_]+(:?)\s*(\{[^}]*\})?`)

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
	//
	// This check is query-WIDE, not per-aggregation: one `by (container)` anywhere
	// silences the whole query, even for a second term that genuinely double-counts.
	// Scoping it to the aggregation that encloses each metric needs a real parser, and
	// erring toward silence is the direction this detector deliberately errs in.
	aggregationByContainer = regexp.MustCompile(`\bby\b\s*\([^)]*\bcontainer\b[^)]*\)`)

	// collapsingByPod matches a max/min/avg aggregation grouped per pod. Such an
	// aggregation collapses {container, pause, rollup} to ONE value per pod BEFORE any
	// outer sum sees them, so `sum(max by (namespace,pod) (…))` — the very shape
	// cadvisorRollupNote tells the model to switch to — does not double-count.
	// Warning on the recommended fix is the fastest way to teach the model to ignore
	// the advisory, so it is suppressed. The trailing \b on each keyword keeps
	// max_over_time/avg_over_time out: those collapse over TIME within one series and
	// leave the rollup series intact, so an outer sum still double-counts.
	//
	// Like aggregationByContainer this is query-WIDE; see the note there.
	collapsingByPod = regexp.MustCompile(`\b(?:max|min|avg)\b\s*by\b\s*\([^)]*\bpod\b[^)]*\)`)

	// rollupLabelMatcher matches one label matcher that can make a selector
	// unambiguous about the rollup, capturing its operator (1) and quoted value (2).
	// Three labels qualify, and all three are in real-world use:
	//   container — container!="", container="dev", container=~"…"
	//   image     — image!="" is what kube-prometheus's kubelet recording rules use
	//   name      — name=~".+" is the cadvisor-dashboard equivalent
	// The rollup series carries all three empty, so constraining any one of them is a
	// deliberate statement about it. Matching only `container` reported the other two
	// canonical idioms as unguarded.
	rollupLabelMatcher = regexp.MustCompile(
		`\b(?:container|image|name)\b\s*(=~|!~|!=|=)\s*("[^"]*"|'[^']*'|` + "`[^`]*`" + `)`)
)

// cadvisorRollupNote is the advisory prepended to a metrics tool result whose query
// would double-count. It names the concrete fix and a falsification step, because
// the failure mode is a confidently wrong number rather than an error: an inflated
// total looks plausible until it is compared with the node's real capacity.
const cadvisorRollupNote = `warning: possible DOUBLE-COUNT in this query. cadvisor exports every cgroup metric twice per pod — once per container, and again as a pod-level rollup series carrying container="" — so sum()/count() over container_memory_*/container_cpu_*/container_fs_*/container_blkio_*/container_spec_* counts each pod twice (a real 17.1 GB pod reads as 32.5 GB, and a node's pods can appear to exceed its physical memory).
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
	// Blank comments and string-literal contents first so only real syntax is matched:
	// {agg="sum"} is not an aggregation, `# not a sum(x)` is not one either, and
	// {__name__="container_memory_…"} is not a selector this detector can reason about.
	q := maskLiteralsAndComments(query)
	if !summingAggregation.MatchString(q) ||
		aggregationByContainer.MatchString(q) ||
		collapsingByPod.MatchString(q) {
		return ""
	}
	for _, m := range cadvisorRollupMetric.FindAllStringSubmatch(q, -1) {
		// m[1] is ":" when the family name is really the head of a recording rule
		// name; m[2] is the selector, "" for a bare metric — which constrains nothing.
		if m[1] == ":" {
			continue
		}
		if !selectorExcludesRollup(m[2]) {
			return cadvisorRollupNote
		}
	}
	return ""
}

// selectorExcludesRollup reports whether a label matcher block is explicit about the
// container="" pod rollup — either excluding it or selecting it alone. An empty
// selector (a bare metric) constrains nothing and so returns false.
func selectorExcludesRollup(selector string) bool {
	for _, m := range rollupLabelMatcher.FindAllStringSubmatch(selector, -1) {
		// container!="POD" — the legacy cAdvisor idiom for dropping the pause
		// container — is NOT a guard: it keeps the container="" rollup, so it
		// double-counts exactly like an unguarded selector. Only a NEGATIVE match
		// against a non-empty value behaves this way; container!="" excludes the
		// rollup and container="dev" selects one container. Literal contents are
		// masked to spaces but their LENGTH is preserved, so `""` (2 bytes with its
		// quotes) is still distinguishable here from `"POD"`.
		if m[1] == "!=" && len(m[2]) > 2 {
			continue
		}
		return true
	}
	return false
}

// maskLiteralsAndComments blanks the CONTENTS of every PromQL string literal (double,
// single or back quoted) and every `#` comment, in one left-to-right pass so that each
// construct is only recognised outside the other: a '#' inside {pod="a#b"} does not
// start a comment, and an apostrophe inside `# it's the rollup` does not open a
// literal that swallows the rest of the query.
func maskLiteralsAndComments(q string) string {
	out := []byte(q)
	i := 0
	for i < len(out) {
		quote := out[i]
		if quote == '#' {
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
			continue
		}
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
