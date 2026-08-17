# Thread Interaction PR2 — Matrix Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A human replying in a Matrix thread with `@runlore note: <text>` gets that text written into the knowledge base, exactly as PR1 delivers for Slack.

**Architecture:** PR1's `internal/thread` core is transport-agnostic and is reused unchanged in substance. Matrix needs no new inbound surface and no new permission: the existing `MatrixFeedback` `/sync` long-poll already reads the room, and `Matrix.Deliver` already stamps a custom event field that the reaction listener reads back. This PR widens both, adds a reply path, and extracts PR1's bounded-dispatch machinery out of `internal/server` so both transports share it.

**Tech Stack:** Go 1.26, stdlib only. Matrix Client-Server API v3 (`/sync`, `/rooms/{id}/send`, `/rooms/{id}/event/{id}`, `/account/whoami`). Existing internals: `internal/thread` (PR1 core), `internal/notify` (Matrix + Slack notifiers), `internal/config`, `internal/app`.

Source spec: `dev/superpowers/specs/2026-08-14-thread-interaction-design.md` (§Matrix transport).
Predecessor: `dev/superpowers/plans/2026-08-14-thread-interaction-pr1.md`.

**Branch:** `feat/thread-interaction-pr2-matrix`, stacked on `feat/thread-interaction-pr1`. Worktree `/home/smana/Sources/runlore.worktrees/matrix-thread`. Rebase onto PR1's final head before Task 1 if PR1 has moved.

## Global Constraints

- **Quality gate, run before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`. `gofmt -l .` must print nothing; golangci-lint must report `0 issues`.
- **TDD.** Failing test first, then the minimal implementation. Prefer table-driven tests. Tests verify behaviour, not mocks.
- **Every file starts with** `// SPDX-License-Identifier: Apache-2.0`.
- **Every exported symbol carries a doc comment** (enforced by `revive`).
- **Errors** wrap with `%w`; compare with `errors.Is` / `errors.As` (enforced by `errorlint`).
- **`context.Context` is the first parameter** of any function that does I/O.
- Module path `github.com/Smana/runlore`. No new third-party dependencies.
- **Nothing in this PR may block or fail investigation delivery.** Every new call site is best-effort relative to its host path.
- **The existing 👍/👎 reaction listener must keep working unchanged.** Its `io.runlore.trigger_key` field and its behaviour are load-bearing and already shipped.
- `notify.matrix.thread_capture` defaults to `false`.
- Known lint traps in this repo: `revive` flags identifiers shadowing Go builtins (`max`, `min`, `clear`); staticcheck QF1012 flags `b.WriteString(fmt.Sprintf(...))` — use `fmt.Fprintf(&b, ...)`; `gosec` G304 on file opens built from a variable (see the `//nolint:gosec` precedent in `internal/outcome/ledger.go`).
- The Hugo on `PATH` is 0.139.0 and **cannot** build this site (theme needs ≥0.146.0). Use `/usr/bin/hugo` (0.164.0), and build to a directory outside the repo so `website/public/` is not dirtied.

---

## Three problems this plan solves that the spec did not anticipate

Read these before Task 1; they explain why tasks 1 and 4 exist at all.

1. **`Multi.ThreadReplier()` is ambiguous with two transports.** `internal/notify/slack.go:932-939` returns the *first* notifier implementing `providers.ThreadNotifier`. In PR1 only `SlackBot` implements it, so it is unambiguous. The moment `Matrix` implements it too, a deployment configuring both gets a single replier for both transports — a Matrix thread would be answered via Slack, or not at all. Task 4 fixes this.
2. **The Matrix sync loop is single-goroutine and synchronous.** `MatrixFeedback.Run` (`matrix_feedback.go:135-140`) calls `handleReaction` inline. A note that opens a PR costs ~6 sequential forge calls, so handling a mention inline would stall long-polling for minutes — pausing 👍/👎 recording and making the room look dead. PR1 already built exactly the right machinery (bounded slots, `Busy` reply, per-handler timeout, drain on shutdown) but it lives inside `internal/server`. Task 1 extracts it so both transports share one implementation.
3. **`Matrix.Deliver` discards its send response** (`matrix.go:103-111`), so it never learns the `event_id` of the message it posted — and that id *is* the thread root. Task 2 decodes it.

**One design refinement over the spec.** The spec said Matrix needs no registry for lookup because context rides on the event. That is half right. `Responder.Handle` writes `Notes` and `NoteURL` back through `Registry.Update`, and a miss there is a silent no-op — so without registry entries every Matrix note would open a fresh PR. Matrix therefore registers on delivery like Slack does, **and** stamps the context onto the event. Registry first, event-stamp as fallback: that combination survives a restart or leader failover, which neither does alone, and is a genuine advantage Matrix has over Slack.

---

## File Structure

**Create:**

| File | Responsibility |
|---|---|
| `internal/thread/dispatch.go` | `Dispatcher` — bounded, timeout-bounded, drainable detached execution. Extracted from `internal/server`; used by both transports. |
| `internal/thread/dispatch_test.go` | Its tests. |
| `internal/notify/matrix_thread.go` | Matrix thread capture: the `io.runlore.thread` content field, `contextFor`, message-event handling, `ReplyInThread`. Kept out of `matrix_feedback.go` so the reaction listener stays readable. |
| `internal/notify/matrix_thread_test.go` | Its tests. |

**Modify:**

