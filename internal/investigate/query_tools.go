// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

const maxToolRows = 50

// windowSince builds the [now-sinceMinutes, now] lookback window shared by the
// tools that take a since_minutes argument, applying defMinutes when the caller
// passed a non-positive value. It dedups the identical default-and-window boilerplate
// those Call methods would otherwise repeat.
func windowSince(sinceMinutes, defMinutes int) providers.TimeWindow {
	if sinceMinutes <= 0 {
		sinceMinutes = defMinutes
	}
	end := time.Now()
	return providers.TimeWindow{Start: end.Add(-time.Duration(sinceMinutes) * time.Minute), End: end}
}

// Dead-end result strings. An empty query result is high-leverage: a bare "no
// series matched" leaves the model to conclude the workload is healthy or to keep
// guessing metric names. These strings instead name the next tool (discover_metrics
// / discover_log_fields) so the agent recovers instead of dead-ending. The
// query_metrics* tests match on the "no series matched" prefix, so it is preserved.
const (
	noSeriesMatched = "no series matched — the metric name or labels may not exist; " +
		"use discover_metrics with a namespace selector to list what this workload actually exports"

	noLogLinesMatched = "no log lines matched — if logs were expected, the collector field names may differ " +
		"from the assumed schema (try discover_log_fields to see the real fields), or narrow/loosen the query; " +
		"consider pod_logs for a specific pod"
)

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

// metricsQLGuidance is the MetricsQL sentence appended to the query_metrics /
// query_metrics_range Descriptions ONLY when the backend flavor is VictoriaMetrics.
// MetricsQL is a PromQL superset, so these helpers would be invalid against real
// Prometheus — hence they are advertised description-only, and only when the flavor
// is known to be VM. No query rewriting happens; the model may use them or not.
const metricsQLGuidance = " This backend is VictoriaMetrics, which also accepts MetricsQL (a PromQL superset): " +
	"when many series match, `outliersk(3, <expr>)` returns only the anomalous ones; " +
	"`<expr> default 0` fills scrape gaps so absent series read as 0 instead of vanishing; " +
	"`rollup_rate(<metric>[5m])` and `<expr> keep_metric_names` are also available."

// QueryMetricsTool lets the model run PromQL instant queries (saturation, error
// rates, health) against the metrics backend.
type QueryMetricsTool struct {
	Metrics providers.MetricsProvider

	// MetricsQL, when true, appends VictoriaMetrics-only MetricsQL guidance to the
	// Description (the backend was detected/configured as VictoriaMetrics). It is
	// description-only — no query is ever rewritten. Default false ⇒ generic
	// Prometheus wording, so a Prometheus (or unknown) backend never sees MetricsQL
	// claims it would reject.
	MetricsQL bool
}

// Name returns the tool name.
func (t QueryMetricsTool) Name() string { return "query_metrics" }

// Description returns the tool description.
func (t QueryMetricsTool) Description() string {
	d := "Run a PromQL instant query against the metrics backend (VictoriaMetrics/Prometheus) — check saturation, error rates, restarts, resource usage. This returns the value NOW only; to see when a metric started rising/spiking around the incident, use query_metrics_range instead. Results cap at 50 series (largest |value| kept); prefer topk(10, sum by(pod)(rate(...))) over a raw selector so the cap doesn't hide the signal."
	if t.MetricsQL {
		d += metricsQLGuidance
	}
	return d
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
		return noSeriesMatched, nil
	}
	// Prometheus/VictoriaMetrics don't order an instant vector, so a first-50 cap
	// (renderRows) would keep an arbitrary slice. Sort by |value| desc first so
	// truncation keeps the extremes — the saturated/erroring series an operator
	// cares about — rather than whatever the backend happened to emit first.
	sort.SliceStable(samples, func(i, j int) bool {
		return math.Abs(samples[i].Value) > math.Abs(samples[j].Value)
	})
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

	// MetricsQL mirrors QueryMetricsTool.MetricsQL: description-only VictoriaMetrics
	// guidance, appended only when the backend flavor is VictoriaMetrics.
	MetricsQL bool
}

// Name returns the tool name.
func (t QueryMetricsRangeTool) Name() string { return "query_metrics_range" }

