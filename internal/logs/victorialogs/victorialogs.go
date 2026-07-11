// SPDX-License-Identifier: Apache-2.0

// Package victorialogs implements providers.LogsProvider against VictoriaLogs,
// querying with LogsQL and normalizing the NDJSON response into log lines.
package victorialogs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

// Client queries a VictoriaLogs backend.
type Client struct {
	baseURL    string
	tokenEnv   string            // env var holding a bearer token; empty ⇒ no auth
	headers    map[string]string // static extra request headers (e.g. tenant header)
	limit      int               // per-request page size
	maxLines   int               // total cap across pages
	levelField string            // hits-split severity field; "" ⇒ defaultLevelField
	http       *http.Client
}

// defaultMaxLines bounds the total number of lines Query returns across pages.
const defaultMaxLines = 1000

// defaultLevelField is the severity field Hits splits by, matching the collector
// convention RunLore shipped with. Overridable via WithLevelField for a collector
// that names its severity field differently. Kept as the fallback so an unset config
// reproduces the previous hardcoded behaviour exactly.
const defaultLevelField = "level"

// New builds a client for a VictoriaLogs base URL, unauthenticated.
func New(baseURL string) *Client {
	return NewWithAuth(baseURL, "", nil)
}

// NewWithAuth builds a client that adds optional auth to every request. tokenEnv
// names an env var holding a bearer token (empty, or pointing at an unset/empty
// var, ⇒ no Authorization header); headers are static request headers (e.g.
// "X-Scope-OrgID" for a multi-tenant backend). The token is read from the
// environment at request-build time and is never logged.
func NewWithAuth(baseURL, tokenEnv string, headers map[string]string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokenEnv:   tokenEnv,
		headers:    headers,
		limit:      100,
		maxLines:   defaultMaxLines,
		levelField: defaultLevelField,
		http:       httpx.SecureClient(30 * time.Second),
	}
}

// WithLevelField overrides the severity field Hits splits by (config.logs.fields).
// An empty name is ignored so a caller passing an unset config keeps the default. It
// returns the client to allow one-line construction: New(url).WithLevelField(f).
func (c *Client) WithLevelField(field string) *Client {
	if field != "" {
		c.levelField = field
	}
	return c
}

var (
	_ providers.LogsProvider = (*Client)(nil)
	// Optional analytics/discovery capabilities — consumers type-assert for these,
	// so a future backend that lacks them still satisfies LogsProvider.
	_ providers.LogStats  = (*Client)(nil)
	_ providers.LogFields = (*Client)(nil)
)

// Query runs a LogsQL query over the window and returns normalized log lines.
//
// It pages with limit+offset up to maxLines so a high-volume window is not
// silently capped at a single page; when the cap binds (the page that reaches it
// was full, implying more matched), a sentinel line signals the partial view.
//
// Offset pagination orders by the largest _time values; concurrent ingestion into
// the queried range could shift the page boundary, but incidents query a settled
// past window, so this is acceptable for v1.
func (c *Client) Query(ctx context.Context, query string, w providers.TimeWindow) (providers.LogResult, error) {
	var out providers.LogResult
	truncated := false
	for offset := 0; ; offset += c.limit {
		page, err := c.queryPage(ctx, query, w, c.limit, offset)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(out) >= c.maxLines {
			out = out[:c.maxLines]
			// A full last page implies more lines likely exist past the cap.
			truncated = len(page) == c.limit
			break
		}
		if len(page) < c.limit { // short page → the stream is exhausted
			break
		}
	}
	if truncated {
		out = append(out, truncationLine(c.maxLines))
	}
	return out, nil
}

// queryPage runs one limit+offset page of a LogsQL query over the window.
func (c *Client) queryPage(ctx context.Context, query string, w providers.TimeWindow, limit, offset int) (providers.LogResult, error) {
	form := url.Values{
		"query":  {query},
		"limit":  {fmt.Sprintf("%d", limit)},
		"offset": {fmt.Sprintf("%d", offset)},
	}
	if !w.Start.IsZero() {
		form.Set("start", w.Start.UTC().Format(time.RFC3339))
	}
	if !w.End.IsZero() {
		form.Set("end", w.End.UTC().Format(time.RFC3339))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/select/logsql/query", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("logs query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("logs status %d: %s", resp.StatusCode, string(body))
	}
	return parseNDJSON(resp.Body)
}

