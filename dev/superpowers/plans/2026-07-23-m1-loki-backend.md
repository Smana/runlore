# M1 — Loki logs backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Grafana Loki as a second logs backend behind the existing pluggable `providers.LogsProvider` interface (`internal/providers/providers.go:258`), at feature parity with the VictoriaLogs provider where Loki's API allows: `query_logs`, `logs_error_summary`, and `discover_log_fields` all work against Loki (LogQL via `/loki/api/v1/query_range`, `/loki/api/v1/labels`, `/loki/api/v1/detected_fields`). Provider selection follows the metrics flavor pattern (`internal/metrics/prometheus/prometheus.go:96` `DetectFlavor`): a new **optional** `logs.provider` key pins the backend; empty auto-detects at startup by probing `/loki/api/v1/status/buildinfo`, **failing safe to VictoriaLogs** so every existing deployment is untouched. No new required config; unset `logs` stays disabled exactly as today.

**Architecture:**
- **New package `internal/logs/loki`** — a `loki.Client` mirroring `internal/logs/victorialogs/victorialogs.go` shape-for-shape: same constructor pair (`New` / `NewWithAuth`), same `WithLevelField` option, same `httpx.SecureClient` (`internal/httpx/client.go:27`), same auth-by-env-indirection. It satisfies `providers.LogsProvider` plus the optional `providers.LogStats` (`providers.go:307`) and `providers.LogFields` (`providers.go:288`) capabilities, so all three tools light up without any tool-side special-casing.
- **New file `internal/logs/detect.go`** (package `logs`) — the one-shot startup probe, mirroring `DetectFlavor`'s "config pin wins; probe is best-effort; fail safe to the shipped default" contract.
- **Query mediation:** today the model writes raw **LogsQL** or (preferred) structured params that `buildLogsQLWith` (`internal/investigate/query_tools.go:399`) compiles into LogsQL. The same mediation gains a **dialect**: `investigate.LogFields` (`internal/investigate/renderlog.go:19`) grows a `Dialect` field (`""` = LogsQL, unchanged zero value; `"logql"` = Loki). `buildLogsQLWith` dispatches to a new `buildLogQL` that compiles the SAME structured params into valid LogQL, and the raw-query guard flips: instead of rejecting `level=` (the LogsQL guard at `query_tools.go:402`), the LogQL path rejects LogsQL-isms (`unpack_json`, `_msg`) and non-selector-first queries, with correcting errors. Tool `Description()`/`Schema()` become dialect-aware so the model is told LogQL, never LogsQL, on a Loki deployment.
- **Field convention:** `config.LogFields.Resolved()` (`internal/config/config.go:164`) keeps its VictoriaLogs defaults; a new `ResolvedFor(provider)` resolves Loki-appropriate defaults (`container`/`namespace`/`pod` stream labels, `detected_level` severity, **no** unpack pipe — Loki 3.x auto-detects level as structured metadata). Operators override via the existing `logs.fields` keys — no new required config.
- **Parity notes (where Loki's API differs):** `Hits` maps to a LogQL metric wrapper `sum by (<level>) (count_over_time(<q> [<step>]))`; `TopMessages` has no LogQL equivalent of `stats by (_msg)` and is aggregated **client-side** over the capped `Query` sample; `FieldNames` merges stream labels (`/loki/api/v1/labels`) with body fields (`/loki/api/v1/detected_fields`, whose count is a value *cardinality*, not a hit count — the discover tool renders the number only when > 0).
- **Wiring:** the `cfg.Logs.URL != ""` block in `BuildModelAndTools` (`internal/app/investigate.go:140-169`) switches on the resolved provider and passes the dialect-carrying `investigate.LogFields` to all three tools (`LogsErrorSummaryTool` and `DiscoverLogFieldsTool` gain a `Fields` field; zero value keeps today's behaviour).

**Tech Stack:** Go (stdlib `net/http`, `encoding/json`), `internal/httpx` secure client, `httptest` fakes with real Loki JSON fixtures for tests, yaml config via existing strict loader (`internal/config/load.go:16`). No new dependencies.

---

### Task 1: Config — `logs.provider` key, validation, Loki field defaults

**Files:**
- Modify: `internal/config/config.go` (LogsConfig at :117, LogFields defaults at :149, Validate at :1013)
- Test: `internal/config/config_test.go`, `internal/config/logfields_test.go`

**Steps:**

- [ ] Write the failing validation test in `internal/config/config_test.go`:

```go
// TestLogsProviderValidate: logs.provider is an enum — "" (auto-detect),
// "victorialogs", "loki". Anything else must abort startup loudly (the Load
// philosophy: a typo'd key never fails silently), because a silent fallback to
// victorialogs against a Loki endpoint would break every logs tool at runtime.
func TestLogsProviderValidate(t *testing.T) {
	for _, ok := range []string{"", "victorialogs", "loki"} {
		c := &Config{}
		c.Logs.Provider = ok
		if err := c.Validate(); err != nil {
			t.Fatalf("provider %q must validate, got %v", ok, err)
		}
	}
	c := &Config{}
	c.Logs.Provider = "grafana"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "logs.provider") {
		t.Fatalf("unknown provider must fail with a logs.provider error, got %v", err)
	}
}
```

- [ ] Write the failing Loki-defaults test in `internal/config/logfields_test.go`:

```go
// TestLogFieldsResolvedForLoki: ResolvedFor(loki) fills the promtail/alloy
// stream-label convention and Loki 3.x's parser-less detected_level severity,
// and deliberately leaves UnpackPipe empty (no parser stage needed). Any
// explicitly-set field wins. ResolvedFor(victorialogs) must equal Resolved().
func TestLogFieldsResolvedForLoki(t *testing.T) {
	got := LogFields{}.ResolvedFor(LogsProviderLoki)
	want := LogFields{ContainerField: "container", NamespaceField: "namespace",
		PodField: "pod", LevelField: "detected_level", UnpackPipe: ""}
	if got != want {
		t.Fatalf("loki defaults = %+v, want %+v", got, want)
	}
	over := LogFields{LevelField: "level", UnpackPipe: "logfmt"}.ResolvedFor(LogsProviderLoki)
	if over.LevelField != "level" || over.UnpackPipe != "logfmt" || over.PodField != "pod" {
		t.Fatalf("overrides must win, defaults fill the rest: %+v", over)
	}
	if LogFields{}.ResolvedFor(LogsProviderVictoriaLogs) != (LogFields{}.Resolved()) {
		t.Fatalf("victorialogs resolution must be unchanged")
	}
}
```

- [ ] Run `go test ./internal/config/ -run 'TestLogsProviderValidate|TestLogFieldsResolvedForLoki' -count=1` — expect **FAIL** (compile error: `Provider`, `LogsProviderLoki`, `ResolvedFor` undefined).
- [ ] Implement in `internal/config/config.go`. Add the provider constants next to the `MetricsFlavor*` block (:93-96):

```go
// Logs backend providers for config.logs.provider. Unlike the metrics backends
// (which share the Prometheus HTTP API and differ only in dialect), VictoriaLogs
// and Loki have entirely different query APIs, so this selects the CLIENT, not a
// flavor of one. Empty ⇒ auto-detect at startup (Loki answers
// /loki/api/v1/status/buildinfo; VictoriaLogs does not), failing safe to
// victorialogs — the provider RunLore shipped with — so an unreachable backend
// at startup reproduces today's behaviour exactly.
const (
	LogsProviderVictoriaLogs = "victorialogs" // LogsQL over /select/logsql/* (shipped default)
	LogsProviderLoki         = "loki"         // LogQL over /loki/api/v1/*
)
```

- [ ] Add the `Provider` key to `LogsConfig` (:117):

```go
type LogsConfig struct {
	Endpoint `yaml:",inline"`

	// Provider optionally pins the logs backend implementation instead of
	// auto-detecting it: "loki" or "victorialogs" (see LogsProvider*). Empty ⇒
	// probe once at startup, failing safe to victorialogs.
	Provider string `yaml:"provider"`

	Fields LogFields `yaml:"fields"`
}
```

- [ ] Add the Loki default constants next to the VictoriaLogs ones (:149-155) and `ResolvedFor` next to `Resolved()` (:164):

```go
// Default log-field convention for Loki: the promtail/Grafana-Alloy stream-label
// layout plus Loki 3.x's auto-detected severity (detected_level is structured
// metadata, filterable WITHOUT a parser stage — hence no default unpack pipe).
// An operator on Loki 2.x (no detected_level) overrides logs.fields, e.g.
// {level_field: level, unpack_pipe: logfmt}.
const (
	defaultLokiContainerField = "container"
	defaultLokiNamespaceField = "namespace"
	defaultLokiPodField       = "pod"
	defaultLokiLevelField     = "detected_level"
)

// ResolvedFor resolves the field convention for a specific logs provider:
// Loki gets Loki-appropriate defaults; anything else (victorialogs, "") keeps
// Resolved()'s shipped VictoriaLogs behaviour. Explicitly-set fields always win.
func (f LogFields) ResolvedFor(provider string) LogFields {
	if provider != LogsProviderLoki {
		return f.Resolved()
	}
	if f.ContainerField == "" {
		f.ContainerField = defaultLokiContainerField
	}
	if f.NamespaceField == "" {
		f.NamespaceField = defaultLokiNamespaceField
	}
	if f.PodField == "" {
		f.PodField = defaultLokiPodField
	}
	if f.LevelField == "" {
		f.LevelField = defaultLokiLevelField
	}
	// UnpackPipe stays as-set (empty = no parser stage): detected_level needs none.
	return f
}
```

- [ ] Add the enum check inside `Config.Validate()` (:1013, before the final `return nil`):

```go
	// logs.provider is a small enum; reject typos at startup rather than silently
	// falling back to the wrong client against a live backend.
	switch c.Logs.Provider {
	case "", LogsProviderVictoriaLogs, LogsProviderLoki:
	default:
		return fmt.Errorf("logs.provider must be %q, %q, or empty (auto-detect); got %q",
			LogsProviderVictoriaLogs, LogsProviderLoki, c.Logs.Provider)
	}
```

- [ ] Run `go test ./internal/config/ -count=1` — expect **PASS** (including the existing `shipped_configs_test.go` / `minimal_values_test.go`, which prove no new key is required).
- [ ] Commit: `feat(config): add logs.provider key with validation and Loki field defaults`

---

### Task 2: Loki client — `Query` over `/loki/api/v1/query_range`

**Files:**
- Create: `internal/logs/loki/loki.go`
- Test: `internal/logs/loki/loki_test.go`

**Steps:**

- [ ] Create `internal/logs/loki/loki_test.go` with the failing core tests (style mirrors `internal/logs/victorialogs/victorialogs_test.go`):

```go
// SPDX-License-Identifier: Apache-2.0

package loki

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestQuery(t *testing.T) {
	var gotPath, gotQuery, gotLimit, gotDirection string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		gotLimit = r.URL.Query().Get("limit")
		gotDirection = r.URL.Query().Get("direction")
		_, _ = io.WriteString(w, `{
		  "status": "success",
		  "data": {
		    "resultType": "streams",
		    "result": [
		      {
		        "stream": {"namespace": "apps", "pod": "harbor-db-0", "container": "db", "detected_level": "error"},
		        "values": [
		          ["1750413601000000000", "retrying"],
		          ["1750413600000000000", "db connection refused"]
		        ]
		      }
		    ]
		  }
		}`)
	}))
	defer srv.Close()

	res, err := New(srv.URL).Query(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotPath != "/loki/api/v1/query_range" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotQuery != `{namespace="apps"}` {
		t.Fatalf("query=%q", gotQuery)
	}
	if gotLimit != "1000" || gotDirection != "backward" {
		t.Fatalf("limit=%q direction=%q, want 1000/backward", gotLimit, gotDirection)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 lines, got %d", len(res))
	}
	// Newest first, matching the VictoriaLogs provider's ordering contract.
	if res[0].Message != "retrying" || res[1].Message != "db connection refused" {
		t.Fatalf("order/messages wrong: %+v", res)
	}
	// Nanosecond epoch parsed; stream labels become LogLine.Fields (so the
	// renderer's streamIdentity finds pod/container under the Loki names).
	if res[1].Time.UTC().Format("2006-01-02T15:04:05Z") != "2026-06-20T10:00:00Z" {
		t.Fatalf("time not parsed from ns epoch: %v", res[1].Time)
	}
	if res[0].Fields["pod"] != "harbor-db-0" || res[0].Fields["container"] != "db" {
		t.Fatalf("stream labels not mapped to fields: %+v", res[0].Fields)
	}
}

func TestQueryAuthHeaders(t *testing.T) {
	t.Setenv("RUNLORE_TEST_LOKI_TOKEN", "s3cr3t")
	var gotAuth, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Scope-OrgID")
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer srv.Close()

	c := NewWithAuth(srv.URL, "RUNLORE_TEST_LOKI_TOKEN", map[string]string{"X-Scope-OrgID": "tenant-b"})
	if _, err := c.Query(context.Background(), `{namespace="apps"}`, providers.TimeWindow{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" || gotTenant != "tenant-b" {
		t.Fatalf("auth not applied: auth=%q tenant=%q", gotAuth, gotTenant)
	}
}

// TestQueryTruncation: Loki has no offset pagination; the client sends
// limit=maxLines once and appends the shared TruncationLine sentinel when the
// server returned exactly the limit (more likely matched upstream).
func TestQueryTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[
		  {"stream":{"namespace":"apps"},"values":[
		    ["1750413602000000000","a"],["1750413601000000000","b"],["1750413600000000000","c"]]}]}}`)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.maxLines = 3
	res, err := c.Query(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 4 || !strings.Contains(res[3].Message, "results truncated at 3") {
		t.Fatalf("want 3 lines + sentinel, got %d: %+v", len(res), res)
	}
}

// TestQueryErrorPaths: backend down, non-200, JSON error status, and malformed
// body must each surface as an error (never a silent empty result), so the tool
// call fails visibly and the loop records a data gap instead of "no logs".
func TestQueryErrorPaths(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close() // connection refused
	if _, err := New(down.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil {
		t.Fatalf("backend down must error")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "too many outstanding requests", http.StatusTooManyRequests)
	}))
	defer bad.Close()
	if _, err := New(bad.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil ||
		!strings.Contains(err.Error(), "429") {
		t.Fatalf("non-200 must error with the status, got %v", err)
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":`)
	}))
	defer malformed.Close()
	if _, err := New(malformed.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil {
		t.Fatalf("malformed body must error")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer empty.Close()
	res, err := New(empty.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{})
	if err != nil || len(res) != 0 {
		t.Fatalf("empty result must be (nil, nil), got %v / %v", res, err)
	}
}
```

- [ ] Run `go test ./internal/logs/loki/ -count=1` — expect **FAIL** (package does not exist).
- [ ] Create `internal/logs/loki/loki.go`:

```go
// SPDX-License-Identifier: Apache-2.0

// Package loki implements providers.LogsProvider against Grafana Loki, querying
// with LogQL over /loki/api/v1/* and normalizing the streams response into log
// lines. It mirrors internal/logs/victorialogs: same construction/auth shape,
// same optional LogStats/LogFields capabilities, same truncation sentinel.
package loki

import (
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
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

// Client queries a Grafana Loki backend.
type Client struct {
	baseURL    string
	tokenEnv   string            // env var holding a bearer token; empty ⇒ no auth
	headers    map[string]string // static extra request headers (e.g. X-Scope-OrgID)
	maxLines   int               // per-query line cap (Loki `limit` param)
	levelField string            // severity label Hits splits by; "" ⇒ defaultLevelField
	http       *http.Client
}

// defaultMaxLines bounds the number of lines Query returns. Loki has no offset
// pagination (a timestamp-cursor walk is deferred), so this is sent as the
// server-side `limit` and doubles as the truncation-sentinel threshold.
const defaultMaxLines = 1000

// defaultLevelField is the severity label Hits groups by: Loki 3.x attaches
// detected_level as structured metadata to every entry (no parser stage
// needed). Overridable via WithLevelField (config.logs.fields.level_field) for
// Loki 2.x or a collector with its own level label.
const defaultLevelField = "detected_level"

// New builds a client for a Loki base URL, unauthenticated.
func New(baseURL string) *Client {
	return NewWithAuth(baseURL, "", nil)
}

// NewWithAuth builds a client that adds optional auth to every request; the
// semantics are identical to victorialogs.NewWithAuth (token read from the env
// at request-build time, never logged).
func NewWithAuth(baseURL, tokenEnv string, headers map[string]string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokenEnv:   tokenEnv,
		headers:    headers,
		maxLines:   defaultMaxLines,
		levelField: defaultLevelField,
		http:       httpx.SecureClient(30 * time.Second),
	}
}

// WithLevelField overrides the severity label Hits splits by. Empty is a no-op
// so an unset config keeps the default; returns the client for chaining.
func (c *Client) WithLevelField(field string) *Client {
	if field != "" {
		c.levelField = field
	}
	return c
}

var (
	_ providers.LogsProvider = (*Client)(nil)
	_ providers.LogStats     = (*Client)(nil)
	_ providers.LogFields    = (*Client)(nil)
)

// queryResponse is Loki's standard success envelope for /loki/api/v1/query_range.
type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// streamResult is one log stream: its label set plus [ns-epoch, line] entries.
type streamResult struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// Query runs a LogQL query over the window and returns normalized log lines,
// newest first (matching the VictoriaLogs provider's ordering). It sends one
// request with limit=maxLines and direction=backward; when the server returns
// exactly the limit, more entries likely matched and the shared truncation
// sentinel is appended (providers.TruncationLine).
func (c *Client) Query(ctx context.Context, query string, w providers.TimeWindow) (providers.LogResult, error) {
	v := url.Values{
		"query":     {query},
		"limit":     {strconv.Itoa(c.maxLines)},
		"direction": {"backward"},
	}
	setWindow(v, w)
	body, err := c.get(ctx, "/loki/api/v1/query_range", v)
	if err != nil {
		return nil, err
	}
	var resp queryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse loki response: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("loki error: status %q", resp.Status)
	}
	if resp.Data.ResultType != "streams" {
		return nil, fmt.Errorf("unexpected loki resultType %q (Query expects a log selector, not a metric query)", resp.Data.ResultType)
	}
	var streams []streamResult
	if err := json.Unmarshal(resp.Data.Result, &streams); err != nil {
		return nil, fmt.Errorf("parse loki streams: %w", err)
	}
	var out providers.LogResult
	for _, s := range streams {
		for _, e := range s.Values {
			ll := providers.LogLine{Message: e[1], Fields: make(map[string]string, len(s.Stream))}
			if ns, err := strconv.ParseInt(e[0], 10, 64); err == nil {
				ll.Time = time.Unix(0, ns).UTC()
			}
			for k, val := range s.Stream {
				ll.Fields[k] = val
			}
			out = append(out, ll)
		}
	}
	// Entries arrive grouped per stream; order newest-first ACROSS streams so the
	// renderer and the VictoriaLogs provider agree on what "first lines" means.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) >= c.maxLines {
		out = out[:c.maxLines]
		out = append(out, providers.TruncationLine(int64(c.maxLines)))
	}
	return out, nil
}