// Description returns the tool description.
func (t QueryMetricsRangeTool) Description() string {
	d := "Run a PromQL RANGE query over a recent window (default 60m, 60s step) to see how a metric TRENDS — rising, spiking, or recovering around the incident — not just its value right now. Use rate()/error-rate/saturation expressions; returns per-series first→last with min/max, a compact downsampled trend, and the biggest adjacent jump so you can tell WHEN a problem started and whether it was a step-change or a ramp. since_minutes bounds the window; step_seconds the resolution (auto-derived/coarsened if it would exceed the backend point cap). Results cap at 50 series (largest |value| kept); prefer topk(10, sum by(pod)(rate(...))) over a raw selector so the cap doesn't hide the signal."
	if t.MetricsQL {
		d += metricsQLGuidance
	}
	return d
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
	window := windowSince(in.SinceMinutes, 60)
	step, clampNote := resolveStep(time.Duration(in.StepSeconds)*time.Second, window)
	series, err := t.Metrics.QueryRange(ctx, in.Query, window, step)
	if err != nil {
		return "", err
	}
	if len(series) == 0 {
		return noSeriesMatched, nil
	}
	var b strings.Builder
	if clampNote != "" {
		b.WriteString(clampNote + "\n")
	}
	renderRows(&b, len(series), "more series", func(i int) {
		s := series[i]
		first, last, lo, hi := summarize(s.Points)
		// Compute the biggest adjacent jump once (delta + when) rather than traversing
		// the points twice.
		jumpIdx, jumpDlt := biggestJump(s.Points)
		var jumpAt time.Time
		if jumpIdx >= 0 {
			jumpAt = s.Points[jumpIdx].Time
		}
		fmt.Fprintf(&b, "%s first=%g last=%g min=%g%s max=%g%s trend=%s biggest jump %+g%s\n",
			formatMetric(s.Metric), first, last, lo.Value, atTime(lo.Time), hi.Value, atTime(hi.Time),
			trend(s.Points), jumpDlt, atTime(jumpAt))
	})
	return b.String(), nil
}

// maxRangeToolPoints bounds the point count the range tool requests. It is well
// below the backend's ~11k hard cap: the LLM reads a compact summary, not raw
// points, so a finer resolution buys nothing and risks a rejected/expensive
// query. The prometheus client keeps a separate 11k backstop for direct callers.
const maxRangeToolPoints = 1000

