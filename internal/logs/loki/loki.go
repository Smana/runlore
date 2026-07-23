// SPDX-License-Identifier: Apache-2.0

// Package loki implements providers.LogsProvider against Grafana Loki, querying
// with LogQL over /loki/api/v1/* and normalizing the streams response into log
// lines. It mirrors internal/logs/victorialogs: same construction/auth shape,
// same optional LogStats/LogFields capabilities, same truncation sentinel.
package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

// Client queries a Grafana Loki backend.
type Client struct {
	baseURL    string
	tokenEnv   string            // env var holding a bearer token; empty ⇒ no auth
	headers    map[string]string // static extra request headers (e.g. X-Scope-OrgID)
	maxLines   int               // per-query line cap (Loki `limit` param)
	levelField string            // severity label Hits splits by; "" ⇒ defaultLevelField
	http       *http.Client
}

// defaultMaxLines bounds the number of lines Query returns. Loki has no offset
// pagination (a timestamp-cursor walk is deferred), so this is sent as the
// server-side `limit` and doubles as the truncation-sentinel threshold.
const defaultMaxLines = 1000

// defaultLevelField is the severity label Hits groups by: Loki 3.x attaches
// detected_level as structured metadata to every entry (no parser stage
// needed). Overridable via WithLevelField (config.logs.fields.level_field) for
// Loki 2.x or a collector with its own level label.
const defaultLevelField = "detected_level"

// New builds a client for a Loki base URL, unauthenticated.
func New(baseURL string) *Client {
	return NewWithAuth(baseURL, "", nil)
}

// NewWithAuth builds a client that adds optional auth to every request; the
// semantics are identical to victorialogs.NewWithAuth (token read from the env
// at request-build time, never logged).
func NewWithAuth(baseURL, tokenEnv string, headers map[string]string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokenEnv:   tokenEnv,
		headers:    headers,
		maxLines:   defaultMaxLines,
		levelField: defaultLevelField,
		http:       httpx.SecureClient(30 * time.Second),
	}
}

// WithLevelField overrides the severity label Hits splits by. Empty is a no-op
// so an unset config keeps the default; returns the client for chaining.
func (c *Client) WithLevelField(field string) *Client {
	if field != "" {
		c.levelField = field
	}
	return c
}

var (
	_ providers.LogsProvider = (*Client)(nil)
	_ providers.LogStats     = (*Client)(nil)
	_ providers.LogFields    = (*Client)(nil)
)

// queryResponse is Loki's standard success envelope for /loki/api/v1/query_range.
type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// streamResult is one log stream: its label set plus [ns-epoch, line] entries.
type streamResult struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// queryRange performs a /loki/api/v1/query_range request and returns the
// status-checked envelope; callers unmarshal resp.Data.Result into their own
// shape (streams for Query, matrix for Hits).
func (c *Client) queryRange(ctx context.Context, v url.Values) (queryResponse, error) {
	body, err := c.get(ctx, "/loki/api/v1/query_range", v)
	if err != nil {
		return queryResponse{}, err
	}
	var resp queryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return queryResponse{}, fmt.Errorf("parse loki response: %w", err)
	}
	if resp.Status != "success" {
		return queryResponse{}, fmt.Errorf("loki error: status %q", resp.Status)
	}
	return resp, nil
}

// Query runs a LogQL query over the window and returns normalized log lines,
// newest first (matching the VictoriaLogs provider's ordering). It sends one
// request with limit=maxLines and direction=backward; when the server returns
// exactly the limit, more entries likely matched and the shared truncation
// sentinel is appended (providers.TruncationLine).
func (c *Client) Query(ctx context.Context, query string, w providers.TimeWindow) (providers.LogResult, error) {
	v := url.Values{
		"query":     {query},
		"limit":     {strconv.Itoa(c.maxLines)},
		"direction": {"backward"},
	}
	setWindow(v, w)
	resp, err := c.queryRange(ctx, v)
	if err != nil {
		return nil, err
	}
	if resp.Data.ResultType != "streams" {
		return nil, fmt.Errorf("unexpected loki resultType %q (Query expects a log selector, not a metric query)", resp.Data.ResultType)
	}
	var streams []streamResult
	if err := json.Unmarshal(resp.Data.Result, &streams); err != nil {
		return nil, fmt.Errorf("parse loki streams: %w", err)
	}
	var out providers.LogResult
	for _, s := range streams {
		for _, e := range s.Values {
			// All lines in one stream share the same label set; alias it rather than
			// copying per line (up to maxLines allocations avoided). Fields is
			// read-only downstream (renderer's streamIdentity / conv.PodField lookup).
			ll := providers.LogLine{Message: e[1], Fields: s.Stream}
			if ns, err := strconv.ParseInt(e[0], 10, 64); err == nil {
				ll.Time = time.Unix(0, ns).UTC()
			}
			out = append(out, ll)
		}
	}
	// Entries arrive grouped per stream; order newest-first ACROSS streams so the
	// renderer and the VictoriaLogs provider agree on what "first lines" means.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) >= c.maxLines {
		out = out[:c.maxLines]
		out = append(out, providers.TruncationLine(int64(c.maxLines)))
	}
	return out, nil
}