| File | Change |
|---|---|
| `internal/server/server.go` | Replace the inline slot pool with `thread.Dispatcher`. Behaviour identical. |
| `internal/notify/matrix.go` | Decode `event_id` from the send response; stamp `io.runlore.thread`; add `Threads ThreadSink`; register the root after a successful send. |
| `internal/notify/matrix_feedback.go` | Widen `matrixEvent` and the `/sync` filter; generalise `triggerKeyFor`; route `m.room.message` to the mention handler via the dispatcher. |
| `internal/notify/slack.go` | Replace `Multi.ThreadReplier()` with a transport-scoped lookup. |
| `internal/config/config.go` | `MatrixNotify.ThreadCapture` + `Validate` rules. |
| `internal/app/notify.go`, `internal/app/serve.go` | Wire the Matrix listener's thread capture. |
| `website/content/docs/…`, `SECURITY.md` | Setup docs + the sync-filter disclosure. |

---

### Task 1: Move the bounded dispatcher into `internal/thread`

**Read this first — the plan's earlier description of this task was written against older code.**
`internal/server` no longer has an *inline* pool. PR1's final commits already factored out:

- `func (s *Server) dispatch(slots chan struct{}, fn func()) bool` (`server.go:574`) — non-blocking
  slot acquire, `s.wg.Add/Done`, slot release, panic recovery, returns whether `fn` started.
- `func (s *Server) Drain(ctx context.Context)` (`server.go:604`) — `s.wg.Wait()` raced against `ctx`.
- Fields `eventSlots`, a second small slots channel for the "busy" reply, `wg sync.WaitGroup`,
  `mentionTimeout`.

So this task is a **move**, not an extraction from tangled code. Matrix needs the identical
behaviour, and a second copy would guarantee the two drift.

**One deliberate behaviour change, and it must not be smuggled in silently.** Today the timeout is
built at the *call site* (`server.go:541`):

```go
ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.mentionTimeout)
```

and that single `ctx`/`cancel` pair is shared by the mention closure, the busy closure, and a
trailing call — guarded by a comment promising "cancel is always invoked by exactly one of the
three paths below … never left uncalled". That is a real invariant held together by hand.

Moving the timeout *inside* the dispatcher gives each dispatched unit its own context and its own
`cancel`, deleting the three-path dance. The busy reply then gets its own timeout rather than
inheriting the mention's remaining budget. **That is an improvement, not a regression — but it IS a
change**, so Step 7 below is "prove the observable behaviour is unchanged", not "prove nothing
changed at all". Call it out in the commit message.

**Files:**
- Create: `internal/thread/dispatch.go`
- Create: `internal/thread/dispatch_test.go`
- Modify: `internal/server/server.go` — replace `dispatch`, `Drain`, `wg`, the slots channels and
  the call-site timeout with two `*thread.Dispatcher` instances (one for mentions, one for the
  busy reply), preserving the existing slot counts, timeout value and log levels exactly.
- Modify: `internal/app/serve.go` — the shutdown drain now drains both dispatchers.

**Interfaces:**
- Produces:
  - `func NewDispatcher(slots int, timeout time.Duration, log *slog.Logger) *Dispatcher`
  - `func (d *Dispatcher) Go(ctx context.Context, fn func(context.Context)) bool`
  - `func (d *Dispatcher) Drain(ctx context.Context)`

`Go` applies `context.WithoutCancel` then `context.WithTimeout` internally and hands the result to
`fn`; it returns `false` immediately, having run nothing, when every slot is busy.

Keep the existing constants where they are defined today (`maxConcurrentMentions`,
`mentionHandlerTimeout`, the busy pool's size) — move only the mechanism, not the policy.

- [ ] **Step 1: Read what exists**

Run: `grep -n "dispatch\|eventSlots\|mentionTimeout\|wg \|func (s \*Server) Drain" internal/server/server.go`
Expected: the helper, both slots channels, the WaitGroup, the timeout field, and the Drain method.
Note the exact slot counts, the timeout value and the log levels — all three must survive the move.

- [ ] **Step 2: Write the failing test**

Create `internal/thread/dispatch_test.go`. These eight tests encode the three bounds and the two
subtleties that matter — a slot must survive a panic, and work must outlive the caller's request
context:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDispatcherRunsWork(t *testing.T) {
	d := NewDispatcher(2, time.Minute, testLogger())
	done := make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(done) }) {
		t.Fatal("Go returned false with free slots")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("work never ran")
	}
}

func TestDispatcherRefusesWhenSaturated(t *testing.T) {
	d := NewDispatcher(1, time.Minute, testLogger())
	block, running := make(chan struct{}), make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(running); <-block }) {
		t.Fatal("first Go refused")
	}
	<-running
	if d.Go(context.Background(), func(context.Context) { t.Error("must not run when saturated") }) {
		t.Fatal("Go returned true while saturated")
	}
	close(block)
}

func TestDispatcherSlotIsReleasedAfterWork(t *testing.T) {
	d := NewDispatcher(1, time.Minute, testLogger())
	for i := 0; i < 3; i++ {
		done := make(chan struct{})
		if !d.Go(context.Background(), func(context.Context) { close(done) }) {
			t.Fatalf("Go %d refused: the slot was not released", i)
		}
		<-done
		d.Drain(context.Background())
	}
}

func TestDispatcherSlotIsReleasedAfterPanic(t *testing.T) {
	d := NewDispatcher(1, time.Minute, testLogger())
	panicked := make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(panicked); panic("boom") }) {
		t.Fatal("first Go refused")
	}
	<-panicked
	d.Drain(context.Background())
	done := make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(done) }) {
		t.Fatal("slot leaked after a panic")
	}
	<-done
}

