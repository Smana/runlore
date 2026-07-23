// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func strconvItoa(i int) string { return strconv.Itoa(i) }

type fakeMetrics struct{ samples providers.Samples }

func (f fakeMetrics) Query(context.Context, string, time.Time) (providers.Samples, error) {
	return f.samples, nil
}
func (f fakeMetrics) QueryRange(context.Context, string, providers.TimeWindow, time.Duration) (providers.Matrix, error) {
	return nil, nil
}
func (f fakeMetrics) LabelValues(context.Context, string, []string, providers.TimeWindow) ([]string, error) {
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

func TestQueryMetricsToolEmptyGuidesDiscovery(t *testing.T) {
	// A bare "no series matched" is a dead end; the rewrite must name discover_metrics.
	tool := QueryMetricsTool{Metrics: fakeMetrics{}}
	out, err := tool.Call(context.Background(), `{"query":"nonexistent_metric"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no series matched") || !strings.Contains(out, "discover_metrics") {
		t.Fatalf("empty result must point at discover_metrics:\n%s", out)
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

func TestQueryLogsToolEmptyGuidesRecovery(t *testing.T) {
	// The empty-logs path must be actionable: schema-mismatch hint + discover_log_fields.
	tool := QueryLogsTool{Logs: fakeLogs{}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no log lines matched") || !strings.Contains(out, "discover_log_fields") {
		t.Fatalf("empty result must guide recovery:\n%s", out)
	}
	if !strings.Contains(out, "pod_logs") {
		t.Fatalf("empty result should mention pod_logs fallback:\n%s", out)
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

// TestBuildLogsQLWithCustomFields covers L1: a non-default field convention (a
// Loki-style flat schema) must retarget the generated selector, unpack pipe, and
// level field. The default (empty LogFields) path is covered by TestBuildLogsQL.
func TestBuildLogsQLWithCustomFields(t *testing.T) {
	conv := LogFields{
		ContainerField: "container",
		NamespaceField: "namespace",
		LevelField:     "level",
		UnpackPipe:     "unpack_logfmt",
	}
	got, err := buildLogsQLWith("", "harbor-core", "apps", "error", conv)
	if err != nil {
		t.Fatalf("buildLogsQLWith: %v", err)
	}
	want := `{container="harbor-core",namespace="apps"} | unpack_logfmt | level:error`
	if got != want {
		t.Fatalf("custom-field query =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildLogsQLDefaultsMatchLiterals pins the default-field generated query to the
// EXACT string the tool produced before logs.fields existed — the L1 safety contract.
func TestBuildLogsQLDefaultsMatchLiterals(t *testing.T) {
	got, err := buildLogsQLWith("", "kustomize-controller", "flux-system", "error", LogFields{})
	if err != nil {
		t.Fatalf("buildLogsQLWith: %v", err)
	}
	want := `{kubernetes.container_name="kustomize-controller",kubernetes.pod_namespace="flux-system"} | unpack_json | log.level:error`
	if got != want {
		t.Fatalf("default query drifted from the shipped literal:\ngot  %q\nwant %q", got, want)
	}
}

// TestQueryLogsToolNamespaceConfinement covers L2: with an allowlist configured, a
// query naming an OUTSIDE namespace is rejected (non-fatally, so the model can
// retry); the incident namespace and allowlisted namespaces are always permitted.
func TestQueryLogsToolNamespaceConfinement(t *testing.T) {
	base := QueryLogsTool{
		Logs:              fakeLogs{lines: providers.LogResult{{Message: "ok"}}},
		AllowedNamespaces: []string{"flux-system"},
	}
	// Bind the incident namespace exactly as the loop's scopeTools does.
	tool := base.withIncidentNamespace("apps").(QueryLogsTool)

	// Outside namespace: rejected, cluster not queried.
	out, err := tool.Call(context.Background(), `{"namespace":"kube-system"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "not permitted") {
		t.Fatalf("outside namespace must be rejected, got:\n%s", out)
	}
	// Incident namespace: always allowed.
	if out, err := tool.Call(context.Background(), `{"namespace":"apps"}`); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("incident namespace must be allowed, out=%q err=%v", out, err)
	}
	// Allowlisted namespace: allowed.
	if out, err := tool.Call(context.Background(), `{"namespace":"flux-system"}`); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("allowlisted namespace must be allowed, out=%q err=%v", out, err)
	}
}

// TestQueryLogsToolPermissiveWithoutAllowlist covers the L2 non-breaking guarantee:
// with NO allowlist and NO bound incident namespace, any namespace argument and any
// raw query pass through unrestricted — today's behaviour is preserved.
func TestQueryLogsToolPermissiveWithoutAllowlist(t *testing.T) {
	tool := QueryLogsTool{Logs: fakeLogs{lines: providers.LogResult{{Message: "ok"}}}}
	// A namespace argument that no allowlist covers must NOT be blocked.
	if out, err := tool.Call(context.Background(), `{"namespace":"kube-system"}`); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("no allowlist ⇒ any namespace allowed, out=%q err=%v", out, err)
	}
	// A raw query naming no namespace is untouched.
	if out, err := tool.Call(context.Background(), `{"query":"{kubernetes.pod_namespace=\"kube-system\"}"}`); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("raw query must pass through, out=%q err=%v", out, err)
	}
}

