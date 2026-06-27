# Outcome Capture (Learning-Loop A1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record whether an investigated incident actually resolved, attributed to the answer used (recall vs fresh), via the Alertmanager resolved-webhook signal and a persistent append-only ledger — making learning measurable.

**Architecture:** A new `internal/outcome` package owns a JSONL event ledger (append-only file + an in-memory open-index replayed on startup). `ParseAlertmanager` is extended to surface `resolved` alerts; `handleAlertmanager` routes them to `ledger.Resolve`. At `OnComplete`, an `open` event is recorded with the AM fingerprint + the recalled-entry path. Four OTel counters/histograms make it observable.

**Tech Stack:** Go, `encoding/json` + `bufio` (JSONL), OpenTelemetry metrics, plain `testing`. Module `github.com/Smana/runlore`.

**Design spec:** `docs/superpowers/specs/2026-06-23-outcome-capture-design.md`

## Global Constraints

- TDD red→green per task; plain `testing`/`t.Fatalf` (no testify).
- Before each commit: `cd /home/smana/Sources/runlore && gofmt -w <files> && go vet ./... && go build ./... && golangci-lint run && <pkg tests>` — golangci-lint must report **0 issues** (it runs gocritic/revive/staticcheck; avoid unused params — drop them or name `_`).
- Conventional commits; **NO `Co-Authored-By` / "Generated with" / attribution trailers.**
- Base branch `feat/outcome-capture` (off main, which has #67 + #68).

**Parallelizable:** Tasks 1, 2, 3 touch disjoint files and have no inter-dependencies — build them concurrently. Tasks 4 → 5 are sequential integration on top.

---

## Task 1: The outcome ledger package

**Files:**
- Create: `internal/outcome/ledger.go`
- Test: `internal/outcome/ledger_test.go`

**Interfaces:**
- Produces: `outcome.Event{Event,Fingerprint,Kind,Entry,Title,Resource string; At time.Time}`; `outcome.Episode{Kind,Entry,Title,Resource string; OpenedAt,ResolvedAt time.Time; Duration time.Duration}`; `outcome.New(path string) (*Ledger, error)`; `(*Ledger).Open(e Event) error`; `(*Ledger).Resolve(fp string, at time.Time) (Episode, bool, error)`. Empty `path` ⇒ no-op (feature off). Nil-receiver-safe.

- [ ] **Step 1: Write the failing test**

```go
package outcome

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLedgerOpenResolveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t0 := time.Unix(1000, 0)
	if err := l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "harbor.md", At: t0}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ep, ok, err := l.Resolve("fp1", t0.Add(90*time.Second))
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if ep.Kind != "recall" || ep.Entry != "harbor.md" || ep.Duration != 90*time.Second {
		t.Fatalf("episode: %+v", ep)
	}
}

func TestLedgerResolveWithoutOpen(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	if _, ok, err := l.Resolve("never-fired", time.Unix(1, 0)); ok || err != nil {
		t.Fatalf("resolve with no open should be ok=false, got ok=%v err=%v", ok, err)
	}
}

func TestLedgerReplayRebuildsOpenIndex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(2000, 0)
	_ = l.Open(Event{Fingerprint: "fpA", Kind: "fresh", At: t0})
	// New ledger over the same file replays the open event.
	l2, err := New(p)
	if err != nil {
		t.Fatalf("replay New: %v", err)
	}
	if _, ok, _ := l2.Resolve("fpA", t0.Add(time.Minute)); !ok {
		t.Fatal("replay must rebuild the open-index so fpA resolves")
	}
}

func TestLedgerDisabledWhenPathEmpty(t *testing.T) {
	l, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if err := l.Open(Event{Fingerprint: "x"}); err != nil {
		t.Fatalf("Open on disabled ledger must be a no-op, got %v", err)
	}
	if _, ok, _ := l.Resolve("x", time.Now()); ok {
		t.Fatal("disabled ledger Resolve must be ok=false")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`go test ./internal/outcome/`) — package/`New` undefined.

- [ ] **Step 3: Implement** `internal/outcome/ledger.go`

```go
// Package outcome records, in an append-only JSONL ledger, whether an
// investigated incident later resolved and which answer was used for it — the
// "did it actually work?" signal the learning loop reads. The ledger keeps an
// in-memory index of still-open incidents, rebuilt by replaying the file on
// startup so a resolve survives a restart / leader failover.
package outcome

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Event is one ledger line: an investigation opened, or an incident resolved.
type Event struct {
	Event       string    `json:"event"`              // "open" | "resolve"
	Fingerprint string    `json:"fingerprint"`        // Alertmanager fingerprint (stable firing↔resolved)
	Kind        string    `json:"kind,omitempty"`     // open: "recall" | "fresh"
	Entry       string    `json:"entry,omitempty"`    // open+recall: the recalled entry path
	Title       string    `json:"title,omitempty"`
	Resource    string    `json:"resource,omitempty"`
	At          time.Time `json:"at"`
}

// Episode is a matched open→resolve pair.
type Episode struct {
	Kind, Entry, Title, Resource string
	OpenedAt, ResolvedAt         time.Time
	Duration                     time.Duration
}

// Ledger is an append-only outcome log with an in-memory open-index.
type Ledger struct {
	path string
	mu   sync.Mutex
	open map[string]Event // fingerprint → latest unresolved open
}

// New opens (replaying) the ledger at path. An empty path returns a disabled,
// no-op ledger (the feature is off).
func New(path string) (*Ledger, error) {
	l := &Ledger{path: path, open: map[string]Event{}}
	if path == "" {
		return l, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // skip a corrupt line rather than fail startup
		}
		switch e.Event {
		case "open":
			l.open[e.Fingerprint] = e
		case "resolve":
			delete(l.open, e.Fingerprint)
		}
	}
	return l, sc.Err()
}