func TestDispatcherAppliesTimeout(t *testing.T) {
	d := NewDispatcher(1, 50*time.Millisecond, testLogger())
	var cancelled atomic.Bool
	done := make(chan struct{})
	d.Go(context.Background(), func(ctx context.Context) {
		<-ctx.Done()
		cancelled.Store(true)
		close(done)
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the timeout never fired")
	}
	if !cancelled.Load() {
		t.Fatal("work context was not cancelled by the timeout")
	}
}

func TestDispatcherWorkOutlivesTheCallersContext(t *testing.T) {
	// The caller's context is a request context that is cancelled the moment the
	// handler returns. Work must NOT die with it — only with the dispatcher's own
	// timeout — or every mention would be cancelled before it could be written.
	d := NewDispatcher(1, time.Minute, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	started, result := make(chan struct{}), make(chan error, 1)
	d.Go(ctx, func(wctx context.Context) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		result <- wctx.Err()
	})
	<-started
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("work context was cancelled with the caller's: %v", err)
	}
}

func TestDispatcherDrainWaitsForInFlightWork(t *testing.T) {
	d := NewDispatcher(2, time.Minute, testLogger())
	var finished atomic.Int32
	release := make(chan struct{})
	for i := 0; i < 2; i++ {
		d.Go(context.Background(), func(context.Context) { <-release; finished.Add(1) })
	}
	go func() { time.Sleep(50 * time.Millisecond); close(release) }()
	d.Drain(context.Background())
	if got := finished.Load(); got != 2 {
		t.Fatalf("Drain returned with %d/2 finished", got)
	}
}

func TestDispatcherDrainIsBounded(t *testing.T) {
	d := NewDispatcher(1, time.Minute, testLogger())
	block := make(chan struct{})
	defer close(block)
	d.Go(context.Background(), func(context.Context) { <-block })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	d.Drain(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Drain blocked %v on a wedged handler; it must honour its context", elapsed)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestDispatcher -v`
Expected: FAIL — `undefined: NewDispatcher`.

- [ ] **Step 4: Write `internal/thread/dispatch.go`**

Create `internal/thread/dispatch.go`. Its `Go`/`Drain` bodies are line-for-line equivalent to the
existing `Server.dispatch`/`Server.Drain` plus the internalised timeout:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Dispatcher runs detached work under three bounds: how much runs at once, how
// long any single piece may run, and a drain that lets shutdown wait for what is
// in flight.
//
// It exists because both transports need identical behaviour here. A chat
// transport acknowledges the platform BEFORE doing the work (Slack retries
// anything unacked within 3 seconds; the Matrix sync loop must keep
// long-polling), so the work necessarily outlives the call that scheduled it —
// and unbounded detached goroutines on an internet-facing path is its own
// problem. Duplicating this per transport would guarantee the two drift.
type Dispatcher struct {
	slots   chan struct{}
	timeout time.Duration
	wg      sync.WaitGroup
	log     *slog.Logger
}

// NewDispatcher returns a Dispatcher allowing `slots` concurrent pieces of work,
// each bounded by `timeout`.
func NewDispatcher(slots int, timeout time.Duration, log *slog.Logger) *Dispatcher {
	if slots <= 0 {
		slots = 1
	}
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{slots: make(chan struct{}, slots), timeout: timeout, log: log}
}

// Go runs fn on a bounded worker and reports whether it was accepted. It never
// blocks: when every slot is busy it returns false immediately, having run
// nothing, so the caller can tell the human rather than queue silently.
//
// fn receives a context derived from ctx with cancellation stripped and the
// dispatcher's timeout applied. Stripping cancellation is deliberate: ctx is
// typically a request context that dies the moment its handler returns, which
// would cancel the work before it could do anything.
func (d *Dispatcher) Go(ctx context.Context, fn func(context.Context)) bool {
	select {
	case d.slots <- struct{}{}:
	default:
		return false
	}
	work := context.WithoutCancel(ctx)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() { <-d.slots }()
		defer func() {
			if rec := recover(); rec != nil {
				d.log.Error("recovered from panic in detached work", "panic", rec, "stack", string(debug.Stack()))
			}
		}()
		if d.timeout > 0 {
			var cancel context.CancelFunc
			work, cancel = context.WithTimeout(work, d.timeout)
			defer cancel()
		}
		fn(work)
	}()
	return true
}

// Drain waits for in-flight work to finish, bounded by ctx. A wedged handler
// must not be able to block shutdown forever.
func (d *Dispatcher) Drain(ctx context.Context) {
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		d.log.Warn("drain timed out with work still in flight")
	}
}
```

- [ ] **Step 5: Run the dispatcher tests**

Run: `go test ./internal/thread/ -run TestDispatcher -v -race`
Expected: PASS — all eight.

- [ ] **Step 6: Move `internal/server` onto it**

Replace the two slots channels, `wg`, `dispatch` and `Drain` with two `*thread.Dispatcher` fields.
`Server.Drain` stays as the public entry point and delegates to both, so `internal/app/serve.go`'s
call site keeps working — check whether it needs a second call and update it if so.

At the saturation path, keep everything exactly as it is: `Error`-level log, the
`MentionsDroppedOnSaturation` metric increment, then the `Busy` reply. `Go` returning `false` is
the saturation signal, replacing `dispatch` returning `false`.

Delete the now-unnecessary shared-`cancel` comment at `server.go:534-540` — it documents an
invariant that no longer exists. Leaving it would be worse than never having written it.

- [ ] **Step 7: Prove the observable behaviour is unchanged**

Run: `go test ./internal/server/ ./internal/thread/ ./internal/app/ -race`
Expected: PASS — every pre-existing server test, unedited.

If a test needs changing for any reason other than a constructor signature, stop and report it:
that means the move altered behaviour the tests pin. The one sanctioned difference is the busy
reply now having its own timeout instead of sharing the mention's; if a test pins the shared-cancel
behaviour, say so in your report rather than quietly rewriting it.

- [ ] **Step 8: Full gate and commit**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`

