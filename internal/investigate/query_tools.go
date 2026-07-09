// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

const maxToolRows = 50

// renderRows writes up to maxToolRows rows, calling row(i) for each kept index. If
// n exceeds the cap it appends a truncation note "… (<remaining> <noun>)". This is
// the shared row-capping shape used by every tool that renders a bounded list.
func renderRows(b *strings.Builder, n int, noun string, row func(i int)) {
	for i := 0; i < n; i++ {
		if i >= maxToolRows {
			fmt.Fprintf(b, "… (%d %s)\n", n-i, noun)
			return
		}
		row(i)
	}
}

// QueryMetricsTool lets the model run PromQL instant queries (saturation, error
// rates, health) against the metrics backend.
type QueryMetricsTool struct {
	Metrics providers.MetricsProvider
}

// Name returns the tool name.
func (t QueryMetricsTool) Name() string { return "query_metrics" }

// Description returns the tool description.
func (t QueryMetricsTool) Description() string {
	return "Run a PromQL instant query against the metrics backend (VictoriaMetrics/Prometheus) — check saturation, error rates, restarts, resource usage. This returns the value NOW only; to see when a metric started rising/spiking around the incident, use query_metrics_range instead."
}

// Schema returns the JSON schema for the arguments.
func (t QueryMetricsTool) Schema() string {
	return `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`
}

// Call runs the instant query and renders the series.
func (t QueryMetricsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	samples, err := t.Metrics.Query(ctx, in.Query, time.Time{})
	if err != nil {
		return "", err
	}
	if len(samples) == 0 {
		return "no series matched", nil
	}
	var b strings.Builder
	renderRows(&b, len(samples), "more series", func(i int) {
		fmt.Fprintf(&b, "%s = %g\n", formatMetric(samples[i].Metric), samples[i].Value)
	})
	return b.String(), nil
}

func formatMetric(m map[string]string) string {
	name := m["__name__"]
	labels := make([]string, 0, len(m))
	for k, v := range m {
		if k != "__name__" {
			labels = append(labels, fmt.Sprintf("%s=%q", k, v))
		}
	}
	sort.Strings(labels)
	return name + "{" + strings.Join(labels, ",") + "}"
}

// QueryMetricsRangeTool lets the model run a PromQL RANGE query and see how a
// metric trends over a recent window — the time dimension an instant query lacks,
// which is what reveals when a problem started (rising / spiking / recovering).
type QueryMetricsRangeTool struct {
	Metrics providers.MetricsProvider
}

// Name returns the tool name.
func (t QueryMetricsRangeTool) Name() string { return "query_metrics_range" }

// Description returns the tool description.
func (t QueryMetricsRangeTool) Description() string {
	return "Run a PromQL RANGE query over a recent window (default 60m, 60s step) to see how a metric TRENDS — rising, spiking, or recovering around the incident — not just its value right now. Use rate()/error-rate/saturation expressions; returns per-series first→last with min/max so you can tell WHEN a problem started. since_minutes bounds the window; step_seconds the resolution."
}

// Schema returns the JSON schema for the arguments.
func (t QueryMetricsRangeTool) Schema() string {
	return `{"type":"object","properties":{"query":{"type":"string"},"since_minutes":{"type":"integer"},"step_seconds":{"type":"integer"}},"required":["query"]}`
}

// Call runs the range query over [now-since, now] and renders a per-series trend.
func (t QueryMetricsRangeTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Query        string `json:"query"`
		SinceMinutes int    `json:"since_minutes"`
		StepSeconds  int    `json:"step_seconds"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = 60
	}
	step := time.Duration(in.StepSeconds) * time.Second
	if step <= 0 {
		step = time.Minute
	}
	end := time.Now()
	start := end.Add(-time.Duration(since) * time.Minute)
	series, err := t.Metrics.QueryRange(ctx, in.Query, providers.TimeWindow{Start: start, End: end}, step)
	if err != nil {
		return "", err
	}
	if len(series) == 0 {
		return "no series matched", nil
	}
	var b strings.Builder
	renderRows(&b, len(series), "more series", func(i int) {
		s := series[i]
		first, last, lo, hi := summarize(s.Points)
		fmt.Fprintf(&b, "%s first=%g last=%g min=%g%s max=%g%s\n",
			formatMetric(s.Metric), first, last, lo.Value, atTime(lo.Time), hi.Value, atTime(hi.Time))
	})
	return b.String(), nil
}

// atTime renders "@<RFC3339>" for a point's timestamp, or "" when the backend
// returned no time — WHEN a metric peaked/bottomed is what lets the model
// correlate the spike to a change/deploy timestamp.
func atTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return "@" + t.UTC().Format(time.RFC3339)
}

// summarize reduces a series' points to first, last, and the min/max POINTS
// (value + when it happened) — the compact trend an LLM needs to tell whether a
// metric climbed, spiked or recovered, and when, without shipping every sample.
// An empty series yields zeros.
func summarize(points []providers.Point) (first, last float64, lo, hi providers.Point) {
	if len(points) == 0 {
		return 0, 0, providers.Point{}, providers.Point{}
	}
	first = points[0].Value
	last = points[len(points)-1].Value
	lo, hi = points[0], points[0]
	for _, p := range points {
		if p.Value < lo.Value {
			lo = p
		}
		if p.Value > hi.Value {
			hi = p
		}
	}
	return first, last, lo, hi
}

// NetworkDropsTool lets the model list recently denied/dropped network flows from
// the configured (pluggable, CNI-agnostic) network-flow source — surfacing
// NetworkPolicy denials, firewall/security-group rejects, and connectivity failures.
type NetworkDropsTool struct {
	Network providers.NetworkProvider
}

// Name returns the tool name.
func (t NetworkDropsTool) Name() string { return "network_drops" }

// Description returns the tool description.
func (t NetworkDropsTool) Description() string {
	return "List recently denied/dropped network flows for a namespace (optionally a pod) — surfaces NetworkPolicy denials, firewall/security-group rejects, and connectivity failures, from the configured network-flow source. (IP-based cloud flow-log sources may return VPC-wide denials rather than pod-scoped.)"
}

// Schema returns the JSON schema for the arguments.
func (t NetworkDropsTool) Schema() string {
	return `{"type":"object","properties":{"namespace":{"type":"string"},"name":{"type":"string"},"since_minutes":{"type":"integer"}},"required":["namespace"]}`
}

// Call lists dropped flows over [now-since, now] and renders them.
func (t NetworkDropsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Namespace    string `json:"namespace"`
		Name         string `json:"name"`
		SinceMinutes int    `json:"since_minutes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = 60
	}
	end := time.Now()
	start := end.Add(-time.Duration(since) * time.Minute)
	lines, err := t.Network.Drops(ctx, providers.Selector{Namespace: in.Namespace, Name: in.Name}, providers.TimeWindow{Start: start, End: end})
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "no dropped flows", nil
	}
	var b strings.Builder
	renderLogLines(&b, lines, "more")
	return b.String(), nil
}