// setWindow adds the optional RFC3339 start/end bounds (Loki accepts RFC3339
// alongside ns epochs; RFC3339 matches the VictoriaLogs client for symmetry).
func setWindow(v url.Values, w providers.TimeWindow) {
	if !w.Start.IsZero() {
		v.Set("start", w.Start.UTC().Format(time.RFC3339))
	}
	if !w.End.IsZero() {
		v.Set("end", w.End.UTC().Format(time.RFC3339))
	}
}

// get performs an authenticated GET against a Loki endpoint and returns the raw
// body; any non-200 becomes an error carrying the status and body (mirrors
// victorialogs.postForm error shape, so tool-visible errors read the same).
func (c *Client) get(ctx context.Context, path string, v url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+v.Encode(), nil)
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
	return body, nil
}

// setAuth applies the optional bearer token and static headers (identical
// semantics to the VictoriaLogs client: token read here, never logged).
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

// reNumToken / collapseNums mirror internal/investigate/renderlog.go:83-92 (and
// VictoriaLogs' collapse_nums pipe): free-standing digit runs collapse to "0" so
// lines differing only by a numeric value share one grouping key. Duplicated
// here (3 lines) rather than exported from investigate, which must not become a
// dependency of a backend client.
var reNumToken = regexp.MustCompile(`\d+`)