```bash
git add internal/thread/dispatch.go internal/thread/dispatch_test.go internal/server/server.go internal/app/serve.go
git commit -m "refactor(thread): move the bounded dispatcher out of internal/server

Slack acks before working and the Matrix sync loop must keep long-polling, so
in both cases the work outlives the call that scheduled it — under the same
three bounds: concurrency, per-item timeout, and a drain shutdown can wait on.
A second copy for Matrix would guarantee the two drift.

One deliberate change: the timeout moves from the call site into the
dispatcher, so each dispatched unit owns its own context and cancel. That
removes the hand-held invariant that one shared cancel was invoked by exactly
one of three paths, and gives the busy reply its own budget rather than the
mention's remainder."
```

---

### Task 2: `Matrix.Deliver` learns its own event id, stamps the context, and registers the root

**Files:**
- Modify: `internal/notify/matrix.go`
- Create: `internal/notify/matrix_thread.go` (the content-field constant and the payload type)
- Test: `internal/notify/matrix_test.go`

**Interfaces:**
- Consumes: `notify.ThreadSink` (PR1), `thread.Context` (PR1).
- Produces:
  - `const threadContentField = "io.runlore.thread"`
  - `type threadStamp struct { … }` with JSON tags — the on-event payload
  - `Matrix.Threads ThreadSink` field
  - `Matrix.Deliver` returns after registering the sent event id as the thread root

- [ ] **Step 1: Write the failing test**

Append to `internal/notify/matrix_test.go`:

```go
func TestMatrixDeliverRegistersTheEventAsThreadRoot(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"event_id":"$evt123"}`))
	}))
	defer srv.Close()

	sink := &recordingThreadSink{}
	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	m.Threads = sink

	inv := providers.Investigation{Title: "OOMKilled", TriggerKey: "tk-1", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := m.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if sink.calls != 1 {
		t.Fatalf("Register calls = %d, want 1", sink.calls)
	}
	if sink.root != "$evt123" {
		t.Errorf("root = %q, want the sent event id $evt123", sink.root)
	}
	if sink.channel != "!room:example.org" {
		t.Errorf("channel = %q, want the room id", sink.channel)
	}
	if body[threadContentField] == nil {
		t.Fatalf("the event content must carry %s: %v", threadContentField, body)
	}
	if body[triggerKeyContentField] != "tk-1" {
		t.Errorf("the pre-existing trigger-key field must be unchanged, got %v", body[triggerKeyContentField])
	}
}

func TestMatrixDeliverSucceedsWithNoThreadSink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"event_id":"$evt123"}`))
	}))
	defer srv.Close()
	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	if err := m.Deliver(context.Background(), providers.Investigation{Title: "OOMKilled"}); err != nil {
		t.Fatalf("Deliver with a nil thread sink: %v", err)
	}
}

func TestMatrixDeliverSucceedsWhenTheResponseHasNoEventID(t *testing.T) {
	// A homeserver that returns 2xx with an unexpected body must not fail
	// delivery — the message was sent; only the thread root is unknown.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	sink := &recordingThreadSink{}
	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	m.Threads = sink
	if err := m.Deliver(context.Background(), providers.Investigation{Title: "OOMKilled"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if sink.calls != 0 {
		t.Fatal("an empty event id must never be registered")
	}
}
```

`recordingThreadSink` already exists in `internal/notify/slack_test.go` (same package) — reuse it, do not redefine it. Check its field names and match them.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/notify/ -run TestMatrixDeliver -v`
Expected: FAIL — `m.Threads undefined`, `undefined: threadContentField`.

- [ ] **Step 3: Create the content-field payload**

Create `internal/notify/matrix_thread.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// threadContentField is the custom event-content field Deliver stamps on
// investigation messages so a threaded reply can be attributed to the
// investigation it answers — the same mechanism triggerKeyContentField already
// uses for 👍/👎, widened to carry the whole thread context.
//
// It is ADDITIVE: triggerKeyContentField keeps its exact meaning and value, so
// the reaction listener and every event sent before this field existed behave
// unchanged. Namespaced per the Matrix convention for custom keys.
const threadContentField = "io.runlore.thread"

// threadStamp is the thread context as carried on a Matrix event. It holds
// identifiers only — never prose — so the event stays small and nothing
// sensitive is duplicated into room history that is not already in the message.
type threadStamp struct {
	TriggerKey     string `json:"trigger_key,omitempty"`
	DupFingerprint string `json:"dup_fingerprint,omitempty"`
	Title          string `json:"title,omitempty"`
	Resource       string `json:"resource,omitempty"`
	Verdict        string `json:"verdict,omitempty"`
	CuratedURL     string `json:"curated_url,omitempty"`
	RecalledEntry  string `json:"recalled_entry,omitempty"`
}