// resolveStep picks the range step for a window and reports whether it coarsened
// an operator-supplied step. With no explicit step it derives one from the window
// (window/maxRangeToolPoints, min 1s) so a wide window stays bounded silently. An
// explicit step that would exceed the point cap is raised — and annotated, so a
// deliberately fine step isn't silently changed under the operator.
func resolveStep(requested time.Duration, w providers.TimeWindow) (step time.Duration, note string) {
	span := w.End.Sub(w.Start)
	minStep := time.Duration(0)
	if span > 0 {
		// round the per-point step UP to whole seconds (the backend step unit) so
		// span/step never lands back above the cap.
		if s := span / maxRangeToolPoints; s > time.Second {
			minStep = (s + time.Second - 1).Truncate(time.Second)
		} else {
			minStep = time.Second
		}
	}
	if requested <= 0 {
		// derived default; nothing was overridden, so no annotation.
		if minStep <= 0 {
			return time.Minute, ""
		}
		return minStep, ""
	}
	if minStep > 0 && requested < minStep {
		return minStep, fmt.Sprintf("note: step coarsened from %s to %s to keep the query under %d points (%s window); pass a smaller since_minutes for finer resolution",
			requested, minStep, maxRangeToolPoints, span.Round(time.Minute))
	}
	return requested, ""
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

// trendBuckets is the fixed number of downsampled points the trend string carries.
// ~12 points is enough for an LLM to read the shape (flat / ramp / step / spike)
// without shipping every raw sample.
const trendBuckets = 12

// trend renders a compact fixed-bucket downsample of the series as
// "v0>v1>...>vN" (~trendBuckets points). first/last/min/max can't distinguish a
// step-change from a ramp — both share the same endpoints and extremes — so the
// shape between them is what the model needs. Fewer points than buckets are
// emitted verbatim.
func trend(points []providers.Point) string {
	if len(points) == 0 {
		return ""
	}
	n := trendBuckets
	if len(points) < n {
		n = len(points)
	}
	vals := make([]string, 0, n)
	for i := 0; i < n; i++ {
		// pick evenly spaced source indices so the first and last are always kept.
		idx := 0
		if n > 1 {
			idx = i * (len(points) - 1) / (n - 1)
		}
		vals = append(vals, fmt.Sprintf("%g", points[idx].Value))
	}
	return strings.Join(vals, ">")
}

// biggestJump finds the adjacent pair with the largest |Δvalue| and returns the
// index of the later point and the signed delta. Returns (-1, 0) for <2 points.
func biggestJump(points []providers.Point) (int, float64) {
	if len(points) < 2 {
		return -1, 0
	}
	bestI, bestD := 1, points[1].Value-points[0].Value
	for i := 2; i < len(points); i++ {
		d := points[i].Value - points[i-1].Value
		if math.Abs(d) > math.Abs(bestD) {
			bestI, bestD = i, d
		}
	}
	return bestI, bestD
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
	lines, err := t.Network.Drops(ctx, providers.Selector{Namespace: in.Namespace, Name: in.Name}, windowSince(in.SinceMinutes, 60))
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

	// Fields is the OPTIONAL collector field-naming convention (config.logs.fields).
	// The zero value reproduces the shipped VictoriaLogs/vector schema exactly, so an
	// unset config is a no-op. The app layer sets it from config.
	Fields LogFields

	// AllowedNamespaces is the operator-configured allowlist of namespaces a namespace-
	// scoped query may target (config.investigation.pod_log_namespaces — the SAME list
	// that guards pod_logs, because query_logs reads the same raw logs). Empty ⇒ the
	// permissive pre-confinement behaviour (any namespace) is preserved. See
	// namespaceAllowed. The incident namespace is always permitted.
	AllowedNamespaces []string

	// IncidentNamespace is this investigation's own namespace; always permitted. Set
	// per-investigation by the loop (scopeTools) via withIncidentNamespace.
	IncidentNamespace string
}

// buildLogsQL composes a LogsQL query with the SHIPPED default field convention. It
// is the entry point for the tools that don't expose field config (discover_log_fields,
// logs_error_summary); QueryLogsTool uses buildLogsQLWith to honour config.logs.fields.
func buildLogsQL(raw, container, namespace, level string) (string, error) {
	return buildLogsQLWith(raw, container, namespace, level, LogFields{})
}

// buildLogsQLWith composes a valid LogsQL query using the resolved field convention.
// With structured fields it builds the canonical
// `{<container_field>="…",<namespace_field>="…"} | <unpack_pipe> | <level_field>:…`
// form, so valid-query generation lives in Go and can't drift. A raw query is used
// as-is but rejected when it uses Prometheus/Loki `level=` syntax (the model's
// recurring mistake) — the error guides a retry.
func buildLogsQLWith(raw, container, namespace, level string, conv LogFields) (string, error) {
	conv = conv.resolved()
	if conv.Dialect == DialectLogQL {
		return buildLogQL(raw, container, namespace, level, conv)
	}
	if raw != "" {
		if strings.Contains(raw, "level=") {
			return "", fmt.Errorf("invalid LogsQL: `level=` is Prometheus/Loki syntax. Filter severity with `| %s | %s:error` (after a stream selector), or use the container/namespace/level params", conv.UnpackPipe, conv.LevelField)
		}
		return raw, nil
	}
	q, err := streamSelector(container, namespace, conv)
	if err != nil {
		return "", err
	}
	if level != "" {
		q += " | " + conv.UnpackPipe + " | " + conv.LevelField + ":" + level
	}
	return q, nil
}

// streamSelector assembles the `{container="…",namespace="…"}` stream selector
// shared by both dialect builders (LogsQL and LogQL use the identical label=value
// form), or an error when neither field is given.
func streamSelector(container, namespace string, conv LogFields) (string, error) {
	var sel []string
	if container != "" {
		sel = append(sel, fmt.Sprintf("%s=%q", conv.ContainerField, container))
	}
	if namespace != "" {
		sel = append(sel, fmt.Sprintf("%s=%q", conv.NamespaceField, namespace))
	}
	if len(sel) == 0 {
		return "", fmt.Errorf("provide a raw `query`, or `container`/`namespace` to build one")
	}
	return "{" + strings.Join(sel, ",") + "}", nil
}

// buildLogQL composes a valid LogQL (Grafana Loki) query from the resolved
// field convention: `{container="…",namespace="…"}` plus an optional parser
// pipe and a `| <level_field>="…"` label filter — detected_level needs no
// parser on Loki 3.x, so the default pipe is empty. A raw query passes through
// but must start with a stream selector, and LogsQL-isms (unpack_json, _msg —
// the model's likely carry-over mistakes) are rejected with a correcting error,
// mirroring the LogsQL branch's `level=` guard in spirit.
func buildLogQL(raw, container, namespace, level string, conv LogFields) (string, error) {
	if raw != "" {
		// Guard against carried-over VictoriaLogs syntax. `_msg:` (its message-field
		// filter) is matched WITH the colon so a Loki label value or |= search string
		// that merely contains "_msg" (e.g. auth_msg_worker) is not misrejected.
		if strings.Contains(raw, "unpack_json") || strings.Contains(raw, "_msg:") {
			return "", fmt.Errorf("invalid LogQL: unpack_json / _msg: are VictoriaLogs LogsQL syntax. Parse with `| json` or `| logfmt` and filter severity with `| %s=\"error\"`, or use the container/namespace/level params", conv.LevelField)
		}
		if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
			return "", fmt.Errorf("invalid LogQL: a query starts with a stream selector, e.g. `{%s=\"apps\"}`", conv.NamespaceField)
		}
		return raw, nil
	}
	q, err := streamSelector(container, namespace, conv)
	if err != nil {
		return "", err
	}
	if level != "" {
		if conv.UnpackPipe != "" {
			q += " | " + conv.UnpackPipe
		}
		q += fmt.Sprintf(" | %s=%q", conv.LevelField, level)
	}
	return q, nil
}