func collapseNums(msg string) string { return reNumToken.ReplaceAllString(msg, "0") }
```

- [ ] Run `go test ./internal/logs/loki/ -count=1` — expect **PASS** (`collapseNums` is unused until Task 3; if `go vet`/lint flags it, move the two `collapseNums` lines into Task 3 instead — do NOT suppress).
- [ ] Run `go build ./...` — expect clean.
- [ ] Commit: `feat(logs): add Loki client — LogQL query_range with normalization, cap sentinel, auth`

---

### Task 3: Loki analytics — `Hits` + `TopMessages` (providers.LogStats)

**Files:**
- Modify: `internal/logs/loki/loki.go`
- Test: `internal/logs/loki/loki_test.go`

**Steps:**

- [ ] Add the failing tests:

```go
func TestHits(t *testing.T) {
	var gotQuery, gotStep string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		gotStep = r.URL.Query().Get("step")
		_, _ = io.WriteString(w, `{
		  "status": "success",
		  "data": {
		    "resultType": "matrix",
		    "result": [
		      {"metric": {"detected_level": "error"}, "values": [[1704103200, "3"], [1704103500, "412"]]},
		      {"metric": {"detected_level": "warn"},  "values": [[1704103200, "1"]]}
		    ]
		  }
		}`)
	}))
	defer srv.Close()

	buckets, err := New(srv.URL).Hits(context.Background(), `{namespace="apps"} | detected_level="error"`,
		providers.TimeWindow{}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Hits: %v", err)
	}
	// The log query must be wrapped in the LogQL metric form, split by the level label.
	want := `sum by (detected_level) (count_over_time({namespace="apps"} | detected_level="error" [300s]))`
	if gotQuery != want {
		t.Fatalf("metric query = %q, want %q", gotQuery, want)
	}
	if gotStep != "300s" {
		t.Fatalf("step=%q, want 300s", gotStep)
	}
	if len(buckets) != 3 {
		t.Fatalf("want 3 buckets, got %d: %+v", len(buckets), buckets)
	}
	var sawSpike bool
	for _, b := range buckets {
		if b.Level == "error" && b.Count == 412 && !b.Time.IsZero() {
			sawSpike = true
		}
	}
	if !sawSpike {
		t.Fatalf("missing error=412 bucket: %+v", buckets)
	}
}

