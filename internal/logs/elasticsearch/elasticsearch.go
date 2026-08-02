// SPDX-License-Identifier: Apache-2.0

// Package elasticsearch implements providers.LogsProvider against Elasticsearch
// and OpenSearch, querying with the classic `_search` DSL — the one dialect both
// ES 8.x and OpenSearch 2.x speak, so one client serves both distributions. It
// mirrors internal/logs/loki: same construction/auth shape, same optional
// LogStats/LogFields capabilities, same truncation sentinel. The one genuine
// dialect difference — a `terms` aggregation over the ECS `message` field,
// which ships `text`-only (no keyword sub-field) by default — is handled by
// falling back to client-side aggregation; see TopMessages.
package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

// Client queries an Elasticsearch or OpenSearch backend over the classic
// `_search` DSL. The distribution (which only matters for startup detection,
// internal/logs.Detect) is irrelevant here: both speak an identical request/
// response envelope for _search, _field_caps, and aggregations.
type Client struct {
	baseURL        string
	index          string            // index pattern, e.g. "logs-*"
	tokenEnv       string            // env var holding a bearer token; empty ⇒ no auth
	headers        map[string]string // static extra request headers (e.g. an API-key header)
	maxLines       int               // per-query hit cap (the `size` parameter)
	levelField     string            // severity field Hits/TopMessages split by; "" ⇒ defaultLevelField
	timestampField string            // field the range filter/sort/date_histogram use; "" ⇒ defaultTimestampField
	messageField   string            // field TopMessages aggregates over; "" ⇒ defaultMessageField
	http           *http.Client
}

// defaultIndex is the ECS/Filebeat convention index pattern, used when
// config.logs.index is left unset.
const defaultIndex = "logs-*"

// defaultMaxLines bounds the number of hits Query returns (the `size` param,
// and the truncation-sentinel threshold), matching victorialogs/loki.
const defaultMaxLines = 1000

// ECS field-convention defaults (Filebeat/ECS-shaped documents, the collector
// convention RunLore documents for this backend). Overridable via
// WithLevelField/WithTimestampField/WithMessageField (config.logs.fields).
const (
	defaultLevelField     = "log.level"
	defaultTimestampField = "@timestamp"
	defaultMessageField   = "message"
)

// New builds a client for an Elasticsearch/OpenSearch base URL, unauthenticated.
// An empty index defaults to "logs-*".
func New(baseURL, index string) *Client {
	return NewWithAuth(baseURL, index, "", nil)
}

// NewWithAuth builds a client that adds optional auth to every request; the
// semantics are identical to loki.NewWithAuth / victorialogs.NewWithAuth (token
// read from the env at request-build time, never logged).
func NewWithAuth(baseURL, index, tokenEnv string, headers map[string]string) *Client {
	if index == "" {
		index = defaultIndex
	}
	return &Client{
		baseURL:        strings.TrimRight(baseURL, "/"),
		index:          index,
		tokenEnv:       tokenEnv,
		headers:        headers,
		maxLines:       defaultMaxLines,
		levelField:     defaultLevelField,
		timestampField: defaultTimestampField,
		messageField:   defaultMessageField,
		http:           httpx.SecureClient(30 * time.Second),
	}
}

// WithLevelField overrides the severity field Hits splits by (config.logs.fields.level_field).
// Empty is a no-op so an unset config keeps the default; returns the client for chaining.
func (c *Client) WithLevelField(field string) *Client {
	if field != "" {
		c.levelField = field
	}
	return c
}

// WithTimestampField overrides the field the range filter, sort, and
// date_histogram key on (config.logs.fields.timestamp_field). Empty is a no-op.
func (c *Client) WithTimestampField(field string) *Client {
	if field != "" {
		c.timestampField = field
	}
	return c
}

// WithMessageField overrides the field TopMessages aggregates over
// (config.logs.fields.message_field). Empty is a no-op.
func (c *Client) WithMessageField(field string) *Client {
	if field != "" {
		c.messageField = field
	}
	return c
}

var (
	_ providers.LogsProvider = (*Client)(nil)
	// Optional analytics/discovery capabilities — consumers type-assert for these.
	_ providers.LogStats  = (*Client)(nil)
	_ providers.LogFields = (*Client)(nil)
)