// Name returns the tool name.
func (t QueryLogsTool) Name() string { return "query_logs" }

// Description returns the tool description.
func (t QueryLogsTool) Description() string {
	return "Query logs with " + t.Fields.dialectLabel() + " over a recent window. " +
		"PREFER the structured params (container/namespace/level) and let the tool build the query. " +
		t.Fields.rawQueryGuidance() + " " +
		"Optional since_minutes bounds the window (default 60)."
}

// Schema returns the JSON schema for the arguments.
func (t QueryLogsTool) Schema() string {
	return `{"type":"object","properties":{` +
		`"container":{"type":"string","description":"kubernetes container name to scope to"},` +
		`"namespace":{"type":"string","description":"kubernetes namespace to scope to"},` +
		`"level":{"type":"string","enum":["error","warn","info"],"description":"severity filter (tool builds the dialect-correct filter)"},` +
		`"query":{"type":"string","description":"raw ` + t.Fields.queryLang() + `; only if the structured fields are insufficient"},` +
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
	// L2 confinement: query_logs reads the SAME raw pod logs (secrets/PII) pod_logs
	// does, so a structured namespace argument is held to the same allowlist. Guard
	// only the structured `namespace` param — a raw LogsQL query that names no
	// namespace stays unrestricted (there is nothing to confine), preserving today's
	// behaviour when no allowlist is configured.
	if in.Namespace != "" && !t.namespaceAllowed(in.Namespace) {
		return fmt.Sprintf("namespace %q is not permitted for query_logs (allowed: the incident namespace plus the configured pod_log_namespaces allowlist)", in.Namespace), nil
	}
	query, err := buildLogsQLWith(in.Query, in.Container, in.Namespace, in.Level, t.Fields)
	if err != nil {
		return "", err
	}
	lines, err := t.Logs.Query(ctx, query, windowSince(in.SinceMinutes, 60))
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return noLogLinesMatched, nil
	}
	var b strings.Builder
	renderLogLinesWith(&b, lines, "more lines", t.Fields)
	return b.String(), nil
}

// namespaceAllowed mirrors PodLogsTool.namespaceAllowed with the KEY difference that
// query_logs stays PERMISSIVE by default: with no configured allowlist AND no bound
// incident namespace the confinement is a no-op (the caller only invokes this for a
// non-empty namespace argument), so a deployment that never set pod_log_namespaces
// sees no behaviour change. Once an allowlist is set, a namespace argument must be
// the incident namespace or in the allowlist.
func (t QueryLogsTool) namespaceAllowed(ns string) bool {
	// No confinement configured ⇒ preserve the pre-L2 unrestricted behaviour.
	if len(t.AllowedNamespaces) == 0 && t.IncidentNamespace == "" {
		return true
	}
	if ns == t.IncidentNamespace { // the incident's own namespace is always allowed
		return true
	}
	return slices.Contains(t.AllowedNamespaces, ns)
}

// withIncidentNamespace binds a copy to this investigation's namespace, implementing
// incidentScoped so the loop's scopeTools can confine query_logs per request exactly
// as it does pod_logs. A value receiver keeps the shared tool untouched.
func (t QueryLogsTool) withIncidentNamespace(ns string) Tool {
	t.IncidentNamespace = ns
	return t
}
