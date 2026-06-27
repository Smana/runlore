# RunLore Phase 1 — React Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `lore serve` — an HTTP server that receives Alertmanager/VMAlert webhooks and decides, per the configured **trigger policy** (environment / severity / namespace / label filters + dedup), which incidents would start an investigation.

**Architecture:** A small, dependency-light vertical slice with three packages: `config` (typed config + YAML loading + policy matching), `trigger` (webhook parsing + dedup + the decision engine), and `server` (HTTP handlers). Everything is pure/deterministic and unit-tested — no Kubernetes, no LLM, no network calls. This is the entry point of the React pillar; later plans hang the investigation loop off the `Investigate` decision.

**Tech Stack:** Go 1.26, stdlib (`net/http` method routing, `log/slog`), `gopkg.in/yaml.v3` for config. Existing files: `internal/config/config.go`, `cmd/lore/main.go`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` *(modify)* | Config types; implement policy matching; add a YAML `Duration` type; add `Incident.Fingerprint` |
| `internal/config/config_test.go` *(create)* | Matching + Duration unit tests |
| `internal/config/load.go` *(create)* | `Load(path)` — read+parse YAML config |
| `internal/config/load_test.go` *(create)* | Round-trip config parsing test |
| `internal/trigger/incident.go` *(create)* | Parse Alertmanager webhook payload → `[]config.Incident` |
| `internal/trigger/incident_test.go` *(create)* | Webhook parsing test |
| `internal/trigger/dedup.go` *(create)* | `Deduper` — suppress still-firing repeats (injectable clock) |
| `internal/trigger/dedup_test.go` *(create)* | Dedup window tests |
| `internal/trigger/engine.go` *(create)* | `Engine.Decide(incident) → Decision` (match + dedup) |
| `internal/trigger/engine_test.go` *(create)* | Decision tests |
| `internal/server/server.go` *(create)* | HTTP handlers: `/webhook/alertmanager`, `/healthz` |
| `internal/server/server_test.go` *(create)* | Handler tests via `httptest` |
| `cmd/lore/main.go` *(modify)* | Wire `lore serve` to load config + run the server |

---

## Task 1: Config matching + Duration type

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Add the yaml dependency**

Run: `go get gopkg.in/yaml.v3@latest`
Expected: `go.mod` gains a `require gopkg.in/yaml.v3` line.

- [ ] **Step 2: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"testing"
	"time"
)

func sampleIncident() Incident {
	return Incident{
		AlertName:   "HarborProbeFailure",
		Severity:    "critical",
		Environment: "prod",
		Namespace:   "apps",
		Labels:      map[string]string{"team": "platform", "severity": "critical"},
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		name string
		tr   IncidentTrigger
		inc  Incident
		want bool
	}{
		{"disabled never matches", IncidentTrigger{Enabled: false}, sampleIncident(), false},
		{"empty match matches anything", IncidentTrigger{Enabled: true}, sampleIncident(), true},
		{"severity+env match", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Severity: []string{"critical"}, Environment: []string{"prod"}}}, sampleIncident(), true},
		{"severity mismatch", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Severity: []string{"warning"}}}, sampleIncident(), false},
		{"namespace glob", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Namespaces: []string{"app*"}}}, sampleIncident(), true},
		{"namespace glob miss", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Namespaces: []string{"payments"}}}, sampleIncident(), false},
		{"label subset match", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Labels: map[string]string{"team": "platform"}}}, sampleIncident(), true},
		{"label mismatch", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Labels: map[string]string{"team": "data"}}}, sampleIncident(), false},
		{"ignore excludes", IncidentTrigger{Enabled: true, Ignore: IncidentMatch{
			AlertNames: []string{"Watchdog", "HarborProbeFailure"}}}, sampleIncident(), false},
	}
	for _, c := range cases {
		if got := c.tr.Matches(c.inc); got != c.want {
			t.Errorf("%s: Matches=%v want %v", c.name, got, c.want)
		}
	}
}

func TestDurationUnmarshal(t *testing.T) {
	var d Duration
	if err := d.UnmarshalYAML(yamlScalar("30m")); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Std() != 30*time.Minute {
		t.Fatalf("got %v want 30m", d.Std())
	}
}
```