// TestQueryLogsToolRawQueryUnconfined: even WITH an allowlist, a raw LogsQL query
// that names no structured namespace argument is not blocked (there is nothing to
// confine on — the SAFETY-MEDIUM carve-out).
func TestQueryLogsToolRawQueryUnconfined(t *testing.T) {
	tool := QueryLogsTool{
		Logs:              fakeLogs{lines: providers.LogResult{{Message: "ok"}}},
		AllowedNamespaces: []string{"flux-system"},
		IncidentNamespace: "apps",
	}
	if out, err := tool.Call(context.Background(), `{"query":"error"}`); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("raw query with no namespace arg must not be blocked, out=%q err=%v", out, err)
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

func (f *fakeRangeMetrics) LabelValues(context.Context, string, []string, providers.TimeWindow) ([]string, error) {
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

func TestQueryMetricsRangeToolTrendAndBiggestJump(t *testing.T) {
	// A step-change and a slow ramp can share the same first/last/min/max, so the
	// summary must also carry (a) a compact downsampled trend and (b) the largest
	// adjacent delta with its timestamp — that's what tells a jump from a ramp.
	fm := &fakeRangeMetrics{matrix: providers.Matrix{{
		Metric: map[string]string{"__name__": "latency", "pod": "api-0"},
		Points: []providers.Point{
			{Time: time.Unix(1700000000, 0).UTC(), Value: 1},
			{Time: time.Unix(1700000060, 0).UTC(), Value: 1},
			{Time: time.Unix(1700000120, 0).UTC(), Value: 4}, // +3 jump here
			{Time: time.Unix(1700000180, 0).UTC(), Value: 4},
		},
	}}}
	tool := QueryMetricsRangeTool{Metrics: fm}
	out, err := tool.Call(context.Background(), `{"query":"latency","since_minutes":30,"step_seconds":60}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "trend=") {
		t.Fatalf("missing compact trend:\n%s", out)
	}
	// biggest adjacent jump is +3 at the third point's timestamp.
	if !strings.Contains(out, "biggest jump +3") {
		t.Fatalf("missing biggest-jump magnitude:\n%s", out)
	}
	if !strings.Contains(out, "@2023-11-14T22:15:20Z") {
		t.Fatalf("biggest jump must carry its timestamp:\n%s", out)
	}
}

func TestQueryMetricsRangeToolClampsStep(t *testing.T) {
	// A 24h window with a 1s step is ~86400 points — far past the ~11k backend cap.
	// The tool must raise the step to bound the point count and annotate that it
	// coarsened, so an operator's explicit fine step isn't silently changed.
	fm := &fakeRangeMetrics{matrix: providers.Matrix{{
		Metric: map[string]string{"__name__": "up"},
		Points: []providers.Point{{Time: time.Unix(1700000000, 0).UTC(), Value: 1}},
	}}}
	tool := QueryMetricsRangeTool{Metrics: fm}
	out, err := tool.Call(context.Background(), `{"query":"up","since_minutes":1440,"step_seconds":1}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	points := int(fm.gotWindow.End.Sub(fm.gotWindow.Start) / fm.gotStep)
	if points > 1000 {
		t.Fatalf("clamped step must bound points <= ~1000, got %d (step=%v)", points, fm.gotStep)
	}
	if fm.gotStep <= time.Second {
		t.Fatalf("step should have been raised above 1s, got %v", fm.gotStep)
	}
	if !strings.Contains(out, "clamp") && !strings.Contains(out, "coarsen") {
		t.Fatalf("clamping must be annotated in output:\n%s", out)
	}
}

func TestQueryMetricsRangeToolDefaultStepFromWindow(t *testing.T) {
	// With no explicit step and a wide window, a derived step keeps points bounded
	// without any clamp annotation (nothing was overridden).
	fm := &fakeRangeMetrics{matrix: providers.Matrix{{
		Metric: map[string]string{"__name__": "up"},
		Points: []providers.Point{{Time: time.Unix(1700000000, 0).UTC(), Value: 1}},
	}}}
	tool := QueryMetricsRangeTool{Metrics: fm}
	out, err := tool.Call(context.Background(), `{"query":"up","since_minutes":1440}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	points := int(fm.gotWindow.End.Sub(fm.gotWindow.Start) / fm.gotStep)
	if points > 1000 {
		t.Fatalf("derived step must bound points, got %d (step=%v)", points, fm.gotStep)
	}
	if strings.Contains(out, "clamp") {
		t.Fatalf("no explicit step ⇒ no clamp annotation expected:\n%s", out)
	}
}

func TestQueryMetricsToolTruncateKeepsExtremes(t *testing.T) {
	// The 50-row cap keeps an arbitrary first-50 unless we sort by |value| desc; the
	// extreme (biggest-magnitude) series is what matters, so it must survive
	// truncation even when it arrives last.
	samples := make(providers.Samples, 0, 60)
	for i := 0; i < 59; i++ {
		samples = append(samples, providers.Sample{
			Metric: map[string]string{"__name__": "x", "i": strconvItoa(i)}, Value: 0.5,
		})
	}
	// the extreme, appended last so first-50 truncation would drop it.
	samples = append(samples, providers.Sample{
		Metric: map[string]string{"__name__": "x", "i": "extreme"}, Value: -999,
	})
	tool := QueryMetricsTool{Metrics: fakeMetrics{samples: samples}}
	out, err := tool.Call(context.Background(), `{"query":"x"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, `i="extreme"`) {
		t.Fatalf("extreme series must survive truncation:\n%s", out)
	}
}

func TestQueryMetricsDescriptionSteersAggregation(t *testing.T) {
	for _, d := range []string{QueryMetricsTool{}.Description(), QueryMetricsRangeTool{}.Description()} {
		if !strings.Contains(d, "topk") {
			t.Fatalf("description must steer toward topk aggregation:\n%s", d)
		}
		if !strings.Contains(d, "50") {
			t.Fatalf("description must mention the 50-series cap:\n%s", d)
		}
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

func TestQueryMetricsMetricsQLGuidance(t *testing.T) {
	// VictoriaMetrics flavor → MetricsQL sentence appended.
	vm := QueryMetricsTool{MetricsQL: true}.Description()
	if !strings.Contains(vm, "MetricsQL") || !strings.Contains(vm, "outliersk(3,") {
		t.Fatalf("VM description missing MetricsQL guidance:\n%s", vm)
	}
	// Prometheus/unknown flavor → NO MetricsQL claims.
	prom := QueryMetricsTool{}.Description()
	if strings.Contains(prom, "MetricsQL") {
		t.Fatalf("Prometheus description must not mention MetricsQL:\n%s", prom)
	}
	// Range tool behaves identically.
	vmr := QueryMetricsRangeTool{MetricsQL: true}.Description()
	if !strings.Contains(vmr, "MetricsQL") {
		t.Fatalf("VM range description missing MetricsQL guidance:\n%s", vmr)
	}
	if strings.Contains(QueryMetricsRangeTool{}.Description(), "MetricsQL") {
		t.Fatalf("Prometheus range description must not mention MetricsQL")
	}
}

// TestBuildLogQL: with Dialect=logql the same structured params compile to
// valid LogQL, raw queries must start with a stream selector, and LogsQL-isms
// are rejected with a correcting error so the model retries in-dialect.
func TestBuildLogQL(t *testing.T) {
	logql := LogFields{Dialect: DialectLogQL}
	tests := []struct {
		name, raw, container, namespace, level string
		conv                                   LogFields
		want                                   string
		wantErr                                string
	}{
		{name: "structured selector + level", container: "harbor-core", namespace: "apps", level: "error", conv: logql,
			want: `{container="harbor-core",namespace="apps"} | detected_level="error"`},
		{name: "namespace only, no level", namespace: "apps", conv: logql,
			want: `{namespace="apps"}`},
		{name: "custom fields add parser pipe", container: "core", level: "error",
			conv: LogFields{Dialect: DialectLogQL, LevelField: "level", UnpackPipe: "logfmt"},
			want: `{container="core"} | logfmt | level="error"`},
		{name: "raw passthrough", raw: `{namespace="apps"} |= "refused"`, conv: logql,
			want: `{namespace="apps"} |= "refused"`},
		{name: "raw LogsQL-ism rejected", raw: `{namespace="apps"} | unpack_json | log.level:error`, conv: logql,
			wantErr: "LogsQL"},
		{name: "raw without selector rejected", raw: `error`, conv: logql,
			wantErr: "stream selector"},
		{name: "nothing given", conv: logql, wantErr: "provide a raw"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLogsQLWith(tc.raw, tc.container, tc.namespace, tc.level, tc.conv)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildLogsQLWith: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLogToolDescriptionsDialect: on a Loki deployment the model must be told
// LogQL — never LogsQL — in every tool description and schema, and vice versa.
func TestLogToolDescriptionsDialect(t *testing.T) {
	logql := LogFields{Dialect: DialectLogQL}
	q := QueryLogsTool{Fields: logql}
	if !strings.Contains(q.Description(), "LogQL (Grafana Loki)") || strings.Contains(q.Description(), "invalid LogsQL") {
		t.Fatalf("LogQL description wrong: %s", q.Description())
	}
	if !strings.Contains(q.Schema(), "raw LogQL") {
		t.Fatalf("LogQL schema wrong: %s", q.Schema())
	}
	if !strings.Contains(QueryLogsTool{}.Description(), "LogsQL (VictoriaLogs)") {
		t.Fatalf("default description must stay LogsQL")
	}
	for _, tool := range []interface {
		Description() string
		Schema() string
	}{LogsErrorSummaryTool{Fields: logql}, DiscoverLogFieldsTool{Fields: logql}} {
		if strings.Contains(tool.Schema(), "LogsQL") {
			t.Fatalf("loki-dialect schema must not mention LogsQL: %s", tool.Schema())
		}
	}
}
