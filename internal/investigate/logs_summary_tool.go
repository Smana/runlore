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

// LogsErrorSummaryTool turns a raw log stream into the two numbers an investigator
// actually needs first: error VOLUME over time (a baseline → spike histogram) and
// the DOMINANT messages behind it. It exploits VictoriaLogs analytics
// (/select/logsql/hits + a `stats by (_msg)` pipe) via the optional LogStats
// capability, so a 10k-line error flood collapses to "3/5m baseline → 412/5m spike
// at 10:02; top message: connection refused (×388)" instead of a truncated dump.
//
// It degrades gracefully: when the configured LogsProvider does not implement
// LogStats (a future backend, or a fake without analytics), Call returns a clear,
// non-fatal note pointing the model at query_logs rather than erroring.
//
// registered in internal/app/investigate.go (alongside query_logs).
type LogsErrorSummaryTool struct {
	Logs providers.LogsProvider

	// Fields is the OPTIONAL field convention + dialect (config.logs.*); the zero
	// value keeps the shipped VictoriaLogs behaviour. The app layer sets it.
	Fields LogFields
}

// Name returns the tool name.
func (t LogsErrorSummaryTool) Name() string { return "logs_error_summary" }

// Description returns the tool description.
func (t LogsErrorSummaryTool) Description() string {
	return "Use FIRST for any log investigation — shows error VOLUME over time (a baseline→spike histogram, e.g. " +
		"'3/5m baseline → 412/5m SPIKE at 10:02') and the DOMINANT error messages with counts and first→last span, " +
		"instead of a raw line dump. It answers 'is this spiking, and what is flooding the logs?' in one call; drill " +
		"into query_logs only AFTER this to read specific lines. PREFER the structured params (container/namespace/level) " +
		"and let the tool build the query (a raw `query` override, if used, is " + t.Fields.queryLang() + "); " +
		"level defaults to error. since_minutes bounds the window (default 60)."
}

// Schema returns the JSON schema for the arguments.
func (t LogsErrorSummaryTool) Schema() string {
	return `{"type":"object","properties":{` +
		`"container":{"type":"string","description":"kubernetes container name to scope to"},` +
		`"namespace":{"type":"string","description":"kubernetes namespace to scope to"},` +
		`"level":{"type":"string","enum":["error","warn","info"],"description":"severity filter (default error)"},` +
		`"query":{"type":"string","description":"raw ` + t.Fields.queryLang() + `; only if the structured fields are insufficient"},` +
		`"since_minutes":{"type":"integer"}},"required":[]}`
}

// Call builds the query, then renders the hits histogram + top messages. It falls
// back to a clear note when the provider offers no analytics capability.
func (t LogsErrorSummaryTool) Call(ctx context.Context, args string) (string, error) {
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
	// Default to errors — this tool is the error-first entry point.
	level := in.Level
	if in.Query == "" && level == "" {
		level = "error"
	}
	query, err := buildLogsQLWith(in.Query, in.Container, in.Namespace, level, t.Fields)
	if err != nil {
		return "", err
	}
	stats, ok := t.Logs.(providers.LogStats)
	if !ok {
		// Graceful, non-fatal: the backend cannot serve analytics.
		return "logs_error_summary is unavailable — the configured logs backend does not support hit/message " +
			"analytics; use query_logs to read the matching lines directly.", nil
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = 60
	}
	end := time.Now()
	start := end.Add(-time.Duration(since) * time.Minute)
	w := providers.TimeWindow{Start: start, End: end}
	step := bucketStep(since)

	buckets, hErr := stats.Hits(ctx, query, w, step)
	msgs, mErr := stats.TopMessages(ctx, query, w, 10)
	if hErr != nil && mErr != nil {
		return "", hErr
	}
	if len(buckets) == 0 && len(msgs) == 0 {
		return noLogLinesMatched, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "query: %s (last %dm)\n", query, since)
	b.WriteString(renderHitsHistogram(buckets, step))
	if mErr != nil {
		fmt.Fprintf(&b, "top messages: unavailable (%v)\n", mErr)
	} else {
		b.WriteString(renderTopMessages(msgs))
	}
	return b.String(), nil
}