- [ ] **Step 3: Add the test helper for Duration**

Append to `internal/config/config_test.go`:

```go
import "gopkg.in/yaml.v3"

func yamlScalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: s}
}
```

(Merge the import into the existing import block rather than duplicating it.)

- [ ] **Step 4: Run the test to verify it fails**

Run: `go test ./internal/config/ -run 'TestMatches|TestDuration' -v`
Expected: FAIL — `Matches` returns only `t.Enabled` (current stub), and `Duration`/`UnmarshalYAML` undefined.

- [ ] **Step 5: Implement matching + the Duration type**

In `internal/config/config.go`, change the import block to:

```go
import (
	"path"
	"time"

	"gopkg.in/yaml.v3"
)
```

Change the `Dedup` struct's field type from `time.Duration` to `Duration`:

```go
// Dedup suppresses re-investigation of a still-firing alert within Window.
type Dedup struct {
	Window Duration `yaml:"window"`
}
```

Replace the stub `Matches` method with the real implementation, and add the helpers + `Duration` type:

```go
// Matches reports whether an incident passes this trigger policy: enabled,
// matched by Match, and not excluded by a non-empty Ignore.
func (t IncidentTrigger) Matches(inc Incident) bool {
	if !t.Enabled {
		return false
	}
	if !t.Match.matches(inc) {
		return false
	}
	if !t.Ignore.isEmpty() && t.Ignore.matches(inc) {
		return false
	}
	return true
}

// matches reports whether the incident satisfies every non-empty criterion.
func (m IncidentMatch) matches(inc Incident) bool {
	if len(m.Severity) > 0 && !contains(m.Severity, inc.Severity) {
		return false
	}
	if len(m.Environment) > 0 && !contains(m.Environment, inc.Environment) {
		return false
	}
	if len(m.Namespaces) > 0 && !globAny(m.Namespaces, inc.Namespace) {
		return false
	}
	if len(m.AlertNames) > 0 && !globAny(m.AlertNames, inc.AlertName) {
		return false
	}
	for k, v := range m.Labels {
		if inc.Labels[k] != v {
			return false
		}
	}
	return true
}

// isEmpty reports whether no criteria are set.
func (m IncidentMatch) isEmpty() bool {
	return len(m.Severity) == 0 && len(m.Environment) == 0 &&
		len(m.Namespaces) == 0 && len(m.AlertNames) == 0 && len(m.Labels) == 0
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func globAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}

// Duration is a time.Duration that unmarshals from a Go duration string ("30m").
type Duration time.Duration

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a duration string such as "30m" or "1h30m".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}
```

Delete the old stub `Matches` (the one with the `TODO(phase1)` comment) so there is exactly one `Matches` method.

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/config/ -run 'TestMatches|TestDuration' -v`
Expected: PASS (all cases).

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go go.mod go.sum
git commit -m "feat(config): implement trigger-policy matching + YAML Duration type"
```

---

## Task 2: Parse Alertmanager webhooks into incidents

**Files:**
- Modify: `internal/config/config.go` (add `Incident.Fingerprint`)
- Create: `internal/trigger/incident.go`
- Test: `internal/trigger/incident_test.go`

- [ ] **Step 1: Add the Fingerprint field**

In `internal/config/config.go`, add `Fingerprint` to `Incident`:

```go
// Incident is the normalized trigger input (from Alertmanager/VMAlert).
type Incident struct {
	AlertName   string
	Severity    string
	Environment string
	Namespace   string
	Labels      map[string]string
	StartsAt    time.Time
	Fingerprint string // stable alert identity, used for dedup
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/trigger/incident_test.go`:

