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
	"strconv"
	"strings"
	"sync/atomic"
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

	// msgFieldNotAggregatable is a sticky, one-shot latch: once the backend has
	// told us messageField cannot be aggregated server-side, TopMessages stops
	// re-asking. Without it the DEFAULT configuration (ECS `message` is
	// text-only, no keyword sub-field) burns a guaranteed-failing round trip on
	// EVERY logs_error_summary call — and each one also lands in the cluster's
	// own error log, which looks like a RunLore bug to whoever reads it.
	// Whether the field is aggregatable is a property of the index MAPPING, so
	// the answer cannot change under a running process without an operator
	// reindexing and restarting; latching it is safe. atomic (not a bare bool)
	// because the investigation loop runs tools concurrently against one Client.
	msgFieldNotAggregatable atomic.Bool
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
	// A 200 with failed shards is a PARTIAL view (see shardFailure). Say so in
	// band rather than dropping the lines: the same reasoning as the truncation
	// sentinel — the model must know the view is incomplete, and a returned
	// error here would throw away hits that are perfectly good evidence.
	if partial, reason := shardFailure(respBody); partial {
		out = append(out, partialLine(reason))
	}
	return out, nil
}

// partialLine is the shard-failure sibling of providers.TruncationLine: a
// Time-less, Fields-less sentinel so it can never be mistaken for a real log
// entry (and so the client-side aggregation in topMessagesClientSide skips it
// by the same test it already uses for the truncation sentinel).
func partialLine(reason string) providers.LogLine {
	return providers.LogLine{
		Message: fmt.Sprintf("… partial results: %s — some indices in the pattern could not be searched; this view is incomplete", reason),
	}
}

// sourceToLine flattens one hit's ECS-nested `_source` document into a
// providers.LogLine: dot-notation Fields (kubernetes.pod.name, log.level, ...,
// matching the convention query_logs' field selectors use), Message pulled
// from messageField, and Time parsed from timestampField.
func (c *Client) sourceToLine(source map[string]any) providers.LogLine {
	flat := map[string]string{}
	flattenJSON("", source, flat)
	ll := providers.LogLine{Message: flat[c.messageField], Fields: flat}
	if t, ok := parseESTime(flat[c.timestampField]); ok {
		ll.Time = t
	}
	return ll
}

// parseESTime decodes the two on-the-wire shapes an Elasticsearch/OpenSearch
// `date` field can reach us in.
//
//  1. RFC 3339 / ISO-8601 ("2026-08-02T10:00:01.000Z") — the default
//     strict_date_optional_time rendering, and by far the common case. Go's
//     time.Parse accepts the fractional second against the plain RFC3339 layout,
//     so no second layout is needed (an earlier ".999Z07:00" fallback here was
//     dead code).
//  2. Bare epoch MILLISECONDS ("1785664800000"). A `date` mapping may be
//     declared `"format": "epoch_millis"`, in which case `_source` carries a JSON
//     NUMBER, not a string. That is legal ES, and before this was handled every
//     LogLine.Time silently came back zero: no error anywhere, but the timeline
//     render and the client-side fallback's first→last spans all collapsed.
//     epoch_millis is the only numeric date format ES applies by default;
//     epoch_second needs an explicit mapping and is deliberately not guessed at,
//     since the two are indistinguishable from the value alone.
func parseESTime(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t, true
	}
	if ms, err := strconv.ParseInt(ts, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC(), true
	}
	return time.Time{}, false
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
	case float64:
		// encoding/json decodes EVERY JSON number into float64, and fmt.Sprint on
		// a float64 switches to scientific notation past ~7 significant digits —
		// so `event.duration: 123456789` would reach the model as "1.23456789e+08"
		// and an epoch-millis `@timestamp` as "1.7856648e+12", which parseESTime
		// could never decode. 'f' with precision -1 prints the shortest form that
		// round-trips, keeping integral values integral.
		out[prefix] = strconv.FormatFloat(t, 'f', -1, 64)
	default:
		out[prefix] = fmt.Sprint(t)
	}
}

// searchBody builds the shared `_search` request body: a query_string query
// (or match_all when query is empty) plus an optional range filter on
// timestampField for the window bounds. A size >= 0 is sent verbatim, INCLUDING
// size:0 — that is the value Hits/TopMessages pass to say "aggregation buckets
// only, return no hits", and omitting the key there would make ES fall back to
// its default of 10 hits per aggregation request. Only a negative size omits the
// key (no caller does today). sortDesc
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

// indexPath renders the configured index pattern as ONE percent-escaped URL
// path segment. Pasting it in raw let an operator's config silently change the
// request's SHAPE rather than its target: `index: "a/b"` added a path segment
// (hitting a different endpoint entirely), and `index: "logs-*?pretty"` moved
// everything after the `?` into the query string. Escaping is the fix, but two
// characters are restored afterwards because they are both legal unescaped in a
// path segment (RFC 3986 sub-delims) and load-bearing to Elasticsearch: `*` is
// the index wildcard `logs-*` itself, and `,` is its multi-index list separator
// (`logs-app-*,logs-infra-*`). Escaping THOSE would depend on the server
// percent-decoding the path to keep working; escaping `/`, `?`, `#`, and
// whitespace — the actual injections — does not.
func (c *Client) indexPath() string {
	esc := url.PathEscape(c.index)
	esc = strings.ReplaceAll(esc, "%2A", "*")
	esc = strings.ReplaceAll(esc, "%2C", ",")
	return esc
}