func (l *Ledger) enabled() bool { return l != nil && l.path != "" }

func (l *Ledger) appendLocked(e Event) error {
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Open records that an investigation completed for an incident (fingerprint).
func (l *Ledger) Open(e Event) error {
	if !l.enabled() {
		return nil
	}
	e.Event = "open"
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.appendLocked(e); err != nil {
		return err
	}
	l.open[e.Fingerprint] = e
	return nil
}

// Resolve records that an incident's alert cleared. When it matches an open
// investigation it returns the Episode (with duration + kind) and ok=true.
func (l *Ledger) Resolve(fp string, at time.Time) (Episode, bool, error) {
	if !l.enabled() {
		return Episode{}, false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.appendLocked(Event{Event: "resolve", Fingerprint: fp, At: at}); err != nil {
		return Episode{}, false, err
	}
	o, ok := l.open[fp]
	if !ok {
		return Episode{}, false, nil
	}
	delete(l.open, fp)
	return Episode{
		Kind: o.Kind, Entry: o.Entry, Title: o.Title, Resource: o.Resource,
		OpenedAt: o.At, ResolvedAt: at, Duration: at.Sub(o.At),
	}, true, nil
}
```

- [ ] **Step 4: Run — expect PASS** (`go test ./internal/outcome/`).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/outcome/*.go && go vet ./internal/outcome/... && golangci-lint run ./internal/outcome/...
git add internal/outcome/
git commit -m "feat(outcome): append-only JSONL outcome ledger with replayed open-index"
```

---

## Task 2: Outcome metrics

**Files:**
- Modify: `internal/telemetry/metrics.go`
- Test: `internal/telemetry/metrics_test.go`

**Interfaces:**
- Produces: `Metrics.OutcomesOpened metric.Int64Counter`, `Metrics.IncidentsResolved metric.Int64Counter`, `Metrics.RecallOutcome metric.Int64Counter`, `Metrics.IncidentResolutionSeconds metric.Float64Histogram`.

- [ ] **Step 1: Write the failing test** (extend the existing nil-safe test, or add)

```go
func TestNewMetricsOutcomeInstruments(t *testing.T) {
	m := NewMetrics()
	ctx := context.Background()
	m.OutcomesOpened.Add(ctx, 1)
	m.IncidentsResolved.Add(ctx, 1)
	m.RecallOutcome.Add(ctx, 1)
	m.IncidentResolutionSeconds.Record(ctx, 90)
}
```

- [ ] **Step 2: Run — expect FAIL** (fields undefined). Run: `go test ./internal/telemetry/ -run Outcome`

- [ ] **Step 3: Implement** — add the four fields to `Metrics` (after `RecallScore`):

```go
	OutcomesOpened            metric.Int64Counter     // investigations recorded as open (label: kind)
	IncidentsResolved         metric.Int64Counter     // resolve events that matched an open investigation
	RecallOutcome             metric.Int64Counter     // resolved incidents whose open was a recall (label: result)
	IncidentResolutionSeconds metric.Float64Histogram // open→resolve duration, seconds
```

and to the returned struct literal in `NewMetrics` (after the `RecallScore` line):

```go
		OutcomesOpened:            ctr("outcomes_opened_total", "investigations recorded in the outcome ledger (label: kind)"),
		IncidentsResolved:         ctr("incidents_resolved_total", "resolve events that matched an open investigation"),
		RecallOutcome:             ctr("recall_outcome_total", "resolved incidents whose answer was a recall (label: result)"),
		IncidentResolutionSeconds: histF("incident_resolution_seconds", "open→resolve duration in seconds"),
```

- [ ] **Step 4: Run — expect PASS**; **Step 5: Commit**

```bash
gofmt -w internal/telemetry/*.go && golangci-lint run ./internal/telemetry/...
git add internal/telemetry/
git commit -m "feat(telemetry): outcome metrics (opened/resolved/recall-outcome/resolution-seconds)"
```

---

## Task 3: Thread fingerprint, status, and recalled-entry

**Files:**
- Modify: `internal/config/config.go` (`Incident` struct)
- Modify: `internal/investigate/investigate.go` (`Request` struct + `FromIncident`)
- Modify: `internal/providers/providers.go` (`Investigation` struct)
- Modify: `internal/investigate/recall.go` (`recalledInvestigation`)
- Test: `internal/investigate/investigate_test.go` (or wherever `FromIncident` is tested) + `internal/investigate/recall_test.go`

**Interfaces:**
- Produces: `config.Incident.Status string`; `investigate.Request.Fingerprint string`; `providers.Investigation.Fingerprint string`, `.RecalledEntry string`. `recalledInvestigation` sets `.RecalledEntry = e.Path`.

- [ ] **Step 1: Write the failing tests**

```go
// investigate_test.go
func TestFromIncidentCarriesFingerprint(t *testing.T) {
	r := FromIncident(config.Incident{AlertName: "A", Namespace: "ns", Fingerprint: "fp-9"})
	if r.Fingerprint != "fp-9" {
		t.Fatalf("Request.Fingerprint = %q, want fp-9", r.Fingerprint)
	}
}
```

```go
// recall_test.go — extend TestRecalledInvestigationUsesDerivedConfidence or add:
func TestRecalledInvestigationCarriesEntryPath(t *testing.T) {
	inv := recalledInvestigation(Request{Title: "x"}, catalog.Entry{Title: "T", Path: "p.md"}, 0.7)
	if inv.RecalledEntry != "p.md" {
		t.Fatalf("RecalledEntry = %q, want p.md", inv.RecalledEntry)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (fields undefined). Run: `go test ./internal/investigate/ -run 'FromIncidentCarriesFingerprint|RecalledInvestigationCarriesEntryPath'`

- [ ] **Step 3: Implement**

`config.go` — add to `Incident` (after `GroupKey`):
```go
	Status string // Alertmanager status: "firing" | "resolved"
```

`investigate.go` — add to `Request`:
```go
	Fingerprint string // Alertmanager fingerprint (stable firing↔resolved); for outcome attribution
```
and set it in `FromIncident`:
```go
		Fingerprint: inc.Fingerprint,
```

`providers.go` — add to `Investigation` (after `Resource Workload`):
```go
	Fingerprint   string // originating alert fingerprint; for outcome-ledger attribution
	RecalledEntry string // when Recalled: the catalog entry Path that was matched
```

`recall.go` — in `recalledInvestigation`'s returned `Investigation`, add:
```go
		RecalledEntry: e.Path,
```

- [ ] **Step 4: Run — expect PASS** (`go test ./internal/investigate/ ./internal/config/`).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/config/config.go internal/investigate/investigate.go internal/providers/providers.go internal/investigate/recall.go internal/investigate/*_test.go
go vet ./... && go build ./... && golangci-lint run
git add internal/config/config.go internal/investigate/ internal/providers/providers.go
git commit -m "feat(investigate): thread alert fingerprint + recalled-entry for outcome attribution"
```

---

## Task 4: Ingest resolved alerts + route to the ledger

**Files:**
- Modify: `internal/trigger/incident.go` (`ParseAlertmanager`)
- Modify: `internal/server/server.go` (`Server` struct + `handleAlertmanager` + a setter)
- Test: `internal/trigger/incident_test.go`, `internal/server/*_test.go`

**Interfaces:**
- Consumes: `outcome.Ledger.Resolve(fp string, at time.Time) (Episode, bool, error)` (Task 1); `config.Incident.Status` (Task 3); `telemetry.Metrics` outcome counters (Task 2).
- Produces: `(*Server).SetOutcomeLedger(l *outcome.Ledger)`.

- [ ] **Step 1: Write failing tests**

```go
// incident_test.go
func TestParseAlertmanagerSurfacesResolved(t *testing.T) {
	body := `{"alerts":[
		{"status":"firing","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"f1"},
		{"status":"resolved","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"f1"}]}`
	incs, err := ParseAlertmanager(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(incs) != 2 {
		t.Fatalf("want 2 incidents (firing+resolved), got %d", len(incs))
	}
	var statuses []string
	for _, i := range incs {
		statuses = append(statuses, i.Status)
	}
	if statuses[0] != "firing" || statuses[1] != "resolved" {
		t.Fatalf("statuses = %v, want [firing resolved]", statuses)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (resolved dropped; `Status` not set). Run: `go test ./internal/trigger/ -run Resolved`

- [ ] **Step 3: Implement**

`incident.go` — in `ParseAlertmanager`, REMOVE the firing-only `continue` and set `Status`. The loop currently does `if a.Status != "" && a.Status != "firing" { continue }`. Replace that filter; set `Status: a.Status` on the built `config.Incident`:
```go
	for _, a := range p.Alerts {
		startsAt, _ := time.Parse(time.RFC3339, a.StartsAt)
		out = append(out, config.Incident{
			AlertName:   a.Labels["alertname"],
			Severity:    a.Labels["severity"],
			Environment: cmp.Or(a.Labels["environment"], a.Labels["env"]),
			Namespace:   a.Labels["namespace"],
			Labels:      a.Labels,
			StartsAt:    startsAt,
			Fingerprint: a.Fingerprint,
			GroupKey:    p.GroupKey,
			Status:      a.Status,
		})
	}
```
(Default `Status` when absent: treat empty as firing in the handler.)

`server.go` — add a field + setter:
```go
	outcomeLedger *outcome.Ledger // optional; records investigation outcomes (resolved alerts)
```
```go
// SetOutcomeLedger attaches the outcome ledger; resolved alerts are recorded into it.
func (s *Server) SetOutcomeLedger(l *outcome.Ledger) { s.outcomeLedger = l }
```
In `handleAlertmanager`, branch on status at the top of the loop (before `Decide`):
```go
	for _, inc := range incidents {
		if inc.Status == "resolved" {
			if s.outcomeLedger != nil {
				if ep, ok, err := s.outcomeLedger.Resolve(inc.Fingerprint, time.Now()); err != nil {
					s.log.Warn("outcome ledger resolve failed", "fingerprint", inc.Fingerprint, "err", err)
				} else if ok && s.otelMetrics != nil {
					s.otelMetrics.IncidentsResolved.Add(r.Context(), 1)
					s.otelMetrics.IncidentResolutionSeconds.Record(r.Context(), ep.Duration.Seconds())
					if ep.Kind == "recall" {
						s.otelMetrics.RecallOutcome.Add(r.Context(), 1, metric.WithAttributes(attribute.String("result", "resolved")))
					}
				}
			}
			continue
		}
		d := s.engine.Decide(inc)
		// ... existing firing path unchanged ...
	}
```
Add imports to server.go: `"time"`, `"github.com/Smana/runlore/internal/outcome"`, and (if not present) `go.opentelemetry.io/otel/attribute` + `go.opentelemetry.io/otel/metric`.

> Server test: extend the package's existing `handleAlertmanager` harness (grep `TestHandleAlertmanager` in `internal/server`) — POST a resolved alert with the ledger set (a ledger over a `t.TempDir()` file with a prior `Open` for that fingerprint) and assert it does NOT enqueue/investigate and the ledger recorded the resolve. If the harness makes ledger injection awkward, a focused `incident_test.go` for the parse + a small server test asserting "resolved alert → no Enqueue" suffices.

- [ ] **Step 4: Run — expect PASS** (`go test ./internal/trigger/ ./internal/server/`).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/trigger/incident.go internal/server/server.go internal/trigger/*_test.go internal/server/*_test.go
go vet ./... && go build ./... && golangci-lint run
git add internal/trigger/incident.go internal/server/server.go internal/trigger/incident_test.go internal/server/
git commit -m "feat(server): ingest resolved alerts and record them in the outcome ledger"
```

---

## Task 5: Record Open at OnComplete + wire the ledger, config, helm

**Files:**
- Modify: `internal/config/config.go` (new `Outcome` config + top-level field)
- Modify: `cmd/lore/main.go` (build the ledger; `SetOutcomeLedger`; record `Open` in the production OnComplete)
- Modify: `deploy/helm/runlore/values.yaml`
- Test: `internal/config/*_test.go`

**Interfaces:**
- Consumes: `outcome.New`, `(*Ledger).Open`, `outcome.Event` (Task 1); `Investigation.{Fingerprint,RecalledEntry,Recalled,Resource,Title}` (Task 3); `(*Server).SetOutcomeLedger` (Task 4); `Metrics.OutcomesOpened` (Task 2).

- [ ] **Step 1: Config test (failing)**

```go
func TestOutcomeConfigParse(t *testing.T) {
	const y = "outcome:\n  ledger_path: /var/lib/runlore/catalog/outcomes.jsonl\n"
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Outcome.LedgerPath != "/var/lib/runlore/catalog/outcomes.jsonl" {
		t.Fatalf("ledger_path: %q", c.Outcome.LedgerPath)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`go test ./internal/config/ -run TestOutcomeConfigParse`).

- [ ] **Step 3: Implement**

`config.go` — add a top-level field to `Config`:
```go
	Outcome Outcome `yaml:"outcome"` // learning-loop outcome ledger
```
and the type:
```go
// Outcome configures the learning-loop outcome ledger.
type Outcome struct {
	LedgerPath string `yaml:"ledger_path"` // append-only JSONL path (e.g. the git-sync mirror PV); empty disables
}
```

`cmd/lore/main.go` — where the Server + investigator are built (the server-serve path, near the `metrics := telemetry.NewMetrics()` instance and `server.New(...)`):
```go
	ledger, err := outcome.New(cfg.Outcome.LedgerPath)
	if err != nil {
		return fmt.Errorf("outcome ledger: %w", err)
	}
	if cfg.Outcome.LedgerPath != "" {
		log.Info("outcome ledger enabled", "path", cfg.Outcome.LedgerPath)
	}
	// after srv := server.New(...):
	srv.SetOutcomeLedger(ledger)
```
Record `Open` in the **production OnComplete** (the `func(found providers.Investigation) { switch ... }` at ~main.go:1030). Add, at the top of that closure:
```go
		if err := ledger.Open(outcome.Event{
			Fingerprint: found.Fingerprint,
			Kind:        outcomeKind(found.Recalled),
			Entry:       found.RecalledEntry,
			Title:       found.Title,
			Resource:    resourceStr(found.Resource),
			At:          time.Now(),
		}); err != nil {
			log.Warn("outcome ledger open failed", "fingerprint", found.Fingerprint, "err", err)
		}
		if metrics != nil {
			metrics.OutcomesOpened.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", outcomeKind(found.Recalled))))
		}
```
Add small helpers in main:
```go
func outcomeKind(recalled bool) string {
	if recalled {
		return "recall"
	}
	return "fresh"
}
func resourceStr(w providers.Workload) string {
	if w.Namespace == "" {
		return ""
	}
	if w.Name == "" {
		return w.Namespace
	}
	return w.Namespace + "/" + w.Name
}
```
Import `"github.com/Smana/runlore/internal/outcome"`, `"time"`, `go.opentelemetry.io/otel/attribute`, `go.opentelemetry.io/otel/metric` (most already present). Ensure `ledger` is in scope for the OnComplete closure (build it before the investigator/server construction).

`values.yaml` — under the commented `config:` example block:
```yaml
  # outcome:
  #   ledger_path: /var/lib/runlore/catalog/outcomes.jsonl  # learning-loop outcome ledger; point at the writable git-sync mirror PV
```

- [ ] **Step 4: Run — expect PASS + green build**

Run: `cd /home/smana/Sources/runlore && go build ./... && go test ./internal/config/ && golangci-lint run && helm lint deploy/helm/runlore`

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/config/config.go cmd/lore/main.go internal/config/*_test.go
git add internal/config/config.go cmd/lore/main.go internal/config/ deploy/helm/runlore/values.yaml
git commit -m "feat(outcome): record investigation outcomes + wire ledger/config/helm"
```

---

## Final verification

- [ ] `gofmt -l ./internal/... ./cmd/...` → no output
- [ ] `go vet ./...` → clean
- [ ] `go build ./...` → clean
- [ ] `golangci-lint run` → 0 issues
- [ ] `go test ./...` → all green
- [ ] Manual trace: a firing alert → `OnComplete` writes an `open` line (kind=recall|fresh) to the JSONL; the matching `resolved` webhook writes a `resolve` line, emits `incidents_resolved` + `incident_resolution_seconds`, and (for a recall) `recall_outcome{result=resolved}`. Restarting re-reads the open lines.

## Notes for the implementer

- **Ledger methods take no `ctx`** (local file I/O) — keep it that way to avoid an unused-parameter lint hit.
- `resourceStr` (main) mirrors `resourceString` (curator) / `canonicalResource` (recall). Three private copies exist now; DRY into `providers.Workload.Resource()` is a fine follow-up but out of scope here.
- Empty `ledger_path` ⇒ the whole feature is a no-op (default off) — every method short-circuits. Tests must cover that path.
