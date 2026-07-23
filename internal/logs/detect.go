// SPDX-License-Identifier: Apache-2.0

// Package logs hosts the backend-agnostic pieces of the logs data source; the
// concrete clients live in the victorialogs and loki sub-packages.
package logs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
)

// Provider identifiers returned by Detect. They deliberately equal the
// config.LogsProvider* string values (internal/config/config.go) — kept as
// plain strings here so the probe does not depend on the config package.
const (
	ProviderVictoriaLogs = "victorialogs"
	ProviderLoki         = "loki"
)

// Detect probes the logs backend once at startup to distinguish Grafana Loki
// from VictoriaLogs, mirroring the metrics flavor probe
// (internal/metrics/prometheus/prometheus.go DetectFlavor): a config pin
// bypasses it (the caller checks logs.provider first), the probe is
// best-effort, and ANY failure — unreachable backend, non-200, non-buildinfo
// payload — FAILS SAFE to VictoriaLogs, the provider RunLore shipped with, so
// existing deployments see no behaviour change. Loki is identified by a 200
// JSON response with a version on /loki/api/v1/status/buildinfo, an endpoint
// VictoriaLogs does not serve. The probe carries the same auth the real client
// will use (a Loki behind auth must still be detectable); the token is read
// from the environment here and never logged.
func Detect(ctx context.Context, baseURL, tokenEnv string, headers map[string]string) string {
	base := strings.TrimRight(baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/loki/api/v1/status/buildinfo", nil)
	if err != nil {
		return ProviderVictoriaLogs
	}
	if tokenEnv != "" {
		if tok := os.Getenv(tokenEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpx.SecureClient(10 * time.Second).Do(req)
	if err != nil {
		return ProviderVictoriaLogs
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ProviderVictoriaLogs
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var bi struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &bi) != nil || bi.Version == "" {
		return ProviderVictoriaLogs // a 200 that is not buildinfo (proxy page) is not Loki
	}
	return ProviderLoki
}
