# Investigation Coalescing + Rate Limiting + Token Efficiency — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop an Alertmanager storm from spawning a flood of redundant, expensive LLM investigations — by coalescing correlated alerts, capping investigation starts per window, trimming per-investigation token cost, and making all of it observable.

**Architecture:** Four orthogonal layers on the existing `webhook → Decide → Queue → loop` path. (1) An ingress `Coalescer` folds correlated incidents into one investigation. (2) A `ratelimit.Window` gate caps starts per window in `Queue.process`. (3) `loop.go` gains a step cap, tool-output truncation, and a token-budget nudge. (4) OpenTelemetry metrics (Prometheus exporter on `/metrics`) make the savings measurable. The loop, queue, and model layers are touched minimally; coalescing is purely an ingress concern.

**Tech Stack:** Go, `k8s.io/client-go/util/workqueue`, `gopkg.in/yaml.v3`, `go.opentelemetry.io/otel` + Prometheus exporter, plain `testing` (no testify), house style `now func() time.Time`.

**Design spec:** `dev/superpowers/specs/2026-06-22-investigation-coalescing-rate-limit-design.md`

**Conventions:**
- Module path `github.com/Smana/runlore`.
- Run all tests for a package: `go test ./internal/<pkg>/...`
- Before each commit: `gofmt -w <files> && go vet ./... && go build ./...`
- Commit style: conventional commits (`feat(coalesce): …`, `test(ratelimit): …`). **No co-author / attribution lines.**
- Each Part is independently committable. Order is foundation → consumers.

---

## Part 1 — Config scaffolding

Adds the `Investigation` and `Telemetry` config structs everything else reads. Pure data + yaml tags; one parse test.