// stampFor renders the investigation's thread identifiers for the event content.
func stampFor(inv providers.Investigation) threadStamp {
	return threadStamp{
		TriggerKey:     inv.TriggerKey,
		Title:          inv.Title,
		Resource:       inv.Resource.Ref(),
		Verdict:        string(inv.Verdict),
		CuratedURL:     inv.CuratedURL,
		RecalledEntry:  inv.RecalledEntry,
	}
}

// contextFromStamp rebuilds a thread.Context from a stamped event. root and
// room come from the event itself, not from the stamp, so a forged stamp cannot
// redirect where a note is written.
func contextFromStamp(s threadStamp, root, room string) thread.Context {
	return thread.Context{
		Transport:      "matrix",
		Root:           root,
		Channel:        room,
		TriggerKey:     s.TriggerKey,
		DupFingerprint: s.DupFingerprint,
		Title:          s.Title,
		Resource:       s.Resource,
		Verdict:        providers.Verdict(s.Verdict),
		CuratedURL:     s.CuratedURL,
		RecalledEntry:  s.RecalledEntry,
	}
}
```

- [ ] **Step 4: Wire it into `Matrix.Deliver`**

In `internal/notify/matrix.go`: add a `Threads ThreadSink` field to `Matrix` with a doc comment matching `SlackBot.Threads`'s. Stamp `content[threadContentField] = stampFor(inv)` alongside the existing trigger-key stamp — unconditionally, like that one, because the field is inert data and the *listener* is the opt-in. Decode the send response into `struct{ EventID string \`json:"event_id"\` }`, and when both `Threads != nil` and the id is non-empty, call `s.Threads.Register(eventID, m.roomID, inv)`.

Registration is best-effort and must not affect the returned error: a send that succeeded stays succeeded.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/notify/ -run TestMatrix -v`
Expected: PASS — the three new tests plus every pre-existing Matrix test, including the reaction-listener ones.

- [ ] **Step 6: Full gate and commit**

```bash
git add internal/notify/matrix.go internal/notify/matrix_thread.go internal/notify/matrix_test.go
git commit -m "feat(notify): stamp the thread context on Matrix investigation events

Deliver discarded its send response, so it never learned the event id of the
message it had just posted — and that id is the thread root. Decode it, stamp
the thread identifiers alongside the existing trigger-key field, and register
the root. The trigger-key field is untouched, so the reaction listener and
every previously-sent event behave exactly as before."
```

---

### Task 3: `Matrix.ReplyInThread`

**Files:**
- Modify: `internal/notify/matrix.go`
- Test: `internal/notify/matrix_test.go`

**Interfaces:**
- Consumes: `providers.ThreadNotifier` (PR1).
- Produces: `func (m *Matrix) ReplyInThread(ctx context.Context, root, channel, text string) error`, and `var _ providers.ThreadNotifier = (*Matrix)(nil)`.

- [ ] **Step 1: Write the failing test**

```go
func TestMatrixReplyInThread(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"event_id":"$reply1"}`))
	}))
	defer srv.Close()

	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	if err := m.ReplyInThread(context.Background(), "$evt123", "!room:example.org", "📝 Noted"); err != nil {
		t.Fatalf("ReplyInThread: %v", err)
	}

	rel, ok := body["m.relates_to"].(map[string]any)
	if !ok {
		t.Fatalf("reply must carry m.relates_to: %v", body)
	}
	if rel["rel_type"] != "m.thread" {
		t.Errorf("rel_type = %v, want m.thread", rel["rel_type"])
	}
	if rel["event_id"] != "$evt123" {
		t.Errorf("event_id = %v, want the thread root $evt123", rel["event_id"])
	}
	if body["body"] != "📝 Noted" {
		t.Errorf("body = %v, want the reply text", body["body"])
	}
}

func TestMatrixReplyInThreadReportsSendFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	if err := m.ReplyInThread(context.Background(), "$evt123", "!room:example.org", "x"); err == nil {
		t.Fatal("a non-2xx send must be reported")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/notify/ -run TestMatrixReplyInThread -v`
Expected: FAIL — `m.ReplyInThread undefined`.

- [ ] **Step 3: Implement it**

Post an `m.room.message` with `msgtype: "m.notice"` (matching `Deliver`, so a reply is not treated as a user message by clients), the plain `body`, and:

```go
"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root},
```

Reuse `Deliver`'s transaction-id and send mechanics rather than duplicating the HTTP call — extract a small unexported `send(ctx, content map[string]any) (string, error)` that both use, returning the event id. Target the passed `channel` when non-empty, falling back to `m.roomID`, mirroring `SlackBot.ReplyInThread`.

Reply text is plain (no `formatted_body`): the responder's replies are RunLore's own short acknowledgements, not investigation prose.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/notify/ -run TestMatrix -v -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/notify/matrix.go internal/notify/matrix_test.go
git commit -m "feat(notify): reply into a Matrix thread"
```

---

### Task 4: Make the replier lookup transport-aware

`Multi.ThreadReplier()` returns the first notifier implementing `providers.ThreadNotifier` (`internal/notify/slack.go:932-939`). With Task 3 done, a deployment configuring **both** Slack and Matrix has two — and gets whichever is registered first for both transports. A Matrix thread would be answered via Slack, or not at all.

**Files:**
- Modify: `internal/notify/slack.go` (the `Multi` methods)
- Modify: `internal/app/serve.go` (the call site)
- Test: `internal/notify/slack_test.go`

**Interfaces:**
- Produces: `func (m *Multi) ThreadRepliers() map[string]providers.ThreadNotifier` — transport name → replier. `ThreadReplier()` is removed.
- Requires: `providers.ThreadNotifier` gains `Transport() string`, implemented as `"slack"` by `SlackBot` and `"matrix"` by `Matrix`.

- [ ] **Step 1: Write the failing test**

```go
func TestMultiThreadRepliersAreTransportScoped(t *testing.T) {
	bot := NewSlackBot("xoxb-test", "C1")
	mx := NewMatrix("https://hs.example.org", "!room:example.org", "tok")
	m := NewMulti(testNotifyLogger(), NewSlack("https://hooks.slack.com/x"), bot, mx)

	got := m.ThreadRepliers()
	if len(got) != 2 {
		t.Fatalf("ThreadRepliers = %d entries, want 2", len(got))
	}
	if got["slack"] != providers.ThreadNotifier(bot) {
		t.Errorf("slack replier = %v, want the SlackBot", got["slack"])
	}
	if got["matrix"] != providers.ThreadNotifier(mx) {
		t.Errorf("matrix replier = %v, want the Matrix notifier", got["matrix"])
	}
}

func TestMultiThreadRepliersEmptyWhenNoneCanReply(t *testing.T) {
	m := NewMulti(testNotifyLogger(), NewSlack("https://hooks.slack.com/x"))
	if got := m.ThreadRepliers(); len(got) != 0 {
		t.Fatalf("ThreadRepliers = %v, want empty", got)
	}
}
```

Use whatever discard-logger helper `slack_test.go` already defines; if there is none, construct `slog.New(slog.NewTextHandler(io.Discard, nil))` inline rather than adding a helper.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/notify/ -run TestMultiThreadRepliers -v`
Expected: FAIL — `m.ThreadRepliers undefined`.

- [ ] **Step 3: Add `Transport()` to the capability**

In `internal/providers/providers.go`, add to `ThreadNotifier`:

```go
	// Transport names the chat system this notifier replies on ("slack",
	// "matrix"). It is what lets a deployment running several transports route
	// each thread's reply back to the system the human is actually in — the
	// alternative, picking the first notifier that can reply, silently answers
	// one transport's threads on another.
	Transport() string
```

Implement `func (s *SlackBot) Transport() string { return "slack" }` and `func (m *Matrix) Transport() string { return "matrix" }`, each with a doc comment.

Replace `Multi.ThreadReplier` with `ThreadRepliers`, keyed by `Transport()`. Where two notifiers report the same transport, keep the first and log a warning naming the collision — silently dropping one would be the same class of bug this task fixes.

- [ ] **Step 4: Update the wiring**

In `internal/app/serve.go`, take the Slack replier from `ThreadRepliers()["slack"]`. Keep the existing "no thread-capable notifier resolved" warning behaviour for the case where the map has no entry for the transport being wired.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/notify/ ./internal/app/ -race`
Expected: PASS, including PR1's Slack-only wiring tests.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/providers.go internal/notify/slack.go internal/notify/matrix.go internal/notify/slack_test.go internal/app/serve.go
git commit -m "fix(notify): scope the thread replier by transport

ThreadReplier returned the first notifier that could reply, which was
unambiguous only while Slack was the sole implementation. With Matrix able to
reply too, a deployment running both would answer one transport's threads on
the other. Key the lookup by transport instead."
```

---

### Task 5: `contextFor` — read the thread context back off an event

Generalise `MatrixFeedback.triggerKeyFor` so the same fetch, the same self-check and the same cache serve both the reaction listener and thread capture.

**Files:**
- Modify: `internal/notify/matrix_feedback.go`
- Modify: `internal/notify/matrix_thread.go`
- Test: `internal/notify/matrix_feedback_test.go`

**Interfaces:**
- Produces: `func (f *MatrixFeedback) contextFor(ctx context.Context, eventID string) (thread.Context, bool, error)` — the second return is `false` when the event is not one of RunLore's own investigation messages. `triggerKeyFor` keeps working, implemented on top of the same fetch.

- [ ] **Step 1: Write the failing test**

Cover, at minimum: an event stamped with `io.runlore.thread` and sent by the bot yields a populated `thread.Context` with `Root` and `Channel` taken from the fetch parameters, not the stamp; **an event carrying the field but sent by somebody else yields `false`** (the trust anchor — a room member must not be able to stamp their own message and redirect where notes are written); an event with only the legacy `io.runlore.trigger_key` still yields a context carrying that key; an unstamped event yields `false`; and the existing `triggerKeyFor` behaviour is unchanged for all four.

Follow the HTTP-fake pattern already in `matrix_feedback_test.go`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/notify/ -run TestMatrixContextFor -v`
Expected: FAIL — `f.contextFor undefined`.

- [ ] **Step 3: Implement**

Refactor the fetch in `triggerKeyFor` into an unexported `fetchEvent(ctx, eventID) (sender string, content map[string]any, err error)`. Build `contextFor` on it: decode `content[threadContentField]` into a `threadStamp` (re-marshal the `any` through `json` rather than hand-casting), fall back to `content[triggerKeyContentField]` when the thread field is absent, and return `false` when `sender != f.self` — the identical anchor `triggerKeyFor` already enforces, for the identical reason.

Change the cache from `map[string]string` to hold the resolved context, keeping the same crude cap-and-reset bound and the same "the live working set is tiny" comment. **The cache is currently unsynchronised because only the sync goroutine touches it** — keep every `contextFor` call on the sync goroutine (Task 6 detaches the *handling*, not the lookup), or add a mutex. State which you chose in a comment.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/notify/ -race -v`
Expected: PASS, including every pre-existing reaction test.

- [ ] **Step 5: Commit**

```bash
git add internal/notify/matrix_feedback.go internal/notify/matrix_thread.go internal/notify/matrix_feedback_test.go
git commit -m "feat(notify): read the thread context back off a Matrix event

One fetch, one self-check, one cache, now serving both the reaction listener
and thread capture. The trust anchor is unchanged and load-bearing: a stamped
event that RunLore did not send attributes nothing, or any room member could
stamp their own message and redirect where a note is written."
```

---

### Task 6: Handle `m.room.message` in the sync loop

**Files:**
- Modify: `internal/notify/matrix_feedback.go`
- Test: `internal/notify/matrix_feedback_test.go`

**Interfaces:**
- Consumes: `contextFor` (Task 5), `thread.Dispatcher` (Task 1), `thread.Mention` (PR1).
- Produces: `MatrixFeedback` gains `Mentions *thread.Mention` and `Dispatch *thread.Dispatcher` fields; `handleMessage(ctx, e matrixEvent)`.

- [ ] **Step 1: Widen the event struct**

`matrixEvent` currently decodes only `Type`, `Sender` and `Content.RelatesTo`. Add the event's own `EventID` (`event_id`), `Content.Body`, `Content.MsgType`, `Content.Mentions.UserIDs` (`m.mentions` → `user_ids`, MSC3952), and `Content.RelatesTo.InReplyTo.EventID` (`m.in_reply_to` → `event_id`).

- [ ] **Step 2: Write the failing test**

Cover: a threaded (`rel_type: m.thread`) message mentioning the bot dispatches to the mention handler with the **root** event id, not the reply's own id; a reply-fallback message (`m.in_reply_to`, no `rel_type`) resolves its root the same way; a message not mentioning the bot dispatches nothing; a message from the bot itself (`sender == self`) dispatches nothing (loop guard); a message with no thread relation dispatches nothing; and a message whose root event is not one of RunLore's dispatches nothing.

Use a fake `thread.Mention` collaborator — or a small interface the listener depends on — rather than a live forge. Make the assertions deterministic: have the fake close a channel the test waits on, never a sleep.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/notify/ -run TestMatrixHandleMessage -v`
Expected: FAIL — `f.handleMessage undefined`.

- [ ] **Step 4: Implement**

In `sync()`, widen the inline filter's `timeline.types` from `["m.reaction"]` to `["m.reaction","m.room.message"]` **only when thread capture is enabled** — a deployment that has not opted in must keep receiving exactly what it receives today. Thread that flag through the constructor.

In `Run`'s event loop, dispatch by `e.Type`: `m.reaction` → `handleReaction` (unchanged, still inline — recording a rating is a local ledger write), `m.room.message` → `handleMessage`.

`handleMessage` must, in order: ignore `sender == f.self`; require the bot to be addressed (`m.mentions.user_ids` contains `f.self`, falling back to the body containing the MXID or its localpart); resolve the thread root (`m.relates_to.rel_type == "m.thread"` → `event_id`; else `m.in_reply_to.event_id`; else return); call `contextFor(root)` **on the sync goroutine** and return when it reports `false`; then hand the work to the dispatcher.

When `Dispatch.Go` returns `false`, call the mention handler's `Busy` — the same contract the Slack path uses, so a saturated Matrix listener tells the human to retype instead of dropping silently.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/notify/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/notify/matrix_feedback.go internal/notify/matrix_feedback_test.go
git commit -m "feat(notify): handle addressed Matrix thread messages

The sync loop now also sees m.room.message when thread capture is on, and
hands an addressed threaded reply to the shared responder. Handling is
detached: a note that opens a PR costs several sequential forge calls, and
doing that inline would stall long-polling and pause feedback recording.
The filter is widened only when the feature is enabled."
```

---

### Task 7: Config — `notify.matrix.thread_capture`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.MatrixNotify.ThreadCapture bool` (yaml `thread_capture`).

- [ ] **Step 1: Write the failing test**

Table-driven, mirroring PR1's `TestValidateThreadCapture`. `thread_capture: true` requires `homeserver`, `room_id`, `access_token_env` (the listener long-polls the configured room and authenticates as the bot) **and** `outcome.ledger_path` (the registry lives beside the ledger; PR1 made this a hard requirement for Slack and the same reasoning applies). Off requires nothing.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestValidateMatrixThreadCapture -v`
Expected: FAIL — `unknown field ThreadCapture`.

- [ ] **Step 3: Implement**

Add the field with a doc comment whose requirement list is **complete** and matches what `Validate` enforces — PR1 shipped a comment that omitted a requirement and a reviewer correctly called it misleading. Add the rules immediately after the existing `matrix.feedback_reactions` block, matching its structure and message style.

- [ ] **Step 4: Run the tests and commit**

Run: `go test ./internal/config/`

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): notify.matrix.thread_capture"
```

---

### Task 8: Wiring, docs, and the sync-filter disclosure

**Files:**
- Modify: `internal/app/notify.go`, `internal/app/serve.go`
- Test: `internal/app/notify_test.go`
- Modify: `website/content/docs/integrations/notifications/matrix.md` (or the existing Matrix page — find it with `grep -rln "feedback_reactions" website/content/`), `website/content/docs/configuration/configuration.md`, `website/content/docs/security/security-model.md`, `SECURITY.md`

- [ ] **Step 1: Write the failing test**

Extend `BuildMatrixFeedback`'s tests: with `thread_capture` on and a registry, forge and replier available, the returned listener carries a non-nil `Mentions` and `Dispatch`; with `thread_capture` off it carries neither and behaves exactly as today; with no Matrix replier resolvable it logs and leaves thread capture off rather than panicking.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run TestBuildMatrixFeedback -v`
Expected: FAIL.

