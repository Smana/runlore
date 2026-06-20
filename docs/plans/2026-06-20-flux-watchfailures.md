# Flux `WatchFailures` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the Flux provider's **`WatchFailures`** React trigger — watch Flux Kustomizations and emit a `providers.FailureEvent` when one goes `Ready=False` (so a bad GitOps rollout triggers an investigation before a metrics alert fires).

**Architecture:** Keep the testable split. The `kustomization` type gains the Ready condition (status/reason/message), extracted in `kustomizationFromUnstructured`. The `Reader` gains `WatchKustomizations(ctx) (<-chan KustomizationEvent, error)`; `Provider.WatchFailures` consumes that stream and maps `Ready=False` events to `FailureEvent` (pure, fake-`Reader`-tested). The dynamic implementation wraps the dynamic client's `Watch`.

**Tech Stack:** Go 1.26, `k8s.io/client-go` + `k8s.io/apimachinery` (already present). Contract: `providers.FailureEvent`, `providers.Workload`, `providers.EngineFlux`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/providers/gitops/flux/flux.go` *(modify)* | `kustomization` Ready fields; `KustomizationEvent`; `Reader.WatchKustomizations`; real `WatchFailures` |
| `internal/providers/gitops/flux/flux_test.go` *(modify)* | `WatchFailures` test via a fake `Reader` stream |
| `internal/providers/gitops/flux/dynamic.go` *(modify)* | extract Ready condition; `dynamicReader.WatchKustomizations` |
| `internal/providers/gitops/flux/dynamic_test.go` *(modify)* | watch test via the dynamic fake |

---

## Task 1: Extract the Ready condition

**Files:**
- Modify: `internal/providers/gitops/flux/flux.go` (add fields), `internal/providers/gitops/flux/dynamic.go` (populate them)
- Test: `internal/providers/gitops/flux/dynamic_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/providers/gitops/flux/dynamic_test.go`:

```go
func TestKustomizationReadyCondition(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "apps", "namespace": "flux-system"},
		"spec":       map[string]any{"path": "./apps", "sourceRef": map[string]any{"name": "flux-system"}},
		"status": map[string]any{
			"lastAppliedRevision": "main@sha1:abc",
			"conditions": []any{
				map[string]any{"type": "Healthy", "status": "True"},
				map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed", "message": "kustomize build failed"},
			},
		},
	}}
	k := kustomizationFromUnstructured(u)
	if k.ReadyStatus != "False" || k.ReadyReason != "BuildFailed" || k.ReadyMessage != "kustomize build failed" {
		t.Fatalf("unexpected ready condition: %+v", k)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -run TestKustomizationReadyCondition -v`
Expected: FAIL — `ReadyStatus`/`ReadyReason`/`ReadyMessage` undefined.

- [ ] **Step 3: Add the fields + extraction**

In `internal/providers/gitops/flux/flux.go`, add three fields to the `kustomization` struct (after `Revision`):

```go
	ReadyStatus  string // status.conditions[type=Ready].status ("True"/"False"/"Unknown")
	ReadyReason  string
	ReadyMessage string
```

In `internal/providers/gitops/flux/dynamic.go`, add a helper and call it from `kustomizationFromUnstructured`:

```go
// readyCondition returns the (status, reason, message) of the Ready condition.
func readyCondition(u *unstructured.Unstructured) (status, reason, message string) {
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return "", "", ""
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "Ready" {
			status, _ = m["status"].(string)
			reason, _ = m["reason"].(string)
			message, _ = m["message"].(string)
			return status, reason, message
		}
	}
	return "", "", ""
}
```

In `kustomizationFromUnstructured`, before the `return`, populate the fields:

```go
	readyStatus, readyReason, readyMessage := readyCondition(u)
```

and add to the returned struct literal:

```go
		ReadyStatus:  readyStatus,
		ReadyReason:  readyReason,
		ReadyMessage: readyMessage,
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -run TestKustomizationReadyCondition -v`
Expected: PASS.

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/providers/gitops/flux/
git commit -m "feat(gitops/flux): extract the Kustomization Ready condition"
```

---

## Task 2: `WatchFailures` over a `Reader` stream

**Files:**
- Modify: `internal/providers/gitops/flux/flux.go`
- Test: `internal/providers/gitops/flux/flux_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/providers/gitops/flux/flux_test.go`:

```go
// streamReader is a fakeReader that also serves a fixed watch stream.
type streamReader struct {
	fakeReader
	events []KustomizationEvent
}

func (s streamReader) WatchKustomizations(ctx context.Context) (<-chan KustomizationEvent, error) {
	ch := make(chan KustomizationEvent, len(s.events))
	for _, e := range s.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestWatchFailures(t *testing.T) {
	r := streamReader{events: []KustomizationEvent{
		{Kustomization: kustomization{Name: "ok", Namespace: "flux-system", ReadyStatus: "True"}},
		{Kustomization: kustomization{Name: "bad", Namespace: "apps", ReadyStatus: "False", ReadyReason: "BuildFailed", ReadyMessage: "boom"}},
		{Kustomization: kustomization{Name: "progressing", Namespace: "apps", ReadyStatus: "Unknown"}},
	}}
	p := New(r, &whatchanged.Differ{})
	ch, err := p.WatchFailures(context.Background())
	if err != nil {
		t.Fatalf("WatchFailures: %v", err)
	}
	var got []providers.FailureEvent
	for e := range ch {
		got = append(got, e)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 failure event (only Ready=False), got %d", len(got))
	}
	e := got[0]
	if e.Engine != providers.EngineFlux || e.Workload.Name != "bad" || e.Workload.Kind != "Kustomization" ||
		e.Reason != "BuildFailed" || e.Message != "boom" {
		t.Fatalf("unexpected failure event: %+v", e)
	}
}
```

Note: this requires `fakeReader` (from the existing tests) to be embeddable — it already has `ListKustomizations`/`GetGitRepository`. `streamReader` adds `WatchKustomizations`, satisfying the extended `Reader`.

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -run TestWatchFailures -v`
Expected: FAIL — `KustomizationEvent` / `Reader.WatchKustomizations` undefined; `WatchFailures` still a stub.

- [ ] **Step 3: Extend `Reader` + implement `WatchFailures`**

In `internal/providers/gitops/flux/flux.go`:

Add the event type (after the `gitRepository` type):

```go
// KustomizationEvent is a single watch event for a Kustomization.
type KustomizationEvent struct {
	Kustomization kustomization
}
```

Add `WatchKustomizations` to the `Reader` interface:

```go
type Reader interface {
	ListKustomizations(ctx context.Context) ([]kustomization, error)
	GetGitRepository(ctx context.Context, namespace, name string) (gitRepository, error)
	WatchKustomizations(ctx context.Context) (<-chan KustomizationEvent, error)
}
```

Replace the `WatchFailures` stub with the real implementation:

```go
// WatchFailures watches Flux Kustomizations and emits a FailureEvent whenever one
// is Ready=False (a failed/blocked reconcile). The returned channel closes when
// the watch ends or ctx is done.
func (p *Provider) WatchFailures(ctx context.Context) (<-chan providers.FailureEvent, error) {
	src, err := p.reader.WatchKustomizations(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan providers.FailureEvent)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-src:
				if !ok {
					return
				}
				k := ev.Kustomization
				if k.ReadyStatus != "False" {
					continue
				}
				fe := providers.FailureEvent{
					Workload: providers.Workload{Kind: "Kustomization", Name: k.Name, Namespace: k.Namespace},
					Engine:   providers.EngineFlux,
					Reason:   k.ReadyReason,
					Message:  k.ReadyMessage,
				}
				select {
				case out <- fe:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}
```

(The existing `fakeReader` in `flux_test.go` now also needs a `WatchKustomizations` method to satisfy the interface — add a trivial one returning a closed channel, OR rely on `streamReader`. If `TestProviderChanges`'s `fakeReader` no longer satisfies `Reader`, add to `flux_test.go`:

```go
func (f fakeReader) WatchKustomizations(context.Context) (<-chan KustomizationEvent, error) {
	ch := make(chan KustomizationEvent)
	close(ch)
	return ch, nil
}
```
)

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -v`
Expected: PASS — `TestWatchFailures` plus all existing tests.

- [ ] **Step 5: Full gate + commit**

Run: `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all clean, `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/providers/gitops/flux/
git commit -m "feat(gitops/flux): WatchFailures emits FailureEvent on Kustomization Ready=False"
```

---

## Task 3: `dynamicReader.WatchKustomizations` over the dynamic client

**Files:**
- Modify: `internal/providers/gitops/flux/dynamic.go`
- Test: `internal/providers/gitops/flux/dynamic_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/providers/gitops/flux/dynamic_test.go`:

```go
func TestDynamicReaderWatch(t *testing.T) {
	gvrToListKind := map[schema.GroupVersionResource]string{
		kustomizationGVR: "KustomizationList",
		gitRepositoryGVR: "GitRepositoryList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind)
	r := NewDynamicReader(client)

	ch, err := r.WatchKustomizations(context.Background())
	if err != nil {
		t.Fatalf("WatchKustomizations: %v", err)
	}

	// Create a failing Kustomization via the fake client; the watch should surface it.
	bad := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "bad", "namespace": "apps"},
		"spec":       map[string]any{"path": "./apps", "sourceRef": map[string]any{"name": "flux-system"}},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed", "message": "boom"},
		}},
	}}
	if _, err := client.Resource(kustomizationGVR).Namespace("apps").Create(context.Background(), bad, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Kustomization.Name != "bad" || ev.Kustomization.ReadyStatus != "False" {
			t.Fatalf("unexpected event: %+v", ev.Kustomization)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}
}
```

Add `"time"` to the test file's imports.

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -run TestDynamicReaderWatch -v`
Expected: FAIL — `WatchKustomizations` not implemented on `dynamicReader`.