// Hits returns the per-step match count over the window, split by log level when
// the level field is present, via /select/logsql/hits (field=level). It powers the
// logs_error_summary histogram ("3/5m baseline → 412/5m spike"). A step <= 0
// defaults to one minute. Buckets carry Level="" for the unsplit series a backend
// without a level field returns.
func (c *Client) Hits(ctx context.Context, query string, w providers.TimeWindow, step time.Duration) ([]providers.Bucket, error) {
	if step <= 0 {
		step = time.Minute
	}
	levelField := c.levelField
	if levelField == "" {
		levelField = defaultLevelField
	}
	form := url.Values{
		"query": {query},
		"step":  {fmt.Sprintf("%ds", int(step.Seconds()))},
		"field": {levelField}, // split by severity when the field exists; harmless otherwise
	}
	setWindow(form, w)
	body, err := c.postForm(ctx, "/select/logsql/hits", form)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Hits []struct {
			Fields     map[string]string `json:"fields"`
			Timestamps []string          `json:"timestamps"`
			Values     []int64           `json:"values"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse hits: %w", err)
	}
	var out []providers.Bucket
	for _, series := range resp.Hits {
		level := series.Fields[levelField]
		for i, ts := range series.Timestamps {
			if i >= len(series.Values) {
				break
			}
			t, _ := time.Parse(time.RFC3339, ts)
			out = append(out, providers.Bucket{Time: t, Level: level, Count: series.Values[i]})
		}
	}
	return out, nil
}

// TopMessages returns up to k dominant messages over the window. It runs
// `<query> | collapse_nums | stats by (_msg) count() rows, min(_time) first,
// max(_time) last | sort by (rows desc) | limit k` — collapse_nums normalizes
// numeric tokens so "took 12ms" and "took 907ms" group into one message. The
// stats pipe streams NDJSON rows through the raw query endpoint. k <= 0 defaults
// to 10.
func (c *Client) TopMessages(ctx context.Context, query string, w providers.TimeWindow, k int) ([]providers.MsgCount, error) {
	if k <= 0 {
		k = 10
	}
	pipe := fmt.Sprintf("%s | collapse_nums | stats by (_msg) count() rows, min(_time) first, max(_time) last | sort by (rows desc) | limit %d", query, k)
	form := url.Values{"query": {pipe}}
	setWindow(form, w)
	body, err := c.postForm(ctx, "/select/logsql/query", form)
	if err != nil {
		return nil, err
	}
	rows, err := parseNDJSON(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	out := make([]providers.MsgCount, 0, len(rows))
	for _, r := range rows {
		mc := providers.MsgCount{Message: r.Message}
		if v := r.Fields["rows"]; v != "" {
			mc.Count, _ = strconv.ParseInt(v, 10, 64)
		}
		mc.First, _ = time.Parse(time.RFC3339, r.Fields["first"])
		mc.Last, _ = time.Parse(time.RFC3339, r.Fields["last"])
		out = append(out, mc)
	}
	return out, nil
}

// FieldNames lists the field names present in the logs matching query over the
// window, via /select/logsql/field_names — the log-schema discovery path so a
// query that assumed the wrong collector field names can be corrected. Results
// are most-frequent first, as the backend orders them.
func (c *Client) FieldNames(ctx context.Context, query string, w providers.TimeWindow) ([]providers.FieldCount, error) {
	form := url.Values{"query": {query}}
	setWindow(form, w)
	body, err := c.postForm(ctx, "/select/logsql/field_names", form)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Values []struct {
			Value string `json:"value"`
			Hits  int64  `json:"hits"`
		} `json:"values"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse field_names: %w", err)
	}
	out := make([]providers.FieldCount, 0, len(resp.Values))
	for _, v := range resp.Values {
		out = append(out, providers.FieldCount{Name: v.Value, Hits: v.Hits})
	}
	return out, nil
}

// setWindow adds the optional start/end RFC3339 bounds to a LogsQL form.
func setWindow(form url.Values, w providers.TimeWindow) {
	if !w.Start.IsZero() {
		form.Set("start", w.Start.UTC().Format(time.RFC3339))
	}
	if !w.End.IsZero() {
		form.Set("end", w.End.UTC().Format(time.RFC3339))
	}
}

// postForm POSTs an x-www-form-urlencoded body to a VictoriaLogs endpoint and
// returns the raw response body, applying auth exactly like queryPage. It is the
// shared request path for the analytics/discovery endpoints (hits, field_names,
// and the stats-pipe query).
func (c *Client) postForm(ctx context.Context, path string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

// setAuth applies the optional bearer token and static headers to req. The token
// is read from the environment here (request-build time) and never logged; an
// unset/empty token var leaves the request unauthenticated.
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

// truncationLine is the sentinel appended when Query stops at its cap with more
// lines upstream, so the model knows the view is partial. It carries no Time or
// Fields, so it cannot be mistaken for a real log line.
func truncationLine(limit int) providers.LogLine {
	return providers.LogLine{
		Message: fmt.Sprintf("… results truncated at %d (more matched — narrow the query or shorten the window)", limit),
	}
}

// parseNDJSON turns VictoriaLogs' newline-delimited JSON into log lines. Each
// object carries `_time`, `_msg`, and arbitrary stream fields.
func parseNDJSON(r io.Reader) (providers.LogResult, error) {
	var out providers.LogResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long log lines
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			continue // skip malformed lines rather than failing the whole query
		}
		ll := providers.LogLine{Fields: map[string]string{}}
		for k, v := range m {
			s := fmt.Sprint(v)
			switch k {
			case "_time":
				ll.Time, _ = time.Parse(time.RFC3339, s)
			case "_msg":
				ll.Message = s
			default:
				ll.Fields[k] = s
			}
		}
		out = append(out, ll)
	}
	return out, sc.Err()
}
