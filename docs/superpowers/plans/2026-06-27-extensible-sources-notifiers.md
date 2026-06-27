# Extensible Sources & Notifiers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make adding an event source or a notifier a "drop one self-registering file" operation, via two typed adapter registries with core-owned transports and a shared ingest pipeline.

**Architecture:** A `source` registry holds `Descriptor`s registered from each adapter's `init()`. Adapters implement only `Webhook.Decode(body) → []Request` (push) or `Watcher.Watch(ctx) → <-chan Request` (pull); the core owns the HTTP transport (auth, body-cap, routing), the watcher runner, and one ingest pipeline (admission policy → dedup → coalesce/debounce → enqueue, plus resolved→ledger routing). Notifiers get the symmetric registry over the existing `providers.Notifier` interface. `config.Incident` is retired and `Severity`/`Environment` move onto `investigate.Request`.

**Tech Stack:** Go 1.26, `net/http` (Go 1.22 method routing), `k8s.io/client-go` dynamic informers, `gopkg.in/yaml.v3` (raw `yaml.Node` per adapter), standard `testing`.

## Global Constraints

- **Release gate:** Do NOT create or push any `v*` git tag until the entire refactor lands. A `v*` tag fires `.github/workflows/build-image.yml` (a release). Branch pushes that build dev images are fine. (Only `v0.1.0` exists.)
- **Branch:** All work on `refactor/extensible-sources-notifiers` (already created; the spec is committed there). Never commit to `main`.
- **No co-authors / no AI attribution** in commit messages or PRs.
- **Go module:** `github.com/Smana/runlore`, `go 1.26.0`.
- **Behavior-preserving until Phase 2:** Phase 1 wraps existing logic behind the new interfaces; the alert + GitOps paths must behave identically and all existing tests must stay green.
- **Normalized seam:** every source ultimately produces `investigate.Request`; every notifier consumes `providers.Investigation`. Never introduce a second normalized type.
- **Run after each task:** `go build ./... && go test ./...` must pass before commit.

---

## Phase 1 — Registry, transports, pipeline; retrofit the two existing sources (behavior-preserving)

End state: `/webhook/alertmanager` and the GitOps informer both flow through `internal/source`, but observable behavior and config are unchanged. Mergeable on its own.

### Task 1: Promote `Severity` and `Environment` onto `investigate.Request`

**Files:**
- Modify: `internal/investigate/investigate.go:32-42` (Request struct), `:69-85` (FromIncident)
- Test: `internal/investigate/investigate_test.go`

**Interfaces:**
- Produces: `investigate.Request` now has `Severity string` and `Environment string` fields.

- [ ] **Step 1: Write the failing test**

```go
func TestFromIncidentCarriesSeverityAndEnvironment(t *testing.T) {
	inc := config.Incident{AlertName: "X", Severity: "critical", Environment: "prod", Namespace: "ns"}
	r := investigate.FromIncident(inc)
	if r.Severity != "critical" || r.Environment != "prod" {
		t.Fatalf("got severity=%q env=%q", r.Severity, r.Environment)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestFromIncidentCarriesSeverityAndEnvironment -v`
Expected: FAIL — `r.Severity undefined`.

- [ ] **Step 3: Add the fields and populate them**