// TestTopMessages: LogQL has no `stats by (message)`, so dominant messages are
// aggregated CLIENT-SIDE over the capped Query sample, with numeric tokens
// collapsed (mirroring VictoriaLogs' collapse_nums) so "took 12ms"/"took 907ms"
// fold into one message. Counts are per-sample, not corpus-wide.
func TestTopMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[
		  {"stream":{"namespace":"apps"},"values":[
		    ["1750413603000000000","connection refused to 10.0.0.7"],
		    ["1750413602000000000","connection refused to 10.0.0.9"],
		    ["1750413601000000000","connection refused to 10.0.0.9"],
		    ["1750413600000000000","timeout waiting for db"]]}]}}`)
	}))
	defer srv.Close()

	msgs, err := New(srv.URL).TopMessages(context.Background(), `{namespace="apps"}`, providers.TimeWindow{}, 10)
	if err != nil {
		t.Fatalf("TopMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 grouped messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Count != 3 || !strings.Contains(msgs[0].Message, "connection refused") {
		t.Fatalf("dominant message wrong: %+v", msgs[0])
	}
	if !msgs[0].First.Before(msgs[0].Last) {
		t.Fatalf("first→last span not tracked: %+v", msgs[0])
	}
	if msgs[1].Message != "timeout waiting for db" || msgs[1].Count != 1 {
		t.Fatalf("second message wrong: %+v", msgs[1])
	}
}

func TestHitsErrorPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "parse error: unexpected token", http.StatusBadRequest)
	}))
	defer srv.Close()
	if _, err := New(srv.URL).Hits(context.Background(), `{a="b"}`, providers.TimeWindow{}, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "400") {
		t.Fatalf("bad request must error with status, got %v", err)
	}
}
```

- [ ] Run `go test ./internal/logs/loki/ -run 'TestHits|TestTopMessages' -count=1` — expect **FAIL** (`Hits`/`TopMessages` undefined; add `"time"` to test imports).
- [ ] Implement in `internal/logs/loki/loki.go`:

```go
// matrixSeries is one series of a Loki metric-query (matrix) result.
type matrixSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"` // [unix seconds (float64), "count" (string)]
}

// Hits returns the per-step match count over the window, split by severity, by
// wrapping the log query in the LogQL metric form
// `sum by (<level>) (count_over_time(<q> [<step>]))` — the Loki analogue of
// VictoriaLogs' /select/logsql/hits, powering the logs_error_summary histogram.
// The level label defaults to detected_level (Loki 3.x structured metadata);
// series without the label fold into one Level=="" bucket series, which the
// renderer already handles. A step <= 0 defaults to one minute.
func (c *Client) Hits(ctx context.Context, query string, w providers.TimeWindow, step time.Duration) ([]providers.Bucket, error) {
	if step <= 0 {
		step = time.Minute
	}
	stepStr := fmt.Sprintf("%ds", int(step.Seconds()))
	metricQ := fmt.Sprintf("sum by (%s) (count_over_time(%s [%s]))", c.levelField, query, stepStr)
	v := url.Values{"query": {metricQ}, "step": {stepStr}}
	setWindow(v, w)
	body, err := c.get(ctx, "/loki/api/v1/query_range", v)
	if err != nil {
		return nil, err
	}
	var resp queryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse loki response: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("loki error: status %q", resp.Status)
	}
	var series []matrixSeries
	if err := json.Unmarshal(resp.Data.Result, &series); err != nil {
		return nil, fmt.Errorf("parse loki matrix: %w", err)
	}
	var out []providers.Bucket
	for _, s := range series {
		level := s.Metric[c.levelField]
		for _, p := range s.Values {
			ts, n := parsePoint(p)
			out = append(out, providers.Bucket{Time: ts, Level: level, Count: n})
		}
	}
	return out, nil
}

// parsePoint parses a Loki/Prometheus [unixSeconds, "value"] matrix point
// (same wire shape as internal/metrics/prometheus/prometheus.go:322 parsePoint,
// narrowed to the int64 count Hits needs).
func parsePoint(p [2]any) (time.Time, int64) {
	var ts time.Time
	if f, ok := p[0].(float64); ok {
		ts = time.Unix(int64(f), 0).UTC()
	}
	var n int64
	if s, ok := p[1].(string); ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			n = int64(f)
		}
	}
	return ts, n
}