// QueryLogsTool lets the model query logs (LogsQL) over a recent window.
type QueryLogsTool struct {
	Logs providers.LogsProvider
}

// buildLogsQL composes a valid LogsQL query. With structured fields it builds the
// canonical `{kubernetes.container_name="…",kubernetes.pod_namespace="…"} |
// unpack_json | log.level:…` form, so valid-query generation lives in Go and can't
// drift. A raw query is used as-is but rejected when it uses Prometheus/Loki
// `level=` syntax (the model's recurring mistake) — the error guides a retry.
func buildLogsQL(raw, container, namespace, level string) (string, error) {
	if raw != "" {
		if strings.Contains(raw, "level=") {
			return "", fmt.Errorf("invalid LogsQL: `level=` is Prometheus/Loki syntax. Filter severity with `| unpack_json | log.level:error` (after a stream selector), or use the container/namespace/level params")
		}
		return raw, nil
	}
	var sel []string
	if container != "" {
		sel = append(sel, fmt.Sprintf("kubernetes.container_name=%q", container))
	}
	if namespace != "" {
		sel = append(sel, fmt.Sprintf("kubernetes.pod_namespace=%q", namespace))
	}
	if len(sel) == 0 {
		return "", fmt.Errorf("provide a raw `query`, or `container`/`namespace` to build one")
	}
	q := "{" + strings.Join(sel, ",") + "}"
	if level != "" {
		q += " | unpack_json | log.level:" + level
	}
	return q, nil
}

// Name returns the tool name.
func (t QueryLogsTool) Name() string { return "query_logs" }

// Description returns the tool description.
func (t QueryLogsTool) Description() string {
	return "Query logs with LogsQL (VictoriaLogs) over a recent window. " +
		"PREFER the structured params (container/namespace/level) and let the tool build the query. " +
		"If you write a raw `query`: stream labels use DOT notation (kubernetes.container_name, " +
		"kubernetes.pod_namespace), NOT underscores; to filter by severity you MUST unpack JSON first, " +
		"e.g. `{kubernetes.container_name=\"x\"} | unpack_json | log.level:error`. " +
		"Do NOT use `level=error` — that is Prometheus/Loki syntax and is invalid LogsQL. " +
		"Optional since_minutes bounds the window (default 60)."
}

// Schema returns the JSON schema for the arguments.
func (t QueryLogsTool) Schema() string {
	return `{"type":"object","properties":{` +
		`"container":{"type":"string","description":"kubernetes container name to scope to"},` +
		`"namespace":{"type":"string","description":"kubernetes namespace to scope to"},` +
		`"level":{"type":"string","enum":["error","warn","info"],"description":"severity filter (unpacks JSON)"},` +
		`"query":{"type":"string","description":"raw LogsQL; only if the structured fields are insufficient"},` +
		`"since_minutes":{"type":"integer"}},"required":[]}`
}

// Call runs the logs query over [now-since, now] and renders the lines.
func (t QueryLogsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Container    string `json:"container"`
		Namespace    string `json:"namespace"`
		Level        string `json:"level"`
		Query        string `json:"query"`
		SinceMinutes int    `json:"since_minutes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	query, err := buildLogsQL(in.Query, in.Container, in.Namespace, in.Level)
	if err != nil {
		return "", err
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = 60
	}
	end := time.Now()
	start := end.Add(-time.Duration(since) * time.Minute)
	lines, err := t.Logs.Query(ctx, query, providers.TimeWindow{Start: start, End: end})
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "no log lines matched", nil
	}
	var b strings.Builder
	renderLogLines(&b, lines, "more lines")
	return b.String(), nil
}