In `Request` (after `Reason string`):
```go
	Severity    string             // alert severity (alert-like sources); shapes prompt + notification
	Environment string             // deployment environment (prod/staging/…)
```
In `FromIncident`, set them in the returned `Request{...}`:
```go
		Severity:     inc.Severity,
		Environment:  inc.Environment,
```
(Keep `Reason: inc.Severity` for now; Phase 2 removes the duplication.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/investigate/ -run TestFromIncidentCarriesSeverityAndEnvironment -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/investigate.go internal/investigate/investigate_test.go
git commit -m "feat(investigate): promote Severity/Environment onto Request"
```

### Task 2: Source registry core (`Descriptor`, `Register`, `BuildEnabled`, interfaces)

**Files:**
- Create: `internal/source/registry.go`
- Test: `internal/source/registry_test.go`

**Interfaces:**
- Produces:
  - `type Kind int` with `Webhook`, `Watcher`.
  - `type Admission int` with `MatchGated`, `EnableGated`.
  - `type Resolution struct { Fingerprint string; At time.Time }`
  - `type DecodeResult struct { Requests []investigate.Request; Resolved []Resolution }`
  - `type WebhookSource interface { Decode(body []byte, h http.Header) (DecodeResult, error) }`
  - `type WatcherSource interface { Watch(ctx context.Context) (<-chan investigate.Request, error) }`
  - `type Deps struct { Cfg *config.Config; GitOps providers.GitOpsProvider; Log *slog.Logger; Raw map[string]yaml.Node }`
  - `type Descriptor struct { Name, ConfigKey string; Kind Kind; Admission Admission; Path string; Build func(Deps) (any, error) }`
  - `type Built struct { Desc Descriptor; Impl any }`
  - `func Register(d Descriptor)` (panics on duplicate `Name`)
  - `func Registered() []Descriptor` (sorted by Name; test helper + deterministic startup)
  - `func BuildEnabled(deps Deps) ([]Built, error)` — builds every descriptor whose `Build` returns a non-nil impl; a `Build` error is returned (fail-fast).

- [ ] **Step 1: Write the failing test**

```go
package source

import (
	"context"
	"net/http"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

type fakeWebhook struct{}
func (fakeWebhook) Decode(_ []byte, _ http.Header) (DecodeResult, error) { return DecodeResult{}, nil }

func TestRegisterAndBuildEnabled(t *testing.T) {
	resetForTest() // clears the package registry between tests
	Register(Descriptor{Name: "fake", Kind: Webhook, Admission: MatchGated, Path: "/webhook/fake",
		Build: func(Deps) (any, error) { return fakeWebhook{}, nil }})
	built, err := BuildEnabled(Deps{Cfg: &config.Config{}})
	if err != nil { t.Fatal(err) }
	if len(built) != 1 || built[0].Desc.Name != "fake" {
		t.Fatalf("got %+v", built)
	}
	if _, ok := built[0].Impl.(WebhookSource); !ok { t.Fatal("impl is not a WebhookSource") }
}

func TestRegisterDuplicatePanics(t *testing.T) {
	resetForTest()
	Register(Descriptor{Name: "dup", Build: func(Deps) (any, error) { return fakeWebhook{}, nil }})
	defer func() { if recover() == nil { t.Fatal("expected panic on duplicate") } }()
	Register(Descriptor{Name: "dup", Build: func(Deps) (any, error) { return fakeWebhook{}, nil }})
}

func TestBuildEnabledSkipsNilImpl(t *testing.T) {
	resetForTest()
	Register(Descriptor{Name: "off", Build: func(Deps) (any, error) { return nil, nil }})
	built, err := BuildEnabled(Deps{Cfg: &config.Config{}})
	if err != nil { t.Fatal(err) }
	if len(built) != 0 { t.Fatalf("expected disabled source skipped, got %d", len(built)) }
}

func TestBuildEnabledFailFast(t *testing.T) {
	resetForTest()
	Register(Descriptor{Name: "bad", Build: func(Deps) (any, error) { return nil, context.DeadlineExceeded }})
	if _, err := BuildEnabled(Deps{Cfg: &config.Config{}}); err == nil {
		t.Fatal("expected build error to propagate")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/ -v`
Expected: FAIL — package/symbols undefined.

- [ ] **Step 3: Implement `registry.go`**

```go
// Package source registers event-source adapters and runs their core-owned
// transports. An adapter implements Webhook (push) or Watcher (pull) and
// self-registers via Register in an init() func; the core owns HTTP auth,
// body-cap, routing, the watcher runner, and the ingest pipeline.
package source

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"gopkg.in/yaml.v3"
)

type Kind int
const ( Webhook Kind = iota; Watcher )

type Admission int
const ( MatchGated Admission = iota; EnableGated )

type Resolution struct {
	Fingerprint string
	At          time.Time
}

type DecodeResult struct {
	Requests []investigate.Request
	Resolved []Resolution
}

type WebhookSource interface {
	Decode(body []byte, h http.Header) (DecodeResult, error)
}
type WatcherSource interface {
	Watch(ctx context.Context) (<-chan investigate.Request, error)
}

type Deps struct {
	Cfg    *config.Config
	GitOps providers.GitOpsProvider
	Log    *slog.Logger
	Raw    map[string]yaml.Node // per-adapter raw config, keyed by Descriptor.ConfigKey
}

type Descriptor struct {
	Name      string
	ConfigKey string
	Kind      Kind
	Admission Admission
	Path      string // webhook only
	Build     func(Deps) (any, error)
}

type Built struct {
	Desc Descriptor
	Impl any // WebhookSource or WatcherSource
}

var registry = map[string]Descriptor{}

func Register(d Descriptor) {
	if _, dup := registry[d.Name]; dup {
		panic("source: duplicate registration for " + d.Name)
	}
	registry[d.Name] = d
}

func resetForTest() { registry = map[string]Descriptor{} }

func Registered() []Descriptor {
	out := make([]Descriptor, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func BuildEnabled(deps Deps) ([]Built, error) {
	var built []Built
	for _, d := range Registered() {
		impl, err := d.Build(deps)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", d.Name, err)
		}
		if impl == nil {
			continue // disabled (no config)
		}
		built = append(built, Built{Desc: d, Impl: impl})
	}
	return built, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/source/ -v`
Expected: PASS (all four).

- [ ] **Step 5: Commit**

```bash
git add internal/source/registry.go internal/source/registry_test.go
git commit -m "feat(source): adapter registry (Descriptor, Register, BuildEnabled)"
```

### Task 3: Ingest pipeline

> **Prerequisite:** this task's `pipeline.go` imports `trigger.MatchRequest`, defined in **Task 4**. Implement Task 4's Steps 1–3 first (or alongside) — the two are a compile unit. The pipeline will not build until `MatchRequest` exists.

**Files:**
- Create: `internal/source/pipeline.go`
- Test: `internal/source/pipeline_test.go`

**Interfaces:**
- Consumes: `investigate.Enqueuer` (`Enqueue(Request)`), `trigger` matcher, `trigger.NewDeduper`.
- Produces:
  - `type ResolveFunc func(fingerprint string, at time.Time)` (caller closes over `outcome.Ledger` + metrics; avoids coupling the pipeline to the ledger's concrete episode type)
  - `type Pipeline struct { ... }`
  - `func NewPipeline(cfg *config.Config, enq investigate.Enqueuer, resolve ResolveFunc, log *slog.Logger) *Pipeline`
  - `func (p *Pipeline) Ingest(ctx context.Context, adm Admission, res DecodeResult)` — admits Requests per mode and invokes `resolve` for each Resolution.
  - `func (p *Pipeline) admit(adm Admission, r investigate.Request) bool`

Note: in Phase 1 the matcher still runs on the existing `config.Incident`-derived fields now present on `Request` (Severity/Environment/Namespace/Title/Labels). Use a new `trigger.MatchRequest` added in Task 4. Coalesce/rate-limit are wired in Task 7 (main) by passing the queue/coalescer as the `Enqueuer`; the pipeline itself only does admission + dedup + ledger routing to stay focused.

- [ ] **Step 1: Write the failing test**

```go
package source

import (
	"context"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
)

type capEnq struct{ reqs []investigate.Request }
func (c *capEnq) Enqueue(r investigate.Request) { c.reqs = append(c.reqs, r) }

func matchAllCfg() *config.Config {
	c := &config.Config{}
	c.Triggers.Incidents.Enabled = true // empty Match ⇒ matches anything
	return c
}

func TestPipelineMatchGatedAdmitsMatching(t *testing.T) {
	enq := &capEnq{}
	p := NewPipeline(matchAllCfg(), enq, nil, nil)
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Requests: []investigate.Request{{Title: "A", Severity: "critical", Fingerprint: "f1"}},
	})
	if len(enq.reqs) != 1 { t.Fatalf("want 1 enqueued, got %d", len(enq.reqs)) }
}

func TestPipelineMatchGatedDropsUnmatched(t *testing.T) {
	enq := &capEnq{}
	c := &config.Config{}
	c.Triggers.Incidents.Enabled = true
	c.Triggers.Incidents.Match.Severity = []string{"critical"}
	p := NewPipeline(c, enq, nil, nil)
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Requests: []investigate.Request{{Title: "A", Severity: "warning", Fingerprint: "f1"}},
	})
	if len(enq.reqs) != 0 { t.Fatalf("want 0 enqueued, got %d", len(enq.reqs)) }
}

func TestPipelineDedupsStillFiring(t *testing.T) {
	enq := &capEnq{}
	p := NewPipeline(matchAllCfg(), enq, nil, nil)
	r := DecodeResult{Requests: []investigate.Request{{Title: "A", Fingerprint: "f1"}}}
	p.Ingest(context.Background(), MatchGated, r)
	p.Ingest(context.Background(), MatchGated, r)
	if len(enq.reqs) != 1 { t.Fatalf("want dedup to 1, got %d", len(enq.reqs)) }
}

func TestPipelineRoutesResolvedToLedger(t *testing.T) {
	enq := &capEnq{}
	var resolved []string
	resolve := func(fp string, _ time.Time) { resolved = append(resolved, fp) }
	p := NewPipeline(matchAllCfg(), enq, resolve, nil)
	p.Ingest(context.Background(), MatchGated, DecodeResult{Resolved: []Resolution{{Fingerprint: "f9", At: time.Now()}}})
	if len(resolved) != 1 || resolved[0] != "f9" {
		t.Fatalf("want resolve f9, got %+v", resolved)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/ -run TestPipeline -v`
Expected: FAIL — `NewPipeline` undefined.

- [ ] **Step 3: Implement `pipeline.go`**

```go
package source

import (
	"context"
	"log/slog"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/trigger"
)

// ResolveFunc records a resolved alert. main supplies a closure over the
// outcome.Ledger (+ metrics), so the pipeline stays decoupled from the ledger's
// concrete episode type. A nil ResolveFunc disables resolved-alert handling.
type ResolveFunc func(fingerprint string, at time.Time)

type Pipeline struct {
	cfg     *config.Config
	enq     investigate.Enqueuer
	resolve ResolveFunc
	dedup   *trigger.Deduper
	log     *slog.Logger
}

func NewPipeline(cfg *config.Config, enq investigate.Enqueuer, resolve ResolveFunc, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg: cfg, enq: enq, resolve: resolve, log: log,
		dedup: trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std()),
	}
}

// Ingest admits each Request per the admission mode and invokes resolve for each
// Resolution. Cascade-suppression and debounce for EnableGated sources are
// applied at the watcher edge (see Task 6) during Phase 1.
func (p *Pipeline) Ingest(ctx context.Context, adm Admission, res DecodeResult) {
	for _, r := range res.Resolved {
		if p.resolve != nil {
			p.resolve(r.Fingerprint, r.At)
		}
	}
	for _, req := range res.Requests {
		if !p.admit(adm, req) {
			continue
		}
		p.enq.Enqueue(req)
	}
}

func (p *Pipeline) admit(adm Admission, r investigate.Request) bool {
	switch adm {
	case MatchGated:
		if !trigger.MatchRequest(p.cfg.Triggers.Incidents, r) {
			return false
		}
	case EnableGated:
		if !p.cfg.Triggers.GitOpsFailures.Enabled {
			return false
		}
	}
	if p.dedup.Seen(dedupKey(r)) {
		return false
	}
	return true
}

func dedupKey(r investigate.Request) string {
	if r.Fingerprint != "" {
		return r.Fingerprint
	}
	return string(r.Source) + "/" + r.Workload.Namespace + "/" + r.Workload.Name + "/" + r.Title
}
```

(EnableGated debounce + cascade-suppression are added in Task 6 when the GitOps adapter is wired, reusing `investigate.Debouncer` and `isCascadeFailure`; for Phase 1 they remain in the GitOps adapter's `Watch` so behavior is preserved — see Task 6.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/source/ -run TestPipeline -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/source/pipeline.go internal/source/pipeline_test.go
git commit -m "feat(source): ingest pipeline (admission, dedup, ledger routing)"
```

### Task 4: `trigger.MatchRequest` (matcher over `Request`)

**Files:**
- Modify: `internal/trigger/incident.go` (add `MatchRequest`; keep `IncidentTrigger.Matches` until Phase 2)
- Modify: `internal/config/config.go` (add `MatchesRequest` shim on `IncidentTrigger`, or expose match fields) — implement matcher in `trigger` reading `config.IncidentTrigger` + `investigate.Request`.
- Test: `internal/trigger/match_request_test.go`

**Interfaces:**
- Produces: `func MatchRequest(t config.IncidentTrigger, r investigate.Request) bool` — same semantics as `IncidentTrigger.Matches(Incident)` but reads `r.Severity`, `r.Environment`, `r.Workload.Namespace`, `r.Title` (alertname), `r.Labels`.

- [ ] **Step 1: Write the failing test**

```go
package trigger

import (
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
)

func TestMatchRequestSeverityAndNamespace(t *testing.T) {
	pol := config.IncidentTrigger{Enabled: true}
	pol.Match.Severity = []string{"critical"}
	pol.Match.Namespaces = []string{"prod-*"}
	yes := investigate.Request{Severity: "critical", Workload: providersWorkload("prod-web")}
	no := investigate.Request{Severity: "warning", Workload: providersWorkload("prod-web")}
	if !MatchRequest(pol, yes) { t.Fatal("expected match") }
	if MatchRequest(pol, no) { t.Fatal("expected no match (severity)") }
}
```

Add a small test helper in the same file:
```go
func providersWorkload(ns string) (w investigateWorkload) { w.Namespace = ns; return }
```
(Use the real `providers.Workload` type; the helper is only to keep the test terse — inline `investigate.Request{Workload: providers.Workload{Namespace: ns}}` if preferred.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/trigger/ -run TestMatchRequest -v`
Expected: FAIL — `MatchRequest` undefined.

- [ ] **Step 3: Implement `MatchRequest`**

Port the glob/severity/env/namespace/label logic from `IncidentMatch.matches` (`config.go:365+`) to read `Request`. Reuse the existing matchers by constructing the comparison inline:
```go
// MatchRequest reports whether a Request passes the incident trigger policy.
// Mirrors IncidentTrigger.Matches but reads the normalized Request shape.
func MatchRequest(t config.IncidentTrigger, r investigate.Request) bool {
	if !t.Enabled {
		return false
	}
	inc := config.Incident{
		AlertName: r.Title, Severity: r.Severity, Environment: r.Environment,
		Namespace: r.Workload.Namespace, Labels: r.Labels,
	}
	return t.Matches(inc)
}
```
(This delegates to the existing, tested `Matches` during Phase 1; Phase 2 inlines it when `Incident` is retired.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/trigger/ -run TestMatchRequest -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/trigger/incident.go internal/trigger/match_request_test.go
git commit -m "feat(trigger): MatchRequest matcher over normalized Request"
```

### Task 5: Alertmanager webhook adapter

**Files:**
- Create: `internal/source/alertmanager/alertmanager.go`
- Test: `internal/source/alertmanager/alertmanager_test.go`, `internal/source/alertmanager/testdata/firing.json`, `.../resolved.json`

**Interfaces:**
- Consumes: `source.Register`, `source.WebhookSource`, `source.DecodeResult`, `source.Resolution`, `trigger.ParseAlertmanager`, `investigate.FromIncident`.
- Produces: registers Descriptor `{Name:"alertmanager", ConfigKey:"sources.alertmanager", Kind:Webhook, Admission:MatchGated, Path:"/webhook/alertmanager"}`.

- [ ] **Step 1: Write the failing test** (golden payloads)

`testdata/firing.json`:
```json
{"groupKey":"g1","alerts":[{"status":"firing","labels":{"alertname":"HighMem","severity":"critical","namespace":"prod-web","deployment":"web"},"startsAt":"2026-06-27T10:00:00Z","fingerprint":"abc"}]}
```
`testdata/resolved.json`:
```json
{"groupKey":"g1","alerts":[{"status":"resolved","labels":{"alertname":"HighMem","severity":"critical","namespace":"prod-web"},"startsAt":"2026-06-27T10:00:00Z","fingerprint":"abc"}]}
```
```go
package alertmanager

import (
	"os"
	"testing"
)

func TestDecodeFiringProducesRequest(t *testing.T) {
	body, _ := os.ReadFile("testdata/firing.json")
	res, err := (&Source{}).Decode(body, nil)
	if err != nil { t.Fatal(err) }
	if len(res.Requests) != 1 { t.Fatalf("want 1 request, got %d", len(res.Requests)) }
	r := res.Requests[0]
	if r.Title != "HighMem" || r.Severity != "critical" || r.Workload.Namespace != "prod-web" || r.Workload.Name != "web" {
		t.Fatalf("bad request: %+v", r)
	}
	if len(res.Resolved) != 0 { t.Fatalf("firing must not resolve") }
}

func TestDecodeResolvedProducesResolution(t *testing.T) {
	body, _ := os.ReadFile("testdata/resolved.json")
	res, err := (&Source{}).Decode(body, nil)
	if err != nil { t.Fatal(err) }
	if len(res.Requests) != 0 { t.Fatalf("resolved must not enqueue") }
	if len(res.Resolved) != 1 || res.Resolved[0].Fingerprint != "abc" {
		t.Fatalf("want resolved abc, got %+v", res.Resolved)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/alertmanager/ -v`
Expected: FAIL — `Source` undefined.

- [ ] **Step 3: Implement the adapter**

```go
// Package alertmanager is the Alertmanager/VMAlert webhook source adapter.
package alertmanager

import (
	"bytes"
	"net/http"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/source"
	"github.com/Smana/runlore/internal/trigger"
)

type Source struct{}

func (Source) Decode(body []byte, _ http.Header) (source.DecodeResult, error) {
	incidents, err := trigger.ParseAlertmanager(bytes.NewReader(body))
	if err != nil {
		return source.DecodeResult{}, err
	}
	var out source.DecodeResult
	for _, inc := range incidents {
		if inc.Status == "resolved" {
			out.Resolved = append(out.Resolved, source.Resolution{Fingerprint: inc.Fingerprint, At: inc.StartsAt})
			continue
		}
		out.Requests = append(out.Requests, investigate.FromIncident(inc))
	}
	return out, nil
}

func init() {
	source.Register(source.Descriptor{
		Name: "alertmanager", ConfigKey: "sources.alertmanager",
		Kind: source.Webhook, Admission: source.MatchGated, Path: "/webhook/alertmanager",
		Build: func(d source.Deps) (any, error) {
			// Enabled when the incident trigger is enabled (Phase 3 moves this to sources.alertmanager).
			if !d.Cfg.Triggers.Incidents.Enabled {
				return nil, nil
			}
			return Source{}, nil
		},
	})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/source/alertmanager/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/source/alertmanager/
git commit -m "feat(source): alertmanager webhook adapter (golden-tested Decode)"
```

### Task 6: GitOps watcher adapter

**Files:**
- Create: `internal/source/gitops/gitops.go`
- Test: `internal/source/gitops/gitops_test.go`

**Interfaces:**
- Consumes: `source.WatcherSource`, `providers.GitOpsProvider.WatchFailures`, `investigate.FromFailureEvent`, `investigate.isCascadeFailure` (export as `investigate.IsCascadeFailure`).
- Produces: registers Descriptor `{Name:"gitops", ConfigKey:"sources.gitops", Kind:Watcher, Admission:EnableGated}`; `Watch` returns a channel of `investigate.Request`, dropping cascade symptoms.

- [ ] **Step 1: Export the cascade check** (small prerequisite)

In `internal/investigate/failures.go`, rename `isCascadeFailure` → `IsCascadeFailure` (exported) and update its one caller in `DrainFailures`. Run `go build ./...`.

- [ ] **Step 2: Write the failing test**

```go
package gitops

import (
	"context"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

type fakeGP struct{ ch chan providers.FailureEvent }
func (f *fakeGP) WatchFailures(context.Context) (<-chan providers.FailureEvent, error) { return f.ch, nil }
// (stub the rest of GitOpsProvider with panics; only WatchFailures is exercised)

func TestWatchMapsFailureToRequestAndDropsCascade(t *testing.T) {
	ch := make(chan providers.FailureEvent, 2)
	src := &Source{gp: &fakeGP{ch: ch}}
	out, err := src.Watch(context.Background())
	if err != nil { t.Fatal(err) }
	ch <- providers.FailureEvent{Workload: providers.Workload{Namespace: "ns", Kind: "Kustomization", Name: "app"}, Reason: "BuildFailed"}
	ch <- providers.FailureEvent{Workload: providers.Workload{Namespace: "ns", Name: "dep"}, Reason: "DependencyNotReady"}
	close(ch)
	var got []string
	for r := range out { got = append(got, r.Workload.Name) }
	if len(got) != 1 || got[0] != "app" {
		t.Fatalf("want [app] (cascade dropped), got %v", got)
	}
	_ = time.Second
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/source/gitops/ -v`
Expected: FAIL — `Source` undefined.

- [ ] **Step 4: Implement the adapter**

```go
// Package gitops is the GitOps-failure watcher source adapter (Flux/Argo CD).
package gitops

import (
	"context"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/source"
)

type Source struct{ gp providers.GitOpsProvider }

func (s *Source) Watch(ctx context.Context) (<-chan investigate.Request, error) {
	in, err := s.gp.WatchFailures(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan investigate.Request)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case fe, ok := <-in:
				if !ok {
					return
				}
				if investigate.IsCascadeFailure(fe) {
					continue
				}
				select {
				case out <- investigate.FromFailureEvent(fe):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func init() {
	source.Register(source.Descriptor{
		Name: "gitops", ConfigKey: "sources.gitops",
		Kind: source.Watcher, Admission: source.EnableGated,
		Build: func(d source.Deps) (any, error) {
			if !d.Cfg.Triggers.GitOpsFailures.Enabled || d.GitOps == nil {
				return nil, nil
			}
			return &Source{gp: d.GitOps}, nil
		},
	})
}
```

Note: debounce stays wired in `main` for Phase 1 (the watcher runner can wrap the channel with `investigate.Debouncer` — see Task 7) to preserve current behavior exactly.

- [ ] **Step 5: Run tests; commit**

Run: `go test ./internal/source/gitops/ ./internal/investigate/ -v` → PASS.
```bash
git add internal/source/gitops/ internal/investigate/failures.go
git commit -m "feat(source): gitops failure watcher adapter; export IsCascadeFailure"
```

### Task 7: Transports + wire into server & main; remove bespoke wiring

**Files:**
- Create: `internal/source/webhook.go` (HTTP transport), `internal/source/watcher.go` (runner)
- Modify: `internal/server/server.go` (mount registered webhook sources; drop the inline `handleAlertmanager` ingest body), `cmd/lore/main.go` (`BuildEnabled`, mount/start; delete `startGitOpsFailureWatch` bespoke wiring and `buildNotifier`'s source coupling), blank-import adapters.
- Test: `internal/source/webhook_test.go`, `internal/server/server_test.go` (adjust)

**Interfaces:**
- Produces:
  - `func (b Built) Handler(auth func(*http.Request) bool, bodyCap int64, pipe *Pipeline) http.HandlerFunc` (webhook kind)
  - `func RunWatchers(ctx context.Context, built []Built, pipe *Pipeline, debounce *investigate.Debouncer)` — starts each watcher goroutine, draining into `pipe.Ingest` with the source's Admission.
  - `func MountWebhooks(mux *http.ServeMux, built []Built, auth func(*http.Request) bool, pipe *Pipeline)`

- [ ] **Step 1: Write the failing test (webhook transport)**

```go
package source

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

type echoWebhook struct{}
func (echoWebhook) Decode(b []byte, _ http.Header) (DecodeResult, error) {
	return DecodeResult{Requests: []investigateRequestForTest(b)}, nil
}

func TestWebhookHandlerRejectsBadAuthAndCapsBody(t *testing.T) {
	pipe := NewPipeline(matchAllCfg(), &capEnq{}, nil, nil)
	b := Built{Desc: Descriptor{Kind: Webhook, Admission: MatchGated}, Impl: echoWebhook{}}
	auth := func(r *http.Request) bool { return r.Header.Get("X") == "ok" }
	h := b.Handler(auth, 16, pipe)

	// bad auth → 401
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest("POST", "/", strings.NewReader("{}")))
	if rr.Code != http.StatusUnauthorized { t.Fatalf("want 401, got %d", rr.Code) }

	// over cap → 413
	rr = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 100)))
	req.Header.Set("X", "ok")
	h(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge { t.Fatalf("want 413, got %d", rr.Code) }
}
```
(Define `investigateRequestForTest` in the test to return a single `investigate.Request`. Keep it minimal.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/ -run TestWebhookHandler -v`
Expected: FAIL — `Handler` undefined.

- [ ] **Step 3: Implement `webhook.go` and `watcher.go`**

`webhook.go`:
```go
package source

import (
	"context"
	"errors"
	"net/http"
)

func (b Built) Handler(auth func(*http.Request) bool, bodyCap int64, pipe *Pipeline) http.HandlerFunc {
	wh := b.Impl.(WebhookSource)
	return func(w http.ResponseWriter, r *http.Request) {
		if auth != nil && !auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, bodyCap)
		body, err := readAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		res, err := wh.Decode(body, r.Header)
		if err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		pipe.Ingest(r.Context(), b.Desc.Admission, res)
		w.WriteHeader(http.StatusAccepted)
	}
}

func MountWebhooks(mux *http.ServeMux, built []Built, auth func(*http.Request) bool, pipe *Pipeline) {
	for _, b := range built {
		if b.Desc.Kind != Webhook {
			continue
		}
		mux.HandleFunc("POST "+b.Desc.Path, b.Handler(auth, 1<<20, pipe))
	}
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
```
(Prefer `io.ReadAll` over the hand-rolled `readAll`; it returns `*http.MaxBytesError` through `errors.As`. Use `io.ReadAll(r.Body)` and drop `readAll`.)

`watcher.go`:
```go
package source

import (
	"context"

	"github.com/Smana/runlore/internal/investigate"
)

func RunWatchers(ctx context.Context, built []Built, pipe *Pipeline, deb *investigate.Debouncer) {
	for _, b := range built {
		if b.Desc.Kind != Watcher {
			continue
		}
		ch, err := b.Impl.(WatcherSource).Watch(ctx)
		if err != nil {
			if pipe.log != nil {
				pipe.log.Error("source watch failed", "source", b.Desc.Name, "err", err)
			}
			continue
		}
		go func(b Built, ch <-chan investigate.Request) {
			for {
				select {
				case <-ctx.Done():
					return
				case r, ok := <-ch:
					if !ok {
						return
					}
					pipe.Ingest(ctx, b.Desc.Admission, DecodeResult{Requests: []investigate.Request{r}})
				}
			}
		}(b, ch)
	}
}
```

- [ ] **Step 4: Wire into `server.go` and `main.go`; delete bespoke wiring**

In `server.New` (`server.go:79`), after building the mux, call `source.MountWebhooks(mux, built, s.webhookAuthorized, pipe)` with the built sources + pipeline passed in via `Actions`/a new field; remove the inline `mux.HandleFunc("POST /webhook/alertmanager", ...)` and the body of `handleAlertmanager` (its logic now lives in the alertmanager adapter + pipeline). Keep `webhookAuthorized`, slack/actions/health routes.

In `cmd/lore/main.go`: add blank imports
```go
import (
	_ "github.com/Smana/runlore/internal/source/alertmanager"
	_ "github.com/Smana/runlore/internal/source/gitops"
)
```
Build sources + pipeline and pass to the server; replace `startGitOpsFailureWatch(...)` with:
```go
built, err := source.BuildEnabled(source.Deps{Cfg: cfg, GitOps: gitops, Log: log})
if err != nil { return fmt.Errorf("build sources: %w", err) }
// resolve closes over the outcome ledger + metrics, mirroring server.go:393-404.
resolve := func(fp string, at time.Time) {
	if outcomeLedger == nil { return }
	if ep, ok, err := outcomeLedger.Resolve(fp, at); err != nil {
		log.Warn("outcome ledger resolve failed", "fingerprint", fp, "err", err)
	} else if ok && metrics != nil {
		metrics.IncidentsResolved.Add(workCtx, 1)
		metrics.IncidentResolutionSeconds.Record(workCtx, ep.Duration.Seconds())
		if ep.Kind == "recall" {
			metrics.RecallOutcome.Add(workCtx, 1, metric.WithAttributes(attribute.String("result", "resolved")))
		}
	}
}
pipe := source.NewPipeline(cfg, enqueuer, resolve, log)
source.RunWatchers(workCtx, built, pipe, debouncer)
```
Delete the now-unused `startGitOpsFailureWatch` and `investigate.DrainFailures` call site (keep `DrainFailures` for now if other tests use it; otherwise remove in Phase 2).

- [ ] **Step 5: Run the full suite; commit**

Run: `go build ./... && go test ./...`
Expected: PASS (adjust `server_test.go` expectations for the moved ingest path; the `/webhook/alertmanager` end-to-end test should still pass).
```bash
git add -A
git commit -m "feat(source): webhook+watcher transports; route all sources through the registry"
```

---

## Phase 2 — Retire `config.Incident`; migrate coalescer to `Request`

### Task 8: Migrate the coalescer to `investigate.Request`

**Files:**
- Modify: `internal/coalesce/coalescer.go` (`New(cfg, out func([]investigate.Request))`, `Add(investigate.Request)`, `key`, `emit`, `newCriticalDuringCooldown`)
- Modify: callers in `cmd/lore/main.go` and `internal/server/*` and `internal/source/pipeline.go` (route admitted Requests through the coalescer as the `Enqueuer`)
- Test: `internal/coalesce/*_test.go` (update to Request)

**Interfaces:**
- Produces: coalescer operates on `investigate.Request`; the pipeline's `enq` may be a coalescing enqueuer that folds correlated Requests before the queue.

- [ ] **Step 1: Update the coalescer tests to use `investigate.Request`** (replace `config.Incident{...}` literals with `investigate.Request{...}`, mapping `Severity`/`Namespace`/`Labels`/`Fingerprint`/`GroupKey`). Run → FAIL (compile).
- [ ] **Step 2: Change `coalesce.New`/`Add`/`key`/`emit` signatures to `investigate.Request`.** `key` reads `r.Labels`/`r.Workload.Namespace`/`GroupKey`. Add a `GroupKey string` field to `Request` (carried from Alertmanager in `FromIncident`).
- [ ] **Step 3: Make the coalescer an `investigate.Enqueuer`** by adding `func (c *Coalescer) Enqueue(r investigate.Request) { c.Add(r) }`, so the pipeline treats it uniformly; the coalescer's `out` flushes folded batches to the real queue.
- [ ] **Step 4: Run `go test ./internal/coalesce/... ./internal/source/...`** → PASS.
- [ ] **Step 5: Commit** `refactor(coalesce): operate on investigate.Request`.

### Task 9: Inline the matcher and delete `config.Incident`

**Files:**
- Modify: `internal/trigger/incident.go` (inline glob/severity/env/namespace/label matching into `MatchRequest`; delete `ParseAlertmanager`'s `Incident` return → return `[]investigate.Request` + resolutions, or move parsing into the alertmanager adapter), `internal/config/config.go` (delete `Incident` type + `IncidentTrigger.Matches`/`IncidentMatch.matches` if now unused), `internal/investigate/investigate.go` (delete `FromIncident`; drop `Reason: inc.Severity` duplication), `internal/source/alertmanager/alertmanager.go` (parse AM JSON → Request directly).
- Test: update affected tests.

- [ ] **Step 1:** Move AM JSON parsing into `internal/source/alertmanager` (`parse(body) (DecodeResult, error)`); its golden tests already assert the output — keep them green.
- [ ] **Step 2:** Reimplement `MatchRequest` to read `Request` fields directly (port `IncidentMatch.matches` glob logic; reuse `config.IncidentMatch` for the policy shape only).
- [ ] **Step 3:** Delete `config.Incident`, `IncidentTrigger.Matches`, `trigger.ParseAlertmanager`, `investigate.FromIncident` and fix all compile errors.
- [ ] **Step 4:** `go build ./... && go test ./...` → PASS.
- [ ] **Step 5:** Commit `refactor: retire config.Incident; single normalized Request seam`.

---

## Phase 3 — Config clean break (`sources.<name>` / `notify.<name>`)

### Task 10: Raw-node-per-adapter config + source enablement

**Files:**
- Modify: `internal/config/config.go` (add `Sources map[string]yaml.Node \`yaml:"sources"\``; keep `Notify` as `map[string]yaml.Node` OR keep typed but route through registry — see Task 11), drop `Server.WebhookTokenEnv` in favor of `sources.alertmanager.token_env`, drop `triggers.gitops_failures.enabled` in favor of `sources.gitops.enabled` (keep `debounce`).
- Modify: adapter `Build` funcs to decode their `d.Raw[ConfigKey]`.
- Modify: `examples/` configs + `docs/data-sources.md`, `docs/getting-started.md`.
- Test: `internal/config/config_test.go`, adapter build tests.

- [ ] **Step 1:** Write a config test asserting `sources.alertmanager.token_env` + `sources.gitops.enabled` decode and drive `BuildEnabled`. FAIL.
- [ ] **Step 2:** Add `Sources map[string]yaml.Node`; populate `Deps.Raw` from it in `main`.
- [ ] **Step 3:** Move each adapter's enablement to decode its own raw node (alertmanager: present ⇒ enabled, reads `token_env`; gitops: `enabled` + `debounce`).
- [ ] **Step 4:** Update every file under `examples/` and the two docs to the new shape; `grep -rn "webhook_token_env\|gitops_failures" examples docs` returns nothing stale. Run `go test ./...`.
- [ ] **Step 5:** Commit `feat(config): sources.<name> raw-node config (clean break)`.

---

## Phase 4 — Notifier registry

### Task 11: Notifier registry + retrofit Slack/Matrix

**Files:**
- Create: `internal/notify/registry.go`
- Modify: `internal/notify/slack.go`, `internal/notify/matrix.go` (add `init()` self-registration with `Build` reading `notify.<name>` raw node), `cmd/lore/main.go` (replace `buildNotifier` with `notify.BuildEnabled`)
- Test: `internal/notify/registry_test.go`

**Interfaces:**
- Produces: `func notify.Register(d Descriptor)`, `func notify.BuildEnabled(cfg, deps) *Multi` where `Descriptor{Name, ConfigKey, Build func(Deps) (providers.Notifier, error)}`.

- [ ] **Step 1:** Test: register a fake notifier, `BuildEnabled` returns a `*Multi` containing it. FAIL.
- [ ] **Step 2:** Implement `registry.go` (mirror `source.Register`/`BuildEnabled`, building `providers.Notifier`s and wrapping in `NewMulti`).
- [ ] **Step 3:** Add `init()` to slack.go (two registrations or one that picks bot-vs-webhook by config) and matrix.go; each `Build` decodes its raw node and returns the existing `NewSlackBot`/`NewSlack`/`NewMatrix`. Native Block Kit buttons unchanged.
- [ ] **Step 4:** Replace `buildNotifier` (`main.go:823-842`) with `notify.BuildEnabled`. `go test ./internal/notify/... && go build ./...` → PASS.
- [ ] **Step 5:** Commit `feat(notify): adapter registry; retrofit slack/matrix`.

---

## Phase 5 — Prove extensibility

### Task 12: Generic outgoing-webhook notifier (the drop-in proof)

**Files:**
- Create: `internal/notify/webhook/webhook.go` (implements `providers.Notifier`; POSTs the formatted investigation as JSON to a configured URL via `httpx.SecureClient`), `internal/notify/webhook/webhook_test.go`
- No edits to `main.go` or `config.Config` (this is the whole point).

- [ ] **Step 1:** Test (httptest server): a configured `notify.webhook.url_env` ⇒ `Deliver` POSTs JSON with the root cause + confidence; assert the received body. FAIL.
- [ ] **Step 2:** Implement the adapter + `init()` self-registration (`ConfigKey:"notify.webhook"`).
- [ ] **Step 3:** Add a blank import in `main.go` (the only line touched) and an `examples/` entry.
- [ ] **Step 4:** `go test ./internal/notify/... && go build ./...` → PASS. Manually confirm: adding this notifier required exactly one file + one import line + one example.
- [ ] **Step 5:** Commit `feat(notify): generic outgoing-webhook sink (proves drop-in extensibility)`.

---

## Final verification (before any release discussion)

- [ ] `go build ./... && go test ./... && go vet ./...` all green.
- [ ] `golangci-lint run` clean (`.golangci.yml`).
- [ ] `grep -rn "config.Incident\|FromIncident\|ParseAlertmanager\|buildNotifier\|startGitOpsFailureWatch" internal cmd` returns nothing (dead code fully removed).
- [ ] `docs/architecture/runlore-architecture.md` "React" box updated to mention the source registry; `docs/data-sources.md` documents the `sources.<name>` shape and how to add an adapter.
- [ ] Open a PR from `refactor/extensible-sources-notifiers` → `main`. **Do NOT push a `v*` tag.** A release is a separate, explicit step after merge, on the user's go-ahead.

## Self-review notes (author)

- **Spec coverage:** registry + transports (Tasks 2,3,7), transport/decode split (Tasks 5,7), unified pipeline + two admission modes (Tasks 3,4), retire Incident + promote fields (Tasks 1,9), coalescer ripple (Task 8), raw-node config clean break (Task 10), notifier registry (Task 11), CloudEvents non-goal (docs only — no task, by design), release gate (Global Constraints + Final verification), prove-it adapter (Task 12). ✓
- **Phase 1 is behavior-preserving** (adapters wrap existing parse/watch; pipeline delegates matching to the tested `Matches`); Phase 2 does the destructive cleanup once green. ✓
- **Line numbers** (e.g. `main.go:823-842`, `server.go:369-424`) are from the snapshot at planning time — confirm before editing.