// TopMessages returns up to k dominant messages over the window. LogQL cannot
// group by message content (no `stats by (_msg)` equivalent; the /patterns
// endpoint is experimental and needs the pattern ingester), so this aggregates
// CLIENT-SIDE over the capped Query sample: numeric tokens collapse so
// near-identical lines group (mirroring collapse_nums), counts/first/last are
// per-sample (up to maxLines newest lines), which is enough to NAME what floods
// the logs — the only question logs_error_summary asks of it. k <= 0 defaults
// to 10.
func (c *Client) TopMessages(ctx context.Context, query string, w providers.TimeWindow, k int) ([]providers.MsgCount, error) {
	if k <= 0 {
		k = 10
	}
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
```

- [ ] Run `go test ./internal/logs/loki/ -count=1` — expect **PASS**.
- [ ] Commit: `feat(logs): Loki analytics — level-split hits histogram and client-side top messages`

---

### Task 4: Loki field discovery — `FieldNames` (providers.LogFields)

**Files:**
- Modify: `internal/logs/loki/loki.go`
- Test: `internal/logs/loki/loki_test.go`

**Steps:**

- [ ] Add the failing tests:

```go
// TestFieldNames: discovery merges STREAM labels (/loki/api/v1/labels — the
// selector building blocks the model needs first) with detected body fields
// (/loki/api/v1/detected_fields, Loki 3.x). Hits carries the detected field's
// value CARDINALITY (Loki reports no per-field hit count); stream labels carry 0
// and the discover tool renders the number only when > 0.
func TestFieldNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/labels":
			if q := r.URL.Query().Get("query"); q != `{namespace="apps"}` {
				t.Errorf("labels query=%q", q)
			}
			_, _ = io.WriteString(w, `{"status":"success","data":["namespace","pod","container"]}`)
		case "/loki/api/v1/detected_fields":
			_, _ = io.WriteString(w, `{"fields":[
			  {"label":"level","type":"string","cardinality":4,"parsers":["logfmt"]},
			  {"label":"duration","type":"duration","cardinality":99,"parsers":["logfmt"]}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	fields, err := New(srv.URL).FieldNames(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("FieldNames: %v", err)
	}
	if len(fields) != 5 {
		t.Fatalf("want 3 labels + 2 detected fields, got %d: %+v", len(fields), fields)
	}
	if fields[0].Name != "namespace" || fields[0].Hits != 0 {
		t.Fatalf("stream labels must come first with Hits=0: %+v", fields[0])
	}
	if fields[3].Name != "level" || fields[3].Hits != 4 {
		t.Fatalf("detected field must carry cardinality as Hits: %+v", fields[3])
	}
}

// TestFieldNamesOldLoki: a Loki without detected_fields (pre-3.0 returns 404)
// still answers discovery with the stream labels alone — degrade, don't fail.
func TestFieldNamesOldLoki(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/labels" {
			_, _ = io.WriteString(w, `{"status":"success","data":["namespace","pod"]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fields, err := New(srv.URL).FieldNames(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("FieldNames must degrade to labels-only, got %v", err)
	}
	if len(fields) != 2 || fields[0].Name != "namespace" {
		t.Fatalf("labels-only result wrong: %+v", fields)
	}
}

// TestFieldNamesBothDown: when neither endpoint answers, the error must surface.
func TestFieldNamesBothDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if _, err := New(srv.URL).FieldNames(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil {
		t.Fatalf("both endpoints failing must error")
	}
}
```

- [ ] Run `go test ./internal/logs/loki/ -run TestFieldNames -count=1` — expect **FAIL**.
- [ ] Implement in `internal/logs/loki/loki.go`:

```go
// FieldNames lists the field names present in the logs matching query over the
// window — the log-schema discovery path (providers.LogFields). Loki splits the
// answer across two endpoints, so both are merged: STREAM labels from
// /loki/api/v1/labels (query-scoped; these are what a selector is built from,
// so they come first, Hits=0 — Loki reports no per-label hit count) and parsed
// body fields from /loki/api/v1/detected_fields (Loki 3.x; Hits carries the
// reported value cardinality). A missing detected_fields endpoint (older Loki)
// degrades to labels-only; only both failing is an error.
func (c *Client) FieldNames(ctx context.Context, query string, w providers.TimeWindow) ([]providers.FieldCount, error) {
	var out []providers.FieldCount

	lv := url.Values{"query": {query}}
	setWindow(lv, w)
	labelsBody, labelsErr := c.get(ctx, "/loki/api/v1/labels", lv)
	if labelsErr == nil {
		var resp struct {
			Status string   `json:"status"`
			Data   []string `json:"data"`
		}
		if err := json.Unmarshal(labelsBody, &resp); err != nil {
			return nil, fmt.Errorf("parse loki labels: %w", err)
		}
		for _, name := range resp.Data {
			out = append(out, providers.FieldCount{Name: name})
		}
	}

	dv := url.Values{"query": {query}}
	setWindow(dv, w)
	dfBody, dfErr := c.get(ctx, "/loki/api/v1/detected_fields", dv)
	if dfErr != nil {
		if labelsErr == nil {
			return out, nil // older Loki: stream labels alone still answer discovery
		}
		return nil, dfErr
	}
	var resp struct {
		Fields []struct {
			Label       string `json:"label"`
			Cardinality int64  `json:"cardinality"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(dfBody, &resp); err != nil {
		return nil, fmt.Errorf("parse loki detected_fields: %w", err)
	}
	for _, f := range resp.Fields {
		out = append(out, providers.FieldCount{Name: f.Label, Hits: f.Cardinality})
	}
	return out, nil
}
```

- [ ] Run `go test ./internal/logs/loki/ -count=1` — expect **PASS**.
- [ ] Commit: `feat(logs): Loki field discovery via labels and detected_fields`

---

### Task 5: Provider auto-detect — `internal/logs/detect.go`

**Files:**
- Create: `internal/logs/detect.go`
- Test: `internal/logs/detect_test.go`

**Steps:**

- [ ] Create `internal/logs/detect_test.go` with the failing test:

```go
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDetect mirrors the metrics flavor probe's contract
// (internal/metrics/prometheus DetectFlavor): best-effort, one shot, FAIL SAFE
// to the shipped default. Only a 200 buildinfo JSON with a version identifies
// Loki; a 404 (VictoriaLogs), an unreachable backend, or a 200 that is not
// buildinfo JSON (a proxy's HTML error page) all resolve to victorialogs.
func TestDetect(t *testing.T) {
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/status/buildinfo" {
			t.Errorf("path=%q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"version":"3.1.0","revision":"935aee77ed","branch":"HEAD","goVersion":"go1.22"}`)
	}))
	defer lokiSrv.Close()
	if got := Detect(context.Background(), lokiSrv.URL, "", nil); got != ProviderLoki {
		t.Fatalf("buildinfo 200 must detect loki, got %q", got)
	}

	vlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // VictoriaLogs serves no /loki/api/v1/status/buildinfo
	}))
	defer vlSrv.Close()
	if got := Detect(context.Background(), vlSrv.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("404 must fail safe to victorialogs, got %q", got)
	}

	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<html>welcome</html>`) // 200 but not buildinfo
	}))
	defer htmlSrv.Close()
	if got := Detect(context.Background(), htmlSrv.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("non-JSON 200 must fail safe to victorialogs, got %q", got)
	}

	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close()
	if got := Detect(context.Background(), down.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("unreachable must fail safe to victorialogs, got %q", got)
	}
}

// TestDetectSendsAuth: a Loki behind auth must still be detectable.
func TestDetectSendsAuth(t *testing.T) {
	t.Setenv("RUNLORE_TEST_DETECT_TOKEN", "s3cr3t")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"version":"3.1.0"}`)
	}))
	defer srv.Close()
	if got := Detect(context.Background(), srv.URL, "RUNLORE_TEST_DETECT_TOKEN", map[string]string{"X-Scope-OrgID": "t"}); got != ProviderLoki {
		t.Fatalf("got %q", got)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("probe must carry auth, got %q", gotAuth)
	}
}
```

- [ ] Run `go test ./internal/logs/ -count=1` — expect **FAIL** (no package).
- [ ] Create `internal/logs/detect.go`:

```go
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
```

- [ ] Run `go test ./internal/logs/ -count=1` — expect **PASS**.
- [ ] Commit: `feat(logs): auto-detect loki vs victorialogs at startup, failing safe to victorialogs`

---

### Task 6: Investigate — LogQL dialect for the three log tools

**Files:**
- Modify: `internal/investigate/renderlog.go` (LogFields at :19, resolved at :41)
- Modify: `internal/investigate/query_tools.go` (buildLogsQL :389, buildLogsQLWith :399, QueryLogsTool Description/Schema :428-446)
- Modify: `internal/investigate/logs_summary_tool.go` (struct :28, Schema :45, Call :72)
- Modify: `internal/investigate/discover_tools.go` (struct :97, Schema :113, Call :133, render :157-160)
- Test: `internal/investigate/query_tools_test.go`, `internal/investigate/discover_tools_test.go`, `internal/investigate/loki_e2e_test.go` (new)

**Steps:**

- [ ] Add the failing query-builder tests to `internal/investigate/query_tools_test.go`:

```go
// TestBuildLogQL: with Dialect=logql the same structured params compile to
// valid LogQL, raw queries must start with a stream selector, and LogsQL-isms
// are rejected with a correcting error so the model retries in-dialect.
func TestBuildLogQL(t *testing.T) {
	logql := LogFields{Dialect: DialectLogQL}
	tests := []struct {
		name, raw, container, namespace, level string
		conv                                   LogFields
		want                                   string
		wantErr                                string
	}{
		{name: "structured selector + level", container: "harbor-core", namespace: "apps", level: "error", conv: logql,
			want: `{container="harbor-core",namespace="apps"} | detected_level="error"`},
		{name: "namespace only, no level", namespace: "apps", conv: logql,
			want: `{namespace="apps"}`},
		{name: "custom fields add parser pipe", container: "core", level: "error",
			conv: LogFields{Dialect: DialectLogQL, LevelField: "level", UnpackPipe: "logfmt"},
			want: `{container="core"} | logfmt | level="error"`},
		{name: "raw passthrough", raw: `{namespace="apps"} |= "refused"`, conv: logql,
			want: `{namespace="apps"} |= "refused"`},
		{name: "raw LogsQL-ism rejected", raw: `{namespace="apps"} | unpack_json | log.level:error`, conv: logql,
			wantErr: "LogsQL"},
		{name: "raw without selector rejected", raw: `error`, conv: logql,
			wantErr: "stream selector"},
		{name: "nothing given", conv: logql, wantErr: "provide a raw"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLogsQLWith(tc.raw, tc.container, tc.namespace, tc.level, tc.conv)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildLogsQLWith: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLogToolDescriptionsDialect: on a Loki deployment the model must be told
// LogQL — never LogsQL — in every tool description and schema, and vice versa.
func TestLogToolDescriptionsDialect(t *testing.T) {
	logql := LogFields{Dialect: DialectLogQL}
	q := QueryLogsTool{Fields: logql}
	if !strings.Contains(q.Description(), "LogQL (Grafana Loki)") || strings.Contains(q.Description(), "invalid LogsQL") {
		t.Fatalf("LogQL description wrong: %s", q.Description())
	}
	if !strings.Contains(q.Schema(), "raw LogQL") {
		t.Fatalf("LogQL schema wrong: %s", q.Schema())
	}
	if !strings.Contains(QueryLogsTool{}.Description(), "LogsQL (VictoriaLogs)") {
		t.Fatalf("default description must stay LogsQL")
	}
	for _, tool := range []interface {
		Description() string
		Schema() string
	}{LogsErrorSummaryTool{Fields: logql}, DiscoverLogFieldsTool{Fields: logql}} {
		if strings.Contains(tool.Schema(), "LogsQL") {
			t.Fatalf("loki-dialect schema must not mention LogsQL: %s", tool.Schema())
		}
	}
}
```

- [ ] Add the failing render test to `internal/investigate/discover_tools_test.go` (a fake returning a `FieldCount` with `Hits: 0` must render without `(×0)` — Loki stream labels carry no count):

```go
// TestDiscoverLogFieldsOmitsZeroCounts: Loki stream labels carry Hits=0 (no
// per-label hit count exists); the render must omit the count rather than
// print a misleading "(×0)".
func TestDiscoverLogFieldsOmitsZeroCounts(t *testing.T) {
	tool := DiscoverLogFieldsTool{Logs: fakeLogFields{fields: []providers.FieldCount{
		{Name: "namespace"}, {Name: "level", Hits: 4},
	}}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(out, "(×0)") {
		t.Fatalf("zero hits must render without a count:\n%s", out)
	}
	if !strings.Contains(out, "level (×4)") {
		t.Fatalf("non-zero hits must keep the count:\n%s", out)
	}
}
```

(Adapt the `fakeLogFields` fake at `internal/investigate/discover_tools_test.go:115` to accept a `fields []providers.FieldCount` member if it doesn't already.)

- [ ] Run `go test ./internal/investigate/ -run 'TestBuildLogQL|TestLogToolDescriptionsDialect|TestDiscoverLogFieldsOmitsZeroCounts' -count=1` — expect **FAIL** (`DialectLogQL` undefined, etc.).
- [ ] Implement the dialect in `internal/investigate/renderlog.go` — extend `LogFields` (:19) and `resolved()` (:41):

```go
// Dialect values for LogFields.Dialect — which query language the configured
// logs backend speaks. The zero value is LogsQL so every existing caller and
// fake is untouched; the app layer sets DialectLogQL when the backend is Loki.
const (
	DialectLogsQL = ""      // VictoriaLogs LogsQL (shipped default)
	DialectLogQL  = "logql" // Grafana Loki LogQL
)
```

Add to the `LogFields` struct: `Dialect string // query dialect; "" ⇒ LogsQL (see Dialect*)`. In `resolved()`, branch first:

```go
func (f LogFields) resolved() LogFields {
	if f.Dialect == DialectLogQL {
		// Loki convention: promtail/alloy stream labels + Loki 3.x detected_level
		// (structured metadata — no parser pipe by default). Mirrors
		// config.LogFields.ResolvedFor (internal/config/config.go), which is the
		// normal fill path; this is the in-package fallback for zero-field callers.
		if f.ContainerField == "" {
			f.ContainerField = "container"
		}
		if f.NamespaceField == "" {
			f.NamespaceField = "namespace"
		}
		if f.PodField == "" {
			f.PodField = "pod"
		}
		if f.LevelField == "" {
			f.LevelField = "detected_level"
		}
		return f
	}
	// ... existing VictoriaLogs default fills, unchanged ...
}
```

Also add the shared language label helper (used by descriptions/schemas):

```go
// queryLang names the dialect for tool descriptions/schemas.
func (f LogFields) queryLang() string {
	if f.Dialect == DialectLogQL {
		return "LogQL"
	}
	return "LogsQL"
}
```

- [ ] Implement the builder dispatch in `internal/investigate/query_tools.go`. In `buildLogsQLWith` (:399), after `conv = conv.resolved()`, insert:

```go
	if conv.Dialect == DialectLogQL {
		return buildLogQL(raw, container, namespace, level, conv)
	}
```

and add below it:

```go
// buildLogQL composes a valid LogQL (Grafana Loki) query from the resolved
// field convention: `{container="…",namespace="…"}` plus an optional parser
// pipe and a `| <level_field>="…"` label filter — detected_level needs no
// parser on Loki 3.x, so the default pipe is empty. A raw query passes through
// but must start with a stream selector, and LogsQL-isms (unpack_json, _msg —
// the model's likely carry-over mistakes) are rejected with a correcting error,
// mirroring the LogsQL branch's `level=` guard in spirit.
func buildLogQL(raw, container, namespace, level string, conv LogFields) (string, error) {
	if raw != "" {
		if strings.Contains(raw, "unpack_json") || strings.Contains(raw, "_msg") {
			return "", fmt.Errorf("invalid LogQL: unpack_json/_msg are VictoriaLogs LogsQL syntax. Parse with `| json` or `| logfmt` and filter severity with `| %s=\"error\"`, or use the container/namespace/level params", conv.LevelField)
		}
		if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
			return "", fmt.Errorf("invalid LogQL: a query starts with a stream selector, e.g. `{%s=\"apps\"}`", conv.NamespaceField)
		}
		return raw, nil
	}
	var sel []string
	if container != "" {
		sel = append(sel, fmt.Sprintf("%s=%q", conv.ContainerField, container))
	}
	if namespace != "" {
		sel = append(sel, fmt.Sprintf("%s=%q", conv.NamespaceField, namespace))
	}
	if len(sel) == 0 {
		return "", fmt.Errorf("provide a raw `query`, or `container`/`namespace` to build one")
	}
	q := "{" + strings.Join(sel, ",") + "}"
	if level != "" {
		if conv.UnpackPipe != "" {
			q += " | " + conv.UnpackPipe
		}
		q += fmt.Sprintf(" | %s=%q", conv.LevelField, level)
	}
	return q, nil
}
```

- [ ] Make `QueryLogsTool.Description()` (:428) dialect-aware:

```go
func (t QueryLogsTool) Description() string {
	if t.Fields.Dialect == DialectLogQL {
		return "Query logs with LogQL (Grafana Loki) over a recent window. " +
			"PREFER the structured params (container/namespace/level) and let the tool build the query. " +
			"If you write a raw `query`: it MUST start with a stream selector using Loki stream labels, " +
			"e.g. `{namespace=\"apps\", container=\"x\"}`; filter severity with `| detected_level=\"error\"` " +
			"(no parser needed) or parse first with `| json` / `| logfmt`. " +
			"Do NOT use VictoriaLogs LogsQL syntax (unpack_json, _msg, field:value filters). " +
			"Optional since_minutes bounds the window (default 60)."
	}
	return "Query logs with LogsQL (VictoriaLogs) over a recent window. " +
		// ... existing text, unchanged ...
}
```

and `Schema()` (:439): replace the literal `"raw LogsQL; only if the structured fields are insufficient"` with `"raw "+t.Fields.queryLang()+"; only if the structured fields are insufficient"` (build the schema string with `fmt.Sprintf` or concatenation — keep the rest byte-identical).

- [ ] Give `LogsErrorSummaryTool` (logs_summary_tool.go:28) and `DiscoverLogFieldsTool` (discover_tools.go:97) a `Fields LogFields` member (doc comment: "OPTIONAL field convention + dialect (config.logs.*); the zero value keeps the shipped VictoriaLogs behaviour"), switch their `Call` bodies from `buildLogsQL(...)` (logs_summary_tool.go:72, discover_tools.go:133) to `buildLogsQLWith(..., t.Fields)`, and make their `Schema()` "raw LogsQL" strings use `t.Fields.queryLang()` the same way.
- [ ] Apply the zero-count render fix in `DiscoverLogFieldsTool.Call` (discover_tools.go:157-160):

```go
	renderRows(&b, len(names), "more", func(i int) {
		if names[i].Hits > 0 {
			fmt.Fprintf(&b, "%s (×%d)\n", names[i].Name, names[i].Hits)
			return
		}
		fmt.Fprintf(&b, "%s\n", names[i].Name) // Loki stream labels carry no hit count
	})
