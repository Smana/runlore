// SPDX-License-Identifier: Apache-2.0

// Package prometheus implements providers.MetricsProvider against the Prometheus
// HTTP API — also spoken by VictoriaMetrics — for instant and range PromQL queries.
package prometheus

import (
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

// Client queries a Prometheus-compatible metrics backend.
type Client struct {
	baseURL  string
	tokenEnv string            // env var holding a bearer token; empty ⇒ no auth
	headers  map[string]string // static extra request headers (e.g. tenant header)
	http     *http.Client
}

// New builds a client for a Prometheus/VictoriaMetrics base URL, unauthenticated.
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
		http:     httpx.SecureClient(30 * time.Second),
	}
}

var _ providers.MetricsProvider = (*Client)(nil)

type apiResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

// Query runs an instant PromQL query (at = zero means "now").
func (c *Client) Query(ctx context.Context, promql string, at time.Time) (providers.Samples, error) {
	v := url.Values{"query": {promql}}
	if !at.IsZero() {
		v.Set("time", strconv.FormatInt(at.Unix(), 10))
	}
	resp, err := c.get(ctx, "/api/v1/query", v)
	if err != nil {
		return nil, err
	}
	out := make(providers.Samples, 0, len(resp.Data.Result))
	for _, raw := range resp.Data.Result {
		var item struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		ts, val := parsePoint(item.Value)
		out = append(out, providers.Sample{Metric: item.Metric, Value: val, Time: ts})
	}
	return out, nil
}

// QueryRange runs a range PromQL query over a window.
func (c *Client) QueryRange(ctx context.Context, promql string, w providers.TimeWindow, step time.Duration) (providers.Matrix, error) {
	if step <= 0 {
		step = time.Minute
	}
	v := url.Values{
		"query": {promql},
		"start": {strconv.FormatInt(w.Start.Unix(), 10)},
		"end":   {strconv.FormatInt(w.End.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step.Seconds()), 10)},
	}
	resp, err := c.get(ctx, "/api/v1/query_range", v)
	if err != nil {
		return nil, err
	}
	out := make(providers.Matrix, 0, len(resp.Data.Result))
	for _, raw := range resp.Data.Result {
		var item struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		s := providers.Series{Metric: item.Metric}
		for _, p := range item.Values {
			ts, val := parsePoint(p)
			s.Points = append(s.Points, providers.Point{Time: ts, Value: val})
		}
		out = append(out, s)
	}
	return out, nil
}

// LabelValues lists the values of a label across series matching matchers, within
// the window — the metric/label discovery path so the agent can find real names
// instead of guessing (label "__name__" enumerates metric names). It hits
// GET /api/v1/label/<label>/values?match[]=…&start=…&end=…, scoping by matcher +
// window so it stays cheap on a big TSDB. Values are returned as the backend
// orders them (Prometheus sorts; VictoriaMetrics may not) — callers that need a
// stable order sort themselves.
func (c *Client) LabelValues(ctx context.Context, label string, matchers []string, w providers.TimeWindow) ([]string, error) {
	v := url.Values{}
	for _, m := range matchers {
		if m != "" {
			v.Add("match[]", m)
		}
	}
	if !w.Start.IsZero() {
		v.Set("start", strconv.FormatInt(w.Start.Unix(), 10))
	}
	if !w.End.IsZero() {
		v.Set("end", strconv.FormatInt(w.End.Unix(), 10))
	}
	// The label name is a path segment; escape it so a label like "__name__" (or an
	// arbitrary one) can't break out of the path.
	resp, err := c.getRaw(ctx, "/api/v1/label/"+url.PathEscape(label)+"/values", v)
	if err != nil {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("parse label values: %w", err)
	}
	return out, nil
}

// getRaw performs a GET and returns the raw `data` field, for endpoints whose
// data shape differs from the query result envelope (label values is a []string).
func (c *Client) getRaw(ctx context.Context, path string, v url.Values) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metrics query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics status %d: %s", resp.StatusCode, string(data))
	}
	var r struct {
		Status string          `json:"status"`
		Error  string          `json:"error"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse metrics response: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("metrics error: %s", r.Error)
	}
	return r.Data, nil
}

func (c *Client) get(ctx context.Context, path string, v url.Values) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metrics query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics status %d: %s", resp.StatusCode, string(data))
	}
	var r apiResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse metrics response: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("metrics error: %s", r.Error)
	}
	return &r, nil
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

// parsePoint parses a Prometheus [unixTime, "value"] pair.
func parsePoint(p [2]any) (time.Time, float64) {
	var ts time.Time
	if f, ok := p[0].(float64); ok {
		ts = time.Unix(int64(f), 0).UTC()
	}
	var val float64
	if s, ok := p[1].(string); ok {
		val, _ = strconv.ParseFloat(s, 64)
	}
	return ts, val
}