// bucketStep picks a histogram resolution that keeps the number of buckets legible
// (~12-60) across the requested window: 5m buckets by default, coarser for long
// windows so the model never gets hundreds of rows.
func bucketStep(sinceMinutes int) time.Duration {
	switch {
	case sinceMinutes <= 60:
		return 5 * time.Minute
	case sinceMinutes <= 360:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

// renderHitsHistogram compresses the per-bucket counts into RUNS of similar volume so a flat
// baseline followed by a spike reads as "10:00–10:45 err≈3/5m; 10:45→ 412/5m SPIKE"
// rather than a column of numbers. Buckets are summed across levels per timestamp
// (the model wants total error volume over time); a run breaks when the count
// changes by more than ~2x, and a run whose level is >=4x the prior run is flagged
// SPIKE. The step label ("/5m") tells the model the bucket size.
func renderHitsHistogram(buckets []providers.Bucket, step time.Duration) string {
	if len(buckets) == 0 {
		return "hits: none\n"
	}
	// Sum levels into one total-per-timestamp series, time-ordered.
	byTime := map[time.Time]int64{}
	var times []time.Time
	for _, bk := range buckets {
		if _, seen := byTime[bk.Time]; !seen {
			times = append(times, bk.Time)
		}
		byTime[bk.Time] += bk.Count
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	stepLabel := compactDuration(step)
	var b strings.Builder
	b.WriteString("hits (err volume over time):\n")

	type run struct {
		start, end time.Time
		count      int64 // representative (max) count in the run
	}
	var runs []run
	for _, tm := range times {
		c := byTime[tm]
		if n := len(runs); n > 0 && similar(runs[n-1].count, c) {
			runs[n-1].end = tm
			if c > runs[n-1].count {
				runs[n-1].count = c
			}
			continue
		}
		runs = append(runs, run{start: tm, end: tm, count: c})
	}
	for i, r := range runs {
		label := fmt.Sprintf("  %s", hhmm(r.start))
		if !r.end.Equal(r.start) {
			label += "–" + hhmm(r.end)
		}
		spike := ""
		if i > 0 && runs[i-1].count > 0 && r.count >= 4*runs[i-1].count {
			spike = " SPIKE"
		}
		fmt.Fprintf(&b, "%s: %d/%s%s\n", label, r.count, stepLabel, spike)
	}
	return b.String()
}

// renderTopMessages lists the dominant messages with counts and first→last span.
func renderTopMessages(msgs []providers.MsgCount) string {
	if len(msgs) == 0 {
		return "top messages: none\n"
	}
	var b strings.Builder
	b.WriteString("top messages (numeric tokens collapsed):\n")
	renderRows(&b, len(msgs), "more", func(i int) {
		m := msgs[i]
		fmt.Fprintf(&b, "  ×%d  %s", m.Count, oneLine(m.Message))
		if !m.First.IsZero() && !m.Last.IsZero() {
			fmt.Fprintf(&b, "  (first %s → last %s)", m.First.UTC().Format(time.RFC3339), m.Last.UTC().Format(time.RFC3339))
		}
		b.WriteString("\n")
	})
	return b.String()
}

// similar reports whether two bucket counts belong to the same volume run: within
// ~2x of each other (both zero counts as similar). It keeps a flat baseline as one
// run while letting a real jump start a new one.
func similar(a, c int64) bool {
	if a == c {
		return true
	}
	lo, hi := a, c
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo == 0 {
		return hi <= 2 // 0→small noise stays one run; 0→many breaks
	}
	return hi <= 2*lo
}

// hhmm renders a UTC HH:MM label for a histogram bucket; a zero time (backend
// omitted timestamps) renders as "?".
func hhmm(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.UTC().Format("15:04")
}

// compactDuration renders a step as a short "5m"/"1h" label for the "/N" suffix.
func compactDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// oneLine flattens a possibly multi-line message to a single trimmed line so the
// top-messages table stays one row per message.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