```

- [ ] Run `go test ./internal/investigate/ -count=1` — expect **PASS** (existing LogsQL tests must be untouched: zero `Dialect` keeps the old behaviour byte-for-byte).
- [ ] Add the end-to-end parity test `internal/investigate/loki_e2e_test.go` — the three REAL tools against the REAL Loki client and one httptest fake Loki (this is the "three investigation tools work against Loki" proof):

```go
// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/logs/loki"
)

// TestLokiToolsEndToEnd wires the three log tools to the real Loki client
// against a fake Loki serving realistic fixtures — the parity proof that
// query_logs, logs_error_summary, and discover_log_fields all work end-to-end
// on a Loki backend (client normalization + dialect query building + renderer).
func TestLokiToolsEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch {
		case r.URL.Path == "/loki/api/v1/query_range" && strings.HasPrefix(q, "sum by"):
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[
			  {"metric":{"detected_level":"error"},"values":[[1704103200,"3"],[1704103500,"412"]]}]}}`)
		case r.URL.Path == "/loki/api/v1/query_range":
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[
			  {"stream":{"namespace":"apps","pod":"harbor-db-0","container":"db"},
			   "values":[["1750413600000000000","db connection refused"]]}]}}`)
		case r.URL.Path == "/loki/api/v1/labels":
			_, _ = io.WriteString(w, `{"status":"success","data":["namespace","pod","container"]}`)
		case r.URL.Path == "/loki/api/v1/detected_fields":
			_, _ = io.WriteString(w, `{"fields":[{"label":"level","type":"string","cardinality":4,"parsers":["logfmt"]}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	lg := loki.New(srv.URL)
	flds := LogFields{Dialect: DialectLogQL}

	out, err := QueryLogsTool{Logs: lg, Fields: flds}.Call(context.Background(),
		`{"namespace":"apps","level":"error"}`)
	if err != nil || !strings.Contains(out, "db connection refused") {
		t.Fatalf("query_logs: %v\n%s", err, out)
	}
	// The renderer must find pod/container under the Loki stream-label names.
	if !strings.Contains(out, "harbor-db-0/db") {
		t.Fatalf("stream identity not derived from loki labels:\n%s", out)
	}

	out, err = LogsErrorSummaryTool{Logs: lg, Fields: flds}.Call(context.Background(),
		`{"namespace":"apps"}`)
	if err != nil || !strings.Contains(out, "412") || !strings.Contains(out, "top messages") {
		t.Fatalf("logs_error_summary: %v\n%s", err, out)
	}

	out, err = DiscoverLogFieldsTool{Logs: lg, Fields: flds}.Call(context.Background(),
		`{"namespace":"apps"}`)
	if err != nil || !strings.Contains(out, "namespace") || !strings.Contains(out, "level (×4)") {
		t.Fatalf("discover_log_fields: %v\n%s", err, out)
	}
}
```

- [ ] Run `go test ./internal/investigate/ -run TestLokiToolsEndToEnd -count=1` — expect **PASS** (fix normalization/render mismatches now, while the whole stack is in view).
- [ ] Commit: `feat(investigate): LogQL dialect for the log tools — query building, guards, descriptions`

---

### Task 7: App wiring — provider selection in `BuildModelAndTools`

**Files:**
- Modify: `internal/app/investigate.go` (the `cfg.Logs.URL != ""` block at :140-169)
- Test: `internal/app/investigate_test.go`

**Steps:**

- [ ] Add the failing selection test to `internal/app/investigate_test.go` (style matches `TestDiscoveryToolsGatedByProvider` at :90; `httptest`/`io` may need importing):

```go
// TestLogsBackendSelection: logs.provider pins the backend; empty auto-detects
// (Loki answers /loki/api/v1/status/buildinfo, VictoriaLogs 404s), failing safe
// to victorialogs. The observable contract is the dialect carried on the
// registered query_logs tool — LogQL means the Loki client + LogQL guidance.
func TestLogsBackendSelection(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent-kubeconfig"))
	log := discardLog()
	base := config.Model{Provider: "openai", BaseURL: "http://vllm:8000/v1", Model: "test-model"}

	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/status/buildinfo" {
			_, _ = io.WriteString(w, `{"version":"3.1.0"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer lokiSrv.Close()
	vlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer vlSrv.Close()

	tests := []struct {
		name, url, pin, wantDialect string
	}{
		{"auto-detect loki", lokiSrv.URL, "", investigate.DialectLogQL},
		{"auto-detect fail-safe victorialogs", vlSrv.URL, "", investigate.DialectLogsQL},
		{"pinned loki skips probe", vlSrv.URL, "loki", investigate.DialectLogQL},
		{"pinned victorialogs skips probe", lokiSrv.URL, "victorialogs", investigate.DialectLogsQL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Model: base}
			cfg.Logs.URL = tc.url
			cfg.Logs.Provider = tc.pin
			_, tools, _, _ := BuildModelAndTools(context.Background(), cfg, nil, nil, log)
			found := false
			for _, tool := range tools {
				if qt, ok := tool.(investigate.QueryLogsTool); ok {
					found = true
					if qt.Fields.Dialect != tc.wantDialect {
						t.Fatalf("dialect = %q, want %q", qt.Fields.Dialect, tc.wantDialect)
					}
				}
			}
			if !found {
				t.Fatalf("query_logs not registered")
			}
		})
	}
}
```

- [ ] Run `go test ./internal/app/ -run TestLogsBackendSelection -count=1` — expect **FAIL** (dialect always `""` — Loki never selected).
- [ ] Replace the logs block in `internal/app/investigate.go` (:140-169) with:

```go
	if cfg.Logs.URL != "" {
		warnIfBackendUnreachable(ctx, log, "logs", cfg.Logs.URL)
		// Backend provider (M1): a config pin wins; otherwise probe once at startup,
		// mirroring the metrics flavor detection above. Detection fails safe to
		// VictoriaLogs — the provider RunLore shipped with — so an existing deployment
		// (or an unreachable backend at startup) sees no behaviour change.
		provider := cfg.Logs.Provider
		if provider == "" {
			provider = logs.Detect(ctx, cfg.Logs.URL, cfg.Logs.TokenEnv, cfg.Logs.Headers)
		}
		// Resolve the OPTIONAL collector field convention once, per provider; an unset
		// config yields the provider's shipped defaults, so this is a no-op unless
		// logs.fields is set.
		lf := cfg.Logs.Fields.ResolvedFor(provider)
		var lg providers.LogsProvider
		dialect := investigate.DialectLogsQL
		switch provider {
		case config.LogsProviderLoki:
			lg = loki.NewWithAuth(cfg.Logs.URL, cfg.Logs.TokenEnv, cfg.Logs.Headers).WithLevelField(lf.LevelField)
			dialect = investigate.DialectLogQL
		default: // victorialogs — the shipped default
			lg = victorialogs.NewWithAuth(cfg.Logs.URL, cfg.Logs.TokenEnv, cfg.Logs.Headers).WithLevelField(lf.LevelField)
		}
		log.Info("logs backend provider", "provider", provider, "pinned", cfg.Logs.Provider != "")
		flds := investigate.LogFields{
			ContainerField: lf.ContainerField,
			NamespaceField: lf.NamespaceField,
			PodField:       lf.PodField,
			LevelField:     lf.LevelField,
			UnpackPipe:     lf.UnpackPipe,
			Dialect:        dialect,
		}
		tools = append(tools,
			// query_logs reads the same raw pod logs pod_logs does, so it shares the
			// pod_log_namespaces allowlist (L2 confinement) and honours the field
			// convention (L1). The incident namespace is injected per-investigation by
			// the loop (scopeTools), exactly like pod_logs.
			investigate.QueryLogsTool{
				Logs:              lg,
				Fields:            flds,
				AllowedNamespaces: cfg.Investigation.PodLogNamespaces,
			},
			// logs_error_summary and discover_log_fields degrade gracefully when the
			// backend lacks the analytics/field capability (both VictoriaLogs and Loki
			// implement them), so they are safe to always register.
			investigate.LogsErrorSummaryTool{Logs: lg, Fields: flds},
			investigate.DiscoverLogFieldsTool{Logs: lg, Fields: flds},
		)
	}
