// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// fakeDiscoverMetrics records the LabelValues call and returns canned values.
type fakeDiscoverMetrics struct {
	values     []string
	gotLabel   string
	gotMatch   []string
	gotWindow  providers.TimeWindow
	labelCalls int
}

func (f *fakeDiscoverMetrics) Query(context.Context, string, time.Time) (providers.Samples, error) {
	return nil, nil
}
func (f *fakeDiscoverMetrics) QueryRange(context.Context, string, providers.TimeWindow, time.Duration) (providers.Matrix, error) {
	return nil, nil
}
func (f *fakeDiscoverMetrics) LabelValues(_ context.Context, label string, matchers []string, w providers.TimeWindow) ([]string, error) {
	f.labelCalls++
	f.gotLabel, f.gotMatch, f.gotWindow = label, matchers, w
	return f.values, nil
}

func TestDiscoverMetricsTool(t *testing.T) {
	fm := &fakeDiscoverMetrics{values: []string{"up", "http_requests_total", "container_memory_working_set_bytes"}}
	tool := DiscoverMetricsTool{Metrics: fm}
	if tool.Name() != "discover_metrics" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"namespace":"apps","matcher":"pod=~\"harbor-.*\"","since_minutes":30}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if fm.gotLabel != "__name__" {
		t.Fatalf("label=%q, want __name__ (metric-name discovery default)", fm.gotLabel)
	}
	if len(fm.gotMatch) != 1 || fm.gotMatch[0] != `{namespace="apps",pod=~"harbor-.*"}` {
		t.Fatalf("matcher=%v", fm.gotMatch)
	}
	if d := fm.gotWindow.End.Sub(fm.gotWindow.Start); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("window=%v, want ~30m", d)
	}
	// Output is sorted (VM does not guarantee order) and lists the real names.
	if !strings.Contains(out, "container_memory_working_set_bytes") || !strings.Contains(out, "http_requests_total") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if i := strings.Index(out, "container_memory"); i < 0 || i > strings.Index(out, "http_requests_total") {
		t.Fatalf("values not sorted:\n%s", out)
	}
}

func TestDiscoverMetricsToolLabelOverride(t *testing.T) {
	fm := &fakeDiscoverMetrics{values: []string{"harbor-core-1", "harbor-core-2"}}
	tool := DiscoverMetricsTool{Metrics: fm}
	if _, err := tool.Call(context.Background(), `{"namespace":"apps","label":"pod"}`); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if fm.gotLabel != "pod" {
		t.Fatalf("label=%q, want pod", fm.gotLabel)
	}
}

func TestDiscoverMetricsToolEmpty(t *testing.T) {
	tool := DiscoverMetricsTool{Metrics: &fakeDiscoverMetrics{}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no __name__ values found") {
		t.Fatalf("want empty-discovery note, got:\n%s", out)
	}
}

func TestBuildMetricsMatcher(t *testing.T) {
	cases := []struct {
		name, ns, extra, want string
		wantErr               bool
	}{
		{name: "namespace only", ns: "apps", want: `{namespace="apps"}`},
		{name: "namespace + bare matcher", ns: "apps", extra: `pod=~"x"`, want: `{namespace="apps",pod=~"x"}`},
		{name: "namespace + braced matcher", ns: "apps", extra: `{pod=~"x"}`, want: `{namespace="apps",pod=~"x"}`},
		{name: "matcher only", extra: `job="api"`, want: `{job="api"}`},
		{name: "nothing", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildMetricsMatcher(tc.ns, tc.extra)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("buildMetricsMatcher = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeLogFields implements LogsProvider + LogFields.
type fakeLogFields struct {
	fields   []providers.FieldCount
	gotQuery string
}

func (f *fakeLogFields) Query(context.Context, string, providers.TimeWindow) (providers.LogResult, error) {
	return nil, nil
}
func (f *fakeLogFields) FieldNames(_ context.Context, query string, _ providers.TimeWindow) ([]providers.FieldCount, error) {
	f.gotQuery = query
	return f.fields, nil
}

func TestDiscoverLogFieldsTool(t *testing.T) {
	f := &fakeLogFields{fields: []providers.FieldCount{
		{Name: "_msg", Hits: 1000},
		{Name: "kubernetes.container_name", Hits: 900},
	}}
	tool := DiscoverLogFieldsTool{Logs: f}
	if tool.Name() != "discover_log_fields" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"namespace":"apps","container":"harbor-core"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if f.gotQuery != `{kubernetes.container_name="harbor-core",kubernetes.pod_namespace="apps"}` {
		t.Fatalf("query=%q", f.gotQuery)
	}
	if !strings.Contains(out, "kubernetes.container_name (×900)") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

// fakeLogsNoCaps implements only LogsProvider (no LogFields / LogStats), to prove
// graceful degradation.
type fakeLogsNoCaps struct{}

func (fakeLogsNoCaps) Query(context.Context, string, providers.TimeWindow) (providers.LogResult, error) {
	return nil, nil
}

func TestDiscoverLogFieldsToolNoCapability(t *testing.T) {
	tool := DiscoverLogFieldsTool{Logs: fakeLogsNoCaps{}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call must not error when capability is absent: %v", err)
	}
	if !strings.Contains(out, "discover_log_fields is unavailable") {
		t.Fatalf("want graceful note, got:\n%s", out)
	}
}

// TestDiscoverLogFieldsOmitsZeroCounts: Loki stream labels carry Hits=0 (no
// per-label hit count exists); the render must omit the count rather than
// print a misleading "(×0)".
func TestDiscoverLogFieldsOmitsZeroCounts(t *testing.T) {
	tool := DiscoverLogFieldsTool{Logs: &fakeLogFields{fields: []providers.FieldCount{
		{Name: "namespace"}, {Name: "level", Hits: 4},
	}}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(out, "(×0)") {
		t.Fatalf("zero hits must render without a count:\n%s", out)
	}
	if !strings.Contains(out, "level (×4)") {
		t.Fatalf("non-zero hits must keep the count:\n%s", out)
	}
}
