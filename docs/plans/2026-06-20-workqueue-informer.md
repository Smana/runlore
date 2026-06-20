# Native Control-Loop Primitives: Workqueue + Dynamic Informer

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the hand-rolled control-loop pieces with the native Kubernetes primitives operators use: the investigation `Queue` → a client-go **`workqueue.RateLimitingInterface`** (dedup + rate-limit + retry-with-backoff — the LLM-cost/noise control), and `WatchFailures` raw `Watch()` → a **dynamic informer** (list-watch with reconnection/resync). Public interfaces are unchanged, so the server/`serve` wiring and the future investigation loop are untouched.

**Architecture:** `investigate.Queue` keeps its `Enqueuer.Enqueue` + `Run(ctx)` surface but is backed by a typed rate-limiting workqueue keyed by a comparable `key` (with a side map holding the latest `Request` payload per key — so duplicate triggers coalesce and the worker gets retries/backoff for free). `flux.dynamicReader.WatchKustomizations` keeps its `(<-chan KustomizationEvent, error)` surface but is backed by a `dynamicinformer` factory (reconnection/resync) instead of a raw watch.

**Tech Stack:** Go 1.26, `k8s.io/client-go` (already a dep): `util/workqueue`, `dynamic/dynamicinformer`, `tools/cache`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/investigate/investigate.go` *(modify)* | `Queue` backed by `workqueue.TypedRateLimitingInterface` |
| `internal/investigate/investigate_test.go` *(modify)* | dedup/coalesce + retry tests |
| `internal/providers/gitops/flux/dynamic.go` *(modify)* | `WatchKustomizations` backed by a dynamic informer |
| `internal/providers/gitops/flux/dynamic_test.go` *(modify)* | informer-based watch test |

---

## Task 1: `Queue` → rate-limiting workqueue

**Files:**
- Modify: `internal/investigate/investigate.go`
- Test: `internal/investigate/investigate_test.go`

- [ ] **Step 1: Write the failing tests**

Replace `TestQueueDispatches` in `internal/investigate/investigate_test.go` with these (and keep the `spyInvestigator` helper; add a `failingInvestigator`):

```go
func TestQueueDispatches(t *testing.T) {
	spy := &spyInvestigator{done: make(chan struct{}, 4)}
	q := NewQueue(spy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(Request{Source: SourceAlert, Title: "A"})
	q.Enqueue(Request{Source: SourceGitOpsFailure, Title: "B", Workload: providers.Workload{Namespace: "ns", Name: "x"}})

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

// failingInvestigator fails n times, then succeeds.
type failingInvestigator struct {
	mu        sync.Mutex
	failsLeft int
	calls     int
	done      chan struct{}
}

func (f *failingInvestigator) Investigate(context.Context, Request) error {
	f.mu.Lock()
	f.calls++
	fail := f.failsLeft > 0
	if fail {
		f.failsLeft--
	}
	f.mu.Unlock()
	if !fail && f.done != nil {
		f.done <- struct{}{}
	}
	if fail {
		return errTransient
	}
	return nil
}

var errTransient = fmt.Errorf("transient")

func TestQueueRetriesOnError(t *testing.T) {
	inv := &failingInvestigator{failsLeft: 2, done: make(chan struct{}, 1)}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(Request{Source: SourceAlert, Title: "flaky"})
	select {
	case <-inv.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out: retried request never succeeded")
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.calls < 3 {
		t.Fatalf("want >=3 calls (2 failures + success), got %d", inv.calls)
	}
}
```

Add `"fmt"` and `"sync"` to the test imports if not present (and `"github.com/Smana/runlore/internal/providers"`). Note `NewQueue` now takes **two** args (no buffer size).

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -run TestQueue -v`
Expected: FAIL — `NewQueue` arity changed / retry behavior absent.

- [ ] **Step 3: Reimplement `Queue` on a workqueue**

In `internal/investigate/investigate.go`, update the imports and replace the `Queue` type, `NewQueue`, `Enqueue`, and `Run`:

```go
import (
	"context"
	"log/slog"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)
```

```go
// key is the comparable workqueue item; duplicate triggers with the same key
// coalesce. The full Request payload is held in Queue.reqs.
type key struct {
	Source    Source
	Namespace string
	Name      string
	Title     string
}

func keyOf(r Request) key {
	return key{Source: r.Source, Namespace: r.Workload.Namespace, Name: r.Workload.Name, Title: r.Title}
}

// Queue is a rate-limiting investigation queue: duplicate triggers coalesce, and
// failed investigations are retried with exponential backoff.
type Queue struct {
	wq   workqueue.TypedRateLimitingInterface[key]
	mu   sync.Mutex
	reqs map[key]Request
	inv  Investigator
	log  *slog.Logger
}

// NewQueue builds an investigation queue.
func NewQueue(inv Investigator, log *slog.Logger) *Queue {
	return &Queue{
		wq:   workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]()),
		reqs: map[key]Request{},
		inv:  inv,
		log:  log,
	}
}

// Enqueue submits a request. Re-enqueuing the same key before it is processed
// coalesces (latest payload wins).
func (q *Queue) Enqueue(r Request) {
	k := keyOf(r)
	q.mu.Lock()
	q.reqs[k] = r
	q.mu.Unlock()
	q.wq.Add(k)
}

// Run consumes the queue until ctx is done.
func (q *Queue) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		q.wq.ShutDown()
	}()
	for {
		k, shutdown := q.wq.Get()
		if shutdown {
			return
		}
		q.process(ctx, k)
	}
}

func (q *Queue) process(ctx context.Context, k key) {
	defer q.wq.Done(k)
	q.mu.Lock()
	r, ok := q.reqs[k]
	q.mu.Unlock()
	if !ok {
		q.wq.Forget(k)
		return
	}
	if err := q.inv.Investigate(ctx, r); err != nil {
		q.log.Error("investigation failed; retrying", "title", r.Title, "err", err)
		q.wq.AddRateLimited(k)
		return
	}
	q.wq.Forget(k)
	q.mu.Lock()
	delete(q.reqs, k)
	q.mu.Unlock()
}
```

(Delete the old channel-based `Queue`/`NewQueue`/`Enqueue`/`Run` and the now-unused `time` import only if nothing else uses it — `time` is no longer referenced here, so drop it from the import block.)

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/investigate/ -v && go test -race ./internal/investigate/`
Expected: PASS, race-clean.

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: clean, `0 issues`. *(`cmd/lore` calls `investigate.NewQueue(LogInvestigator{...}, log, 64)` — drop the `64` arg to match the new signature; commit that one-line `cmd/lore/main.go` change too.)*

```bash
cd /home/smana/Sources/runlore
git add internal/investigate/ cmd/lore/main.go
git commit -m "refactor(investigate): back Queue with a rate-limiting workqueue (dedup + retry/backoff)"
```

---

## Task 2: `WatchKustomizations` → dynamic informer

**Files:**
- Modify: `internal/providers/gitops/flux/dynamic.go`
- Test: `internal/providers/gitops/flux/dynamic_test.go`

- [ ] **Step 1: Adjust the test**

`TestDynamicReaderWatch` already creates a Kustomization and expects an event. It should keep passing against the informer implementation. Replace it with an informer-friendly version (a buffered channel + a slightly longer timeout to allow cache sync):

```go
func TestDynamicReaderWatch(t *testing.T) {
	gvrToListKind := map[schema.GroupVersionResource]string{
		kustomizationGVR: "KustomizationList",
		gitRepositoryGVR: "GitRepositoryList",
	}
	bad := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "bad", "namespace": "apps"},
		"spec":       map[string]any{"path": "./apps", "sourceRef": map[string]any{"name": "flux-system"}},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed", "message": "boom"},
		}},
	}}
	// Seed the object before starting the informer so the initial list surfaces it.
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, bad)
	r := NewDynamicReader(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := r.WatchKustomizations(ctx)
	if err != nil {
		t.Fatalf("WatchKustomizations: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Kustomization.Name != "bad" || ev.Kustomization.ReadyStatus != "False" {
			t.Fatalf("unexpected event: %+v", ev.Kustomization)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for informer event")
	}
}
```

- [ ] **Step 2: Run to verify failure (or current-impl behavior)**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -run TestDynamicReaderWatch -v`
Expected: the test compiles; it may pass against the old raw-watch impl, but Step 3 switches to the informer. (If it already passes, proceed — the goal is the informer implementation behind the same behavior.)

- [ ] **Step 3: Reimplement `WatchKustomizations` with a dynamic informer**

In `internal/providers/gitops/flux/dynamic.go`, update imports (add `dynamicinformer`, `tools/cache`; you can drop `apimachinery/pkg/watch` if no longer used) and replace the `WatchKustomizations` method:

```go
import (
	// ... existing imports ...
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)
```

```go
// WatchKustomizations watches all Kustomizations via a dynamic informer (list-watch
// with reconnection + periodic resync) and forwards each add/update as a
// KustomizationEvent. The channel closes when ctx is done.
func (r *dynamicReader) WatchKustomizations(ctx context.Context) (<-chan KustomizationEvent, error) {
	factory := dynamicinformer.NewDynamicSharedInformerFactory(r.client, 10*time.Minute)
	informer := factory.ForResource(kustomizationGVR).Informer()

	out := make(chan KustomizationEvent, 128)
	send := func(obj any) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		ev := KustomizationEvent{Kustomization: kustomizationFromUnstructured(u)}
		select {
		case out <- ev:
		case <-ctx.Done():
		default: // never block the informer; drop under backpressure
		}
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { send(obj) },
		UpdateFunc: func(_, obj any) { send(obj) },
	}); err != nil {
		return nil, fmt.Errorf("add event handler: %w", err)
	}

	go func() {
		defer close(out)
		factory.Start(ctx.Done())
		<-ctx.Done()
	}()
	return out, nil
}
```

Add `"time"` to the imports if not already present.

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -v && go test -race ./internal/providers/gitops/flux/`
Expected: PASS, race-clean.

> client-go latitude: `NewDynamicSharedInformerFactory`, `ForResource(gvr).Informer()`, and `AddEventHandler` (returning `(registration, error)`) are correct for v0.36.2. If `AddEventHandler` has a different return shape in the resolved version, adjust the call accordingly; keep the handler behavior identical.

- [ ] **Step 5: Tidy, full gate, commit**

Run:
```bash
cd /home/smana/Sources/runlore
go mod tidy
go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l . && golangci-lint run ./...
```
Expected: clean, `0 issues`.

```bash
git add internal/providers/gitops/flux/ go.mod go.sum
git commit -m "refactor(gitops/flux): back WatchKustomizations with a dynamic informer (reconnection/resync)"
```

---

## Task 3: Verify `lore serve` still works end-to-end

**Files:** none (verification only).

- [ ] **Step 1: Build + smoke test**

Run:
```bash
cd /home/smana/Sources/runlore
go build -o /tmp/lore ./cmd/lore
cat > /tmp/rl.yaml <<'EOF'
triggers:
  incidents:
    enabled: true
    match: { severity: [critical], environment: [prod] }
    dedup: { window: 30m }
  gitops_failures: { enabled: false }
EOF
/tmp/lore serve --config /tmp/rl.yaml --addr :18082 >/tmp/rl.log 2>&1 &
SRV=$!
curl -s --retry-connrefused --retry 10 --retry-delay 1 -o /dev/null localhost:18082/healthz
curl -s -o /dev/null -XPOST localhost:18082/webhook/alertmanager --data @examples/alertmanager-webhook.json
curl -s -o /dev/null --retry 3 --retry-delay 1 localhost:18082/healthz
kill $SRV 2>/dev/null
grep -E 'msg=incident|msg=investigate' /tmp/rl.log
```
Expected: unchanged behavior — `HarborProbeFailure … investigate=true` followed by `msg=investigate source=alert title=HarborProbeFailure` from the workqueue worker. (No commit; this task only confirms the refactor preserved end-to-end behavior.)

---

## What this plan delivers

The control-loop foundation is now the native one operators use: a rate-limiting workqueue (coalescing + retry/backoff — the LLM-cost guard) and a dynamic informer (robust list-watch). Public interfaces (`Enqueuer`, `Reader`, `GitOpsProvider`) are unchanged, so `serve` and the upcoming investigation loop are unaffected — the loop will simply be the workqueue's `process` function.

## Next plan (not in this plan)

The **real investigation loop** — replace `LogInvestigator` with a ReAct loop over a `ModelProvider` + the what-changed tool, producing a `providers.Investigation`. It plugs into this workqueue unchanged.

---

## Self-Review

- **Spec coverage:** `Queue` → `workqueue.TypedRateLimitingInterface` (dedup via comparable `key` + retry via `AddRateLimited`); `WatchKustomizations` → dynamic informer (reconnection/resync). Interfaces unchanged → server/`serve` untouched; Task 3 verifies e2e. ✅
- **Placeholder scan:** Complete code per step; the `cmd/lore` `NewQueue` arity fix and the client-go latitude note are explicit. ✅
- **Type consistency:** new `key`/`keyOf` internal to `Queue`; `Enqueuer.Enqueue`/`Run` signatures preserved (callers unchanged except the dropped `64` buffer arg); informer rebuild preserves `WatchKustomizations(ctx) (<-chan KustomizationEvent, error)`. `KustomizationEvent`/`kustomizationFromUnstructured` reused. ✅