```

Add the imports `"github.com/Smana/runlore/internal/logs"` and `"github.com/Smana/runlore/internal/logs/loki"` (the `victorialogs` import at :22 stays).

- [ ] Run `go test ./internal/app/ -count=1` — expect **PASS**, including the pre-existing `TestDiscoveryToolsGatedByProvider` (:90), whose unreachable `http://logs:9428` URL now takes the auto-detect path and must still resolve to VictoriaLogs via the fail-safe.
- [ ] Run the full suite: `go test ./... -count=1` and `go vet ./...` — expect **PASS/clean**.
- [ ] Commit: `feat(app): wire Grafana Loki logs backend behind logs.provider with startup auto-detect`

---

### Task 8: Docs — data-sources.md, Helm values comment

**Files:**
- Modify: `docs/data-sources.md` (table row :13; new section after :66)
- Modify: `deploy/helm/runlore/values.yaml` (:270)

**Steps:**

- [ ] Update the table row at `docs/data-sources.md:13` to:

```markdown
| Logs | `query_logs`, `logs_error_summary`, `discover_log_fields` | `LogsProvider` | VictoriaLogs (LogsQL) · **Grafana Loki (LogQL)** | `logs.url` (+ optional `logs.provider`) |
```

- [ ] Insert this section after the "Adding another provider" network subsection (after `docs/data-sources.md:66`, before "## Custom webhooks"):

