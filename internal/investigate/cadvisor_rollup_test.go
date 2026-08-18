// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// cadvisor exports every cgroup-accounted metric twice per pod: once per container
// and once more as a pod-level rollup carrying container="". Summing without
// excluding the rollup double-counts the pod. Observed live: a 17.1 GB workspace
// pod reported as 32.48 GB (17100390400 + 229376 pause + 15384018944 rollup), and
// the same query over a whole node returning 50.2 GB of pods on a 33 GB node.
func TestCadvisorRollupWarning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  bool
	}{
		{"sum of working set, no container matcher", `sum(container_memory_working_set_bytes{node="n1"})`, true},
		{"sum by pod, no container matcher", `sum by (pod) (container_memory_working_set_bytes{node="n1"})`, true},
		{"bare metric, no selector at all", `sum by(namespace,pod) (container_memory_working_set_bytes)`, true},
		{"count double-counts too", `count(container_memory_usage_bytes)`, true},
		{"sum of cpu rate", `sum(rate(container_cpu_usage_seconds_total[5m]))`, true},
		{"sum of fs usage", `sum(container_fs_usage_bytes{namespace="ns"})`, true},
		{"blkio family rolls up as well", `sum(container_blkio_device_usage_total)`, true},
		{"without(container) drops the guard label, still double-counts", `sum without (container) (container_memory_rss)`, true},
		{"only the second selector is guarded", `sum(container_memory_working_set_bytes{container!=""}) / sum(container_memory_usage_bytes)`, true},

		{"guarded with container!=''", `sum(container_memory_working_set_bytes{container!=""})`, false},
		{"pinned to one container", `sum(container_memory_working_set_bytes{container="dev"})`, false},
		{"regex container matcher", `sum(container_memory_working_set_bytes{container=~"dev|cache"})`, false},
		{"guarded cpu rate", `sum(rate(container_cpu_usage_seconds_total{container!=""}[5m]))`, false},
		{"max is not a summing aggregation", `max by (pod) (container_memory_working_set_bytes)`, false},
		{"avg is not a summing aggregation", `avg by (pod) (container_memory_working_set_bytes)`, false},
		{"the recommended shape", `topk(5, max by (namespace,pod) (container_memory_working_set_bytes{node="n1",container!=""}))`, false},
		{"grouping by container keeps the rollup in its own series", `sum by (namespace,pod,container) (container_memory_working_set_bytes)`, false},
		{"sum_over_time aggregates over time, not over series", `sum_over_time(container_memory_working_set_bytes[5m])`, false},
		{"network metrics are pod-level only", `sum(container_network_receive_bytes_total)`, false},
		{"network rate is pod-level only", `sum by (pod) (rate(container_network_transmit_bytes_total[5m]))`, false},
		{"no selector, no aggregation", `container_memory_working_set_bytes{pod="p"}`, false},
		{"sum inside a label value is not an aggregation", `max by (pod) (container_memory_working_set_bytes{agg="sum"})`, false},
		{"metric named only inside a string literal", `sum({__name__="container_memory_working_set_bytes"})`, false},
		{"unrelated metric", `sum(kube_pod_status_phase)`, false},

		// container_spec_* carries the same pod-level rollup, and "total limits on this
		// node" is exactly the capacity cross-check the advisory itself recommends.
		{"spec family rolls up as well", `sum(container_spec_memory_limit_bytes)`, true},
		{"guarded spec family", `sum(container_spec_memory_limit_bytes{container!=""})`, false},

		// image!="" is kube-prometheus's kubelet-rule idiom for dropping the rollup and
		// name=~".+" the cadvisor-dashboard one; the rollup carries all three labels
		// empty, so constraining any of them is a deliberate statement about it.
		{"image!='' excludes the rollup too", `sum(container_memory_working_set_bytes{image!="",node="n1"})`, false},
		{"name matcher excludes the rollup too", `sum(container_memory_working_set_bytes{name=~".+"})`, false},

		// A recording rule is pre-aggregated by whoever defined it and can never carry a
		// matcher on the raw family, so matching inside its name is a permanent false
		// positive. ':' is a word boundary, so \b alone does not exclude either side.
		{"family name inside a recording rule name", `sum(node_namespace_pod_container:container_memory_working_set_bytes:sum_irate)`, false},
		{"family name heading a recording rule name", `sum(container_memory_working_set_bytes:sum_irate)`, false},

		// Comments are masked in the same pass as literals, so neither construct can be
		// recognised inside the other.
		{"apostrophe in a comment must not blank the query", "# it's the rollup\nsum(container_memory_working_set_bytes{node=\"n1\"})", true},
		{"by (container) in a comment is not a grouping", `sum(container_memory_working_set_bytes{node="n1"}) # by (container)`, true},
		{"sum( in a comment is not an aggregation", `max by (pod) (container_memory_working_set_bytes) # not a sum(x)`, false},
		{"# inside a label value is not a comment", `sum(container_memory_working_set_bytes{pod="a#b"})`, true},

		// The selector is attached to its metric across whitespace, and an unterminated
		// brace constrains nothing.
		{"space between metric and selector", `sum(container_memory_working_set_bytes {container!=""})`, false},
		{"unterminated selector constrains nothing", `sum(container_memory_working_set_bytes{node="n1"`, true},

		// max/min/avg grouped per pod collapses {container, pause, rollup} to one value
		// per pod BEFORE the outer sum, so this is the corrected shape the advisory
		// itself recommends — warning on it would train the model to ignore the advisory.
		{"sum over a per-pod max does not double-count", `sum(max by (namespace,pod) (container_memory_working_set_bytes{node="n1"}))`, false},
		{"sum by node over a per-pod max", `sum by (node) (max by (namespace,pod,node) (container_memory_working_set_bytes))`, false},
		{"cadvisor metric never summed at all", `sum(node_memory_MemTotal_bytes) - max by (pod) (container_memory_working_set_bytes)`, false},
		// max_over_time collapses over TIME within one series and leaves the rollup
		// series intact, so the outer sum still double-counts.
		{"max_over_time is not a per-pod collapse", `sum(max_over_time(container_memory_working_set_bytes[1h]))`, true},

		// container!="POD" is the legacy cAdvisor idiom for dropping the pause container.
		// It KEEPS the container="" rollup, so it double-counts like an unguarded query.
		{"container!='POD' still admits the rollup", `sum(container_memory_working_set_bytes{container!="POD"})`, true},
		{"POD exclusion plus a real guard", `sum(container_memory_working_set_bytes{container!="POD",container!=""})`, false},
		{"negative image match is not a guard", `sum(container_memory_working_set_bytes{image!="pause"})`, true},
		{"empty query", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cadvisorRollupAdvisory(tc.query) != ""
			if got != tc.want {
				t.Fatalf("cadvisorRollupAdvisory(%q) warned=%v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// The warning is only useful if it names the concrete fix and a way to falsify the
// number, so an inflated total can't be shipped as a verdict.
func TestCadvisorRollupWarningNamesFixAndCrossCheck(t *testing.T) {
	w := cadvisorRollupAdvisory(`sum by (pod) (container_memory_working_set_bytes{node="n1"})`)
	for _, want := range []string{`container!=""`, "node_memory_MemTotal_bytes", "kube_node_status_capacity"} {
		if !strings.Contains(w, want) {
			t.Fatalf("warning must mention %q, got:\n%s", want, w)
		}
	}
}

// The warning has to reach the model, so it is prepended to the rendered result
// rather than logged — an advisory the investigation can act on mid-loop.
func TestQueryMetricsToolWarnsOnRollupDoubleCount(t *testing.T) {
	tool := QueryMetricsTool{Metrics: fakeMetrics{samples: providers.Samples{
		{Metric: map[string]string{"__name__": "container_memory_working_set_bytes", "pod": "p"}, Value: 32484638720},
	}}}
	out, err := tool.Call(context.Background(), `{"query":"sum by (pod) (container_memory_working_set_bytes{node=\"n1\"})"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, `container!=""`) {
		t.Fatalf("expected a double-count warning naming the fix, got:\n%s", out)
	}
	if !strings.Contains(out, "container_memory_working_set_bytes{pod=\"p\"}") {
		t.Fatalf("warning must not replace the result rows, got:\n%s", out)
	}
}

// A guarded query must stay clean: a spurious advisory on every metrics call would
// train the model to ignore it.
func TestQueryMetricsToolNoWarningWhenGuarded(t *testing.T) {
	tool := QueryMetricsTool{Metrics: fakeMetrics{samples: providers.Samples{
		{Metric: map[string]string{"__name__": "container_memory_working_set_bytes", "pod": "p"}, Value: 17100390400},
	}}}
	out, err := tool.Call(context.Background(), `{"query":"max by (pod) (container_memory_working_set_bytes{container!=\"\"})"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(out, "double-count") {
		t.Fatalf("guarded query must not be warned about, got:\n%s", out)
	}
}

// The range tool runs the same PromQL against the same backend, so it double-counts
// identically — and it is the tool an investigation uses to establish a peak.
func TestQueryMetricsRangeToolWarnsOnRollupDoubleCount(t *testing.T) {
	fm := &fakeRangeMetrics{matrix: providers.Matrix{{
		Metric: map[string]string{"__name__": "container_memory_working_set_bytes", "pod": "p"},
		Points: []providers.Point{
			{Time: time.Unix(1700000000, 0).UTC(), Value: 17100390400},
			{Time: time.Unix(1700000060, 0).UTC(), Value: 32484638720},
		},
	}}}
	tool := QueryMetricsRangeTool{Metrics: fm}
	out, err := tool.Call(context.Background(), `{"query":"sum by (pod) (container_memory_working_set_bytes{node=\"n1\"})","since_minutes":30,"step_seconds":60}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, `container!=""`) {
		t.Fatalf("expected a double-count warning naming the fix, got:\n%s", out)
	}
	if !strings.Contains(out, "first=1.71003904e+10") {
		t.Fatalf("warning must not replace the trend rows, got:\n%s", out)
	}
}