- [ ] **Step 3: Wire it**

Extend `BuildMatrixFeedback` (`internal/app/notify.go:50`) to accept what thread capture needs and populate the listener's `Mentions`/`Dispatch` **only** when `thread_capture` is on, the registry is enabled, a forge is configured and `ThreadRepliers()["matrix"]` is non-nil. Reuse the same `*thread.Responder` and `*thread.Registry` instances the Slack path already builds — one responder, two transports, per the spec. Follow PR1's guard-and-warn structure in `serve.go` rather than inventing a new shape.

Drain the Matrix dispatcher at shutdown alongside the Slack one.

- [ ] **Step 4: Write the docs**

The Matrix integration page gets a "Write knowledge back from a thread" section mirroring the Slack page's: the `note:` grammar, what happens when the finding has no KB PR, the config block, and the fact that no Slack-style endpoint or ingress change is required — this is the transport's genuine advantage and worth stating.

Add `thread_capture` to the Matrix keys in `configuration.md`, matching the treatment `notify.slack.thread_capture` received.

**The disclosure, which is the point of this step.** In `website/content/docs/security/security-model.md` and `SECURITY.md`, state plainly: with `notify.matrix.thread_capture` on, RunLore's `/sync` filter widens from `["m.reaction"]` to also include `["m.room.message"]`, so the process **receives message events** from the configured room where today it receives only reactions. It acts only on messages that address it and are rooted in one of its own messages, and non-matching events are dropped immediately without their bodies being logged — but they do transit the process. Say that the filter is widened only when the feature is enabled. Do not soften it.

