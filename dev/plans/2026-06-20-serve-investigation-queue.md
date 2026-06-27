# Wire `lore serve` → Investigation Queue Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route both React triggers — incident webhooks (matched by the trigger policy) **and** Flux `WatchFailures` events — into a single async **investigation queue**, so `lore serve` actually dispatches investigations. The investigation itself is a stub (`LogInvestigator`) for now; this is the wiring.

**Architecture:** A new `internal/investigate` package owns a normalized `Request`, an `Investigator` interface (stubbed by `LogInvestigator`), and a `Queue` (buffered channel + worker). The webhook handler enqueues a `Request` when the policy says *investigate*; a `DrainFailures` loop turns the `WatchFailures` stream into enqueued `Request`s (deduped). `cmd/lore serve` builds the queue + stub investigator + server, starts the worker, and — best-effort — starts the GitOps-failure watch when a cluster is reachable.

**Tech Stack:** Go 1.26 stdlib + existing deps (`client-go`). Contracts: `providers.Workload`/`FailureEvent`, `config.Incident`, `trigger.Engine`/`Deduper`, `flux.Provider`, `flux.NewDynamicReader`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/investigate/investigate.go` *(create)* | `Request`, `Source`, mappers, `Investigator`, `LogInvestigator`, `Queue`, `Enqueuer` |
| `internal/investigate/investigate_test.go` *(create)* | queue + mapper tests |
| `internal/investigate/failures.go` *(create)* | `DrainFailures` (stream → queue, deduped) |
| `internal/investigate/failures_test.go` *(create)* | drain test |
| `internal/server/server.go` *(modify)* | `Server` holds an `Enqueuer`; handler enqueues on *investigate* |
| `internal/server/server_test.go` *(modify)* | spy enqueuer; assert enqueue |
| `cmd/lore/main.go` *(modify)* | build queue + stub investigator + server; run worker; best-effort GitOps-failure watch |

---

## Task 1: The investigation queue

**Files:**
- Create: `internal/investigate/investigate.go`
- Test: `internal/investigate/investigate_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/investigate/investigate_test.go`:

```go
package investigate

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

// spyInvestigator records the requests it receives.
type spyInvestigator struct {
	mu   sync.Mutex
	got  []Request
	done chan struct{}
}

func (s *spyInvestigator) Investigate(_ context.Context, r Request) error {
	s.mu.Lock()
	s.got = append(s.got, r)
	s.mu.Unlock()
	if s.done != nil {
		s.done <- struct{}{}
	}
	return nil
}