// shardFailure reports whether a HTTP 200 `_search` response is only PARTIALLY
// complete, and a short reason if so.
//
// Elasticsearch does not fail a search that some shards could not serve: it
// returns 200 with `_shards.failed > 0` and whatever the surviving shards
// produced. The realistic trigger here is mapping drift across a rolling
// `logs-*` pattern — the index template gained a `message.keyword` sub-field
// partway through, so a `terms` aggregation on it succeeds on the newer indices
// and is rejected on the older ones. The aggregation then comes back looking
// perfectly well-formed while covering only part of the window, and
// logs_error_summary would report those counts as complete. Callers must treat
// a true here as "this answer is not the whole answer".
func shardFailure(body []byte) (bool, string) {
	var resp struct {
		Shards struct {
			Total      int `json:"total"`
			Successful int `json:"successful"`
			Failed     int `json:"failed"`
			Failures   []struct {
				Index  string `json:"index"`
				Reason struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				} `json:"reason"`
			} `json:"failures"`
		} `json:"_shards"`
	}
	// A body that does not parse is not this function's problem — the caller
	// unmarshals it properly right after and reports the real error.
	if err := json.Unmarshal(body, &resp); err != nil || resp.Shards.Failed == 0 {
		return false, ""
	}
	reason := fmt.Sprintf("%d of %d shards failed", resp.Shards.Failed, resp.Shards.Total)
	if len(resp.Shards.Failures) > 0 {
		f := resp.Shards.Failures[0]
		reason += fmt.Sprintf(" (%s on %s: %s)", f.Reason.Type, f.Index, f.Reason.Reason)
	}
	return true, reason
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+c.indexPath()+"/_search", bytes.NewReader(buf))
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
	// Unlike Query, []Bucket has no channel for an in-band notice, and a
	// silently-short histogram is worse than no histogram: it understates the
	// very spike logs_error_summary exists to find. Fail loudly instead — the
	// caller (internal/investigate/logs_summary_tool.go) degrades gracefully,
	// still rendering top messages and surfacing this reason.
	if partial, reason := shardFailure(respBody); partial {
		return nil, fmt.Errorf("logs histogram is incomplete: %s", reason)
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
// response is the specific "aggregating over a text-only field" rejection a
// terms aggregation gets when its target field is mapped `text` with no keyword
// sub-field — exactly how ECS ships `message` by default.
//
// DO NOT "simplify" this to a single, more literal substring. The rejection
// wording is NOT identical across the two distributions; it was captured
// verbatim from both (see live_verify_test.go):
//
//	ES 8.15.0:        "Fielddata is disabled on [message] in [logs-demo]. Text
//	                   fields are not optimised for operations that require
//	                   per-document field data … Please use a keyword field
//	                   instead. Alternatively, set fielddata=true on [message] …"
//	OpenSearch 2.17.0: "Text fields are not optimised for operations that require
//	                   per-document field data … Alternatively, set
//	                   fielddata=true on [message] …"
//
// The `Fielddata is disabled on [x] in [y].` sentence is ELASTICSEARCH-ONLY —
// matching on it would silently break the fallback on OpenSearch, turning every
// logs_error_summary there into a hard error. What both wordings do share, and
// all this therefore keys on, is two lowercased tokens: "fielddata" (from
// `set fielddata=true`) and "text field" (a substring of "Text fields are not
// optimised"). Both were confirmed present in both distributions' real output.
//
// Any OTHER 400 (a malformed query, a missing index) must NOT match, so a real
// failure still surfaces as an error instead of being silently swallowed into
// the fallback — TestTopMessagesRealErrorPropagates pins that.
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
	// Sticky: a previous call already learned this field is not aggregatable, so
	// skip straight to the fallback instead of re-issuing a request we know the
	// backend will reject (and log as an error on its side).
	if c.msgFieldNotAggregatable.Load() {
		return c.topMessagesClientSide(ctx, query, w, k)
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
			// Latch it: the mapping will not change under this process.
			c.msgFieldNotAggregatable.Store(true)
			return c.topMessagesClientSide(ctx, query, w, k)
		}
		return nil, fmt.Errorf("logs status %d: %s", status, string(respBody))
	}
	// A 200 whose shards partly failed (see shardFailure) — the mapping-drift
	// case, where only the OLDER indices in the pattern lack the aggregatable
	// sub-field. The buckets that came back parse perfectly and would be
	// reported as the definitive top messages while covering only part of the
	// window. Take the client-side path instead: a plain `_search` over a `text`
	// field is not what the shards choked on, so the fallback gets a COMPLETE
	// (if sample-capped) answer. Deliberately NOT latched — unlike the mapping
	// rejection above, a shard failure can be transient (a node briefly out),
	// and permanently downgrading a working aggregation over one blip would be
	// the wrong trade.
	if partial, _ := shardFailure(respBody); partial {
		return c.topMessagesClientSide(ctx, query, w, k)
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
	if t, ok := parseESTime(a.ValueAsString); ok {
		return t
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+c.indexPath()+"/_field_caps?"+v.Encode(), nil)
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