// setWindow adds the optional RFC3339 start/end bounds (Loki accepts RFC3339
// alongside ns epochs; RFC3339 matches the VictoriaLogs client for symmetry).
func setWindow(v url.Values, w providers.TimeWindow) {
	if !w.Start.IsZero() {
		v.Set("start", w.Start.UTC().Format(time.RFC3339))
	}
	if !w.End.IsZero() {
		v.Set("end", w.End.UTC().Format(time.RFC3339))
	}
}

// get performs an authenticated GET against a Loki endpoint and returns the raw
// body; any non-200 becomes an error carrying the status and body (mirrors
// victorialogs.postForm error shape, so tool-visible errors read the same).
func (c *Client) get(ctx context.Context, path string, v url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("logs query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("logs status %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// setAuth applies the optional bearer token and static headers (identical
// semantics to the VictoriaLogs client: token read here, never logged).
func (c *Client) setAuth(req *http.Request) {
	if c.tokenEnv != "" {
		if tok := os.Getenv(c.tokenEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
}

// matrixSeries is one series of a Loki metric-query (matrix) result.
type matrixSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"` // [unix seconds (float64), "count" (string)]
}

// Hits returns the per-step match count over the window, split by severity, by
// wrapping the log query in the LogQL metric form
// `sum by (<level>) (count_over_time(<q> [<step>]))` — the Loki analogue of
// VictoriaLogs' /select/logsql/hits, powering the logs_error_summary histogram.
// The level label defaults to detected_level (Loki 3.x structured metadata);
// series without the label fold into one Level=="" bucket series, which the
// renderer already handles. A step <= 0 defaults to one minute.
func (c *Client) Hits(ctx context.Context, query string, w providers.TimeWindow, step time.Duration) ([]providers.Bucket, error) {
	if step <= 0 {
		step = time.Minute
	}
	stepStr := fmt.Sprintf("%ds", int(step.Seconds()))
	metricQ := fmt.Sprintf("sum by (%s) (count_over_time(%s [%s]))", c.levelField, query, stepStr)
	v := url.Values{"query": {metricQ}, "step": {stepStr}}
	setWindow(v, w)
	resp, err := c.queryRange(ctx, v)
	if err != nil {
		return nil, err
	}
	var series []matrixSeries
	if err := json.Unmarshal(resp.Data.Result, &series); err != nil {
		return nil, fmt.Errorf("parse loki matrix: %w", err)
	}
	var out []providers.Bucket
	for _, s := range series {
		level := s.Metric[c.levelField]
		for _, p := range s.Values {
			ts, n := parsePoint(p)
			out = append(out, providers.Bucket{Time: ts, Level: level, Count: n})
		}
	}
	return out, nil
}

// parsePoint parses a Loki/Prometheus [unixSeconds, "value"] matrix point
// (same wire shape as internal/metrics/prometheus/prometheus.go parsePoint,
// narrowed to the int64 count Hits needs).
func parsePoint(p [2]any) (time.Time, int64) {
	var ts time.Time
	if f, ok := p[0].(float64); ok {
		ts = time.Unix(int64(f), 0).UTC()
	}
	var n int64
	if s, ok := p[1].(string); ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			n = int64(f)
		}
	}
	return ts, n
}

// TopMessages returns up to k dominant messages over the window. LogQL cannot
// group by message content (no `stats by (_msg)` equivalent; the /patterns
// endpoint is experimental and needs the pattern ingester), so this aggregates
// CLIENT-SIDE over the capped Query sample: numeric tokens collapse so
// near-identical lines group (mirroring collapse_nums), counts/first/last are
// per-sample (up to maxLines newest lines), which is enough to NAME what floods
// the logs — the only question logs_error_summary asks of it. k <= 0 defaults
// to 10.
func (c *Client) TopMessages(ctx context.Context, query string, w providers.TimeWindow, k int) ([]providers.MsgCount, error) {
	if k <= 0 {
		k = 10
	}
	lines, err := c.Query(ctx, query, w)
	if err != nil {
		return nil, err
	}
	type agg struct {
		msg         string
		count       int64
		first, last time.Time
	}
	byKey := map[string]int{}
	var groups []agg
	for _, l := range lines {
		if l.Time.IsZero() && len(l.Fields) == 0 {
			continue // the truncation sentinel is not a log message
		}
		key := collapseNums(l.Message)
		i, ok := byKey[key]
		if !ok {
			byKey[key] = len(groups)
			groups = append(groups, agg{msg: l.Message, count: 1, first: l.Time, last: l.Time})
			continue
		}
		g := &groups[i]
		g.count++
		if !l.Time.IsZero() && (g.first.IsZero() || l.Time.Before(g.first)) {
			g.first = l.Time
		}
		if !l.Time.IsZero() && l.Time.After(g.last) {
			g.last = l.Time
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].count > groups[j].count })
	if len(groups) > k {
		groups = groups[:k]
	}
	out := make([]providers.MsgCount, 0, len(groups))
	for _, g := range groups {
		out = append(out, providers.MsgCount{Message: g.msg, Count: g.count, First: g.first, Last: g.last})
	}
	return out, nil
}