// Query runs a Lucene query_string query over the window and returns
// normalized log lines, newest first (matching VictoriaLogs/Loki ordering). It
// sends one `_search` request with size=maxLines and sort desc on the
// timestamp field; when the server returns exactly the cap, more hits likely
// matched and the shared truncation sentinel is appended.
func (c *Client) Query(ctx context.Context, query string, w providers.TimeWindow) (providers.LogResult, error) {
	body := c.searchBody(query, w, c.maxLines, true)
	respBody, err := c.doSearch(ctx, body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Hits struct {
			Hits []struct {
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parse elasticsearch response: %w", err)
	}
	var out providers.LogResult
	for _, h := range resp.Hits.Hits {
		out = append(out, c.sourceToLine(h.Source))
	}
	if len(resp.Hits.Hits) >= c.maxLines {
		out = append(out, providers.TruncationLine(int64(c.maxLines)))
	}
	return out, nil
}

// sourceToLine flattens one hit's ECS-nested `_source` document into a
// providers.LogLine: dot-notation Fields (kubernetes.pod.name, log.level, ...,
// matching the convention query_logs' field selectors use), Message pulled
// from messageField, and Time parsed from timestampField.
func (c *Client) sourceToLine(source map[string]any) providers.LogLine {
	flat := map[string]string{}
	flattenJSON("", source, flat)
	ll := providers.LogLine{Message: flat[c.messageField], Fields: flat}
	if ts := flat[c.timestampField]; ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			ll.Time = t
		} else if t, err := time.Parse("2006-01-02T15:04:05.999Z07:00", ts); err == nil {
			ll.Time = t
		}
	}
	return ll
}

// flattenJSON walks a decoded JSON value and writes leaf scalars into out,
// keyed by their dot-notation path (kubernetes.pod.name), so an ECS document's
// nested objects become the SAME flat field-name convention VictoriaLogs/Loki
// already expose via stream labels. Arrays are stringified whole (rare in log
// documents, and none of the three tools need per-element access). Explicit
// JSON null is skipped rather than rendered as the literal "<nil>".
func flattenJSON(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenJSON(key, vv, out)
		}
	case nil:
		// skip
	case string:
		out[prefix] = t
	default:
		out[prefix] = fmt.Sprint(t)
	}
}

// searchBody builds the shared `_search` request body: a query_string query
// (or match_all when query is empty) plus an optional range filter on
// timestampField for the window bounds. size <= 0 omits the size key (used by
// Hits/TopMessages, which want size:0 — hits only from aggregations). sortDesc
// adds the newest-first sort Query needs; Hits/TopMessages don't (they read
// only aggregations, so sorting individual hits is pointless cost).
func (c *Client) searchBody(query string, w providers.TimeWindow, size int, sortDesc bool) map[string]any {
	must := []map[string]any{}
	if query != "" {
		must = append(must, map[string]any{"query_string": map[string]any{"query": query}})
	} else {
		must = append(must, map[string]any{"match_all": map[string]any{}})
	}
	boolQ := map[string]any{"must": must}
	if rangeFilter := c.rangeFilter(w); rangeFilter != nil {
		boolQ["filter"] = []map[string]any{rangeFilter}
	}
	body := map[string]any{"query": map[string]any{"bool": boolQ}}
	if size >= 0 {
		body["size"] = size
	}
	if sortDesc {
		body["sort"] = []map[string]any{{c.timestampField: map[string]any{"order": "desc"}}}
	}
	return body
}

// rangeFilter returns the range clause bounding the window on timestampField,
// or nil when the window has neither bound set.
func (c *Client) rangeFilter(w providers.TimeWindow) map[string]any {
	r := map[string]any{}
	if !w.Start.IsZero() {
		r["gte"] = w.Start.UTC().Format(time.RFC3339)
	}
	if !w.End.IsZero() {
		r["lte"] = w.End.UTC().Format(time.RFC3339)
	}
	if len(r) == 0 {
		return nil
	}
	return map[string]any{"range": map[string]any{c.timestampField: r}}
}

