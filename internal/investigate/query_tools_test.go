// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

type fakeMetrics struct{ samples providers.Samples }

func (f fakeMetrics) Query(context.Context, string, time.Time) (providers.Samples, error) {
	return f.samples, nil
}
func (f fakeMetrics) QueryRange(context.Context, string, providers.TimeWindow, time.Duration) (providers.Matrix, error) {
	return nil, nil
}

func TestQueryMetricsTool(t *testing.T) {
	tool := QueryMetricsTool{Metrics: fakeMetrics{samples: providers.Samples{
		{Metric: map[string]string{"__name__": "up", "job": "api"}, Value: 0},
		{Metric: map[string]string{"__name__": "up", "job": "db"}, Value: 1},
	}}}
	if tool.Name() != "query_metrics" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"query":"up"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, `up{job="api"} = 0`) || !strings.Contains(out, `up{job="db"} = 1`) {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

type fakeNetwork struct{ lines providers.LogResult }

func (f fakeNetwork) Drops(context.Context, providers.Selector, providers.TimeWindow) (providers.LogResult, error) {
	return f.lines, nil
}

func TestNetworkDropsTool(t *testing.T) {
	tool := NetworkDropsTool{Network: fakeNetwork{lines: providers.LogResult{
		{Message: "apps/harbor-core-1 -> db/postgres-0 DROPPED (POLICY_DENIED)"},
	}}}
	if tool.Name() != "network_drops" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"namespace":"apps","since_minutes":30}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "POLICY_DENIED") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

type fakeLogs struct{ lines providers.LogResult }

func (f fakeLogs) Query(context.Context, string, providers.TimeWindow) (providers.LogResult, error) {
	return f.lines, nil
}

func TestQueryLogsTool(t *testing.T) {
	tool := QueryLogsTool{Logs: fakeLogs{lines: providers.LogResult{
		{Time: time.Unix(1700000000, 0).UTC(), Message: "db connection refused"},
	}}}
	if tool.Name() != "query_logs" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"query":"error","since_minutes":30}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "db connection refused") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestBuildLogsQL(t *testing.T) {
	cases := []struct {
		name                             string
		raw, container, namespace, level string
		want                             string
		wantErr                          bool
	}{
		{name: "structured with level", container: "kustomize-controller", namespace: "flux-system", level: "error",
			want: `{kubernetes.container_name="kustomize-controller",kubernetes.pod_namespace="flux-system"} | unpack_json | log.level:error`},
		{name: "structured container only", container: "harbor-core",
			want: `{kubernetes.container_name="harbor-core"}`},
		{name: "raw passthrough", raw: `{kubernetes.pod_namespace="apps"} | unpack_json | log.level:warn`,
			want: `{kubernetes.pod_namespace="apps"} | unpack_json | log.level:warn`},
		{name: "reject prometheus level= syntax", raw: `{job="x"} | level=error`, wantErr: true},
		{name: "nothing provided", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLogsQL(tc.raw, tc.container, tc.namespace, tc.level)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got query %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("buildLogsQL = %q, want %q", got, tc.want)
			}
		})
	}
}

type fakeRangeMetrics struct {
	matrix    providers.Matrix
	gotQuery  string
	gotWindow providers.TimeWindow
	gotStep   time.Duration
}

func (f *fakeRangeMetrics) Query(context.Context, string, time.Time) (providers.Samples, error) {
	return nil, nil
}

func (f *fakeRangeMetrics) QueryRange(_ context.Context, q string, w providers.TimeWindow, step time.Duration) (providers.Matrix, error) {
	f.gotQuery, f.gotWindow, f.gotStep = q, w, step
	return f.matrix, nil
}

func TestQueryMetricsRangeTool(t *testing.T) {
	fm := &fakeRangeMetrics{matrix: providers.Matrix{{
		Metric: map[string]string{"__name__": "http_errors", "job": "api"},
		Points: []providers.Point{
			{Time: time.Unix(1700000000, 0).UTC(), Value: 1},
			{Time: time.Unix(1700000060, 0).UTC(), Value: 9},
			{Time: time.Unix(1700000120, 0).UTC(), Value: 4},
		},
	}}}
	tool := QueryMetricsRangeTool{Metrics: fm}
	if tool.Name() != "query_metrics_range" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"query":"http_errors","since_minutes":30,"step_seconds":60}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The model must see the TREND, not a single value: first->last with min/max
	// so a spike-then-partial-recovery (1 -> 9 -> 4) is legible — and WHEN the
	// extremes happened, so the spike can be correlated to a change/deploy time.
	if !strings.Contains(out, `http_errors{job="api"}`) {
		t.Fatalf("missing series label:\n%s", out)
	}
	for _, want := range []string{"first=1", "last=4", "min=1@2023-11-14T22:13:20Z", "max=9@2023-11-14T22:14:20Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if fm.gotQuery != "http_errors" {
		t.Fatalf("query=%q", fm.gotQuery)
	}
	if fm.gotStep != 60*time.Second {
		t.Fatalf("step=%v, want 60s", fm.gotStep)
	}
	if d := fm.gotWindow.End.Sub(fm.gotWindow.Start); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("window width=%v, want ~30m", d)
	}
}

func TestQueryMetricsRangeToolEmpty(t *testing.T) {
	tool := QueryMetricsRangeTool{Metrics: &fakeRangeMetrics{}}
	out, err := tool.Call(context.Background(), `{"query":"up"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no series matched") {
		t.Fatalf("want no-series note, got:\n%s", out)
	}
}