### Task 1.1: Add `Investigation` and `Telemetry` config structs

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_investigation_test.go` (create)

- [ ] **Step 1: Write the failing test**

```go
package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestInvestigationConfigParse(t *testing.T) {
	const y = `
investigation:
  coalesce:
    enabled: true
    debounce: 30s
    max_wait: 2m
    max_batch: 50
    cooldown: 10m
    correlation_labels: [alertname, namespace]
  rate_limit:
    max_per_window: 20
    window: 1h
    max_requeues: 10
  max_steps: 15
  max_tool_output_bytes: 16384
  max_tokens_per_investigation: 120000
telemetry:
  metrics_enabled: true
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !c.Investigation.Coalesce.Enabled {
		t.Fatal("coalesce.enabled should be true")
	}
	if c.Investigation.Coalesce.Debounce.Std() != 30*time.Second {
		t.Fatalf("debounce: got %v", c.Investigation.Coalesce.Debounce.Std())
	}
	if c.Investigation.RateLimit.MaxPerWindow != 20 {
		t.Fatalf("max_per_window: got %d", c.Investigation.RateLimit.MaxPerWindow)
	}
	if c.Investigation.RateLimit.MaxRequeues != 10 {
		t.Fatalf("max_requeues: got %d", c.Investigation.RateLimit.MaxRequeues)
	}
	if c.Investigation.MaxSteps != 15 || c.Investigation.MaxToolOutputBytes != 16384 {
		t.Fatalf("scalar fields: %+v", c.Investigation)
	}
	if got := strings.Join(c.Investigation.Coalesce.CorrelationLabels, ","); got != "alertname,namespace" {
		t.Fatalf("correlation_labels: got %q", got)
	}
	if !c.Telemetry.MetricsEnabled {
		t.Fatal("telemetry.metrics_enabled should be true")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestInvestigationConfigParse`
Expected: FAIL — `c.Investigation` / `c.Telemetry` undefined fields.

- [ ] **Step 3: Add the structs and Config fields**

In `internal/config/config.go`, add the two fields to the top-level `Config` struct (after `Server ServerConfig`):

```go
	Investigation Investigation `yaml:"investigation"` // coalescing + rate-limit + per-investigation token controls
	Telemetry     Telemetry     `yaml:"telemetry"`     // OpenTelemetry metrics
```

Add the new types (place near the other policy structs):

```go
// Investigation holds cost/throughput controls on the alert→investigation→LLM path.
type Investigation struct {
	Coalesce                  Coalesce  `yaml:"coalesce"`
	RateLimit                 RateLimit `yaml:"rate_limit"`
	MaxSteps                  int       `yaml:"max_steps"`                     // 0 ⇒ loop default (20)
	MaxToolOutputBytes        int       `yaml:"max_tool_output_bytes"`         // 0 ⇒ unlimited
	MaxTokensPerInvestigation int       `yaml:"max_tokens_per_investigation"`  // 0 ⇒ unlimited
}

// Coalesce folds correlated incidents into one investigation.
type Coalesce struct {
	Enabled           bool     `yaml:"enabled"`
	Debounce          Duration `yaml:"debounce"`
	MaxWait           Duration `yaml:"max_wait"`
	MaxBatch          int      `yaml:"max_batch"`
	Cooldown          Duration `yaml:"cooldown"`
	CorrelationLabels []string `yaml:"correlation_labels"` // empty ⇒ AM groupKey, else namespace+label values
}

// RateLimit caps investigation starts per sliding window.
type RateLimit struct {
	MaxPerWindow int      `yaml:"max_per_window"` // 0 ⇒ unlimited
	Window       Duration `yaml:"window"`
	MaxRequeues  int      `yaml:"max_requeues"` // drop a key after this many backoff requeues
}

// Telemetry configures OpenTelemetry metrics export.
type Telemetry struct {
	MetricsEnabled bool   `yaml:"metrics_enabled"` // serve OTel metrics on GET /metrics (Prometheus exposition)
	OTLPEndpoint   string `yaml:"otlp_endpoint"`   // optional OTLP push (phase-2); empty ⇒ scrape-only
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestInvestigationConfigParse`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/config/config.go internal/config/config_investigation_test.go
go vet ./internal/config/... && go build ./...
git add internal/config/config.go internal/config/config_investigation_test.go
git commit -m "feat(config): add investigation (coalesce/rate-limit/token) + telemetry config"
```

---

## Part 2 — Telemetry foundation (OpenTelemetry + `/metrics`)

A small `internal/telemetry` package: a `Metrics` instrument set (no-op safe) plus a Prometheus-exporter `/metrics` handler. Adds the first self-instrumentation to RunLore.

### Task 2.1: Add the OTel dependencies

**Files:** `go.mod`, `go.sum`

- [ ] **Step 1: Add deps**

Run:
```bash
go get go.opentelemetry.io/otel@latest \
       go.opentelemetry.io/otel/sdk/metric@latest \
       go.opentelemetry.io/otel/exporters/prometheus@latest
```
Expected: `go.mod` gains the three modules.

- [ ] **Step 2: Commit**

```bash
go build ./...
git add go.mod go.sum
git commit -m "build: add OpenTelemetry metric SDK + prometheus exporter"
```

### Task 2.2: `Metrics` instrument set

**Files:**
- Create: `internal/telemetry/metrics.go`
- Test: `internal/telemetry/metrics_test.go`

- [ ] **Step 1: Write the failing test**

```go
package telemetry

import (
	"context"
	"testing"
)

func TestNewMetricsNoProvider(t *testing.T) {
	// With no provider configured, the global meter is a no-op; instruments must
	// still construct and be safe to call.
	m := NewMetrics()
	ctx := context.Background()
	m.AlertsReceived.Add(ctx, 1)
	m.AlertsCoalesced.Add(ctx, 3)
	m.InvestigationsStarted.Add(ctx, 1)
	m.ToolOutputTruncatedBytes.Add(ctx, 4096)
	m.CoalesceBatchSize.Record(ctx, 12)
	m.InvestigationTokens.Record(ctx, 5000)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/telemetry/ -run TestNewMetricsNoProvider`
Expected: FAIL — package/`NewMetrics` undefined.

- [ ] **Step 3: Implement `Metrics`**

```go
// Package telemetry provides RunLore's self-instrumentation: an OpenTelemetry
// metric set plus a Prometheus-exporter HTTP handler. Instruments are safe to
// call even when no provider is configured (the global meter is a no-op), so
// callers never need nil checks.
package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const scope = "github.com/Smana/runlore"

// Metrics is the RunLore instrument set, created once and shared.
type Metrics struct {
	AlertsReceived           metric.Int64Counter
	AlertsCoalesced          metric.Int64Counter
	AlertsSuppressed         metric.Int64Counter
	InvestigationsStarted    metric.Int64Counter
	InvestigationsThrottled  metric.Int64Counter
	InvestigationsDropped    metric.Int64Counter
	ToolOutputTruncatedBytes metric.Int64Counter
	RecallHits               metric.Int64Counter     // KB cache hits, labelled by verify result
	RecallTokensSaved        metric.Int64Counter     // estimated tokens saved by a recall short-circuit
	CoalesceBatchSize        metric.Int64Histogram
	InvestigationTokens      metric.Int64Histogram
	RecallScore              metric.Float64Histogram // BM25 score at the recall decision (tunes min_score)
}

// NewMetrics builds the instrument set from the global meter provider.
func NewMetrics() *Metrics {
	m := otel.Meter(scope)
	ctr := func(name, desc string) metric.Int64Counter {
		c, _ := m.Int64Counter("runlore_"+name, metric.WithDescription(desc))
		return c
	}
	hist := func(name, desc string) metric.Int64Histogram {
		h, _ := m.Int64Histogram("runlore_"+name, metric.WithDescription(desc))
		return h
	}
	histF := func(name, desc string) metric.Float64Histogram {
		h, _ := m.Float64Histogram("runlore_"+name, metric.WithDescription(desc))
		return h
	}
	return &Metrics{
		AlertsReceived:           ctr("alerts_received_total", "incidents passing Decide into the coalescer"),
		AlertsCoalesced:          ctr("alerts_coalesced_total", "incidents folded into an existing batch"),
		AlertsSuppressed:         ctr("alerts_suppressed_total", "incidents dropped by cooldown"),
		InvestigationsStarted:    ctr("investigations_started_total", "investigations actually begun"),
		InvestigationsThrottled:  ctr("investigations_throttled_total", "starts requeued by the rate limiter"),
		InvestigationsDropped:    ctr("investigations_dropped_total", "keys dropped after max_requeues"),
		ToolOutputTruncatedBytes: ctr("tool_output_truncated_bytes_total", "bytes elided by output truncation"),
		RecallHits:               ctr("recall_hits_total", "KB instant-recall short-circuits (label: result)"),
		RecallTokensSaved:        ctr("recall_tokens_saved_total", "estimated tokens saved by recall short-circuits"),
		CoalesceBatchSize:        hist("coalesce_batch_size", "incidents per flushed batch"),
		InvestigationTokens:      hist("investigation_tokens_estimated", "per-investigation token estimate"),
		RecallScore:              histF("recall_score", "BM25 score at the recall decision point"),
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/telemetry/ -run TestNewMetricsNoProvider`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/telemetry/*.go && go vet ./internal/telemetry/... && go build ./...
git add internal/telemetry/
git commit -m "feat(telemetry): add OTel runlore_* metric instrument set"
```

### Task 2.3: Prometheus exporter + `/metrics` handler

**Files:**
- Create: `internal/telemetry/setup.go`
- Test: `internal/telemetry/setup_test.go`

- [ ] **Step 1: Write the failing test**

```go
package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetupServesMetrics(t *testing.T) {
	h, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	m := NewMetrics() // instruments now bind to the configured provider
	m.AlertsReceived.Add(context.Background(), 7)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "runlore_alerts_received_total") {
		t.Fatalf("metrics output missing series:\n%s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/telemetry/ -run TestSetupServesMetrics`
Expected: FAIL — `Setup` undefined.

- [ ] **Step 3: Implement `Setup`**

```go
package telemetry

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Setup installs a global OTel meter provider backed by a Prometheus exporter
// and returns an http.Handler that serves the exposition format, plus a
// shutdown func. Call NewMetrics AFTER Setup so instruments bind to this provider.
func Setup(ctx context.Context) (http.Handler, func(context.Context) error, error) {
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(mp)
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return handler, mp.Shutdown, nil
}
```

(`github.com/prometheus/client_golang` is pulled in transitively by the OTel Prometheus exporter; if `go build` reports it missing, run `go get github.com/prometheus/client_golang`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/telemetry/`
Expected: PASS (both telemetry tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/telemetry/*.go && go vet ./internal/telemetry/... && go build ./...
git add internal/telemetry/ go.mod go.sum
git commit -m "feat(telemetry): Prometheus exporter + /metrics handler"
```

### Task 2.4: Wire `/metrics` into the server + main

**Files:**
- Modify: `internal/server/server.go` (the mux setup near line 76)
- Modify: `cmd/lore/main.go` (server construction / startup)

- [ ] **Step 1: Register the route**

In `internal/server/server.go`, where the mux is built (the block with `mux.HandleFunc("POST /webhook/alertmanager", …)` ~line 76), accept an optional metrics handler on the `Server` and register it:

Add a field to `Server`: `metrics http.Handler // optional; GET /metrics`. In the mux block:
```go
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}
```

- [ ] **Step 2: Wire in main**

In `cmd/lore/main.go`, near server construction, before building the `Server`, when `cfg.Telemetry.MetricsEnabled`:
```go
	var metricsHandler http.Handler
	if cfg.Telemetry.MetricsEnabled {
		h, shutdown, err := telemetry.Setup(ctx)
		if err != nil {
			return fmt.Errorf("telemetry setup: %w", err)
		}
		defer func() { _ = shutdown(context.Background()) }()
		metricsHandler = h
	}
	metrics := telemetry.NewMetrics() // no-op safe when telemetry disabled
```
Pass `metricsHandler` into the `Server` (new field) and `metrics` into the investigator/queue/coalescer constructors (Parts 3–5). Import `"github.com/Smana/runlore/internal/telemetry"`.

- [ ] **Step 3: Verify build + existing server tests**

Run: `go build ./... && go test ./internal/server/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
gofmt -w internal/server/server.go cmd/lore/main.go
git add internal/server/server.go cmd/lore/main.go
git commit -m "feat(server): serve OTel metrics on GET /metrics when enabled"
```

---

### Task 2.5: Instrument the KB recall (cache) path

Recall is RunLore's KB cache: a BM25 match short-circuits the whole investigation with **0 LLM calls** (`internal/investigate/recall.go`, used at the short-circuit in `loop.go` ~L99-112). Instrument it for hit-rate, token savings, and — critically — *trustworthiness*, not just raw hits.

**Design guardrails (the "challenge"):**
- **Join hits to correctness, not vanity.** A bare hit counter rewards a fast-but-wrong cache (recall's known failure mode: symptom ≠ root cause). Label the hit counter by the **verify-pass result** (`verified` / `downgraded` / `rejected`) so the KPI is *trustworthy* hits.
- **No high-cardinality labels.** KB-entry id, alertname, and score must NOT be metric labels (Prometheus cardinality blowup). They go in a **structured log** line; metrics stay bounded (`result` only).
- **Score histogram is the high-value metric** — it's what lets you tune the non-portable `min_score` threshold.

**Files:** `internal/investigate/recall.go`, `internal/investigate/loop.go`

- [ ] **Step 1: Emit the score histogram + structured log at the recall decision** (`recall.go`, where the top BM25 hit's score is compared to `MinScore`):

```go
	m.RecallScore.Record(ctx, score)
	log.Info("kb recall decision",
		"alert", req.Title, "entry_id", hit.ID, "score", score,
		"min_score", minScore, "hit", score >= minScore)
```

Pass the `*telemetry.Metrics` and a `*slog.Logger` into the `Recall` struct; both nil-safe / discard in tests.

- [ ] **Step 2: Count hits by verify result + estimate tokens saved** at the recall short-circuit in `loop.go` (after the verify pass runs on the recalled finding, ~L104-112):

```go
	result := "verified"
	switch {
	case len(inv.RootCauses) == 0:
		result = "rejected"
	case downgraded:
		result = "downgraded"
	}
	m.RecallHits.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	m.RecallTokensSaved.Add(ctx, int64(li.MaxTokensPerInvestigation)) // conservative proxy for a skipped investigation
```

Use whatever the verify pass already exposes to decide `downgraded`/`rejected` (it trims rejected hypotheses — key off that field). Import `go.opentelemetry.io/otel/attribute`.

- [ ] **Step 3: Test**

A recall-path test (reuse `recall_test.go` if present) asserting a hit records a score and increments `RecallHits`. Under the no-op provider the calls must not panic; for value assertions use a test `MeterProvider` + manual reader, or scrape the `/metrics` text from Task 2.3.

Run: `go test ./internal/investigate/ -run Recall`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
gofmt -w internal/investigate/recall.go internal/investigate/loop.go
go vet ./internal/investigate/... && go build ./...
git add internal/investigate/recall.go internal/investigate/loop.go
git commit -m "feat(recall): instrument KB cache hits (score, result-labelled hits, tokens saved)"
```

> **Out of scope here** (it's the bigger learning-loop workstream from the project review): joining a recall hit to its *real* outcome — did the cached answer actually resolve the incident. The `result` label is a verify-pass proxy, not ground truth. Track that as phase-2.

---

## Part 3 — Per-investigation token efficiency (`loop.go`)

Three independent brakes on `LoopInvestigator`. Add fields, default-off (0 ⇒ unlimited), wire from config in main.

### Task 3.1: Tool-output truncation

**Files:**
- Create: `internal/investigate/truncate.go`
- Test: `internal/investigate/truncate_test.go`
- Modify: `internal/investigate/loop.go`

- [ ] **Step 1: Write the failing test**

```go
package investigate

import "testing"

func TestTruncateOutput(t *testing.T) {
	if got := truncateOutput("short", 100); got != "short" {
		t.Fatalf("under limit must pass through, got %q", got)
	}
	big := ""
	for i := 0; i < 1000; i++ {
		big += "x"
	}
	got := truncateOutput(big, 100)
	if len(got) >= len(big) {
		t.Fatalf("expected truncation, got len %d", len(got))
	}
	if !contains(got, "truncated") {
		t.Fatalf("missing truncation marker: %q", got)
	}
	// head and tail are preserved
	if got[:10] != "xxxxxxxxxx" || got[len(got)-10:] != "xxxxxxxxxx" {
		t.Fatalf("head/tail not preserved: %q", got)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestTruncateOutput`
Expected: FAIL — `truncateOutput` undefined.

- [ ] **Step 3: Implement**

`internal/investigate/truncate.go`:
```go
package investigate

import "fmt"

// truncateOutput caps s to max bytes, keeping a head and tail with a marker in
// the middle. max <= 0 disables truncation. Returns bytes trimmed via the marker.
func truncateOutput(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	// reserve room for the marker; split the remaining budget head/tail.
	const minMarker = 40
	keep := max - minMarker
	if keep < 2 {
		keep = 2
	}
	head := keep / 2
	tail := keep - head
	trimmed := len(s) - head - tail
	return s[:head] + fmt.Sprintf("\n…[truncated %d bytes]…\n", trimmed) + s[len(s)-tail:]
}
```

- [ ] **Step 4: Apply in the loop**

In `internal/investigate/loop.go`: add field `MaxToolOutputBytes int` to `LoopInvestigator`. At the tool-result append (currently `messages = append(messages, providers.Message{Role: "tool", ToolCallID: tc.ID, Content: li.runTool(ctx, byName, tc)})`), wrap the content:
```go
			out := truncateOutput(li.runTool(ctx, byName, tc), li.MaxToolOutputBytes)
			messages = append(messages, providers.Message{Role: "tool", ToolCallID: tc.ID, Content: out})
```

- [ ] **Step 5: Run + commit**

Run: `go test ./internal/investigate/ -run TestTruncateOutput && go build ./...`
Expected: PASS.
```bash
gofmt -w internal/investigate/truncate.go internal/investigate/truncate_test.go internal/investigate/loop.go
git add internal/investigate/truncate.go internal/investigate/truncate_test.go internal/investigate/loop.go
git commit -m "feat(loop): truncate oversized tool outputs before they enter history"
```

### Task 3.2: Token-budget nudge + `max_steps` wiring

**Files:**
- Create: `internal/investigate/budget.go`
- Test: `internal/investigate/budget_test.go`
- Modify: `internal/investigate/loop.go`

- [ ] **Step 1: Write the failing test**

```go
package investigate

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestEstimateTokens(t *testing.T) {
	msgs := []providers.Message{
		{Role: "user", Content: "12345678"},     // 8 chars
		{Role: "assistant", Content: "1234"},      // 4 chars
	}
	// (len(system)=4 + 8 + 4) / 4 = 4
	if got := estimateTokens("sys!", msgs); got != 4 {
		t.Fatalf("estimateTokens: got %d, want 4", got)
	}
}

func TestOverBudget(t *testing.T) {
	if overBudget(100, 50) != true {
		t.Fatal("100>50 should be over budget")
	}
	if overBudget(10, 50) != false {
		t.Fatal("10<50 should be under budget")
	}
	if overBudget(100, 0) != false {
		t.Fatal("budget 0 means unlimited")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run 'TestEstimateTokens|TestOverBudget'`
Expected: FAIL — `estimateTokens`/`overBudget` undefined.

- [ ] **Step 3: Implement helpers**

`internal/investigate/budget.go`:
```go
package investigate

import "github.com/Smana/runlore/internal/providers"

// estimateTokens approximates the request size (~4 chars/token) over the system
// prompt plus the full message history — the cost actually re-sent each step.
// Provider-reported usage is not exposed in CompletionResponse today.
func estimateTokens(system string, msgs []providers.Message) int {
	n := len(system)
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n / 4
}

// overBudget reports whether est exceeds budget. budget <= 0 means unlimited.
func overBudget(est, budget int) bool { return budget > 0 && est > budget }

const budgetNudge = "⚠️ token budget reached — call submit_findings now with your best current hypotheses and the evidence gathered so far."
```

- [ ] **Step 4: Wire into the loop**

In `internal/investigate/loop.go`, add fields `MaxTokensPerInvestigation int` to `LoopInvestigator`. Just before the `li.Model.Complete(...)` call, inject the nudge once when over budget:
```go
		if !nudged && overBudget(estimateTokens(li.system(), messages), li.MaxTokensPerInvestigation) {
			messages = append(messages, providers.Message{Role: "user", Content: budgetNudge})
			nudged = true
		}
		resp, err := li.Model.Complete(ctx, providers.CompletionRequest{System: li.system(), Messages: messages, Tools: specs})
```
Declare `nudged := false` above the loop. `maxSteps` remains the hard bound (already defaults to 20 at lines 122-128).

- [ ] **Step 5: Wire `max_steps` + the two budgets from config (main)**

In `cmd/lore/main.go`, where the `LoopInvestigator` is constructed, set from config:
```go
		MaxSteps:                  cfg.Investigation.MaxSteps,
		MaxToolOutputBytes:        cfg.Investigation.MaxToolOutputBytes,
		MaxTokensPerInvestigation: cfg.Investigation.MaxTokensPerInvestigation,
```

- [ ] **Step 6: Run + commit**

Run: `go test ./internal/investigate/ && go build ./...`
Expected: PASS.
```bash
gofmt -w internal/investigate/budget.go internal/investigate/budget_test.go internal/investigate/loop.go cmd/lore/main.go
git add internal/investigate/budget.go internal/investigate/budget_test.go internal/investigate/loop.go cmd/lore/main.go
git commit -m "feat(loop): token-budget nudge + max_steps/output/token config wiring"
```

---

## Part 4 — Safety rate limiter

A reusable sliding-window limiter, then a start-cap gate in `Queue.process`.

### Task 4.1: `ratelimit.Window`

**Files:**
- Create: `internal/ratelimit/window.go`
- Test: `internal/ratelimit/window_test.go`

- [ ] **Step 1: Write the failing test**

```go
package ratelimit

import (
	"testing"
	"time"
)

func TestWindowAllowAndSlide(t *testing.T) {
	now := time.Unix(0, 0)
	w := New(2, time.Minute)
	w.now = func() time.Time { return now }

	if !w.Allow() || !w.Allow() {
		t.Fatal("first two starts within budget should be allowed")
	}
	if w.Allow() {
		t.Fatal("third start should be denied (budget 2)")
	}
	if got := w.Count(); got != 2 {
		t.Fatalf("Count: got %d, want 2", got)
	}
	// roll the window forward; old entries expire
	now = now.Add(2 * time.Minute)
	if w.Count() != 0 {
		t.Fatalf("window should have slid clear, Count=%d", w.Count())
	}
	if !w.Allow() {
		t.Fatal("after slide, a new start should be allowed")
	}
}

func TestWindowZeroMaxUnlimited(t *testing.T) {
	w := New(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !w.Allow() {
			t.Fatal("max 0 must be unlimited")
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ratelimit/`
Expected: FAIL — package/`New` undefined.

- [ ] **Step 3: Implement (mirrors `action/auto.go:reserve()`)**

```go
// Package ratelimit provides a sliding-window start limiter — the windowed
// timestamp pattern from internal/action/auto.go:reserve(), reusable.
package ratelimit

import (
	"sync"
	"time"
)

// Window allows up to max events per sliding window. max <= 0 is unlimited.
// Safe for concurrent use; clock injectable for tests.
type Window struct {
	max    int
	window time.Duration
	now    func() time.Time
	mu     sync.Mutex
	recent []time.Time
}

// New returns a Window allowing max events per window.
func New(max int, window time.Duration) *Window {
	return &Window{max: max, window: window, now: time.Now}
}

func (w *Window) slideLocked() {
	cutoff := w.now().Add(-w.window)
	kept := w.recent[:0]
	for _, t := range w.recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.recent = kept
}

// Allow reports whether an event fits the budget, recording it if so.
func (w *Window) Allow() bool {
	if w.max <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.slideLocked()
	if len(w.recent) >= w.max {
		return false
	}
	w.recent = append(w.recent, w.now())
	return true
}

// Count returns the number of events currently in the window (peek; no record).
func (w *Window) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.slideLocked()
	return len(w.recent)
}
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/ratelimit/`
Expected: PASS.
```bash
gofmt -w internal/ratelimit/*.go && go vet ./internal/ratelimit/...
git add internal/ratelimit/
git commit -m "feat(ratelimit): sliding-window start limiter"
```

### Task 4.2: Gate `Queue.process` on the start budget

**Files:**
- Modify: `internal/investigate/investigate.go`
- Test: `internal/investigate/queue_ratelimit_test.go`

- [ ] **Step 1: Write the failing test**

```go
package investigate

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/ratelimit"
)

type countingInv struct{ n int }

func (c *countingInv) Investigate(ctx context.Context, _ Request) error { c.n++; return nil }

func TestQueueRateLimitGate(t *testing.T) {
	inv := &countingInv{}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.starts = ratelimit.New(1, time.Hour) // budget 1 per hour
	q.maxRequeues = 3

	// process two distinct keys in one window; only one should reach Investigate.
	q.Enqueue(Request{Source: SourceAlert, Title: "A", Workload: workloadNS("a")})
	q.Enqueue(Request{Source: SourceAlert, Title: "B", Workload: workloadNS("b")})
	drainOnce(t, q) // helper runs process() for each queued key synchronously
	drainOnce(t, q)

	if inv.n != 1 {
		t.Fatalf("rate limit should allow exactly one investigation, got %d", inv.n)
	}
}
```

> The exact `NewQueue` constructor name and a synchronous `drainOnce` test helper depend on the current `investigate.go` test harness — reuse the package's existing queue test helpers (grep `func TestQueue` in `internal/investigate/investigate_test.go`). `workloadNS` builds a `providers.Workload{Namespace: …}`. If no constructor exists, add `NewQueue(inv Investigator, log *slog.Logger) *Queue` that initializes `reqs: map[key]pending{}`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestQueueRateLimitGate`
Expected: FAIL — `q.starts`/`q.maxRequeues` fields undefined.

- [ ] **Step 3: Add fields + gate**

In `internal/investigate/investigate.go`, add to `Queue`:
```go
	starts      *ratelimit.Window      // nil = unlimited
	maxRequeues int                    // drop a key after this many backoff requeues
	metrics     *telemetry.Metrics     // nil-safe via NewMetrics; counters for started/throttled/dropped
	onThrottle  func()                 // optional once-per-window notice; may be nil
	throttled   *ratelimit.Window      // 1-per-window guard for onThrottle
```
In `process`, immediately after fetching `p` and before `q.inv.Investigate`:
```go
	if q.starts != nil && !q.starts.Allow() {
		if wq.NumRequeues(k) >= q.maxRequeues {
			if q.metrics != nil {
				q.metrics.InvestigationsDropped.Add(ctx, 1)
			}
			q.log.Warn("investigation budget exhausted; dropping (Alertmanager will re-fire)", "title", p.req.Title)
			wq.Forget(k)
			q.maybeNotifyThrottle()
			return
		}
		if q.metrics != nil {
			q.metrics.InvestigationsThrottled.Add(ctx, 1)
		}
		wq.AddRateLimited(k)
		q.maybeNotifyThrottle()
		return
	}
	if q.metrics != nil {
		q.metrics.InvestigationsStarted.Add(ctx, 1)
	}
```
Add the helper:
```go
func (q *Queue) maybeNotifyThrottle() {
	if q.onThrottle != nil && (q.throttled == nil || q.throttled.Allow()) {
		q.onThrottle()
	}
}
```
Import `"github.com/Smana/runlore/internal/ratelimit"` and `"github.com/Smana/runlore/internal/telemetry"`. Wire `q.starts`, `q.maxRequeues`, `q.metrics` from `cfg.Investigation.RateLimit` in `cmd/lore/main.go` where the Queue is constructed (set `q.throttled = ratelimit.New(1, cfg.Investigation.RateLimit.Window.Std())`).

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/investigate/ && go build ./...`
Expected: PASS.
```bash
gofmt -w internal/investigate/investigate.go internal/investigate/queue_ratelimit_test.go cmd/lore/main.go
git add internal/investigate/investigate.go internal/investigate/queue_ratelimit_test.go cmd/lore/main.go
git commit -m "feat(investigate): cap investigation starts per window with backoff overflow"
```

---

## Part 5 — Coalescer (the headline)

Fold correlated incidents into one investigation at ingress.

### Task 5.1: Capture AM `GroupKey` onto `Incident`

**Files:**
- Modify: `internal/config/config.go` (`Incident` struct)
- Modify: `internal/trigger/incident.go`
- Test: `internal/trigger/incident_groupkey_test.go`

- [ ] **Step 1: Write the failing test**

```go
package trigger

import "strings"

import "testing"

func TestParseAlertmanagerGroupKey(t *testing.T) {
	body := `{"groupKey":"{}:{alertname=\"X\"}","alerts":[
		{"status":"firing","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"fp1"},
		{"status":"firing","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"fp2"}]}`
	incs, err := ParseAlertmanager(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(incs) != 2 {
		t.Fatalf("want 2 incidents, got %d", len(incs))
	}
	for _, inc := range incs {
		if inc.GroupKey != `{}:{alertname="X"}` {
			t.Fatalf("GroupKey not threaded: %q", inc.GroupKey)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/trigger/ -run TestParseAlertmanagerGroupKey`
Expected: FAIL — `inc.GroupKey` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add to `Incident`:
```go
	GroupKey string // Alertmanager group identity (shared by all alerts in one webhook POST)
```
In `internal/trigger/incident.go`, add `GroupKey string \`json:"groupKey"\`` to `amPayload`, and set it on each incident in the build loop:
```go
		out = append(out, config.Incident{
			AlertName:   a.Labels["alertname"],
			Severity:    a.Labels["severity"],
			Environment: cmp.Or(a.Labels["environment"], a.Labels["env"]),
			Namespace:   a.Labels["namespace"],
			Labels:      a.Labels,
			StartsAt:    startsAt,
			Fingerprint: a.Fingerprint,
			GroupKey:    p.GroupKey,
		})
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/trigger/ && go build ./...`
Expected: PASS.
```bash
gofmt -w internal/config/config.go internal/trigger/incident.go internal/trigger/incident_groupkey_test.go
git add internal/config/config.go internal/trigger/incident.go internal/trigger/incident_groupkey_test.go
git commit -m "feat(trigger): thread Alertmanager groupKey onto Incident"
```

### Task 5.2: Coalescer `key()` + `Summarize()`

**Files:**
- Create: `internal/coalesce/coalescer.go`
- Test: `internal/coalesce/coalescer_test.go`

- [ ] **Step 1: Write the failing test**

```go
package coalesce

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

func inc(name, ns, sev, gk string) config.Incident {
	return config.Incident{AlertName: name, Namespace: ns, Severity: sev, GroupKey: gk,
		Labels: map[string]string{"alertname": name, "namespace": ns}}
}

func TestKeyGroupKeyDefault(t *testing.T) {
	c := New(Config{}, nil)
	if got := c.key(inc("X", "ns", "warning", "GK1")); got != "GK1" {
		t.Fatalf("default key should be groupKey, got %q", got)
	}
	// fallback when groupKey empty
	if got := c.key(inc("X", "ns", "warning", "")); got != "ns/X" {
		t.Fatalf("fallback key should be ns/alertname, got %q", got)
	}
}

func TestKeyCorrelationLabels(t *testing.T) {
	c := New(Config{CorrelationLabels: []string{"alertname"}}, nil)
	if got := c.key(inc("X", "ns", "warning", "GK1")); got != "ns/X" {
		t.Fatalf("label key, got %q", got)
	}
}

func TestSummarize(t *testing.T) {
	s := Summarize([]config.Incident{inc("X", "ns", "warning", "g"), inc("X", "ns", "warning", "g"), inc("Y", "ns", "warning", "g")})
	if !strings.Contains(s, "3 correlated alerts") || !strings.Contains(s, "X×2") {
		t.Fatalf("summary: %q", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/coalesce/`
Expected: FAIL — package undefined.

- [ ] **Step 3: Implement key + Summarize + skeleton**

```go
// Package coalesce folds correlated Alertmanager incidents into a single
// investigation, suppressing the redundant per-alert investigations a storm
// would otherwise spawn. In-memory, mutex-guarded, clock injected for tests.
package coalesce

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/config"
)

// Config mirrors config.Coalesce with std durations.
type Config struct {
	Enabled           bool
	Debounce          time.Duration
	MaxWait           time.Duration
	MaxBatch          int
	Cooldown          time.Duration
	CorrelationLabels []string
}

type batch struct {
	incidents          []config.Incident
	firstSeen, lastSeen time.Time
}

// Coalescer buffers correlated incidents and flushes one investigation per key.
type Coalescer struct {
	cfg    Config
	now    func() time.Time
	out    func([]config.Incident) // flush sink (build a Request + enqueue)
	notify func(key string, n int) // optional cooldown-suppression notice; may be nil

	mu        sync.Mutex
	pending   map[string]*batch
	recent    map[string]time.Time
	suppressed map[string]int
}

// New builds a Coalescer. out is called with each flushed batch.
func New(cfg Config, out func([]config.Incident)) *Coalescer {
	return &Coalescer{cfg: cfg, now: time.Now, out: out,
		pending: map[string]*batch{}, recent: map[string]time.Time{}, suppressed: map[string]int{}}
}

func (c *Coalescer) key(inc config.Incident) string {
	if len(c.cfg.CorrelationLabels) > 0 {
		parts := make([]string, 0, len(c.cfg.CorrelationLabels))
		for _, l := range c.cfg.CorrelationLabels {
			parts = append(parts, inc.Labels[l])
		}
		return inc.Namespace + "/" + strings.Join(parts, "/")
	}
	if inc.GroupKey != "" {
		return inc.GroupKey
	}
	return inc.Namespace + "/" + inc.AlertName
}

// Summarize renders a one-line digest of a coalesced batch for the seed prompt.
func Summarize(incs []config.Incident) string {
	counts := map[string]int{}
	for _, in := range incs {
		counts[in.AlertName]++
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", n, counts[n]))
	}
	return fmt.Sprintf("%d correlated alerts: %s", len(incs), strings.Join(parts, ", "))
}
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/coalesce/`
Expected: PASS.
```bash
gofmt -w internal/coalesce/*.go && go vet ./internal/coalesce/...
git add internal/coalesce/
git commit -m "feat(coalesce): correlation key + batch summary"
```

### Task 5.3: `Add()` — buffer / cooldown / critical fast-path

**Files:**
- Modify: `internal/coalesce/coalescer.go`
- Test: `internal/coalesce/add_test.go`

- [ ] **Step 1: Write the failing test**

```go
package coalesce

import (
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
)

type sink struct{ batches [][]config.Incident }

func (s *sink) out(b []config.Incident) { s.batches = append(s.batches, b) }

func newAt(cfg Config, s *sink, now *time.Time) *Coalescer {
	c := New(cfg, s.out)
	c.now = func() time.Time { return *now }
	return c
}

func TestAddBuffersUntilMaxBatch(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 2}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK")) // buffered
	if len(s.batches) != 0 {
		t.Fatal("first alert must buffer, not flush")
	}
	c.Add(inc("X", "ns", "warning", "GK")) // hits MaxBatch=2 → flush
	if len(s.batches) != 1 || len(s.batches[0]) != 2 {
		t.Fatalf("MaxBatch should flush 2, got %v", s.batches)
	}
}

func TestAddCooldownSuppresses(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 1, Cooldown: 10 * time.Minute}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK")) // MaxBatch=1 → immediate flush, seeds recent[GK]
	now = now.Add(time.Minute)
	c.Add(inc("X", "ns", "warning", "GK")) // within cooldown → suppressed
	if len(s.batches) != 1 {
		t.Fatalf("second alert should be suppressed, batches=%d", len(s.batches))
	}
}

func TestAddCriticalFastPath(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Hour, MaxBatch: 100}, s, &now)
	c.Add(inc("X", "ns", "critical", "GK")) // critical bypasses debounce → immediate flush
	if len(s.batches) != 1 {
		t.Fatalf("critical should flush immediately, batches=%d", len(s.batches))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/coalesce/ -run TestAdd`
Expected: FAIL — `Add` undefined.

- [ ] **Step 3: Implement `Add` (compute flush under lock, call `out` after unlock)**

```go
// Add ingests one incident: critical → flush now; within cooldown → suppress;
// else buffer (flushing at MaxBatch).
func (c *Coalescer) Add(inc config.Incident) {
	k := c.key(inc)
	var flush []config.Incident
	c.mu.Lock()
	now := c.now()
	switch {
	case strings.EqualFold(inc.Severity, "critical"):
		flush = []config.Incident{inc}
		if b, ok := c.pending[k]; ok {
			flush = append(b.incidents, inc)
			delete(c.pending, k)
		}
		c.recent[k] = now
	case c.withinCooldown(k, now):
		c.suppressed[k]++
		n := c.suppressed[k]
		c.mu.Unlock()
		if c.notify != nil {
			c.notify(k, n)
		}
		return
	default:
		b := c.pending[k]
		if b == nil {
			b = &batch{firstSeen: now}
			c.pending[k] = b
		}
		b.incidents = append(b.incidents, inc)
		b.lastSeen = now
		if c.cfg.MaxBatch > 0 && len(b.incidents) >= c.cfg.MaxBatch {
			flush = b.incidents
			delete(c.pending, k)
			c.recent[k] = now
		}
	}
	c.mu.Unlock()
	if flush != nil {
		c.out(flush)
	}
}

func (c *Coalescer) withinCooldown(k string, now time.Time) bool {
	t, ok := c.recent[k]
	return ok && c.cfg.Cooldown > 0 && now.Sub(t) < c.cfg.Cooldown
}
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/coalesce/`
Expected: PASS.
```bash
gofmt -w internal/coalesce/*.go
git add internal/coalesce/coalescer.go internal/coalesce/add_test.go
git commit -m "feat(coalesce): Add — buffer, cooldown suppression, critical fast-path"
```

### Task 5.4: `sweep()` + `Run()` debounce flush

**Files:**
- Modify: `internal/coalesce/coalescer.go`
- Test: `internal/coalesce/sweep_test.go`

- [ ] **Step 1: Write the failing test**

```go
package coalesce

import (
	"testing"
	"time"
)

func TestSweepFlushesAfterDebounce(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: 30 * time.Second, MaxWait: 2 * time.Minute, MaxBatch: 100}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK"))

	now = now.Add(10 * time.Second)
	c.sweep() // still within debounce → no flush
	if len(s.batches) != 0 {
		t.Fatal("should not flush before debounce elapses")
	}
	now = now.Add(30 * time.Second)
	c.sweep() // quiet for >30s → flush
	if len(s.batches) != 1 {
		t.Fatalf("should flush after debounce, batches=%d", len(s.batches))
	}
}

func TestSweepMaxWaitCap(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxWait: 90 * time.Second, MaxBatch: 100}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK"))
	// keep it "active" so debounce never elapses, but MaxWait should still cap it
	for i := 0; i < 3; i++ {
		now = now.Add(40 * time.Second)
		c.Add(inc("X", "ns", "warning", "GK"))
		c.sweep()
	}
	if len(s.batches) == 0 {
		t.Fatal("MaxWait should force a flush despite continued activity")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/coalesce/ -run TestSweep`
Expected: FAIL — `sweep` undefined.

- [ ] **Step 3: Implement `sweep` + `Run`**

```go
import "context" // add to imports

// sweep flushes every pending batch quiet for >= Debounce or older than MaxWait.
func (c *Coalescer) sweep() {
	var flushes [][]config.Incident
	c.mu.Lock()
	now := c.now()
	for k, b := range c.pending {
		if now.Sub(b.lastSeen) >= c.cfg.Debounce || (c.cfg.MaxWait > 0 && now.Sub(b.firstSeen) >= c.cfg.MaxWait) {
			flushes = append(flushes, b.incidents)
			delete(c.pending, k)
			c.recent[k] = now
		}
	}
	c.mu.Unlock()
	for _, f := range flushes {
		c.out(f)
	}
}

// Run sweeps on a ticker until ctx is cancelled. tick should be ~Debounce/2.
func (c *Coalescer) Run(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweep()
		}
	}
}
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/coalesce/`
Expected: PASS.
```bash
gofmt -w internal/coalesce/*.go
git add internal/coalesce/coalescer.go internal/coalesce/sweep_test.go
git commit -m "feat(coalesce): debounce/max-wait sweeper"
```

### Task 5.5: Wire the Coalescer into the webhook path

**Files:**
- Modify: `internal/server/server.go` (`handleAlertmanager` + `Server` struct + constructor)
- Modify: `cmd/lore/main.go` (build Coalescer, start `Run`)
- Test: `internal/server/coalesce_test.go`

- [ ] **Step 1: Write the failing test**

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	// plus the package's existing test imports for building a Server + fake enqueuer
)

func TestWebhookCoalescesGroup(t *testing.T) {
	var enq int
	// Build a Server whose coalescer folds a single AM group into one Enqueue.
	// Reuse the package's existing Server test harness (grep TestHandleAlertmanager
	// in server_test.go); set s.coalesceAdd to a func recording flushes, OR inject a
	// fake enqueuer counting Enqueue calls and a coalescer with MaxBatch large + Debounce 0.
	srv := newTestServerCoalescing(t, func() { enq++ })

	body := `{"groupKey":"gk","alerts":[
		{"status":"firing","labels":{"alertname":"X","namespace":"ns","severity":"warning"},"fingerprint":"1"},
		{"status":"firing","labels":{"alertname":"X","namespace":"ns","severity":"warning"},"fingerprint":"2"},
		{"status":"firing","labels":{"alertname":"X","namespace":"ns","severity":"warning"},"fingerprint":"3"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body))
	// add the webhook bearer token the harness expects
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	if enq != 1 {
		t.Fatalf("3 correlated alerts should coalesce to 1 enqueue, got %d", enq)
	}
}
```

> Build the test `Server` with coalescing enabled and `Debounce`/`MaxBatch` set so one group flushes synchronously (`MaxBatch: 3`, or call `srv.coalescer.sweep()` after the POST). Mirror the existing `server_test.go` setup for auth + `Server` construction. If the test reveals you need a seam, expose the coalescer on the test build only.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestWebhookCoalesces`
Expected: FAIL — no coalescing; 3 enqueues.

- [ ] **Step 3: Implement the wiring**

Add to `Server`: `coalescer *coalesce.Coalescer` and `metrics *telemetry.Metrics`. Rewrite the `handleAlertmanager` dispatch loop (currently enqueues per incident):
```go
	for _, inc := range incidents {
		d := s.engine.Decide(inc)
		s.log.Info("incident", "alert", inc.AlertName, "severity", inc.Severity,
			"namespace", inc.Namespace, "investigate", d.Investigate, "reason", d.Reason)
		if !d.Investigate {
			continue
		}
		if s.metrics != nil {
			s.metrics.AlertsReceived.Add(r.Context(), 1)
		}
		if s.coalescer != nil {
			s.coalescer.Add(inc)
		} else {
			s.enqueuer.Enqueue(investigate.FromIncident(inc))
		}
	}
```
The Coalescer's `out` (built in main) turns a batch into one Request:
```go
	out := func(incs []config.Incident) {
		rep := investigate.FromIncident(incs[0])
		if len(incs) > 1 {
			rep.Message = coalesce.Summarize(incs)
			metrics.AlertsCoalesced.Add(context.Background(), int64(len(incs)-1))
		}
		metrics.CoalesceBatchSize.Record(context.Background(), int64(len(incs)))
		enqueuer.Enqueue(rep)
	}
```
In `cmd/lore/main.go`, when `cfg.Investigation.Coalesce.Enabled`, build the Coalescer with the std-duration `coalesce.Config` (map each `config.Duration` via `.Std()`), set `c.notify` to nil (metrics provide visibility in v1), pass it into the `Server`, and start the sweeper:
```go
	if cfg.Investigation.Coalesce.Enabled {
		cz := coalesce.New(coalesce.Config{
			Enabled:           true,
			Debounce:          cfg.Investigation.Coalesce.Debounce.Std(),
			MaxWait:           cfg.Investigation.Coalesce.MaxWait.Std(),
			MaxBatch:          cfg.Investigation.Coalesce.MaxBatch,
			Cooldown:          cfg.Investigation.Coalesce.Cooldown.Std(),
			CorrelationLabels: cfg.Investigation.Coalesce.CorrelationLabels,
		}, out)
		go cz.Run(ctx, cfg.Investigation.Coalesce.Debounce.Std()/2)
		// pass cz into the Server
	}
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/server/ ./internal/coalesce/ && go build ./...`
Expected: PASS.
```bash
gofmt -w internal/server/server.go cmd/lore/main.go
git add internal/server/server.go internal/server/coalesce_test.go cmd/lore/main.go
git commit -m "feat(server): coalesce correlated alerts into one investigation"
```

---

## Part 6 — Deploy + integration

### Task 6.1: Helm values + `/metrics` + VMServiceScrape

**Files:**
- Modify: `deploy/helm/runlore/values.yaml`
- Modify: `deploy/helm/runlore/templates/` (the config Secret/ConfigMap that renders `config.yaml`; the Service must expose the metrics port)
- Create: `deploy/helm/runlore/templates/vmservicescrape.yaml`

- [ ] **Step 1: Add values**

In `deploy/helm/runlore/values.yaml`, extend the config block with the new sections (match the existing dedup-window block's nesting):
```yaml
investigation:
  coalesce:
    enabled: true
    debounce: 30s
    max_wait: 2m
    max_batch: 50
    cooldown: 10m
    correlation_labels: []
  rate_limit:
    max_per_window: 20
    window: 1h
    max_requeues: 10
  max_steps: 20
  max_tool_output_bytes: 16384
  max_tokens_per_investigation: 120000
telemetry:
  metrics_enabled: true

metrics:
  serviceMonitor:
    enabled: true   # render a VMServiceScrape for /metrics
```

- [ ] **Step 2: Ensure the config template renders the new keys**

Confirm the template that builds `config.yaml` passes `.Values.investigation` and `.Values.telemetry` through (it likely renders `.Values` wholesale — verify with `helm template`). Ensure the Service exposes the HTTP port that serves `/metrics` (same port as the webhook listener).

- [ ] **Step 3: Add the VMServiceScrape**

`deploy/helm/runlore/templates/vmservicescrape.yaml`:
```yaml
{{- if .Values.metrics.serviceMonitor.enabled }}
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMServiceScrape
metadata:
  name: {{ include "runlore.fullname" . }}
  labels: {{- include "runlore.labels" . | nindent 4 }}
spec:
  selector:
    matchLabels: {{- include "runlore.selectorLabels" . | nindent 6 }}
  endpoints:
    - port: http
      path: /metrics
{{- end }}
```
(Match the chart's actual helper names + the Service port name — grep the existing `service.yaml`/`_helpers.tpl`.)

- [ ] **Step 4: Render-check + commit**

Run: `helm template deploy/helm/runlore | grep -A3 VMServiceScrape && helm lint deploy/helm/runlore`
Expected: VMServiceScrape rendered; lint clean.
```bash
git add deploy/helm/runlore/
git commit -m "feat(helm): coalesce/rate-limit/token/telemetry values + VMServiceScrape"
```

### Task 6.2: Storm integration smoke (mock model)

**Files:**
- Modify: `hack/e2e-k3d.sh` (or add `hack/e2e/storm_test.sh`)

- [ ] **Step 1: Add a storm assertion**

Extend the e2e harness: after RunLore is up with the mock model, POST a single Alertmanager webhook carrying 40 firing alerts in one group, then assert exactly one investigation ran (mock model call count == 1 batch, or `runlore_investigations_started_total == 1` and `runlore_alerts_coalesced_total == 39` scraped from `/metrics`).

```bash
# 40 alerts, one group, same alertname/namespace
alerts=$(python3 -c 'import json;print(json.dumps({"groupKey":"gk","alerts":[{"status":"firing","labels":{"alertname":"KubePodCrashLooping","namespace":"apps","severity":"warning"},"fingerprint":str(i)} for i in range(40)]}))')
curl -fsS -H "Authorization: Bearer $WEBHOOK_TOKEN" -d "$alerts" "$RUNLORE_URL/webhook/alertmanager"
sleep 2
started=$(curl -fsS "$RUNLORE_URL/metrics" | awk '/^runlore_investigations_started_total/{print $2}')
[ "$started" = "1" ] || { echo "FAIL: expected 1 investigation, got $started"; exit 1; }
```

- [ ] **Step 2: Run (if a k3d/docker env is available) + commit**

Run: `bash hack/e2e-k3d.sh` (skip if no docker; CI runs it)
```bash
git add hack/
git commit -m "test(e2e): assert a 40-alert storm coalesces to one investigation"
```

---

## Final verification

- [ ] `gofmt -l ./internal/... ./cmd/...` → no output
- [ ] `go vet ./...` → clean
- [ ] `go build ./...` → clean
- [ ] `go test ./...` → all green
- [ ] `golangci-lint run` (if configured) → clean
- [ ] `helm lint deploy/helm/runlore` → clean

---

## Optional follow-up (not blocking)

### Task O.1: Refactor `action/auto.go` onto `ratelimit.Window`

Replace `Auto.recent []time.Time` + `reserve()` with a `*ratelimit.Window`, removing the duplicated sliding-window logic. **Sequence last**, and re-run `go test ./internal/action/...` (especially `TestAutoRateLimit`) to prove no behavior change. Keep `Auto.now` injection by adding a `WithClock` setter to `ratelimit.Window` if the action tests need it. Commit: `refactor(action): reuse ratelimit.Window in auto limiter`.