```markdown
## Logs backends

The logs signal is pluggable behind `LogsProvider`: **VictoriaLogs** (LogsQL) and **Grafana
Loki** (LogQL). All three log tools — `query_logs`, `logs_error_summary`,
`discover_log_fields` — work on both; the model is told the right query language automatically.

The provider is **auto-detected** at startup (Loki answers `/loki/api/v1/status/buildinfo`;
VictoriaLogs does not) and **fails safe to VictoriaLogs**, so existing configs are untouched.
Pin it with `provider:` when the backend is unreachable at startup or sits behind a proxy that
confuses the probe.

### `victorialogs` — VictoriaLogs (default)
```yaml
logs:
  url: http://victorialogs.observability.svc:9428
```

### `loki` — Grafana Loki
```yaml
logs:
  url: http://loki-gateway.observability.svc:80
  provider: loki          # optional — auto-detected when omitted
  # token_env: LOKI_TOKEN                 # bearer token, by env-var indirection
  # headers: { X-Scope-OrgID: my-tenant } # multi-tenant Loki
```

Field-convention defaults differ per provider; override any of them via `logs.fields`:

| `logs.fields` key | VictoriaLogs default | Loki default |
|---|---|---|
| `container_field` | `kubernetes.container_name` | `container` |
| `namespace_field` | `kubernetes.pod_namespace` | `namespace` |
| `pod_field` | `kubernetes.pod_name` | `pod` |
| `level_field` | `log.level` | `detected_level` |
| `unpack_pipe` | `unpack_json` | *(none — `detected_level` is structured metadata)* |

> Loki parity notes: `logs_error_summary`'s histogram uses a LogQL metric query
> (`sum by (detected_level) (count_over_time(…))`); its *top messages* are aggregated
> client-side over the (capped, newest-first) query sample, so counts are per-sample rather
> than corpus-wide. `discover_log_fields` merges stream labels (`/loki/api/v1/labels`) with
> detected body fields (`/loki/api/v1/detected_fields`, Loki ≥ 3.0; older Loki degrades to
> labels only). On Loki 2.x there is no `detected_level` — set
> `logs: { fields: { level_field: level, unpack_pipe: logfmt } }` (or `json`) to match your
> collector.
```

- [ ] Replace the logs comment line in `deploy/helm/runlore/values.yaml` (:270) with:

```yaml
  # Logs backend (query_logs / logs_error_summary / discover_log_fields). The provider is
  # auto-detected (Loki answers /loki/api/v1/status/buildinfo; anything else = VictoriaLogs);
  # pin it with `provider:` if the backend is unreachable at chart-install time.
  # logs:    { url: http://victorialogs.observability.svc:9428 }                # VictoriaLogs (LogsQL)
  # logs:    { url: http://loki-gateway.observability.svc:80, provider: loki }  # Grafana Loki (LogQL)
```

- [ ] Run `go test ./internal/config/ -run TestShipped -count=1` (shipped-config/values tests must still parse the chart values) and `helm lint deploy/helm/runlore` if available locally — expect **PASS**.
- [ ] Commit: `docs: document the Grafana Loki logs backend (data-sources, chart values)`

---

## Acceptance criteria

- [ ] `internal/logs/loki` implements `providers.LogsProvider`, `providers.LogStats`, and `providers.LogFields` (compile-time asserted with `var _ =` like `victorialogs.go:77-83`), against `/loki/api/v1/query_range`, `/loki/api/v1/labels`, and `/loki/api/v1/detected_fields`.
- [ ] All three tools (`query_logs`, `logs_error_summary`, `discover_log_fields`) work end-to-end against a fake Loki (`internal/investigate/loki_e2e_test.go` passes).
- [ ] `logs.provider` accepts `""`/`victorialogs`/`loki` and rejects anything else at startup; empty auto-detects via the buildinfo probe and **fails safe to victorialogs** (unreachable, 404, and non-JSON-200 cases all covered by tests).
- [ ] No new required config: a config with only `logs.url` set keeps working against VictoriaLogs byte-for-byte (zero `Dialect`, `Resolved()` defaults, existing `internal/investigate` and `internal/app` tests unmodified except the new ones); unset `logs` still registers no log tools.
- [ ] On a Loki deployment the model is told **LogQL** in every log-tool description/schema, raw LogsQL-isms get a correcting error, and structured params compile to valid LogQL (`buildLogQL` tests).
- [ ] Error paths covered: Loki down, non-200, malformed response, empty results (empty ⇒ the tools' existing `noLogLinesMatched` / "no fields found" messages, never a crash).
- [ ] Loki-specific parity gaps are explicit in code comments and docs: no offset pagination (single capped request + `TruncationLine`), client-side `TopMessages` (per-sample counts), `detected_fields` cardinality-as-count (rendered only when > 0), Loki 2.x `detected_level` caveat.
- [ ] `docs/data-sources.md` and `deploy/helm/runlore/values.yaml` document both backends with working YAML examples.
- [ ] `go test ./... -count=1` and `go vet ./...` pass; every commit uses a conventional message with **no** Co-Authored-By line.