- [ ] **Step 3: Implement `WatchKustomizations`**

In `internal/providers/gitops/flux/dynamic.go`, add (and import `"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"` is already there; add `"k8s.io/apimachinery/pkg/watch"`):

```go
// WatchKustomizations watches all Kustomizations and forwards each add/modify as
// a KustomizationEvent. The channel closes when the underlying watch stops or ctx
// is done.
func (r *dynamicReader) WatchKustomizations(ctx context.Context) (<-chan KustomizationEvent, error) {
	w, err := r.client.Resource(kustomizationGVR).Namespace(metav1.NamespaceAll).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("watch kustomizations: %w", err)
	}
	out := make(chan KustomizationEvent)
	go func() {
		defer close(out)
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-w.ResultChan():
				if !ok {
					return
				}
				if e.Type != watch.Added && e.Type != watch.Modified {
					continue
				}
				u, ok := e.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				select {
				case out <- KustomizationEvent{Kustomization: kustomizationFromUnstructured(u)}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/smana/Sources/runlore && go test ./internal/providers/gitops/flux/ -run TestDynamicReaderWatch -v`
Expected: PASS.

> If the dynamic fake's `Watch` doesn't emit `Create`d objects in this client-go version, adjust the test to use the fake's watch-reactor/tracker to inject the event, keeping the assertions identical. The production `WatchKustomizations` code (real dynamic client) is unaffected.

- [ ] **Step 5: Tidy, full gate, commit**

Run:
```bash
cd /home/smana/Sources/runlore
go mod tidy
go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l . && golangci-lint run ./...
```
Expected: all clean, `0 issues`.

```bash
git add internal/providers/gitops/flux/ go.mod go.sum
git commit -m "feat(gitops/flux): dynamic-client watch for Kustomization failures"
```

---

## What this plan delivers

The Flux provider's `WatchFailures` is now real: a bad GitOps rollout (`Ready=False`) emits a `FailureEvent`, ready to drive an investigation — the **GitOps-failure React trigger** from the design. Combined with the incident webhook (Phase 1) and the what-changed spine, RunLore can now react to a failed reconcile *before* a metrics alert.

## Next plans (not in this plan)

- Wire `WatchFailures` into `lore serve` (start the watch, route events into the investigation queue).
- HelmRelease failures (`Ready=False`/`RemediationFailed`) + GitRepository `FetchFailed` (source-level failures).
- ArgoCD `Application` health=Degraded / sync=OutOfSync.

---

## Self-Review

- **Spec coverage:** Implements the Flux `WatchFailures` React trigger (Kustomization `Ready=False` → `FailureEvent`), keeping the `Reader`-abstraction so the mapping is unit-tested without a live cluster. HelmRelease/GitRepository/ArgoCD and the `serve` wiring are named follow-ups. ✅
- **Placeholder scan:** Complete code in every step; the two transitional notes (fake-watch latitude; the `fakeReader` interface-satisfaction addition) are explicit, not missing work. ✅
- **Type consistency:** `kustomization` Ready fields (Task 1) consumed by `WatchFailures` (Task 2) and populated by `kustomizationFromUnstructured` (Task 1) + the watch (Task 3); `KustomizationEvent` and the extended `Reader` interface are consistent across the fake (Task 2) and dynamic (Task 3) implementations. ✅