func TestQueueDispatches(t *testing.T) {
	spy := &spyInvestigator{done: make(chan struct{}, 2)}
	q := NewQueue(spy, slog.New(slog.NewTextHandler(io.Discard, nil)), 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(Request{Source: SourceAlert, Title: "A"})
	q.Enqueue(Request{Source: SourceGitOpsFailure, Title: "B"})

	for i := 0; i < 2; i++ {
		select {
		case <-spy.done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for dispatch")
		}
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.got) != 2 {
		t.Fatalf("want 2 dispatched, got %d", len(spy.got))
	}
}

func TestFromIncident(t *testing.T) {
	inc := config.Incident{AlertName: "HighLatency", Severity: "critical", Namespace: "apps", Labels: map[string]string{"team": "x"}}
	r := FromIncident(inc)
	if r.Source != SourceAlert || r.Title != "HighLatency" || r.Reason != "critical" || r.Workload.Namespace != "apps" {
		t.Fatalf("unexpected request: %+v", r)
	}
}

func TestFromFailureEvent(t *testing.T) {
	fe := providers.FailureEvent{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Engine:   providers.EngineFlux, Reason: "BuildFailed", Message: "boom",
	}
	r := FromFailureEvent(fe)
	if r.Source != SourceGitOpsFailure || r.Workload != fe.Workload || r.Reason != "BuildFailed" || r.Message != "boom" {
		t.Fatalf("unexpected request: %+v", r)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -v`
Expected: FAIL — package/types undefined.

- [ ] **Step 3: Implement the package**

Create `internal/investigate/investigate.go`:

```go
// Package investigate routes triggers (incident alerts, GitOps failures) into a
// single async investigation queue. The investigation itself is pluggable via
// Investigator; LogInvestigator is the read-only placeholder until the ReAct loop
// lands.
package investigate

import (
	"context"
	"log/slog"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

// Source identifies what triggered an investigation.
type Source string

const (
	SourceAlert         Source = "alert"
	SourceGitOpsFailure Source = "gitops-failure"
)

// Request is a normalized investigation trigger.
type Request struct {
	Source   Source
	Title    string
	Workload providers.Workload // optional; zero for alerts without a workload
	Reason   string
	Message  string
	Labels   map[string]string
	At       time.Time
}

// FromIncident builds a Request from a matched incident alert.
func FromIncident(inc config.Incident) Request {
	return Request{
		Source:   SourceAlert,
		Title:    inc.AlertName,
		Workload: providers.Workload{Namespace: inc.Namespace},
		Reason:   inc.Severity,
		Labels:   inc.Labels,
		At:       inc.StartsAt,
	}
}

// FromFailureEvent builds a Request from a GitOps failure.
func FromFailureEvent(fe providers.FailureEvent) Request {
	return Request{
		Source:   SourceGitOpsFailure,
		Title:    fe.Workload.Kind + "/" + fe.Workload.Name + " " + fe.Reason,
		Workload: fe.Workload,
		Reason:   fe.Reason,
		Message:  fe.Message,
		At:       fe.When,
	}
}

// Investigator runs an investigation for a Request.
type Investigator interface {
	Investigate(ctx context.Context, r Request) error
}

// LogInvestigator is the read-only placeholder: it logs the request it would
// investigate. Replaced by the ReAct loop in a later phase.
type LogInvestigator struct {
	Log *slog.Logger
}

// Investigate logs the request.
func (l LogInvestigator) Investigate(_ context.Context, r Request) error {
	l.Log.Info("investigate",
		"source", string(r.Source), "title", r.Title,
		"workload", r.Workload.Namespace+"/"+r.Workload.Name, "reason", r.Reason)
	return nil
}

// Enqueuer accepts investigation requests.
type Enqueuer interface {
	Enqueue(r Request)
}

// Queue is a buffered, single-worker investigation queue.
type Queue struct {
	ch  chan Request
	inv Investigator
	log *slog.Logger
}

// NewQueue builds a Queue with the given buffer size.
func NewQueue(inv Investigator, log *slog.Logger, buffer int) *Queue {
	return &Queue{ch: make(chan Request, buffer), inv: inv, log: log}
}

// Enqueue submits a request. If the buffer is full it logs and drops (backpressure)
// rather than blocking the caller (e.g. the webhook handler).
func (q *Queue) Enqueue(r Request) {
	select {
	case q.ch <- r:
	default:
		q.log.Warn("investigation queue full; dropping", "title", r.Title, "source", string(r.Source))
	}
}

// Run consumes the queue until ctx is done.
func (q *Queue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-q.ch:
			if err := q.inv.Investigate(ctx, r); err != nil {
				q.log.Error("investigation failed", "title", r.Title, "err", err)
			}
		}
	}
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -v`
Expected: PASS.

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/investigate/
git commit -m "feat(investigate): investigation queue + Request + LogInvestigator stub"
```

---

## Task 2: Enqueue from the webhook handler

**Files:**
- Modify: `internal/server/server.go`
- Test: `internal/server/server_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/server/server_test.go`, add a spy enqueuer and update `testServer()` to inject it; add an assertion that a matching incident enqueues. Replace the existing `testServer` helper and add a spy:

```go
type spyEnqueuer struct{ reqs []investigate.Request }

func (s *spyEnqueuer) Enqueue(r investigate.Request) { s.reqs = append(s.reqs, r) }

func testServerWith(enq investigate.Enqueuer) *Server {
	cfg := &config.Config{}
	cfg.Triggers.Incidents = config.IncidentTrigger{
		Enabled: true,
		Match:   config.IncidentMatch{Severity: []string{"critical"}},
	}
	return New(cfg, enq, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testServer() *Server { return testServerWith(&spyEnqueuer{}) }

func TestHandleAlertmanagerEnqueues(t *testing.T) {
	enq := &spyEnqueuer{}
	body := `{"alerts":[
	  {"status":"firing","labels":{"alertname":"A","severity":"critical","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp1"},
	  {"status":"firing","labels":{"alertname":"B","severity":"warning","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp2"}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body))
	rr := httptest.NewRecorder()
	testServerWith(enq).Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rr.Code)
	}
	if len(enq.reqs) != 1 || enq.reqs[0].Title != "A" {
		t.Fatalf("want 1 enqueued (only critical A), got %v", enq.reqs)
	}
}
```

Add the `investigate` import to the test file's import block: `"github.com/Smana/runlore/internal/investigate"`.

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/server/ -v`
Expected: FAIL — `New` now needs an enqueuer; `investigate` undefined in server.

- [ ] **Step 3: Wire the server**

In `internal/server/server.go`, add the `investigate` import, the `enqueuer` field, the new `New` signature, and the enqueue call in the handler:

```go
import (
	"log/slog"
	"net/http"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/trigger"
)
```

```go
// Server handles incoming incident webhooks and applies the trigger policy.
type Server struct {
	engine   *trigger.Engine
	enqueuer investigate.Enqueuer
	log      *slog.Logger
	handler  http.Handler
}

// New builds a Server from config and an investigation enqueuer.
func New(cfg *config.Config, enq investigate.Enqueuer, log *slog.Logger) *Server {
	s := &Server{engine: trigger.NewEngine(cfg.Triggers.Incidents), enqueuer: enq, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.handler = mux
	return s
}
```

In `handleAlertmanager`, replace the `// Phase 1+ …` comment line with an enqueue on investigate:

```go
	for _, inc := range incidents {
		d := s.engine.Decide(inc)
		s.log.Info("incident",
			"alert", inc.AlertName, "severity", inc.Severity, "namespace", inc.Namespace,
			"investigate", d.Investigate, "reason", d.Reason)
		if d.Investigate {
			s.enqueuer.Enqueue(investigate.FromIncident(inc))
		}
	}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/server/ -v`
Expected: PASS (the enqueue test + the existing handler tests via the updated `testServer`).

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all clean, `0 issues`. *(`cmd/lore` won't build yet because `New` changed — that's fixed in Task 4; if `go build ./...` fails only on `cmd/lore`, proceed; the package-level gate for `internal/...` must pass. To keep the gate fully green, temporarily update the `server.New(...)` call in `cmd/lore/main.go` to pass `nil` as the enqueuer in this task, then complete the real wiring in Task 4.)*

```bash
cd /home/smana/Sources/runlore
git add internal/server/ cmd/lore/main.go
git commit -m "feat(serve): enqueue an investigation when an incident matches the policy"
```

---

## Task 3: Drain GitOps failures into the queue

**Files:**
- Create: `internal/investigate/failures.go`
- Test: `internal/investigate/failures_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/investigate/failures_test.go`:

```go
package investigate

import (
	"context"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/trigger"
)

type collectEnqueuer struct{ reqs []Request }

func (c *collectEnqueuer) Enqueue(r Request) { c.reqs = append(c.reqs, r) }

func TestDrainFailures(t *testing.T) {
	src := make(chan providers.FailureEvent, 3)
	wl := providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}
	src <- providers.FailureEvent{Workload: wl, Reason: "BuildFailed"}
	src <- providers.FailureEvent{Workload: wl, Reason: "BuildFailed"} // duplicate within window → deduped
	src <- providers.FailureEvent{Workload: providers.Workload{Kind: "Kustomization", Name: "infra", Namespace: "flux-system"}, Reason: "HealthCheckFailed"}
	close(src)

	enq := &collectEnqueuer{}
	DrainFailures(context.Background(), src, enq, trigger.NewDeduper(30*time.Minute))

	if len(enq.reqs) != 2 {
		t.Fatalf("want 2 enqueued (one deduped), got %d", len(enq.reqs))
	}
	if enq.reqs[0].Source != SourceGitOpsFailure {
		t.Fatalf("unexpected source: %v", enq.reqs[0].Source)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -run TestDrainFailures -v`
Expected: FAIL — `DrainFailures` undefined.

- [ ] **Step 3: Implement `DrainFailures`**

Create `internal/investigate/failures.go`:

```go
package investigate

import (
	"context"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/trigger"
)

// DrainFailures forwards GitOps FailureEvents into the queue as investigation
// requests, deduped by workload (a Ready=False resource emits repeated events).
// A nil dedup disables dedup. It returns when src closes or ctx is done.
func DrainFailures(ctx context.Context, src <-chan providers.FailureEvent, q Enqueuer, dedup *trigger.Deduper) {
	for {
		select {
		case <-ctx.Done():
			return
		case fe, ok := <-src:
			if !ok {
				return
			}
			if dedup != nil && dedup.Seen(fe.Workload.Namespace+"/"+fe.Workload.Name) {
				continue
			}
			q.Enqueue(FromFailureEvent(fe))
		}
	}
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -v`
Expected: PASS.

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: clean (with the Task-2 `nil` enqueuer stub in `cmd/lore`, `go build ./...` succeeds). `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/investigate/
git commit -m "feat(investigate): DrainFailures forwards GitOps failures into the queue (deduped)"
```

---

## Task 4: Wire it all in `lore serve`

**Files:**
- Modify: `cmd/lore/main.go`

- [ ] **Step 1: Replace `runServe`**

In `cmd/lore/main.go`, update the imports and `runServe` to build the queue + stub investigator + server, run the worker, and best-effort start the GitOps-failure watch. Replace the `runServe` function with:

```go
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	queue := investigate.NewQueue(investigate.LogInvestigator{Log: log}, log, 64)
	go queue.Run(ctx)

	// Best-effort GitOps-failure watch: only if enabled and a cluster is reachable.
	if cfg.Triggers.GitOpsFailures.Enabled {
		startGitOpsFailureWatch(ctx, cfg, queue, log)
	}

	srv := server.New(cfg, queue, log)
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		_ = httpSrv.Shutdown(context.Background())
	}()
	log.Info("runlore serving", "addr", *addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// startGitOpsFailureWatch builds a dynamic client and drains Flux WatchFailures
// into the queue. Failures here are logged, not fatal — webhook-only serving
// continues if no cluster is reachable.
func startGitOpsFailureWatch(ctx context.Context, cfg *config.Config, q investigate.Enqueuer, log *slog.Logger) {
	client, err := dynamicClient()
	if err != nil {
		log.Warn("gitops-failure watch disabled: no kube client", "err", err)
		return
	}
	provider := flux.New(flux.NewDynamicReader(client), &whatchanged.Differ{})
	events, err := provider.WatchFailures(ctx)
	if err != nil {
		log.Warn("gitops-failure watch disabled", "err", err)
		return
	}
	log.Info("watching gitops failures (Flux Kustomizations)")
	go investigate.DrainFailures(ctx, events, q, trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std()))
}

// dynamicClient builds a dynamic client from in-cluster config, falling back to
// the default kubeconfig.
func dynamicClient() (dynamic.Interface, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		restCfg, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return nil, err
		}
	}
	return dynamic.NewForConfig(restCfg)
}
```

Update the import block to add:

```go
	"context"
	"os/signal"
	"syscall"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
	"github.com/Smana/runlore/internal/whatchanged"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
```

(Keep the existing `flag`, `fmt`, `log/slog`, `net/http`, `os`, `config`, `server` imports.)

- [ ] **Step 2: Build + smoke test**

Run:
```bash
cd /home/smana/Sources/runlore
go mod tidy
go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l . && golangci-lint run ./...
```
Expected: all clean, `0 issues`.

Manual smoke (webhook → queue → stub investigator):
```bash
cat > /tmp/runlore.yaml <<'EOF'
triggers:
  incidents:
    enabled: true
    match: { severity: [critical], environment: [prod] }
    dedup: { window: 30m }
  gitops_failures: { enabled: false }
EOF
go run ./cmd/lore serve --config /tmp/runlore.yaml --addr :18080 >/tmp/lore.log 2>&1 &
SRV=$!
curl -s --retry-connrefused --retry 10 --retry-delay 1 -o /dev/null localhost:18080/healthz
curl -s -o /dev/null -XPOST localhost:18080/webhook/alertmanager --data @examples/alertmanager-webhook.json
sleep 1 || true
kill $SRV 2>/dev/null
grep -E 'msg=incident|msg=investigate' /tmp/lore.log
```
Expected: the HarborProbeFailure incident logs `investigate=true`, **followed by** an `msg=investigate source=alert title=HarborProbeFailure …` line from the queue worker (the others filtered/deduped). *(If `sleep` is unavailable in your shell, replace it with a brief `curl` retry against `/healthz` to allow the async worker to run before kill.)*

- [ ] **Step 3: Commit**

```bash
cd /home/smana/Sources/runlore
git add cmd/lore/main.go go.mod go.sum
git commit -m "feat(serve): route webhooks + GitOps failures into the investigation queue"
```

---

## What this plan delivers

`lore serve` now wires both React triggers into one async investigation queue: matched incident webhooks **and** (best-effort, when a cluster is reachable) Flux `Ready=False` failures both enqueue a normalized `Request` that the worker dispatches to the (stub) investigator. The React pillar is end-to-end: trigger → policy → queue → dispatch.

## Next plans (not in this plan)

- **The real `Investigator`** (`internal/investigate` → the ReAct loop) replacing `LogInvestigator`: model + providers + what-changed + catalog → `providers.Investigation`.
- Per-source trigger gating for GitOps failures (namespace/severity), and surfacing queue depth as a metric.

---

## Self-Review

- **Spec coverage:** Both triggers (webhook + `WatchFailures`) converge on one `Queue`; the investigation is an explicit `LogInvestigator` stub (the named next step). The GitOps-failure watch is best-effort so webhook-only serving never breaks. ✅
- **Placeholder scan:** Complete code per step; the `LogInvestigator` stub and the Task-2 `nil`-enqueuer-then-Task-4-real-wiring sequencing are explicit, not missing work. The smoke test is concrete. ✅
- **Type consistency:** `Request`/`Source`/`Enqueuer`/`Queue`/`Investigator` defined in Task 1, consumed by the server (Task 2) and `DrainFailures` (Task 3) and wired in `serve` (Task 4); `FromIncident`/`FromFailureEvent` map the existing `config.Incident`/`providers.FailureEvent` exactly; `trigger.Deduper` reused for failure dedup. `server.New` signature change is propagated to `cmd/lore` (Task 4). ✅
