// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Smana/runlore/internal/httpx"
)

// Firing reports whether Alertmanager holds an active, unsilenced, uninhibited alert named alert
// (and, when namespace is not empty, carrying that namespace label). A non-200 answer, a body that
// is not a JSON list, or a transport error is an error: the verdict is never guessed.
func Firing(ctx context.Context, hc *http.Client, baseURL, alert, namespace string) (bool, error) {
	q := url.Values{
		"active":    {"true"},
		"silenced":  {"false"},
		"inhibited": {"false"},
		"filter":    {"alertname=" + strconv.Quote(alert)},
	}
	if namespace != "" {
		q.Add("filter", "namespace="+strconv.Quote(namespace))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v2/alerts?"+q.Encode(), nil)
	if err != nil {
		return false, httpx.SanitizeURLError(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("alertmanager: %w", httpx.SanitizeURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, fmt.Errorf("alertmanager: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("alertmanager: status %d", resp.StatusCode)
	}
	var alerts []json.RawMessage
	if err := json.Unmarshal(body, &alerts); err != nil {
		return false, fmt.Errorf("alertmanager: malformed response: %w", err)
	}
	return len(alerts) > 0, nil
}
