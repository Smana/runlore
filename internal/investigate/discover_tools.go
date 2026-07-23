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

// DiscoverMetricsTool lets the model list the metric names (or a label's values)
// that ACTUALLY exist for a selector — so a query that matched nothing becomes a
// recoverable step instead of a dead end. It backs the discover_metrics tool.
//
// registered in internal/app/investigate.go (alongside the query_metrics tools).
type DiscoverMetricsTool struct {
	Metrics providers.MetricsProvider
}

// Name returns the tool name.
func (t DiscoverMetricsTool) Name() string { return "discover_metrics" }

// Description returns the tool description.
func (t DiscoverMetricsTool) Description() string {
	return "List the metric names that actually exist for a workload — use when query_metrics/query_metrics_range " +
		"returns 'no series matched' or you don't know the workload's metric names. Give a namespace (and optionally " +
		"a PromQL label matcher like `pod=~\"harbor-.*\"`) and it returns the real metric names exported there, so you " +
		"can stop guessing. Set label to enumerate the values of a specific label instead of metric names " +
		"(e.g. label=\"pod\" to list pods, label=\"le\" for histogram buckets). since_minutes bounds the lookup window " +
		"(default 60) so it stays cheap on a large metrics backend."
}

// Schema returns the JSON schema for the arguments.
func (t DiscoverMetricsTool) Schema() string {
	return `{"type":"object","properties":{` +
		`"namespace":{"type":"string","description":"kubernetes namespace to scope discovery to"},` +
		`"matcher":{"type":"string","description":"extra PromQL label matcher, e.g. pod=~\"harbor-.*\" (combined with namespace)"},` +
		`"label":{"type":"string","description":"label whose values to list; default __name__ (metric names)"},` +
		`"since_minutes":{"type":"integer"}},"required":["namespace"]}`
}

// Call resolves the selector to a match[] expression and lists the label values.
func (t DiscoverMetricsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Namespace    string `json:"namespace"`
		Matcher      string `json:"matcher"`
		Label        string `json:"label"`
		SinceMinutes int    `json:"since_minutes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	label := in.Label
	if label == "" {
		label = "__name__"
	}
	matcher, err := buildMetricsMatcher(in.Namespace, in.Matcher)
	if err != nil {
		return "", err
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = 60
	}
	end := time.Now()
	start := end.Add(-time.Duration(since) * time.Minute)
	vals, err := t.Metrics.LabelValues(ctx, label, []string{matcher}, providers.TimeWindow{Start: start, End: end})
	if err != nil {
		return "", err
	}
	if len(vals) == 0 {
		return fmt.Sprintf("no %s values found for %s — nothing is exporting metrics under this selector in the "+
			"last %dm; widen the matcher, check the namespace, or the workload may emit no Prometheus metrics", label, matcher, since), nil
	}
	// Stable order: VictoriaMetrics does not guarantee sorted label values.
	sort.Strings(vals)
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s value(s) for %s:\n", len(vals), label, matcher)
	renderRows(&b, len(vals), "more", func(i int) {
		fmt.Fprintf(&b, "%s\n", vals[i])
	})
	return b.String(), nil
}

// DiscoverLogFieldsTool lists the field names that actually exist in the logs a
// selector matches — so a query_logs that returned nothing (because the assumed
// collector schema was wrong, e.g. `kubernetes.container_name` vs `container`)
// becomes a recoverable step. It backs the discover_log_fields tool and uses the
// optional LogFields capability, degrading gracefully when unavailable.
//
// registered in internal/app/investigate.go (alongside query_logs).
type DiscoverLogFieldsTool struct {
	Logs providers.LogsProvider

	// Fields is the OPTIONAL field convention + dialect (config.logs.*); the zero
	// value keeps the shipped VictoriaLogs behaviour. The app layer sets it.
	Fields LogFields
}

// Name returns the tool name.
func (t DiscoverLogFieldsTool) Name() string { return "discover_log_fields" }

// Description returns the tool description.
func (t DiscoverLogFieldsTool) Description() string {
	return "List the log FIELD NAMES that actually exist for a selector — use when query_logs returns 'no log lines " +
		"matched' or you're unsure of the collector's schema (e.g. is it kubernetes.container_name or container?). " +
		"Give a namespace/container (or a raw LogsQL query) and it returns the real field names with hit counts, so you " +
		"can fix the query instead of guessing. since_minutes bounds the window (default 60)."
}

// Schema returns the JSON schema for the arguments.
func (t DiscoverLogFieldsTool) Schema() string {
	return `{"type":"object","properties":{` +
		`"container":{"type":"string","description":"kubernetes container name to scope to"},` +
		`"namespace":{"type":"string","description":"kubernetes namespace to scope to"},` +
		`"query":{"type":"string","description":"raw ` + t.Fields.queryLang() + `; only if the structured fields are insufficient"},` +
		`"since_minutes":{"type":"integer"}},"required":[]}`
}

// Call resolves the selector, lists the field names, or degrades gracefully.
func (t DiscoverLogFieldsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Container    string `json:"container"`
		Namespace    string `json:"namespace"`
		Query        string `json:"query"`
		SinceMinutes int    `json:"since_minutes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	// Field discovery has no severity dimension, so build with an empty level.
	query, err := buildLogsQLWith(in.Query, in.Container, in.Namespace, "", t.Fields)
	if err != nil {
		return "", err
	}
	fields, ok := t.Logs.(providers.LogFields)
	if !ok {
		return "discover_log_fields is unavailable — the configured logs backend does not support field discovery; " +
			"try query_logs with a broad selector and inspect the returned lines' fields.", nil
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = 60
	}
	end := time.Now()
	start := end.Add(-time.Duration(since) * time.Minute)
	names, err := fields.FieldNames(ctx, query, providers.TimeWindow{Start: start, End: end})
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return fmt.Sprintf("no fields found for %s in the last %dm — nothing matched; widen the selector or check the "+
			"namespace/container name.", query, since), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d field(s) for %s:\n", len(names), query)
	renderRows(&b, len(names), "more", func(i int) {
		if names[i].Hits > 0 {
			fmt.Fprintf(&b, "%s (×%d)\n", names[i].Name, names[i].Hits)
			return
		}
		fmt.Fprintf(&b, "%s\n", names[i].Name) // Loki stream labels carry no hit count
	})
	return b.String(), nil
}

// buildMetricsMatcher composes a PromQL series selector from a namespace and an
// optional extra label matcher. The namespace label is the Kubernetes-standard
// `namespace` used by kube-state-metrics / cAdvisor. A blank namespace with a raw
// matcher is allowed (the model gave its own selector); both blank is rejected so
// discovery is always scoped and never enumerates the whole TSDB by accident.
func buildMetricsMatcher(namespace, extra string) (string, error) {
	var parts []string
	if namespace != "" {
		parts = append(parts, fmt.Sprintf("namespace=%q", namespace))
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		// Accept either a bare matcher (`pod=~"x"`) or a braced one (`{pod=~"x"}`).
		extra = strings.TrimPrefix(extra, "{")
		extra = strings.TrimSuffix(extra, "}")
		parts = append(parts, extra)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("provide a namespace (or a matcher) so discovery stays scoped")
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}
