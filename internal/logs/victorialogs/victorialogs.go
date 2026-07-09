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
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

// Client queries a VictoriaLogs backend.
type Client struct {
	baseURL  string
	tokenEnv string            // env var holding a bearer token; empty ⇒ no auth
	headers  map[string]string // static extra request headers (e.g. tenant header)
	limit    int               // per-request page size
	maxLines int               // total cap across pages
	http     *http.Client
}

// defaultMaxLines bounds the total number of lines Query returns across pages.
const defaultMaxLines = 1000

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
		baseURL:  strings.TrimRight(baseURL, "/"),
		tokenEnv: tokenEnv,
		headers:  headers,
		limit:    100,
		maxLines: defaultMaxLines,
		http:     httpx.SecureClient(30 * time.Second),
	}
}

var _ providers.LogsProvider = (*Client)(nil)

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