// search POSTs a `_search` (or `_field_caps` via searchGet) request body and
// returns the raw response body + HTTP status WITHOUT interpreting the status —
// TopMessages needs the raw (status, body) pair to distinguish a genuine error
// from the specific text-field aggregation rejection it falls back on.
func (c *Client) search(ctx context.Context, body map[string]any) ([]byte, int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("encode search body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+c.index+"/_search", bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("logs query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, nil
}

// doSearch runs search and turns a non-200 into an error (the common case;
// only TopMessages needs the raw status to special-case one specific 400).
func (c *Client) doSearch(ctx context.Context, body map[string]any) ([]byte, error) {
	respBody, status, err := c.search(ctx, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("logs status %d: %s", status, string(respBody))
	}
	return respBody, nil
}

// Hits returns the per-step match count over the window, split by severity,
// via a `date_histogram` aggregation (keyed on timestampField) with a `terms`
// sub-aggregation on levelField — the Elasticsearch/OpenSearch analogue of
// VictoriaLogs' /select/logsql/hits and Loki's count_over_time wrapper,
// powering the logs_error_summary histogram. A step <= 0 defaults to one
// minute. size:0 on the outer search — only the aggregation buckets are read,
// no individual hits are fetched.
func (c *Client) Hits(ctx context.Context, query string, w providers.TimeWindow, step time.Duration) ([]providers.Bucket, error) {
	if step <= 0 {
		step = time.Minute
	}
	body := c.searchBody(query, w, 0, false)
	body["aggs"] = map[string]any{
		"by_time": map[string]any{
			"date_histogram": map[string]any{
				"field":          c.timestampField,
				"fixed_interval": fmt.Sprintf("%ds", int(step.Seconds())),
			},
			"aggs": map[string]any{
				"by_level": map[string]any{
					"terms": map[string]any{"field": c.levelField, "size": 20},
				},
			},
		},
	}
	respBody, err := c.doSearch(ctx, body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Aggregations struct {
			ByTime struct {
				Buckets []struct {
					Key     int64 `json:"key"` // epoch millis
					ByLevel struct {
						Buckets []struct {
							Key      string `json:"key"`
							DocCount int64  `json:"doc_count"`
						} `json:"buckets"`
					} `json:"by_level"`
				} `json:"buckets"`
			} `json:"by_time"`
		} `json:"aggregations"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parse elasticsearch aggregation: %w", err)
	}
	var out []providers.Bucket
	for _, tb := range resp.Aggregations.ByTime.Buckets {
		ts := time.UnixMilli(tb.Key).UTC()
		for _, lb := range tb.ByLevel.Buckets {
			out = append(out, providers.Bucket{Time: ts, Level: lb.Key, Count: lb.DocCount})
		}
	}
	return out, nil
}

// isTextFieldAggError reports whether an Elasticsearch/OpenSearch error
// response is the specific "aggregating over a text-only field" rejection —
// the wording ES emits verbatim (OpenSearch, forked from ES 7.10, kept it) when
// a terms aggregation targets a field mapped `text` with no keyword sub-field,
// which is exactly how ECS ships the `message` field by default. Any OTHER 400
// (a malformed query, a missing index) must NOT match, so a real failure still
// surfaces as an error instead of being silently swallowed into the fallback.
func isTextFieldAggError(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "fielddata") && strings.Contains(s, "text field")
}

// TopMessages returns up to k dominant messages over the window. It FIRST
// tries a server-side `terms` aggregation on messageField (cheap and
// corpus-wide when the field is aggregatable — e.g. an operator added a
// `message.keyword` multi-field and pointed logs.fields.message_field at it).
// When the server rejects that specific aggregation because the field is
// `text`-only (see isTextFieldAggError) — the default for ECS's `message`
// field — it falls back to CLIENT-SIDE aggregation over the capped Query
// sample, identical in spirit to loki.Client.TopMessages: numeric tokens
// collapse so near-identical lines group, counts/first/last are per-sample
// (up to maxLines newest lines) rather than corpus-wide. Any OTHER error
// propagates rather than triggering the fallback. k <= 0 defaults to 10.
func (c *Client) TopMessages(ctx context.Context, query string, w providers.TimeWindow, k int) ([]providers.MsgCount, error) {
	if k <= 0 {
		k = 10
	}
	body := c.searchBody(query, w, 0, false)
	body["aggs"] = map[string]any{
		"top_messages": map[string]any{
			"terms": map[string]any{"field": c.messageField, "size": k},
			"aggs": map[string]any{
				"first_seen": map[string]any{"min": map[string]any{"field": c.timestampField}},
				"last_seen":  map[string]any{"max": map[string]any{"field": c.timestampField}},
			},
		},
	}
	respBody, status, err := c.search(ctx, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if isTextFieldAggError(status, respBody) {
			return c.topMessagesClientSide(ctx, query, w, k)
		}
		return nil, fmt.Errorf("logs status %d: %s", status, string(respBody))
	}
	var resp struct {
		Aggregations struct {
			TopMessages struct {
				Buckets []struct {
					Key       string  `json:"key"`
					DocCount  int64   `json:"doc_count"`
					FirstSeen aggStat `json:"first_seen"`
					LastSeen  aggStat `json:"last_seen"`
				} `json:"buckets"`
			} `json:"top_messages"`
		} `json:"aggregations"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parse elasticsearch aggregation: %w", err)
	}
	out := make([]providers.MsgCount, 0, len(resp.Aggregations.TopMessages.Buckets))
	for _, b := range resp.Aggregations.TopMessages.Buckets {
		out = append(out, providers.MsgCount{
			Message: b.Key, Count: b.DocCount,
			First: b.FirstSeen.parsed(), Last: b.LastSeen.parsed(),
		})
	}
	return out, nil
}

// aggStat is a min/max metric aggregation's result: a numeric epoch-millis
// value plus ES/OpenSearch's own formatted echo of it (value_as_string), which
// parsed() prefers since it needs no epoch math.
type aggStat struct {
	Value         float64 `json:"value"`
	ValueAsString string  `json:"value_as_string"`
}

func (a aggStat) parsed() time.Time {
	if a.ValueAsString != "" {
		if t, err := time.Parse(time.RFC3339, a.ValueAsString); err == nil {
			return t
		}
		if t, err := time.Parse("2006-01-02T15:04:05.999Z07:00", a.ValueAsString); err == nil {
			return t
		}
	}
	if a.Value != 0 {
		return time.UnixMilli(int64(a.Value)).UTC()
	}
	return time.Time{}
}

// topMessagesClientSide is the fallback path TopMessages takes when the
// message field can't be aggregated server-side: run the capped, newest-first
// Query and group client-side, exactly like loki.Client.TopMessages.
func (c *Client) topMessagesClientSide(ctx context.Context, query string, w providers.TimeWindow, k int) ([]providers.MsgCount, error) {
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

// reNumToken / collapseNums mirror internal/investigate/renderlog.go and
// loki.Client's own copy: free-standing digit runs collapse to "0" so lines
// differing only by a numeric value share one grouping key. Duplicated here (3
// lines) rather than exported from investigate, which must not become a
// dependency of a backend client — the same call loki.go makes.
var reNumToken = regexp.MustCompile(`\d+`)

func collapseNums(msg string) string { return reNumToken.ReplaceAllString(msg, "0") }

// FieldNames lists the field names present in the configured index pattern via
// `_field_caps` — present on both ES 8.x and OpenSearch 2.x — the log-schema
// discovery path so a query_logs that assumed the wrong collector field names
// can be corrected. UNLIKE VictoriaLogs'/Loki's discovery, field_caps has no
// query/window scope (it is a MAPPING introspection endpoint, not a data
// query) and reports no per-field hit count, so every FieldCount.Hits is 0 and
// the query/window arguments are accepted for interface parity but unused —
// documented as a parity caveat on the docs page. Results are sorted
// alphabetically for determinism (field_caps has no frequency signal to order
// by).
func (c *Client) FieldNames(ctx context.Context, _ string, _ providers.TimeWindow) ([]providers.FieldCount, error) {
	v := url.Values{"fields": {"*"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+c.index+"/_field_caps?"+v.Encode(), nil)
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
	var fc struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(body, &fc); err != nil {
		return nil, fmt.Errorf("parse field_caps: %w", err)
	}
	names := make([]string, 0, len(fc.Fields))
	for name := range fc.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]providers.FieldCount, 0, len(names))
	for _, name := range names {
		out = append(out, providers.FieldCount{Name: name})
	}
	return out, nil
}

// setAuth applies the optional bearer token and static headers to req — the
// SAME semantics as loki.Client.setAuth / victorialogs.Client.setAuth: token
// read from the environment here, never logged. For an Elasticsearch/
// OpenSearch API key (Authorization: ApiKey <base64>) rather than a bearer
// token, set headers.Authorization explicitly instead of token_env — it is
// applied AFTER the bearer header, so it wins.
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