```go
package trigger

import (
	"strings"
	"testing"
)

func TestParseAlertmanager(t *testing.T) {
	body := `{"alerts":[
	  {"status":"firing","labels":{"alertname":"HarborProbeFailure","severity":"critical","environment":"prod","namespace":"apps","team":"platform"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"abc123"},
	  {"status":"resolved","labels":{"alertname":"Old"},"startsAt":"2026-06-20T01:00:00Z","fingerprint":"def456"}
	]}`
	incs, err := ParseAlertmanager(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(incs) != 1 {
		t.Fatalf("want 1 firing incident, got %d", len(incs))
	}
	got := incs[0]
	if got.AlertName != "HarborProbeFailure" || got.Severity != "critical" ||
		got.Environment != "prod" || got.Namespace != "apps" || got.Fingerprint != "abc123" {
		t.Fatalf("unexpected incident: %+v", got)
	}
	if got.Labels["team"] != "platform" {
		t.Fatal("labels should be preserved")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/trigger/ -run TestParseAlertmanager -v`
Expected: FAIL — package `trigger` / `ParseAlertmanager` does not exist.

- [ ] **Step 4: Implement the parser**

Create `internal/trigger/incident.go`:

```go
// Package trigger ingests incidents (Alertmanager/VMAlert webhooks) and decides,
// per the configured policy, which ones start an investigation.
package trigger

import (
	"encoding/json"
	"io"
	"time"

	"github.com/Smana/runlore/internal/config"
)

// amPayload is the subset of the Alertmanager webhook payload we consume.
type amPayload struct {
	Alerts []amAlert `json:"alerts"`
}

type amAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	StartsAt    string            `json:"startsAt"`
	Fingerprint string            `json:"fingerprint"`
}

// ParseAlertmanager reads an Alertmanager webhook body into incidents. Only
// firing alerts are returned. "environment" is taken from the label of the same
// name, falling back to "env".
func ParseAlertmanager(r io.Reader) ([]config.Incident, error) {
	var p amPayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, err
	}
	out := make([]config.Incident, 0, len(p.Alerts))
	for _, a := range p.Alerts {
		if a.Status != "" && a.Status != "firing" {
			continue
		}
		startsAt, _ := time.Parse(time.RFC3339, a.StartsAt)
		out = append(out, config.Incident{
			AlertName:   a.Labels["alertname"],
			Severity:    a.Labels["severity"],
			Environment: firstNonEmpty(a.Labels["environment"], a.Labels["env"]),
			Namespace:   a.Labels["namespace"],
			Labels:      a.Labels,
			StartsAt:    startsAt,
			Fingerprint: a.Fingerprint,
		})
	}
	return out, nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/trigger/ -run TestParseAlertmanager -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/trigger/incident.go internal/trigger/incident_test.go
git commit -m "feat(trigger): parse Alertmanager webhooks into incidents"
```

---

## Task 3: Dedup still-firing alerts

**Files:**
- Create: `internal/trigger/dedup.go`
- Test: `internal/trigger/dedup_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/trigger/dedup_test.go`:

```go
package trigger

import (
	"testing"
	"time"
)

func TestDeduper(t *testing.T) {
	d := NewDeduper(30 * time.Minute)
	base := time.Date(2026, 6, 20, 3, 0, 0, 0, time.UTC)
	cur := base
	d.now = func() time.Time { return cur }

	if d.Seen("abc") {
		t.Fatal("first sighting should not be deduped")
	}
	cur = base.Add(10 * time.Minute)
	if !d.Seen("abc") {
		t.Fatal("within window should be deduped")
	}
	cur = base.Add(31 * time.Minute)
	if d.Seen("abc") {
		t.Fatal("after window should not be deduped")
	}
}

func TestDeduperDisabled(t *testing.T) {
	d := NewDeduper(0)
	if d.Seen("abc") || d.Seen("abc") {
		t.Fatal("zero window disables dedup")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/trigger/ -run TestDeduper -v`
Expected: FAIL — `NewDeduper` undefined.

- [ ] **Step 3: Implement the Deduper**

Create `internal/trigger/dedup.go`:

```go
package trigger

import (
	"sync"
	"time"
)

// Deduper suppresses repeated investigations of the same still-firing alert
// within a time window. Safe for concurrent use. The clock is injectable for tests.
type Deduper struct {
	window time.Duration
	now    func() time.Time
	mu     sync.Mutex
	seen   map[string]time.Time
}

// NewDeduper returns a Deduper with the given window. A zero window disables dedup.
func NewDeduper(window time.Duration) *Deduper {
	return &Deduper{window: window, now: time.Now, seen: map[string]time.Time{}}
}

// Seen records the key and reports whether it was already seen within the window.
func (d *Deduper) Seen(key string) bool {
	if d.window <= 0 || key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if last, ok := d.seen[key]; ok && now.Sub(last) < d.window {
		return true
	}
	d.seen[key] = now
	return false
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/trigger/ -run TestDeduper -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/trigger/dedup.go internal/trigger/dedup_test.go
git commit -m "feat(trigger): dedup still-firing alerts within a window"
```

---

## Task 4: The decision engine

**Files:**
- Create: `internal/trigger/engine.go`
- Test: `internal/trigger/engine_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/trigger/engine_test.go`:

```go
package trigger

import (
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
)

func TestEngineDecide(t *testing.T) {
	p := config.IncidentTrigger{
		Enabled: true,
		Match:   config.IncidentMatch{Severity: []string{"critical"}, Environment: []string{"prod"}},
		Dedup:   config.Dedup{Window: config.Duration(30 * time.Minute)},
	}
	e := NewEngine(p)

	crit := config.Incident{AlertName: "A", Severity: "critical", Environment: "prod", Fingerprint: "fp1"}
	if d := e.Decide(crit); !d.Investigate {
		t.Fatalf("critical/prod should investigate, got %q", d.Reason)
	}
	if d := e.Decide(crit); d.Investigate {
		t.Fatal("repeat within window should be deduped")
	}
	warn := config.Incident{AlertName: "B", Severity: "warning", Environment: "prod", Fingerprint: "fp2"}
	if d := e.Decide(warn); d.Investigate {
		t.Fatal("warning should be filtered by policy")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/trigger/ -run TestEngineDecide -v`
Expected: FAIL — `NewEngine` / `Decide` / `Decision` undefined.

- [ ] **Step 3: Implement the engine**

Create `internal/trigger/engine.go`:

```go
package trigger

import "github.com/Smana/runlore/internal/config"

// Decision is the outcome of evaluating an incident against the trigger policy.
type Decision struct {
	Investigate bool
	Reason      string
}

// Engine evaluates incidents against a policy, with dedup.
type Engine struct {
	policy config.IncidentTrigger
	dedup  *Deduper
}

// NewEngine builds an Engine from the incident trigger policy.
func NewEngine(p config.IncidentTrigger) *Engine {
	return &Engine{policy: p, dedup: NewDeduper(p.Dedup.Window.Std())}
}

// Decide returns whether the incident should start an investigation.
func (e *Engine) Decide(inc config.Incident) Decision {
	if !e.policy.Matches(inc) {
		return Decision{false, "filtered by trigger policy"}
	}
	if e.dedup.Seen(dedupKey(inc)) {
		return Decision{false, "deduplicated (still-firing)"}
	}
	return Decision{true, "matched trigger policy"}
}

func dedupKey(inc config.Incident) string {
	if inc.Fingerprint != "" {
		return inc.Fingerprint
	}
	return inc.AlertName + "/" + inc.Namespace
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/trigger/ -run TestEngineDecide -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/trigger/engine.go internal/trigger/engine_test.go
git commit -m "feat(trigger): decision engine combining policy match + dedup"
```

---

## Task 5: Load config from YAML

**Files:**
- Create: `internal/config/load.go`
- Test: `internal/config/load_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/config/load_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	doc := `
triggers:
  incidents:
    enabled: true
    match:
      severity: [critical]
      environment: [prod]
      namespaces: ["apps*"]
      labels: { team: platform }
    ignore:
      alertnames: [Watchdog]
    dedup: { window: 30m }
  gitops_failures: { enabled: true }
actions:
  mode: off
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.Triggers.Incidents.Enabled {
		t.Fatal("incidents should be enabled")
	}
	if c.Triggers.Incidents.Dedup.Window.Std() != 30*time.Minute {
		t.Fatalf("window: got %v", c.Triggers.Incidents.Dedup.Window.Std())
	}
	if c.Actions.Enabled() {
		t.Fatal("actions mode off should be disabled")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run TestLoad -v`
Expected: FAIL — `Load` undefined.

- [ ] **Step 3: Implement Load**

Create `internal/config/load.go`:

```go
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads and parses a RunLore config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &c, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/config/ -run TestLoad -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/load.go internal/config/load_test.go
git commit -m "feat(config): load configuration from YAML"
```

---

## Task 6: `lore serve` webhook server

**Files:**
- Create: `internal/server/server.go`
- Test: `internal/server/server_test.go`
- Modify: `cmd/lore/main.go`

- [ ] **Step 1: Write the failing test**

Create `internal/server/server_test.go`:

```go
package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

func testServer() *Server {
	cfg := &config.Config{}
	cfg.Triggers.Incidents = config.IncidentTrigger{
		Enabled: true,
		Match:   config.IncidentMatch{Severity: []string{"critical"}},
	}
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHandleAlertmanager(t *testing.T) {
	body := `{"alerts":[{"status":"firing","labels":{"alertname":"A","severity":"critical","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body))
	rr := httptest.NewRecorder()
	testServer().Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rr.Code)
	}
}

func TestHandleAlertmanagerBadBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader("not json"))
	rr := httptest.NewRecorder()
	testServer().Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	testServer().Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/server/ -v`
Expected: FAIL — package `server` does not exist.

- [ ] **Step 3: Implement the server**

Create `internal/server/server.go`:

```go
// Package server exposes RunLore's HTTP endpoints (incident webhooks).
package server

import (
	"log/slog"
	"net/http"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/trigger"
)

// Server handles incoming incident webhooks and applies the trigger policy.
type Server struct {
	engine *trigger.Engine
	log    *slog.Logger
}

// New builds a Server from config.
func New(cfg *config.Config, log *slog.Logger) *Server {
	return &Server{engine: trigger.NewEngine(cfg.Triggers.Incidents), log: log}
}

// Handler returns the HTTP mux (Go 1.22+ method routing).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *Server) handleAlertmanager(w http.ResponseWriter, r *http.Request) {
	incidents, err := trigger.ParseAlertmanager(r.Body)
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	for _, inc := range incidents {
		d := s.engine.Decide(inc)
		s.log.Info("incident",
			"alert", inc.AlertName, "severity", inc.Severity, "namespace", inc.Namespace,
			"investigate", d.Investigate, "reason", d.Reason)
		// Phase 1+ (later plan): if d.Investigate, enqueue an investigation here.
	}
	w.WriteHeader(http.StatusAccepted)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/server/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Wire `lore serve` in main.go**

Replace the entire contents of `cmd/lore/main.go` with:

```go
// Command lore is the RunLore CLI and in-cluster agent entrypoint.
//
// RunLore is a self-improving, GitOps-native SRE agent: it reacts to incidents,
// investigates by correlating "what changed" across the GitOps engine and the
// observability stack, and learns into an open knowledge catalog.
//
// See docs/design.md.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/server"
)

var version = "0.0.0-dev"

const usage = `lore — the RunLore SRE agent

Usage:
  lore investigate [--alert <name>] [--since <dur>]   investigate an alert/symptom (on-demand)
  lore serve [--config <path>] [--addr <addr>]        run the in-cluster agent (react to incidents)
  lore catalog sync                                   sync + index the knowledge catalog
  lore eval                                           replay past incidents, score root-cause identification
  lore version                                        print version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Printf("lore %s\n", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
	case "investigate", "catalog", "eval":
		fmt.Printf("lore %s: not yet implemented (scaffold). See docs/design.md\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	addr := fs.String("addr", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	srv := server.New(cfg, log)
	log.Info("runlore serving", "addr", *addr)
	return http.ListenAndServe(*addr, srv.Handler())
}
```

- [ ] **Step 6: Verify the whole module builds, vets, and tests pass**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./...`
Expected: build OK; vet OK; `gofmt -l` prints nothing; all tests PASS.

- [ ] **Step 7: Manual smoke test**

Run:
```bash
cat > /tmp/runlore.yaml <<'EOF'
triggers:
  incidents:
    enabled: true
    match:
      severity: [critical]
      environment: [prod]
    dedup: { window: 30m }
EOF
go run ./cmd/lore serve --config /tmp/runlore.yaml --addr :8080 &
sleep 1
curl -s -XPOST localhost:8080/webhook/alertmanager -d '{"alerts":[
  {"status":"firing","labels":{"alertname":"HarborProbeFailure","severity":"critical","environment":"prod","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp1"},
  {"status":"firing","labels":{"alertname":"Noisy","severity":"warning","environment":"prod","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp2"}]}'
kill %1
```
Expected log lines: `HarborProbeFailure … investigate=true reason="matched trigger policy"` and `Noisy … investigate=false reason="filtered by trigger policy"`.

- [ ] **Step 8: Commit**

```bash
git add internal/server/ cmd/lore/main.go
git commit -m "feat(serve): lore serve — Alertmanager webhook -> trigger-policy decision"
```

---

## What this plan delivers

A runnable `lore serve` that ingests Alertmanager/VMAlert webhooks and logs, per the configured policy, which incidents *would* start an investigation (with dedup) — the React pillar's foundation, fully unit-tested with zero external dependencies.

## Subsequent Phase-1 plans (not in this plan)

1. **What-changed spine** — `internal/whatchanged` + `providers/gitops/flux`: `client-go` revision history + `go-git` diff between revisions → `[]providers.Change`. (Deterministic, fixture-tested.)
2. **Correlation** — `providers/metrics` (PromQL: VM/Prom) + `providers/logs/victorialogs` + `providers/network/hubble`.
3. **Catalog read** — `internal/catalog`: syncer + `bleve` index + `kb_search` (instant recall over the OKF bundle).
4. **Investigation loop** — `internal/investigate`: the ReAct loop wiring model + providers + what-changed + catalog → `providers.Investigation`.
5. **Delivery** — `internal/notify/{slack,matrix}` rendering the investigation.
6. **Eval skeleton** — `lore eval`: replay a recorded incident, assert end-state root cause.

These each become their own plan, hung off the `Investigate` decision produced here.

---

## Self-Review

- **Spec coverage:** Implements the Phase-1 React row of the roadmap — incident-triggered + trigger policy (env/severity/namespace/label filters + dedup). The other Phase-1 rows (Investigate spine, Learn read) are explicitly deferred to named follow-up plans above. ✅
- **Placeholder scan:** No TBD/TODO-as-implementation; every code step is complete and compilable. The one inline `// Phase 1+ (later plan)` comment marks a deliberate seam, not a missing step. ✅
- **Type consistency:** `config.Duration` (with `.Std()`) used consistently in `Dedup.Window` (Task 1), the engine (`NewDeduper(p.Dedup.Window.Std())`, Task 4), and the load test (Task 5). `config.Incident.Fingerprint` defined in Task 2 and consumed by `dedupKey` (Task 4) and parsing (Task 2). `trigger.Engine`/`Decision`/`Decide` consistent across Tasks 4 and 6. ✅
