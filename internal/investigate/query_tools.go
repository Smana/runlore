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

// QueryMetricsTool lets the model run PromQL instant queries (saturation, error
// rates, health) against the metrics backend.
type QueryMetricsTool struct {
	Metrics providers.MetricsProvider
}

// Name returns the tool name.
func (t QueryMetricsTool) Name() string { return "query_metrics" }

// Description returns the tool description.
func (t QueryMetricsTool) Description() string {
	return "Run a PromQL instant query against the metrics backend (VictoriaMetrics/Prometheus) — check saturation, error rates, restarts, resource usage."
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
	for i, s := range samples {
		if i >= maxToolRows {
			fmt.Fprintf(&b, "… (%d more series)\n", len(samples)-i)
			break
		}
		fmt.Fprintf(&b, "%s = %g\n", formatMetric(s.Metric), s.Value)
	}
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

// QueryLogsTool lets the model query logs (LogsQL) over a recent window.
type QueryLogsTool struct {
	Logs providers.LogsProvider
}

// Name returns the tool name.
func (t QueryLogsTool) Name() string { return "query_logs" }

// Description returns the tool description.
func (t QueryLogsTool) Description() string {
	return "Query logs (LogsQL) over a recent window for errors/anomalies. Optional since_minutes bounds the window (default 60)."
}

// Schema returns the JSON schema for the arguments.
func (t QueryLogsTool) Schema() string {
	return `{"type":"object","properties":{"query":{"type":"string"},"since_minutes":{"type":"integer"}},"required":["query"]}`
}

// Call runs the logs query over [now-since, now] and renders the lines.
func (t QueryLogsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Query        string `json:"query"`
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
	lines, err := t.Logs.Query(ctx, in.Query, providers.TimeWindow{Start: start, End: end})
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "no log lines matched", nil
	}
	var b strings.Builder
	for i, l := range lines {
		if i >= maxToolRows {
			fmt.Fprintf(&b, "… (%d more lines)\n", len(lines)-i)
			break
		}
		fmt.Fprintf(&b, "%s %s\n", l.Time.Format(time.RFC3339), l.Message)
	}
	return b.String(), nil
}
