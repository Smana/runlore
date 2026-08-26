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

// probeBody issues one probe GET and returns the (bounded) response body, or nil
// on ANY failure — a malformed URL, a transport error, or a non-200. Every probe
// is best-effort by contract: nil means "not this backend", never an error to
// propagate, so the caller returns "" and Detect moves to the next probe (or its
// fail-safe default). The read is capped at 4 KiB: the documents parsed here are
// small, and an unbounded read of an arbitrary operator-supplied endpoint is not.
func probeBody(ctx context.Context, url, tokenEnv string, headers map[string]string) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	applyProbeAuth(req, tokenEnv, headers)
	resp, err := httpx.SecureClient(10 * time.Second).Do(req)
	if err != nil {
		return nil
	}
	defer func() { httpx.Drain(resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return body
}

// detectLoki probes /loki/api/v1/status/buildinfo, returning ProviderLoki on a
// recognised response and "" (not a fail-safe default — Detect owns that) on
// any failure, so Detect can try the next probe.
func detectLoki(ctx context.Context, base, tokenEnv string, headers map[string]string) string {
	var bi struct {
		Version string `json:"version"`
	}
	body := probeBody(ctx, base+"/loki/api/v1/status/buildinfo", tokenEnv, headers)
	if json.Unmarshal(body, &bi) != nil || bi.Version == "" {
		return "" // a 200 that is not buildinfo (proxy page) is not Loki
	}
	return ProviderLoki
}

// detectElastic probes GET / (the cluster info document both Elasticsearch and
// OpenSearch serve at their root), returning ProviderOpenSearch or
// ProviderElasticsearch on a recognised response, "" on any failure.
func detectElastic(ctx context.Context, base, tokenEnv string, headers map[string]string) string {
	var root struct {
		Version struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
	}
	body := probeBody(ctx, base+"/", tokenEnv, headers)
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
