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
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Client queries a VictoriaLogs backend.
type Client struct {
	baseURL string
	limit   int
	http    *http.Client
}

// New builds a client for a VictoriaLogs base URL.
func New(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), limit: 100, http: &http.Client{Timeout: 30 * time.Second}}
}

var _ providers.LogsProvider = (*Client)(nil)

// Query runs a LogsQL query over the window and returns normalized log lines.
func (c *Client) Query(ctx context.Context, query string, w providers.TimeWindow) (providers.LogResult, error) {
	form := url.Values{"query": {query}, "limit": {fmt.Sprintf("%d", c.limit)}}
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