- [ ] **Step 5: Verify the docs build**

Run: `cd website && /usr/bin/hugo --quiet --destination /tmp/hugo-pr2-check`
Expected: exit 0, no warnings. (`hugo` on `PATH` is too old — see Global Constraints.) Then confirm the new content rendered: `grep -rl thread_capture /tmp/hugo-pr2-check --include=*.html`.

- [ ] **Step 6: Full gate and commit**

Run: `go build ./... && go vet ./... && go test ./... -race && gofmt -l . && golangci-lint run ./...`

```bash
git add internal/app/ website/ SECURITY.md
git commit -m "feat(app): wire Matrix thread capture

One responder, two transports: Matrix reuses the same responder and registry
the Slack path builds. Documents the sync-filter widening honestly — with the
feature on, the process receives room messages where it previously received
only reactions."
```

---

## Self-Review

**Spec coverage** (§Matrix transport of the design doc):

| Spec requirement | Task |
|---|---|
| Stamp `io.runlore.thread`, keep `io.runlore.trigger_key` unchanged | 2 |
| Generalise `triggerKeyFor` into a context lookup, same fetch + cache | 5 |
| Registry used for the `NoteURL` write-back | 2 (registers on delivery) |
| Widen the `/sync` filter, only when enabled | 6 |
| Addressed detection: `m.mentions` with MXID/localpart fallback | 6 |
| `m.thread` with `m.in_reply_to` fallback | 6 |
| Trust anchor: root event sent by `self` | 5 |
| Loop guard: ignore own messages | 6 |
| Reply via `m.relates_to: {rel_type: m.thread}` | 3 |
| Honest disclosure of the widened filter | 8 |
| `notify.matrix.thread_capture` | 7 |

