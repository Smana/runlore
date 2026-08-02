// SPDX-License-Identifier: Apache-2.0

// Package logs hosts the backend-agnostic pieces of the logs data source; the
// concrete clients live in the victorialogs, loki, and elasticsearch sub-packages.
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
// plain strings here so the probe does not depend on the config package. They
// are also the ids internal/docsguard reflects a docs/integrations page's
// `integration: {kind: logs, id: ...}` claim against, so a rename here must be
// mirrored on the matching page.
const (
	ProviderVictoriaLogs  = "victorialogs"
	ProviderLoki          = "loki"
	ProviderElasticsearch = "elasticsearch"
	ProviderOpenSearch    = "opensearch"
)

// Detect probes the logs backend once at startup to distinguish Grafana Loki,
// Elasticsearch, and OpenSearch from VictoriaLogs, mirroring the metrics flavor
// probe (internal/metrics/prometheus/prometheus.go DetectFlavor): a config pin
// bypasses it (the caller checks logs.provider first), the probe is
// best-effort, and ANY failure — unreachable backend, non-200, unrecognised
// payload — FAILS SAFE to VictoriaLogs, the provider RunLore shipped with, so
// existing deployments see no behaviour change.
//
// Two independent probes run in order, the first hit wins:
//  1. Loki: a 200 JSON response with a version on
//     /loki/api/v1/status/buildinfo, an endpoint the others do not serve.
//  2. Elasticsearch/OpenSearch: a 200 JSON response on GET / (the cluster info
//     document both distributions serve at their root) carrying a non-empty
//     version.number. version.distribution == "opensearch" identifies
//     OpenSearch (the field OpenSearch itself adds so clients can tell the two
//     apart); its absence (plain Elasticsearch) identifies Elasticsearch.
//
// Neither probe fires on a real VictoriaLogs backend: its root path serves an
// HTML welcome page (no version.number to parse) and it has no
// /loki/api/v1/status/buildinfo endpoint (404).
//
// Both probes carry the same auth the real client will use (a backend behind
// auth must still be detectable); the token is read from the environment here
// and never logged.
func Detect(ctx context.Context, baseURL, tokenEnv string, headers map[string]string) string {
	base := strings.TrimRight(baseURL, "/")
	if p := detectLoki(ctx, base, tokenEnv, headers); p != "" {
		return p
	}
	if p := detectElastic(ctx, base, tokenEnv, headers); p != "" {
		return p
	}
	return ProviderVictoriaLogs
}

// detectLoki probes /loki/api/v1/status/buildinfo, returning ProviderLoki on a
// recognised response and "" (not a fail-safe default — Detect owns that) on
// any failure, so Detect can try the next probe.
func detectLoki(ctx context.Context, base, tokenEnv string, headers map[string]string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/loki/api/v1/status/buildinfo", nil)
	if err != nil {
		return ""
	}
	applyProbeAuth(req, tokenEnv, headers)
	resp, err := httpx.SecureClient(10 * time.Second).Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var bi struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &bi) != nil || bi.Version == "" {
		return "" // a 200 that is not buildinfo (proxy page) is not Loki
	}
	return ProviderLoki
}

// detectElastic probes GET / (the cluster info document both Elasticsearch and
// OpenSearch serve at their root), returning ProviderOpenSearch or
// ProviderElasticsearch on a recognised response, "" on any failure.
func detectElastic(ctx context.Context, base, tokenEnv string, headers map[string]string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return ""
	}
	applyProbeAuth(req, tokenEnv, headers)
	resp, err := httpx.SecureClient(10 * time.Second).Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var root struct {
		Version struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
	}
	if json.Unmarshal(body, &root) != nil || root.Version.Number == "" {
		return "" // a 200 that carries no version.number (e.g. VictoriaLogs' HTML root) is neither
	}
	if root.Version.Distribution == "opensearch" {
		return ProviderOpenSearch
	}
	return ProviderElasticsearch
}

// applyProbeAuth carries the same auth the real client will use, so a backend
// behind auth is still detectable. The token is read from the environment here
// (probe-time) and never logged.
func applyProbeAuth(req *http.Request, tokenEnv string, headers map[string]string) {
	if tokenEnv != "" {
		if tok := os.Getenv(tokenEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}