// reNumToken / collapseNums mirror internal/investigate/renderlog.go (and
// VictoriaLogs' collapse_nums pipe): free-standing digit runs collapse to "0" so
// lines differing only by a numeric value share one grouping key. Duplicated
// here (3 lines) rather than exported from investigate, which must not become a
// dependency of a backend client.
var reNumToken = regexp.MustCompile(`\d+`)

func collapseNums(msg string) string { return reNumToken.ReplaceAllString(msg, "0") }

// FieldNames lists the field names present in the logs matching query over the
// window — the log-schema discovery path (providers.LogFields). Loki splits the
// answer across two endpoints, so both are merged: STREAM labels from
// /loki/api/v1/labels (query-scoped; these are what a selector is built from,
// so they come first, Hits=0 — Loki reports no per-label hit count) and parsed
// body fields from /loki/api/v1/detected_fields (Loki 3.x; Hits carries the
// reported value cardinality). A missing detected_fields endpoint (older Loki)
// degrades to labels-only; only both failing is an error.
func (c *Client) FieldNames(ctx context.Context, query string, w providers.TimeWindow) ([]providers.FieldCount, error) {
	var out []providers.FieldCount

	lv := url.Values{"query": {query}}
	setWindow(lv, w)
	labelsBody, labelsErr := c.get(ctx, "/loki/api/v1/labels", lv)
	if labelsErr == nil {
		var resp struct {
			Status string   `json:"status"`
			Data   []string `json:"data"`
		}
		if err := json.Unmarshal(labelsBody, &resp); err != nil {
			return nil, fmt.Errorf("parse loki labels: %w", err)
		}
		for _, name := range resp.Data {
			out = append(out, providers.FieldCount{Name: name})
		}
	}

	dv := url.Values{"query": {query}}
	setWindow(dv, w)
	dfBody, dfErr := c.get(ctx, "/loki/api/v1/detected_fields", dv)
	if dfErr != nil {
		if labelsErr == nil {
			return out, nil // older Loki: stream labels alone still answer discovery
		}
		return nil, dfErr
	}
	var resp struct {
		Fields []struct {
			Label       string `json:"label"`
			Cardinality int64  `json:"cardinality"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(dfBody, &resp); err != nil {
		return nil, fmt.Errorf("parse loki detected_fields: %w", err)
	}
	for _, f := range resp.Fields {
		out = append(out, providers.FieldCount{Name: f.Label, Hits: f.Cardinality})
	}
	return out, nil
}