Not in the spec, added here because implementation requires them: Task 1 (shared dispatcher — the spec assumed handling could be inline), Task 4 (transport-scoped replier — the spec did not foresee the `Multi.ThreadReplier` ambiguity), and Task 2's `event_id` decode (the spec assumed `Deliver` already knew its own event id).

**Placeholder scan:** none — every step names exact files, commands and expected output. Tasks 5, 6 and 8 describe test *cases* rather than pasting full test bodies, because each depends on the fake/HTTP-stub conventions already established in `matrix_feedback_test.go`; the implementer is told to follow those and exactly which behaviours to cover.

**Type consistency:** `threadContentField`, `threadStamp`, `stampFor`, `contextFromStamp`, `Matrix.Threads`, `Matrix.ReplyInThread`, `Matrix.Transport`, `Multi.ThreadRepliers`, `MatrixFeedback.contextFor`, `fetchEvent`, `handleMessage`, `MatrixFeedback.Mentions`, `MatrixFeedback.Dispatch`, `thread.NewDispatcher`, `Dispatcher.Go`, `Dispatcher.Drain` are each declared once and referenced with the same signature throughout. `Dispatcher.Go` returning `bool` (accepted / refused) is relied on by both Task 1's server switch and Task 6's `Busy` path.

**Risk to flag at execution time:** Task 1 touches PR1 code that is already merged-pending-review in #482. If PR1 changes before this lands, rebase and re-run Task 1 Step 7 — its whole value is proving the extraction changed nothing.
