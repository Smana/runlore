# Investigation Silence (the third button) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a third feedback verdict — 🔕 *accurate, but known; don't tell me for N* — that suppresses re-investigating the same `TriggerKey` for a configurable window, enforced before the paid model loop.

**Architecture:** A new `"silence"` event kind in the append-only outcome ledger, folded into a per-`TriggerKey` expiry map and surfaced on the existing `TriggerRecurrence` snapshot. `RecurrenceGate.decide` gains one branch that reads it. Slack renders an overflow menu (window in `option.value`, `TriggerKey` in the block's `block_id`); Matrix gets a 🔕 reaction plus a `silence:` thread command. No new store, no new sync path — `POST /slack/interactions` is already forwarded to the leader, which is the process the gate reads from.

**Tech Stack:** Go 1.x (module `github.com/Smana/runlore`), stdlib only for this feature. Slack Block Kit. Matrix client-server API. Testing: stdlib `testing`, table-driven.

**Spec:** [`docs/superpowers/specs/2026-08-25-investigation-silence-design.md`](../specs/2026-08-25-investigation-silence-design.md)

## Global Constraints

Copied verbatim from `AGENTS.md` and the spec. **Every task's requirements implicitly include this section.**

- **Quality gate — run before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
- `gofmt -l .` must print **nothing**. `hack/lint.sh` must report **`0 issues`**. Use `hack/lint.sh`, never a bare `golangci-lint run ./...`, and never `--disable=staticcheck`.
- **TDD.** Write the failing test first, then the minimal implementation. Prefer table-driven tests. Tests must verify behaviour, not mocks.
- **Errors.** Wrap with `%w`; compare with `errors.Is` / `errors.As` (enforced by `errorlint`).
- **Context.** `context.Context` is the first parameter of any function that does I/O.
- **Exported symbols carry doc comments** (enforced by `revive`).
- **Never compare `Severity` to a literal.** `internal/investigate/investigate.go:50` states it: `SeverityCritical` is "the ONE spelling behind `Request.IsCritical`". Use `req.IsCritical()`.
- **Metric label values are spelled as literals at the call site**, never derived from an internal constant — see the doc comment on `recurrenceDecision`.
- **Commit style:** Conventional Commits. **No co-author trailers. No AI attribution lines.** English.
- **Branch:** `feat/investigation-silence` (already created; the spec commit is on it).

## Naming Contract

Every task uses these exact names. A mismatch between tasks is a bug.

| Symbol | Package | Signature / type |
|---|---|---|
| `Ledger.Silence` | `internal/outcome` | `func (l *Ledger) Silence(triggerKey string, window time.Duration, user string, at time.Time) error` |
| `Ledger.MaxSilenceWindow` | `internal/outcome` | `time.Duration` — exported field, zero = uncapped |
| `Ledger.silences` | `internal/outcome` | `map[string]time.Time` — TriggerKey → expiry |
| `checkpointData.Silences` | `internal/outcome` | `map[string]time.Time` `json:"silences,omitempty"` |
| `TriggerRecurrence.SilencedUntil` | `internal/outcome` | `time.Time` — zero = no standing silence |
| `recurrenceSilenced` | `internal/investigate` | `recurrenceDecision = "silenced_by_human"` |
| `Notify.SilenceEnabled` | `internal/config` | `func (n Notify) SilenceEnabled() bool` |
| `SilenceNotify` | `internal/config` | `struct { Windows []Duration; MaxWindow Duration }` at `Notify.Silence` |
| `SilenceNotify.Default` | `internal/config` | `func (s SilenceNotify) Default() time.Duration` |
| `SlackNotify.SilenceButton` | `internal/config` | `bool` `yaml:"silence_button"` |
| `MatrixNotify.SilenceReactions` | `internal/config` | `bool` `yaml:"silence_reactions"` |
| `SilenceRecorder` | `internal/server` | `interface { Silence(string, time.Duration, string, time.Time) error }` |
| `SilenceSink` | `internal/notify` | same method set |
| `SilenceRecorder` | `internal/thread` | same method set |
| `silenceActionID` | `internal/notify` | `= "runlore_silence"` |
| `silenceBlockIDPrefix` | `internal/notify` | `= "sil:"` |
| `feedbackBlocks` | `internal/notify` | `func(inv providers.Investigation, silenceWindows []time.Duration) []map[string]any` |
| `IntentSilence` | `internal/thread` | `Intent` — prefix `"silence:"` |

---

## File Structure

**Created:**

| File | Responsibility |
|---|---|
| `internal/outcome/ledger_silence_test.go` | Silence fold, latest-wins, cap, checkpoint round-trip, resolve re-arm |
| `internal/investigate/recurrence_silence_test.go` | The `decide()` matrix for the silence branch |
| `internal/notify/slack_silence_test.go` | Overflow rendering, `block_id` carrier, the 255-char degradation |
| `internal/server/silence_test.go` | Interaction handler: recording, ack text, disabled-path |
| `internal/notify/matrix_silence_test.go` | 🔕 reaction: recording, non-self rejection, flag gating |
| `internal/thread/grammar_silence_test.go` | `silence:` parsing and priority |

**Modified:**

| File | Change |
|---|---|
| `internal/outcome/ledger.go` | `silences` map, `"silence"` fold case, checkpoint field, `Silence()`, `MaxSilenceWindow`, `SilencedUntil`, resolve re-arm |
| `internal/investigate/recurrence.go` | `recurrenceSilenced` + the `decide()` branch |
| `internal/investigate/loop.go:~440` | The `recurrenceSilenced` case: metric literal + INFO log |
| `internal/config/config.go` | `SilenceNotify`, two bools, `SilenceEnabled()`, validation |
| `internal/app/investigate.go:~628` | Gate construction — the nil-gate trap |
| `internal/app/serve.go` | `MaxSilenceWindow`, `Actions.Silence`, Matrix option |
| `internal/notify/slack.go` | `silenceActionID`, `feedbackBlocks` signature, `SilenceWindows` on both notifiers |
| `internal/notify/registry.go` | Pass windows through from config |
| `internal/server/server.go` | `SilenceRecorder`, `Actions.Silence`, `Server.silence`, handler case, ack |
| `internal/notify/matrix_feedback.go` | 🔕 case, `silenceReactions` flag, `WithSilenceReactions` |
| `internal/thread/grammar.go` | `IntentSilence` + prefix |
| `internal/thread/responder.go` | `IntentSilence` case, `Silence` field |
| `internal/notify/card_golden_test.go` | Coverage-guard entry + a fixture that renders it |
| `website/content/docs/...`, `deploy/helm/runlore/values*.yaml` | Docs + chart |

---

## Task 1: Ledger — the silence event, fold, and checkpoint

**Files:**
- Modify: `internal/outcome/ledger.go`
- Test: `internal/outcome/ledger_silence_test.go` (create)

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `Ledger.Silence(triggerKey string, window time.Duration, user string, at time.Time) error`; exported field `Ledger.MaxSilenceWindow time.Duration`; `TriggerRecurrence.SilencedUntil time.Time`.

**Background for the implementer.** The ledger is an append-only JSONL file. Every write is *durable first*: append the line, and only fold it into memory if the append succeeded (see `Feedback` at `ledger.go:1194`). The same file is replayed on startup and on every leadership acquisition, so **every piece of in-memory state must be reconstructible from the file alone**. Compaction rewrites the file as `[checkpoint][recent tail]`, so any new fold state must also be carried in `checkpointData` or it is lost the first time the ledger exceeds `maxEvents`.

- [ ] **Step 1: Write the failing tests**

Create `internal/outcome/ledger_silence_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package outcome

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := New(filepath.Join(t.TempDir(), "outcome.jsonl"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

// TestSilenceIsReadBackAsAnExpiry: the whole point of the event kind.
func TestSilenceIsReadBackAsAnExpiry(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := l.Silence("ns/app:CrashLoop", 4*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	got := l.Recurrence("ns/app:CrashLoop").SilencedUntil
	if want := now.Add(4 * time.Hour); !got.Equal(want) {
		t.Errorf("SilencedUntil = %v, want %v", got, want)
	}
}

// TestSilenceLatestWinsPerTrigger pins the dedup rule that differs from votes:
// a silence is ONE standing decision about the trigger, not a per-user opinion,
// so a second click REPLACES the first — including shortening it.
func TestSilenceLatestWinsPerTrigger(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	// A DIFFERENT user, a SHORTER window, one minute later.
	if err := l.Silence("k", 1*time.Hour, "U2", now.Add(time.Minute)); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	got := l.Recurrence("k").SilencedUntil
	if want := now.Add(time.Minute).Add(1 * time.Hour); !got.Equal(want) {
		t.Errorf("SilencedUntil = %v, want the SECOND silence %v", got, want)
	}
}

func TestSilenceRejectsBadWindows(t *testing.T) {
	l := newTestLedger(t)
	l.MaxSilenceWindow = 24 * time.Hour
	now := time.Now()
	for _, tc := range []struct {
		name   string
		window time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Hour},
		{"above the cap", 25 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := l.Silence("k", tc.window, "U1", now); err == nil {
				t.Fatal("want an error, got nil")
			}
			if !l.Recurrence("k").SilencedUntil.IsZero() {
				t.Error("a rejected silence must not be folded")
			}
		})
	}
}

// TestSilenceUnattributableIsRejected: with no TriggerKey there is nothing to
// key the suppression on, so the write must fail loudly rather than append a
// line that can never be read back.
func TestSilenceUnattributableIsRejected(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Silence("", time.Hour, "U1", time.Now()); err == nil {
		t.Fatal("want an error for an empty trigger key, got nil")
	}
}

// TestSilenceSurvivesReload proves the state is reconstructible from the file
// alone — the property every leadership failover depends on.
func TestSilenceSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outcome.jsonl")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	first, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := first.Silence("k", 4*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}

	second, err := New(path) // a fresh process replaying the same file
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}
	if got, want := second.Recurrence("k").SilencedUntil, now.Add(4*time.Hour); !got.Equal(want) {
		t.Errorf("after reload SilencedUntil = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/outcome/ -run TestSilence -v`
Expected: FAIL — compile error, `l.Silence undefined` and `SilencedUntil undefined`.

- [ ] **Step 3: Add the fold state and the `SilencedUntil` field**

In `internal/outcome/ledger.go`, add to the `Ledger` struct (after the `votes` field, ~line 172):

```go
	// silences holds the expiry of the standing human silence per TriggerKey — the
	// 🔕 verdict, which suppresses re-investigation rather than weighing an entry.
	//
	// LATEST WINS, keyed by TriggerKey ALONE. This is deliberately unlike votes,
	// which dedup per (TriggerKey, user) because ratings AGGREGATE: a silence is one
	// standing decision ABOUT THE TRIGGER, so the most recent human wins outright,
	// including shortening or effectively lifting a colleague's. Rebuilt on load and
	// checkpointed on compaction like votes; a lapsed entry is inert (see Recurrence)
	// and pruned opportunistically rather than swept by a timer.
	silences map[string]time.Time

	// MaxSilenceWindow caps what Silence accepts (zero = uncapped). Set by the serve
	// wiring from notify.silence.max_window. It lives HERE, not in each caller, so
	// there is one place the invariant holds: a Matrix `silence:` command is free
	// text and must not be able to exceed what the Slack presets offer.
	MaxSilenceWindow time.Duration
```

Add to `TriggerRecurrence` (~line 875), after `FeedbackDown`:

```go
	// SilencedUntil is the expiry of the standing human 🔕 silence on this trigger;
	// zero when none stands. Read by the suppression gate, which compares it against
	// now — a lapsed silence is simply inert, never swept.
	SilencedUntil time.Time
```

- [ ] **Step 4: Fold, reset, and checkpoint the new state**

In `resetStateLocked` (~line 408), beside `l.votes`:

```go
	l.silences = map[string]time.Time{}
```

In `foldLocked` (~line 427), add a case beside `"feedback"`:

```go
	case "silence":
		l.applySilenceLocked(e)
```

Add the fold function beside `applyFeedbackLocked`:

```go
// applySilenceLocked folds one silence event into the per-trigger expiry map.
// Latest wins: the event's own At + window replaces whatever stood, so a replay
// in file order lands on the newest silence exactly as the live path did.
// A malformed replayed line (no key, unparseable or non-positive window) is
// dropped rather than folded — the same posture applyFeedbackLocked takes.
//
// The cap is NOT re-applied here. MaxSilenceWindow is a write-time policy that
// can legitimately change between runs, and re-applying it on replay would let a
// config edit retroactively rewrite history the file already records.
func (l *Ledger) applySilenceLocked(e Event) {
	if e.TriggerKey == "" {
		return
	}
	window, err := time.ParseDuration(e.Kind)
	if err != nil || window <= 0 {
		return
	}
	l.silences[e.TriggerKey] = e.At.Add(window)
}
```

In `checkpointData` (~line 221), after `Votes`:

```go
	// Silences carries the standing per-TriggerKey silence expiries across compaction.
	// Without it a compaction would drop a live silence (RunLore starts talking again
	// mid-window) or, on an older reader, resurrect none at all. Absent from a
	// checkpoint written before this field existed; a replay then reconstructs from the
	// retained tail only, which degrades to "no silence stands" — conservative
	// (RunLore investigates), never the reverse.
	Silences map[string]time.Time `json:"silences,omitempty"`
```

In `seedCheckpointLocked` (~line 449), beside the `cd.Votes` loop:

```go
	for k, v := range cd.Silences {
		l.silences[k] = v
	}
```

In `snapshotCheckpointLocked` (~line 516), beside the `l.votes` block:

```go
	if len(l.silences) > 0 {
		cd.Silences = make(map[string]time.Time, len(l.silences))
		for k, v := range l.silences {
			cd.Silences[k] = v
		}
	}
```

In `Recurrence` (~line 924), after the `tr` literal is built:

```go
	tr.SilencedUntil = l.silences[triggerKey]
```

- [ ] **Step 5: Add the public `Silence` method**

Beside `Feedback` in `internal/outcome/ledger.go`:

```go
// Silence records a human 🔕 verdict: this trigger's diagnosis is accurate and
// known, so do not re-investigate it for window. It is the third feedback verdict
// — 👍 accurate, 👎 off-base, 🔕 accurate but known — and the only one that changes
// what RunLore DOES rather than how it weighs an entry.
//
// Validation is deliberately at the write, not the caller: window must be positive
// and within MaxSilenceWindow, so a Matrix `silence:` command (free text) cannot
// exceed what the Slack presets offer. triggerKey must be non-empty — with nothing
// to key the suppression on there is nothing for the gate to read back, and a line
// that can never be read is worse than a refused write.
//
// Durable-first, like Open/Resolve/Feedback: a failed append leaves the fold
// untouched. Attribution is entirely TriggerKey-based; user is recorded for the
// audit trail, NOT for dedup — see Ledger.silences for why latest-wins per trigger.
func (l *Ledger) Silence(triggerKey string, window time.Duration, user string, at time.Time) error {
	if triggerKey == "" {
		return fmt.Errorf("silence: empty trigger key")
	}
	if window <= 0 {
		return fmt.Errorf("silence window %v: must be positive", window)
	}
	if l.MaxSilenceWindow > 0 && window > l.MaxSilenceWindow {
		return fmt.Errorf("silence window %v exceeds the %v cap", window, l.MaxSilenceWindow)
	}
	if !l.enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Event{Event: "silence", TriggerKey: triggerKey, Kind: window.String(), User: user, At: at}
	if err := l.appendLocked(e); err != nil {
		return err
	}
	l.applySilenceLocked(e)
	return nil
}
```

Note the validation runs **before** the `enabled()` check, so a disabled ledger still rejects a bad window — the caller's error message is then the same in both deployments.

Extend the `Event.Event` doc comment (~line 64) to list the new kind:

```go
	Event          string `json:"event"`                     // "open" | "resolve" | "feedback" | "silence" | "checkpoint" | "confirm"
```

And extend `Event.User`'s comment, which currently says "Empty on open/resolve lines":

```go
	// User identifies the human behind a feedback or silence event (a Slack user id
	// or a Matrix user id). On feedback it is the dedup key that keeps one live vote
	// per (TriggerKey, user), latest wins; on silence it is AUDIT ONLY — silences
	// dedup per trigger alone. Empty on open/resolve lines. On a feedback line Kind
	// carries the rating ("up" | "down"); on a silence line it carries the window as
	// a duration string ("4h").
	User string `json:"user,omitempty"`
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/outcome/ -run TestSilence -v`
Expected: PASS (5 tests, including the 3 sub-tests of `TestSilenceRejectsBadWindows`).

- [ ] **Step 7: Add the compaction round-trip test**

This is the test most likely to catch a real bug — append to `internal/outcome/ledger_silence_test.go`:

```go
// TestSilenceSurvivesCompaction is the load-bearing one: compaction rewrites the
// file as [checkpoint][tail], so a silence older than the horizon exists ONLY in
// the checkpoint. Forgetting checkpointData.Silences makes RunLore start talking
// again mid-window, silently.
func TestSilenceSurvivesCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.jsonl")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	// maxEvents low enough that the silence is certain to fall before the horizon.
	l, err := NewWithMaxEvents(path, 8)
	if err != nil {
		t.Fatalf("NewWithMaxEvents: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	// Push the silence past the compaction horizon with unrelated opens.
	for i := 0; i < 20; i++ {
		if err := l.Open(Event{Fingerprint: "fp", Title: "noise", At: now}); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}

	// THREE loads are required, and the reason is subtle enough to be worth
	// stating: compactLocked runs only inside loadLocked, and only AFTER the full
	// event list has already been folded into memory. So the load that COMPACTS
	// still holds correct state whether or not the checkpoint captured anything —
	// asserting on it would pass with a completely broken snapshotCheckpointLocked.
	// Only the load that replays [checkpoint][tail] actually reads the checkpoint.
	compacting, err := NewWithMaxEvents(path, 8) // folds all 21, THEN rewrites the file
	if err != nil {
		t.Fatalf("NewWithMaxEvents (compacting load): %v", err)
	}
	_ = compacting

	replayed, err := NewWithMaxEvents(path, 8) // reads [checkpoint][tail] — the real assertion
	if err != nil {
		t.Fatalf("NewWithMaxEvents (replaying load): %v", err)
	}
	if got, want := replayed.Recurrence("k").SilencedUntil, now.Add(24*time.Hour); !got.Equal(want) {
		t.Errorf("after compaction SilencedUntil = %v, want %v — the checkpoint dropped it", got, want)
	}
}
```

**Verify the test actually bites.** After Step 8 passes, temporarily comment out the
`cd.Silences` block in `snapshotCheckpointLocked` and re-run: the test MUST fail. If it
still passes, the fixture is not crossing the compaction horizon and the test is worthless
— raise the open count until it does, then restore the code.

- [ ] **Step 8: Run the compaction test**

Run: `go test ./internal/outcome/ -run TestSilenceSurvivesCompaction -v`
Expected: PASS.

If it FAILS with a zero `SilencedUntil`, the `snapshotCheckpointLocked` or `seedCheckpointLocked` edit in Step 4 is missing — that is exactly the bug this test exists to catch.

- [ ] **Step 9: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
Expected: all pass; `gofmt -l .` prints nothing; lint reports `0 issues`.

- [ ] **Step 10: Commit**

```bash
git add internal/outcome/ledger.go internal/outcome/ledger_silence_test.go
git commit -m "feat(outcome): record a human silence verdict per trigger

A third feedback verdict alongside up/down: 'accurate, but known — do not
re-investigate for N'. Folded into a per-TriggerKey expiry map, surfaced on
TriggerRecurrence, and checkpointed so a compaction cannot drop a live
silence or resurrect a lapsed one.

Latest-wins per trigger, unlike votes, which dedup per (trigger, user):
a silence is one standing decision about the trigger, not an opinion that
aggregates. The window cap lives on the ledger so a free-text Matrix
command cannot exceed what the Slack presets offer."
```

---

## Task 2: Ledger — a resolve clears the silence

**Files:**
- Modify: `internal/outcome/ledger.go` (`applyResolveLocked`, ~line 690)
- Test: `internal/outcome/ledger_silence_test.go` (append)

**Interfaces:**
- Consumes: `Ledger.Silence`, `TriggerRecurrence.SilencedUntil` (Task 1).
- Produces: no new API — a behaviour change inside `applyResolveLocked`.

**Background.** `applyResolveLocked` receives only a fingerprint. The bridge to `TriggerKey` already exists: `Ledger.open` is `map[string]Event` — fingerprint → latest unresolved open (`ledger.go:151`) — and `Event` carries `TriggerKey`. **Order matters:** `foldLocked`'s `"resolve"` case does `delete(l.open, e.Fingerprint)` *before* calling `applyResolveLocked`, so the lookup must happen inside `applyResolveLocked` while the live path still has it — read the spec section "Re-arm: resolve" and check the ordering yourself before implementing.

- [ ] **Step 1: Write the failing test**

Append to `internal/outcome/ledger_silence_test.go`:

```go
// TestResolveClearsTheSilence: the incident actually went away, so the silence
// has served its purpose; a later firing is arguably a new occurrence and
// deserves a fresh look.
func TestResolveClearsTheSilence(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	if err := l.Open(Event{Fingerprint: "fp1", TriggerKey: "k", Title: "boom", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	if l.Recurrence("k").SilencedUntil.IsZero() {
		t.Fatal("precondition: the silence should stand before the resolve")
	}

	if _, _, err := l.Resolve("fp1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := l.Recurrence("k").SilencedUntil; !got.IsZero() {
		t.Errorf("SilencedUntil = %v after a resolve, want zero", got)
	}
}

// TestResolveOfAnUnrelatedFingerprintKeepsTheSilence guards the obvious
// over-reach: one incident ending must not un-silence a different one.
func TestResolveOfAnUnrelatedFingerprintKeepsTheSilence(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	if err := l.Open(Event{Fingerprint: "fp1", TriggerKey: "k", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Open(Event{Fingerprint: "fp2", TriggerKey: "other", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	if _, _, err := l.Resolve("fp2", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if l.Recurrence("k").SilencedUntil.IsZero() {
		t.Error("an unrelated resolve cleared the silence")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/outcome/ -run 'TestResolve.*Silence' -v`
Expected: `TestResolveClearsTheSilence` FAILS (`SilencedUntil` is still set); `TestResolveOfAnUnrelatedFingerprintKeepsTheSilence` PASSES already (it is the guard, and must keep passing).

- [ ] **Step 3: Clear the silence in `applyResolveLocked`**

At the very top of `applyResolveLocked` (`ledger.go:690`), **before** `l.resolvesSeen++`:

```go
	// A resolve re-arms investigation: the incident ended, so a standing human
	// silence has served its purpose and a later firing deserves a fresh look.
	// Done FIRST and unconditionally, because whether this resolve goes on to
	// pair, buffer, or be discarded as stale says nothing about whether the
	// incident is over — which is the only question the silence turns on.
	//
	// l.open is the fingerprint → latest-unresolved-open index, and an open
	// carries its TriggerKey, so no new index is needed. foldLocked's resolve
	// case deletes from l.open only AFTER calling this, so the entry is still
	// here on both the live and the replay path.
	if tk := l.open[fp].TriggerKey; tk != "" {
		delete(l.silences, tk)
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/outcome/ -run 'TestResolve.*Silence' -v`
Expected: both PASS.

- [ ] **Step 5: Verify the replay path agrees with the live path**

The live path (`Resolve`) and the replay path (`foldLocked`) must reach the same state, or a restart would resurrect a cleared silence. Append:

```go
// TestResolveClearingSurvivesReplay: the live path and the file replay must
// agree, or a leadership failover would resurrect a silence a resolve cleared.
func TestResolveClearingSurvivesReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.jsonl")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := l.Open(Event{Fingerprint: "fp1", TriggerKey: "k", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	if _, _, err := l.Resolve("fp1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	reloaded, err := New(path)
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}
	if got := reloaded.Recurrence("k").SilencedUntil; !got.IsZero() {
		t.Errorf("after replay SilencedUntil = %v, want zero — replay disagrees with the live path", got)
	}
}
```

Run: `go test ./internal/outcome/ -run TestResolveClearingSurvivesReplay -v`
Expected: PASS.

- [ ] **Step 6: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
Expected: all pass. Pay attention to the existing `ledger_test.go` resolve tests — they must be unaffected.

- [ ] **Step 7: Commit**

```bash
git add internal/outcome/ledger.go internal/outcome/ledger_silence_test.go
git commit -m "feat(outcome): a resolve re-arms a silenced trigger

The incident ended, so the standing silence has served its purpose. Cleared
before any pairing decision: whether the resolve pairs, buffers or is
discarded as stale says nothing about whether the incident is over.

Uses the existing fingerprint -> latest-unresolved-open index to reach the
TriggerKey, so no new state is introduced."
```

---

## Task 3: The gate — `decide()` learns the silence branch

**Files:**
- Modify: `internal/investigate/recurrence.go`
- Modify: `internal/investigate/loop.go` (~line 440, the `switch decision` block)
- Test: `internal/investigate/recurrence_silence_test.go` (create)

**Interfaces:**
- Consumes: `outcome.TriggerRecurrence.SilencedUntil` (Task 1).
- Produces: `recurrenceSilenced recurrenceDecision = "silenced_by_human"`; the metric label literal `"silenced"` on `investigations_completed_total{result=…}`.

**Background.** `decide` is a pure function of `(config, history, clock)` — `now` is a parameter precisely so the matrix is testable without sleeping. Keep it that way. `RecurrenceStats` is the interface the gate reads through; `*outcome.Ledger` satisfies it.

**Read `internal/investigate/recurrence.go` in full before starting.** Its doc comment explains why the cooldown turns on two independent facts, and why conflating them broke the gate once already (#471). The silence branch must not disturb that logic.

- [ ] **Step 1: Write the failing test**

Create `internal/investigate/recurrence_silence_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
)

// TestDecideSilence is the full matrix for the human silence branch. The cases
// that matter most are the ones where a silence must NOT win: the escape hatches
// are the whole reason suppressing the paid loop is acceptable at all.
func TestDecideSilence(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	past := now.Add(-2 * time.Hour)

	for _, tc := range []struct {
		name  string
		gate  *RecurrenceGate
		req   Request
		prior outcome.TriggerRecurrence
		want  recurrenceDecision
	}{
		{
			name:  "a standing silence suppresses, with the cooldown OFF",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceSilenced,
		},
		{
			name:  "a standing silence suppresses even with NO conclusive prior",
			gate:  &RecurrenceGate{Cooldown: time.Hour},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{Count: 3, Last: now, SilencedUntil: future},
			want:  recurrenceSilenced,
		},
		{
			name:  "a LAPSED silence does not suppress",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: past},
			want:  recurrenceOff,
		},
		{
			name:  "a CRITICAL firing is never silenced",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "critical"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceOff,
		},
		{
			name:  "a CRITICAL firing is never silenced, whatever the casing",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "CRITICAL"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceOff,
		},
		{
			name:  "a standing thumbs-down re-arms investigation",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future, FeedbackDown: 1},
			want:  recurrenceOff,
		},
		{
			name:  "no trigger key: nothing to silence on",
			gate:  &RecurrenceGate{Cooldown: 0},
			req:   Request{Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceOff,
		},
		{
			name:  "a nil gate never suppresses",
			gate:  nil,
			req:   Request{TriggerKey: "k", Severity: "warning"},
			prior: outcome.TriggerRecurrence{SilencedUntil: future},
			want:  recurrenceOff,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.gate.decide(tc.req, tc.prior, now); got != tc.want {
				t.Errorf("decide() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDecideSilenceDoesNotDisturbTheCooldown: the existing ladder must behave
// identically when no silence stands. This is a regression guard on #471.
func TestDecideSilenceDoesNotDisturbTheCooldown(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	g := &RecurrenceGate{Cooldown: time.Hour}
	req := Request{TriggerKey: "k", Severity: "warning"}

	prior := outcome.TriggerRecurrence{
		Count:      2,
		Last:       now.Add(-10 * time.Minute),
		Conclusive: outcome.ConclusivePrior{At: now.Add(-10 * time.Minute), Verdict: "no_action"},
	}
	if got := g.decide(req, prior, now); got != recurrenceSuppressed {
		t.Errorf("decide() = %q, want %q — the cooldown ladder changed", got, recurrenceSuppressed)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/investigate/ -run TestDecideSilence -v`
Expected: FAIL — compile error, `recurrenceSilenced` undefined.

- [ ] **Step 3: Add the decision constant**

In `internal/investigate/recurrence.go`, in the `recurrenceDecision` const block, **before** `recurrenceSuppressed`:

```go
	recurrenceSilenced recurrenceDecision = "silenced_by_human" // a human 🔕 stands: skip the paid loop
```

Extend `suppressed()` so both suppressing decisions are covered:

```go
// suppressed reports whether d is a decision that skips the paid loop. Two now
// do: the machine's cooldown and the human's silence. Callers must never test
// `== recurrenceSuppressed` directly, or the human branch silently stops
// suppressing while every log line still claims it decided.
func (d recurrenceDecision) suppressed() bool {
	return d == recurrenceSuppressed || d == recurrenceSilenced
}
```

- [ ] **Step 4: Add the branch to `decide`**

Replace the opening of `decide` (currently `if g == nil || g.Cooldown <= 0 || req.TriggerKey == ""`):

```go
func (g *RecurrenceGate) decide(req Request, prior outcome.TriggerRecurrence, now time.Time) recurrenceDecision {
	if g == nil || req.TriggerKey == "" {
		return recurrenceOff
	}
	// A HUMAN silence, checked before the cooldown short-circuit below. Three
	// reasons the order is load-bearing:
	//
	//   - recurrence_cooldown defaults to 0 (off). Left below the short-circuit,
	//     the entire feature would be inert in a default install while the UI
	//     still confirmed each click — the worst possible failure mode.
	//   - A silence stands regardless of prior history, INCLUDING the
	//     no_conclusive_prior case the cooldown deliberately refuses to suppress.
	//     That case (a trigger that keeps coming back inconclusive, re-running the
	//     full paid loop on every firing) is exactly the one a human is most
	//     likely to want silenced.
	//   - It is the human's decision, so it outranks the machine's heuristics.
	//
	// Two escapes are checked here rather than at the click, because both can
	// become true AFTER a silence was recorded: a CRITICAL firing (a silence must
	// never mute a page — the same carve-out the debouncer makes, see
	// source/debounce.go) and a standing 👎 (a colleague saying the diagnosis is
	// wrong, read through the ONE shared definition of contested-ness, #288).
	if now.Before(prior.SilencedUntil) && !req.IsCritical() && !prior.Contested() {
		return recurrenceSilenced
	}
	if g.Cooldown <= 0 {
		return recurrenceOff
	}
	switch {
	// … existing ladder, unchanged
	}
}
```

Also extend the `RecurrenceGate` type's doc comment — it is no longer only a cooldown:

```go
// RecurrenceGate is the one place that decides a trigger is not worth
// re-investigating right now, for either of two reasons: the MACHINE's cooldown
// (a conclusive answer landed moments ago) or a HUMAN's standing 🔕 silence
// (accurate, known, stop telling me). The two are independent — a silence works
// with Cooldown at 0 — and the human one is checked first; see decide.
type RecurrenceGate struct {
	Cooldown time.Duration // 0 disables the COOLDOWN only; a human silence still applies
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/investigate/ -run TestDecideSilence -v`
Expected: PASS (8 sub-tests plus the cooldown regression guard).

- [ ] **Step 6: Wire the metric and the log**

In `internal/investigate/loop.go`, in the `switch decision := li.Recurrence.decide(...)` block (~line 441), add a case **beside** `recurrenceSuppressed`:

```go
	case recurrenceSilenced:
		// A distinct metric label from recurrence_suppressed, deliberately: an
		// operator asking "why is RunLore quiet?" must be able to tell a machine
		// decision from a human one on the dashboard alone. Spelled as a LITERAL,
		// like every other result value — see recurrenceDecision's doc comment for
		// why the internal name must never become the label.
		result = "silenced"
		// INFO, not DEBUG, for the same reason recurrenceNoAnswer is INFO: a human
		// deliberately switched something off, and the operator who did not click it
		// must be able to find out why the channel went quiet without raising log
		// levels on a production deployment.
		li.Log.Info("silenced by a human: skipping re-investigation",
			"title", req.Title, "trigger_key", req.TriggerKey,
			"silenced_until", prior.SilencedUntil,
			"occurrences", prior.Count, "last_investigated", prior.Last)
		return nil
```

- [ ] **Step 7: Verify the whole package still passes**

Run: `go test ./internal/investigate/ -v 2>&1 | tail -30`
Expected: PASS. In particular the existing recurrence tests must be untouched — if any now fail, the `decide` reordering in Step 4 changed the cooldown ladder and must be corrected.

- [ ] **Step 8: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
Expected: all pass.

- [ ] **Step 9: Commit**

```bash
git add internal/investigate/recurrence.go internal/investigate/recurrence_silence_test.go internal/investigate/loop.go
git commit -m "feat(investigate): honour a human silence before the paid loop

One branch in RecurrenceGate.decide, checked ahead of the cooldown
short-circuit so the feature works with recurrence_cooldown at its default
of 0, and so a silence covers the no_conclusive_prior case the cooldown
deliberately refuses to suppress.

Two escapes are evaluated at decision time rather than at the click,
because both can become true after a silence is recorded: a CRITICAL
firing is never muted, and a standing thumbs-down re-arms investigation.

Reports investigations_completed_total{result=\"silenced\"} — a distinct
label from recurrence_suppressed, so a dashboard can tell a human decision
from a machine one."
```

---

## Task 4: Config — the silence block and its validation

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces: `config.SilenceNotify{Windows []Duration; MaxWindow Duration}` at `Notify.Silence`; `Notify.SilenceEnabled() bool`; `SilenceNotify.Default() time.Duration`; `SilenceNotify.Std() []time.Duration`; `SlackNotify.SilenceButton bool`; `MatrixNotify.SilenceReactions bool`.

**Background.** `config.Duration` (`config.go:1385`) is `time.Duration` with a YAML string unmarshaller — `4h` parses, a bare number does not. Validation lives in `Validate` and must **fail loud at startup**, never render a control whose clicks vanish. Read the existing `feedback_buttons` rules at `config.go:2070` and mirror their shape and their error-message style.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestSilenceConfigValidation(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Outcome.LedgerPath = "/tmp/outcome.jsonl"
		c.Notify.Slack.SigningSecretEnv = "SLACK_SIGNING_SECRET"
		c.Notify.Slack.SilenceButton = true
		c.Notify.Silence.Windows = []Duration{Duration(time.Hour)}
		c.Notify.Silence.MaxWindow = Duration(24 * time.Hour)
		return c
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "the happy path validates",
			mutate: func(*Config) {},
		},
		{
			name:    "the silence button needs a signing secret",
			mutate:  func(c *Config) { c.Notify.Slack.SigningSecretEnv = "" },
			wantErr: "signing_secret_env",
		},
		{
			name:    "silencing needs a ledger",
			mutate:  func(c *Config) { c.Outcome.LedgerPath = "" },
			wantErr: "outcome.ledger_path",
		},
		{
			name:    "an empty preset list is a misconfiguration",
			mutate:  func(c *Config) { c.Notify.Silence.Windows = nil },
			wantErr: "notify.silence.windows",
		},
		{
			name:    "a non-positive preset is rejected",
			mutate:  func(c *Config) { c.Notify.Silence.Windows = []Duration{0} },
			wantErr: "must be positive",
		},
		{
			name: "a preset above the cap is rejected",
			mutate: func(c *Config) {
				c.Notify.Silence.Windows = []Duration{Duration(48 * time.Hour)}
			},
			wantErr: "max_window",
		},
		{
			name: "the Matrix path needs a ledger too",
			mutate: func(c *Config) {
				c.Notify.Slack.SilenceButton = false
				c.Notify.Matrix.SilenceReactions = true
				c.Outcome.LedgerPath = ""
			},
			wantErr: "outcome.ledger_path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestSilenceDefaultIsTheFirstPreset pins the contract the Matrix reaction path
// depends on — a reaction carries no duration, so it uses windows[0].
func TestSilenceDefaultIsTheFirstPreset(t *testing.T) {
	s := SilenceNotify{Windows: []Duration{Duration(4 * time.Hour), Duration(time.Hour)}}
	if got, want := s.Default(), 4*time.Hour; got != want {
		t.Errorf("Default() = %v, want %v", got, want)
	}
	if got := (SilenceNotify{}).Default(); got != 0 {
		t.Errorf("Default() with no presets = %v, want 0", got)
	}
}
```

If `strings` or `time` are not yet imported in `config_test.go`, add them.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run TestSilence -v`
Expected: FAIL — compile error, `SilenceButton`, `SilenceNotify` undefined.

- [ ] **Step 3: Add the config types**

In `internal/config/config.go`, add to `Notify` (~line 829), after the `Thread` field:

```go
	Silence SilenceNotify `yaml:"silence"` // shared Slack/Matrix silence windows — see SilenceNotify
```

Add the type beside the other notify types:

```go
// SilenceNotify configures the human 🔕 silence verdict — the presets offered on
// the Slack overflow menu and the hard cap every transport is held to. Shared
// across transports on purpose: a Matrix `silence: 999h` command is free text,
// and must not be able to exceed what the Slack presets offer.
type SilenceNotify struct {
	// Windows are the durations offered, in the order they are rendered. The FIRST
	// entry is the default used where no duration can be carried — notably a Matrix
	// 🔕 reaction, which is a bare emoji.
	Windows []Duration `yaml:"windows"`
	// MaxWindow is the hard cap. Zero means uncapped, which Validate rejects
	// whenever a silence transport is on — an uncapped silence is indistinguishable
	// from a permanent one, and nothing else in the system would ever lift it.
	MaxWindow Duration `yaml:"max_window"`
}

// Std returns the presets as standard-library durations, in render order.
func (s SilenceNotify) Std() []time.Duration {
	out := make([]time.Duration, 0, len(s.Windows))
	for _, w := range s.Windows {
		out = append(out, w.Std())
	}
	return out
}

// Default is the window used where none can be carried (a Matrix reaction).
// Zero when no presets are configured — callers treat that as "silencing off".
func (s SilenceNotify) Default() time.Duration {
	if len(s.Windows) == 0 {
		return 0
	}
	return s.Windows[0].Std()
}

// SilenceEnabled reports whether ANY transport can record a silence. It is the
// condition the investigation gate must be constructed on: without it, a
// deployment with silencing on but no recurrence cooldown would record every
// click durably and then ignore it, while the UI confirmed success.
func (n Notify) SilenceEnabled() bool {
	return n.Slack.SilenceButton || n.Matrix.SilenceReactions
}
```

Add to `SlackNotify` (~line 1140), beside `FeedbackButtons`:

```go
	// SilenceButton (opt-in, notify.slack.silence_button) appends a 🔕 overflow menu
	// offering notify.silence.windows. A click suppresses re-investigating the same
	// TriggerKey for the chosen window. Deliberately a SEPARATE flag from
	// feedback_buttons: a rating records an opinion, a silence changes what RunLore
	// does, and an operator must be able to take the first without the second.
	SilenceButton bool `yaml:"silence_button"`
```

Add to `MatrixNotify` (~line 1164), beside `FeedbackReactions`:

```go
	// SilenceReactions (opt-in, notify.matrix.silence_reactions) records a 🔕
	// reaction as a silence at notify.silence.windows[0], and enables the
	// `silence: <duration>` thread command. Separate from feedback_reactions for
	// the same reason SlackNotify.SilenceButton is separate from FeedbackButtons.
	SilenceReactions bool `yaml:"silence_reactions"`
```

- [ ] **Step 4: Add the validation**

In `Validate`, immediately after the existing `c.Notify.Slack.FeedbackButtons` block (~line 2070):

```go
	// Silencing is click-driven and CHANGES BEHAVIOUR: enabling it without the
	// pieces a click needs would record every silence durably and then ignore it,
	// while the UI confirmed success. Fail loud at startup instead — the same
	// posture feedback_buttons and recurrence_cooldown already take.
	if c.Notify.Slack.SilenceButton && c.Notify.Slack.SigningSecretEnv == "" {
		return fmt.Errorf("notify.slack.silence_button requires notify.slack.signing_secret_env: clicks arrive on the exposed POST /slack/interactions endpoint and must be signature-verified")
	}
	if c.Notify.SilenceEnabled() {
		if c.Outcome.LedgerPath == "" {
			return fmt.Errorf("notify.silence requires outcome.ledger_path: a silence is recorded in the outcome ledger, and the suppression gate reads it back from there")
		}
		if len(c.Notify.Silence.Windows) == 0 {
			return fmt.Errorf("notify.silence.windows must list at least one duration when a silence transport is enabled")
		}
		if c.Notify.Silence.MaxWindow.Std() <= 0 {
			return fmt.Errorf("notify.silence.max_window must be positive when a silence transport is enabled: an uncapped silence is indistinguishable from a permanent one")
		}
		for _, w := range c.Notify.Silence.Windows {
			if w.Std() <= 0 {
				return fmt.Errorf("notify.silence.windows entry %v must be positive", w.Std())
			}
			if w.Std() > c.Notify.Silence.MaxWindow.Std() {
				return fmt.Errorf("notify.silence.windows entry %v exceeds notify.silence.max_window (%v)", w.Std(), c.Notify.Silence.MaxWindow.Std())
			}
		}
	}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/config/ -run TestSilence -v`
Expected: PASS.

- [ ] **Step 6: Check the notifier-name collision guard**

`internal/notify` has `TestRegisteredNotifierNamesDoNotCollideWithConfigFields`, which reflects over every named `Notify` field's yaml tag and compares it against `notify.Registered()`. The new `silence` field is a named field, so **no notifier may ever be registered under the name `silence`**.

Run: `go test ./internal/notify/ -run TestRegisteredNotifierNames -v`
Expected: PASS. If it fails, a notifier is registered as `silence` and one of the two must be renamed.

- [ ] **Step 7: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
Expected: all pass.

- [ ] **Step 8: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): notify.silence windows, cap, and per-transport flags

Presets and the hard cap are shared across transports on purpose: a Matrix
'silence:' command is free text and must not exceed what the Slack presets
offer. The first preset is the default for paths that cannot carry a
duration, such as a bare Matrix reaction.

Validation fails loud at startup rather than rendering a control whose
clicks vanish, mirroring the feedback_buttons rules. An uncapped window is
rejected: it is indistinguishable from a permanent silence, and nothing
else in the system would ever lift it."
```

---

## Task 5: App wiring — the nil-gate trap, the cap, and the sinks

**Files:**
- Modify: `internal/app/investigate.go` (~line 628, gate construction)
- Modify: `internal/app/serve.go` (~line 449, `Actions` wiring)
- Modify: `internal/app/notify.go` (~line 498, `BuildMatrixFeedback`)
- Modify: `internal/server/server.go` (the `SilenceRecorder` interface + field)
- Test: `internal/app/notify_test.go` (append)

**Interfaces:**
- Consumes: `Ledger.MaxSilenceWindow` (Task 1); `recurrenceSilenced` (Task 3); `Notify.SilenceEnabled()`, `SilenceNotify.MaxWindow` (Task 4).
- Produces: `server.SilenceRecorder` interface; `server.Actions.Silence` field; `Server.silence` field (consumed by Task 6).

**THIS IS THE HIGHEST-RISK TASK IN THE PLAN.** `internal/app/investigate.go:628` currently builds the gate **only** when a cooldown is configured:

```go
var recurrence *investigate.RecurrenceGate
if d := cfg.Investigation.RecurrenceCooldown.Std(); d > 0 && ledger.Enabled() {
	recurrence = &investigate.RecurrenceGate{Cooldown: d}
}
```

`recurrence_cooldown` defaults to `0`. So in a default install with silencing enabled, `recurrence` is nil, `decide` returns `recurrenceOff` on its `g == nil` guard, and **every click is durably recorded and then silently ignored while the UI confirms success.** The regression test in Step 1 exists solely to pin this.

- [ ] **Step 1: Write the failing test**

Append to `internal/app/notify_test.go`:

```go
// TestRecurrenceGateBuiltForSilenceWithoutACooldown pins the nil-gate trap.
//
// recurrence_cooldown defaults to 0. If the gate is only constructed when a
// cooldown is set, a deployment that turned on silencing and nothing else gets a
// nil gate — every click is durably recorded, the ack says it worked, and not one
// investigation is ever suppressed. The failure is invisible from outside.
func TestRecurrenceGateBuiltForSilenceWithoutACooldown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cooldown time.Duration
		silence  bool
		want     bool // is a gate expected?
	}{
		{name: "neither: no gate", want: false},
		{name: "cooldown only", cooldown: time.Hour, want: true},
		{name: "silence only — the regression", silence: true, want: true},
		{name: "both", cooldown: time.Hour, silence: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Investigation.RecurrenceCooldown = config.Duration(tc.cooldown)
			cfg.Notify.Slack.SilenceButton = tc.silence

			got := recurrenceGateWanted(cfg)
			if got != tc.want {
				t.Errorf("recurrenceGateWanted() = %v, want %v", got, tc.want)
			}
		})
	}
}
```

If `internal/app/notify_test.go` lacks the `time` or `config` imports, add them.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/app/ -run TestRecurrenceGateBuiltForSilence -v`
Expected: FAIL — compile error, `recurrenceGateWanted` undefined.

- [ ] **Step 3: Extract the predicate and fix the construction**

In `internal/app/investigate.go`, add above the gate construction:

```go
// recurrenceGateWanted reports whether the suppression gate must exist at all.
//
// Extracted as a named predicate rather than left inline because getting it wrong
// is invisible: recurrence_cooldown defaults to 0, so a deployment that enabled
// ONLY silencing would get a nil gate, and every click would be durably recorded,
// acked as successful, and then ignored. The gate is needed whenever EITHER a
// machine cooldown or a human silence can suppress — see RecurrenceGate.
func recurrenceGateWanted(cfg *config.Config) bool {
	return cfg.Investigation.RecurrenceCooldown.Std() > 0 || cfg.Notify.SilenceEnabled()
}
```

Replace the construction block:

```go
	// The suppression gate: the machine's cooldown, the human's silence, or both.
	// Cooldown may legitimately be 0 here — a silence-only deployment gets a gate
	// whose cooldown ladder never fires but whose silence branch does.
	var recurrence *investigate.RecurrenceGate
	if recurrenceGateWanted(cfg) && ledger.Enabled() {
		d := cfg.Investigation.RecurrenceCooldown.Std()
		recurrence = &investigate.RecurrenceGate{Cooldown: d}
		if d > 0 {
			log.Info("recurrence cooldown enabled", "cooldown", d)
		}
		if cfg.Notify.SilenceEnabled() {
			log.Info("investigation silencing enabled", "max_window", cfg.Notify.Silence.MaxWindow.Std())
		}
	}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/app/ -run TestRecurrenceGateBuiltForSilence -v`
Expected: PASS (4 sub-tests).

- [ ] **Step 5: Add the server's `SilenceRecorder`**

In `internal/server/server.go`, beside `FeedbackRecorder` (~line 107):

```go
// SilenceRecorder persists a human 🔕 silence on a delivered investigation — the
// verdict that suppresses re-investigating the same trigger for a window
// (implemented by *outcome.Ledger). Kept SEPARATE from FeedbackRecorder rather
// than widening it: a deployment may enable ratings without silencing, and the
// server's nil-means-off guards must be able to express that per capability.
type SilenceRecorder interface {
	Silence(triggerKey string, window time.Duration, user string, at time.Time) error
}
```

Add to the `Server` struct, beside `feedback`:

```go
	silence SilenceRecorder // nil unless notify.slack.silence_button is on (with an enabled ledger)
```

Add to `Actions`, beside `Feedback`:

```go
	Silence SilenceRecorder // opt-in 🔕 silencing (notify.slack.silence_button)
```

In `New`, beside where `feedback` is assigned from `acts`:

```go
		silence: acts.Silence,
```

Extend the endpoint's enablement guard in `handleSlackInteraction` so the route is reachable when silencing alone is on:

```go
	if (s.approvals == nil && s.feedback == nil && s.silence == nil) || s.slackSecret == "" {
```

- [ ] **Step 6: Wire the ledger, the cap, and the Matrix option**

In `internal/app/serve.go`, immediately after the ledger is constructed (~line 188):

```go
	// The silence cap lives on the ledger so there is ONE place the invariant
	// holds: a Matrix `silence:` command is free text and must not be able to
	// exceed what the Slack presets offer. Zero when silencing is off, which the
	// ledger reads as uncapped — harmless, since nothing can then record one.
	ledger.MaxSilenceWindow = cfg.Notify.Silence.MaxWindow.Std()
```

After the existing `acts.Feedback` block (~line 460):

```go
	// Opt-in 🔕 silencing, wired on the same terms as feedback: the recorder is
	// wired whenever a click could arrive, but the capability is only ANNOUNCED
	// when a Slack message can actually carry the control.
	if cfg.Notify.Slack.SilenceButton && ledger.Enabled() {
		acts.Silence = ledger
		if SlackFeedbackDeliverable(cfg, log) {
			log.Info("slack silence button enabled", "endpoint", "/slack/interactions",
				"windows", cfg.Notify.Silence.Std(), "max_window", cfg.Notify.Silence.MaxWindow.Std())
		}
	}
```

In `internal/app/notify.go`, `BuildMatrixFeedback` — extend the early return and add the option:

```go
	if !mc.FeedbackReactions && !mc.ThreadCapture && !mc.SilenceReactions {
		return nil
	}
```

```go
	if mc.SilenceReactions {
		opts = append(opts, notify.WithSilenceReactions(cfg.Notify.Silence.Default(), cfg.Notify.Silence.MaxWindow.Std()))
	}
```

`WithSilenceReactions` is implemented in Task 7. **If you are executing tasks in order, this line will not compile until Task 7 lands.** Add it now and complete Task 5's commit *without* it, then add it as the first step of Task 7 — or implement Task 7's option constructor first. Do not leave the tree broken at a commit boundary.

- [ ] **Step 7: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
Expected: all pass.

- [ ] **Step 8: Commit**

```bash
git add internal/app/investigate.go internal/app/serve.go internal/app/notify.go internal/server/server.go internal/app/notify_test.go
git commit -m "feat(app): wire silencing, and build the gate when only it is on

recurrence_cooldown defaults to 0, so constructing the suppression gate
only when a cooldown is set left a silence-only deployment with a nil gate:
every click durably recorded, acked as successful, and then ignored. The
predicate is now named and tested rather than inline.

The window cap is set on the ledger rather than in each caller, so a
free-text Matrix command is held to the same bound as the Slack presets."
```

---

## Task 6: Slack — the overflow menu, the handler, and the warning

**Files:**
- Modify: `internal/notify/slack.go`
- Modify: `internal/notify/registry.go`
- Modify: `internal/server/server.go` (`handleSlackInteraction`)
- Modify: `internal/notify/card_golden_test.go` (coverage-guard entry)
- Create: `internal/thread/silence.go` (the shared `SilenceAck`)
- Test: `internal/notify/slack_silence_test.go` (create)
- Test: `internal/server/silence_test.go` (create)
- Test: `internal/thread/silence_test.go` (create)

**Interfaces:**
- Consumes: `server.SilenceRecorder`, `Server.silence` (Task 5); `SilenceNotify.Std()` (Task 4).
- Produces: `silenceActionID = "runlore_silence"`; `silenceBlockIDPrefix = "sil:"`; `feedbackBlocks(inv providers.Investigation, silenceWindows []time.Duration) []map[string]any`; `Slack.SilenceWindows` / `SlackBot.SilenceWindows` (`[]time.Duration`); **`thread.SilenceAck(user string, window time.Duration, until time.Time) string`** — the one acknowledgement text every transport posts, consumed again by Task 8.

**Background — the encoding constraint.** Slack caps an overflow `option.value` at **75 characters** (a `button.value` gets 2000). A GitOps `TriggerKey` is `Workload.Ref() + ":" + Reason` = `namespace/name:Reason`; `kube-system/nginx-ingress-controller-abc123:ProgressDeadlineExceeded` is already 68 characters and Kubernetes names run to 253. **Putting the key in `option.value` would make Slack reject the entire message** — killing the notification, not just the button.

So: the **window** rides in `option.value` (e.g. `"4h"`, 2 chars) and the **`TriggerKey`** rides in the actions block's `block_id` (255 chars), which Slack echoes back on every `payload.actions[]` entry.

`feedbackBlocks` has two call sites — `slack.go:197` (bot path, buttons on the channel summary) and `slack.go:345` (webhook path). Both must pass the windows.

- [ ] **Step 1: Write the failing rendering test**

Create `internal/notify/slack_silence_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func silenceWindows() []time.Duration {
	return []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour}
}

// findOverflow returns the silence overflow element from a rendered block set,
// plus the block_id of the actions block carrying it.
func findOverflow(t *testing.T, blocks []map[string]any) (map[string]any, string) {
	t.Helper()
	for _, b := range blocks {
		els, ok := b["elements"].([]map[string]any)
		if !ok {
			continue
		}
		for _, el := range els {
			if el["action_id"] == silenceActionID {
				id, _ := b["block_id"].(string)
				return el, id
			}
		}
	}
	return nil, ""
}

// TestSilenceOverflowCarriesTheKeyInTheBlockID pins the encoding: Slack caps an
// overflow option value at 75 chars, so the TriggerKey CANNOT live there.
func TestSilenceOverflowCarriesTheKeyInTheBlockID(t *testing.T) {
	inv := providers.Investigation{Title: "boom", TriggerKey: "production/payments-api:ProgressDeadlineExceeded"}
	el, blockID := findOverflow(t, feedbackBlocks(inv, silenceWindows()))
	if el == nil {
		t.Fatal("no silence overflow element rendered")
	}
	if want := silenceBlockIDPrefix + inv.TriggerKey; blockID != want {
		t.Errorf("block_id = %q, want %q", blockID, want)
	}
	opts, ok := el["options"].([]map[string]any)
	if !ok || len(opts) != 3 {
		t.Fatalf("options = %v, want 3 entries", el["options"])
	}
	for _, o := range opts {
		v, _ := o["value"].(string)
		if len(v) > 75 {
			t.Errorf("option value %q is %d chars; Slack caps it at 75", v, len(v))
		}
		if _, err := time.ParseDuration(v); err != nil {
			t.Errorf("option value %q is not a duration: %v", v, err)
		}
	}
}

// TestSilenceOverflowEveryValueIsUnder75 is the one that would have caught the
// original design: a realistic long GitOps key must not reach an option value.
func TestSilenceOverflowEveryValueIsUnder75(t *testing.T) {
	long := "kube-system/nginx-ingress-controller-cluster-wide-abcdef123456:ProgressDeadlineExceeded"
	inv := providers.Investigation{Title: "boom", TriggerKey: long}
	blocks := feedbackBlocks(inv, silenceWindows())
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"value":"`+long) {
		t.Error("the TriggerKey reached an option value; Slack would reject the whole message")
	}
}

// TestSilenceOmittedWhenTheBlockIDWouldOverflow: a pathological resource name
// degrades ONE control, never the card. Mirrors feedbackBlocks' existing posture
// of rendering no buttons when there is nothing to attribute.
func TestSilenceOmittedWhenTheBlockIDWouldOverflow(t *testing.T) {
	inv := providers.Investigation{Title: "boom", TriggerKey: strings.Repeat("x", 300)}
	blocks := feedbackBlocks(inv, silenceWindows())
	if el, _ := findOverflow(t, blocks); el != nil {
		t.Error("silence element rendered with an over-long block_id")
	}
	if len(blocks) == 0 {
		t.Error("the whole actions block was dropped; only the silence element should be")
	}
}

// TestSilenceAbsentWithoutWindows: no configured presets means the capability is
// off, and nothing should render.
func TestSilenceAbsentWithoutWindows(t *testing.T) {
	inv := providers.Investigation{Title: "boom", TriggerKey: "k"}
	if el, _ := findOverflow(t, feedbackBlocks(inv, nil)); el != nil {
		t.Error("silence element rendered with no configured windows")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/notify/ -run TestSilence -v`
Expected: FAIL — compile error, `silenceActionID` undefined and `feedbackBlocks` takes one argument.

- [ ] **Step 3: Implement the rendering**

In `internal/notify/slack.go`, extend the action-id const block (~line 305):

```go
	silenceActionID      = "runlore_silence"
```

Add beside it:

```go
// silenceBlockIDPrefix namespaces the actions block whose block_id carries the
// TriggerKey for the silence overflow, and slackBlockIDMax is Slack's cap on that
// field.
//
// The key rides in block_id rather than in the overflow's option values because
// Slack caps an option value at 75 characters (a button value gets 2000), and a
// GitOps TriggerKey is `namespace/name:Reason` — routinely 60-70 characters, and
// unbounded in principle since Kubernetes names run to 253. An over-long option
// value makes Slack reject the ENTIRE message, so the failure would take out the
// notification, not just the control.
const (
	silenceBlockIDPrefix = "sil:"
	slackBlockIDMax      = 255
)
```

Replace `feedbackBlocks`:

```go
// feedbackBlocks renders the human end of the learning loop: 👍/👎 plus, when
// silenceWindows is non-empty, a 🔕 overflow offering each configured window.
//
// The three are one row and one verdict vocabulary — 👍 accurate, 👎 off-base,
// 🔕 accurate but known — but they are NOT one capability: a rating weighs a
// recalled entry's trust, while a silence suppresses re-investigation. They are
// enabled by separate config flags and this function renders whichever are on.
//
// Attribution is the TriggerKey (incident identity — ratings and silences survive
// re-worded re-investigations), falling back to the alert fingerprint; with
// neither there is nothing for the ledger to attribute, so nothing renders.
//
// The silence element's TriggerKey travels in the block's block_id, not in the
// option values — see silenceBlockIDPrefix for why. If the key is too long for
// even that, the silence element alone is dropped: a pathological resource name
// must degrade ONE control, never the card.
//
// Labels are plain_text (never escaped); values are opaque to Slack.
func feedbackBlocks(inv providers.Investigation, silenceWindows []time.Duration) []map[string]any {
	key := cmp.Or(inv.TriggerKey, inv.Fingerprint)
	if key == "" {
		return nil
	}
	block := map[string]any{"type": "actions", "elements": []map[string]any{
		{"type": "button", "action_id": feedbackUpActionID, "value": key,
			"text": map[string]any{"type": "plain_text", "text": "👍 Accurate", "emoji": true}},
		{"type": "button", "action_id": feedbackDownActionID, "value": key,
			"text": map[string]any{"type": "plain_text", "text": "👎 Off-base", "emoji": true}},
	}}
	blockID := silenceBlockIDPrefix + key
	if len(silenceWindows) > 0 && len(blockID) <= slackBlockIDMax {
		opts := make([]map[string]any, 0, len(silenceWindows))
		for _, w := range silenceWindows {
			opts = append(opts, map[string]any{
				"text":  map[string]any{"type": "plain_text", "text": "🔕 Silence " + w.String(), "emoji": true},
				"value": w.String(),
			})
		}
		block["block_id"] = blockID
		block["elements"] = append(block["elements"].([]map[string]any), map[string]any{
			"type": "overflow", "action_id": silenceActionID, "options": opts,
		})
	}
	return []map[string]any{block}
}
```

Add a `SilenceWindows []time.Duration` field to both `Slack` and `SlackBot`, documented as "presets offered by the 🔕 overflow (notify.silence.windows); empty disables the control", and update both call sites to `feedbackBlocks(inv, s.SilenceWindows)`.

In `internal/notify/registry.go`, populate the new field from `cfg.Notify.Silence.Std()` wherever `FeedbackButtons` is currently copied across (there are copies for both the webhook and bot constructors — grep for `FeedbackButtons` in that file and mirror each one).

- [ ] **Step 4: Run the rendering tests**

Run: `go test ./internal/notify/ -run TestSilence -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Satisfy the golden coverage guard**

`internal/notify/card_golden_test.go:427` is a **coverage guard**: it asserts a fixture reaches each listed branch, so the golden digest would move if that branch changed. Add an entry beside `"👍 Accurate"`:

```go
		"🔕 Silence",                            // feedbackBlocks' silence overflow
```

Run: `go test ./internal/notify/ -run Golden -v`
Expected: FAIL with *"no fixture reaches the branch that renders …"* — no fixture renders the silence element yet.

Find the fixture that exercises `feedbackBlocks` (search the golden test for where feedback buttons are enabled) and give it silence windows. Re-run and regenerate the golden if the repo has an update flag (check the golden test's header comment for the exact mechanism — do **not** hand-edit `testdata/incident-card.golden.json`).

Expected after: PASS.

- [ ] **Step 6: Write the failing handler test**

Create `internal/server/silence_test.go`. It reuses `slackSign` and `discardLog` from `server_test.go` (same package), mirroring `TestSlackFeedbackInteraction` at `server_test.go:383`:

```go
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type recordedSilence struct {
	key    string
	window time.Duration
	user   string
}

type recordSilence struct {
	got []recordedSilence
	err error
}

func (r *recordSilence) Silence(key string, window time.Duration, user string, _ time.Time) error {
	if r.err != nil {
		return r.err
	}
	r.got = append(r.got, recordedSilence{key: key, window: window, user: user})
	return nil
}

// sendSilence posts a signed overflow-selection interaction. Note the payload
// shape: an overflow carries its choice in selected_option.value, NOT in value,
// and the TriggerKey rides in the action's block_id.
func sendSilence(t *testing.T, srv *Server, secret, blockID, value string) *httptest.ResponseRecorder {
	t.Helper()
	payload := `{"user":{"id":"U9","username":"bob"},"actions":[{"action_id":"runlore_silence",` +
		`"block_id":"` + blockID + `","selected_option":{"value":"` + value + `"}}]}`
	body := "payload=" + url.QueryEscape(payload)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", slackSign(secret, ts, body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestSlackSilenceInteraction(t *testing.T) {
	rec := &recordSilence{}
	const secret = "shh"
	srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)

	if rr := sendSilence(t, srv, secret, "sil:ns/app:CrashLoop", "4h"); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	want := recordedSilence{key: "ns/app:CrashLoop", window: 4 * time.Hour, user: "U9"}
	if len(rec.got) != 1 || rec.got[0] != want {
		t.Fatalf("recorded = %+v, want exactly one %+v", rec.got, want)
	}
}

// TestSlackSilenceMalformedPayloadsRecordNothing: every rejected shape must be
// acked (200, so Slack does not retry) and must record nothing.
func TestSlackSilenceMalformedPayloadsRecordNothing(t *testing.T) {
	const secret = "shh"
	for _, tc := range []struct {
		name    string
		blockID string
		value   string
	}{
		{"block_id without the sil: prefix", "ns/app:CrashLoop", "4h"},
		{"empty key after the prefix", "sil:", "4h"},
		{"a value that is not a duration", "sil:k", "forever"},
		{"an empty value", "sil:k", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordSilence{}
			srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
			if rr := sendSilence(t, srv, secret, tc.blockID, tc.value); rr.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200 (acked, not retried)", rr.Code)
			}
			if len(rec.got) != 0 {
				t.Errorf("recorded %+v, want nothing", rec.got)
			}
		})
	}
}

// TestSlackSilenceDisabledIsAckedNotFatal: silencing off must ack the click
// rather than 404, panic, or silently succeed.
func TestSlackSilenceDisabledIsAckedNotFatal(t *testing.T) {
	const secret = "shh"
	// A feedback-only server: the endpoint is up, but s.silence is nil.
	srv := New(nil, Actions{Feedback: &recordFeedback{}, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
	if rr := sendSilence(t, srv, secret, "sil:k", "4h"); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
}

// TestSlackSilenceEndpointUpWithSilenceAlone: a deployment that enabled ONLY
// silencing must get a live endpoint. Guards the enablement condition in
// handleSlackInteraction, which previously required approvals or feedback.
func TestSlackSilenceEndpointUpWithSilenceAlone(t *testing.T) {
	const secret = "shh"
	srv := New(nil, Actions{Silence: &recordSilence{}, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
	if rr := sendSilence(t, srv, secret, "sil:k", "1h"); rr.Code == http.StatusNotFound {
		t.Fatal("endpoint 404s with silencing as the only enabled capability")
	}
}

// TestSlackSilenceRecorderErrorIsAcked: a ledger write failure must reach the
// human as a message, never as a 500 Slack will retry.
func TestSlackSilenceRecorderErrorIsAcked(t *testing.T) {
	const secret = "shh"
	rec := &recordSilence{err: errTestSilence}
	srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
	if rr := sendSilence(t, srv, secret, "sil:k", "4h"); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
}

var errTestSilence = errors.New("ledger is full of bees")
```

Add `"errors"` to the imports. If `recordFeedback` is declared in `server_test.go` with different field names, adjust the reference in `TestSlackSilenceDisabledIsAckedNotFatal` — do not redeclare it.

**The ack WORDING is tested separately**, in `internal/thread/silence_test.go`, because it reaches the human through `updateSlack(…, p.ResponseURL, …)` and the test payload carries no response URL — so the handler test above cannot see it. `SilenceAck` is a pure function; test it as one:

```go
func TestSilenceAckCarriesTheWarning(t *testing.T) {
	got := SilenceAck("bob", 4*time.Hour, time.Date(2026, 8, 25, 18, 42, 0, 0, time.UTC))
	// Substrings, not the whole string: the wording should be tunable without
	// breaking the test, but these four facts must always survive a rewrite.
	for _, want := range []string{"will NOT investigate", "CRITICAL", "👎", "4h"} {
		if !strings.Contains(got, want) {
			t.Errorf("SilenceAck() = %q, missing %q", got, want)
		}
	}
}
```

- [ ] **Step 7: Implement the handler case**

In `handleSlackInteraction`, add a case beside the feedback one:

```go
	case "runlore_silence":
		// Unprivileged, like feedback and unlike approve/reject: the signature
		// proves the workspace, and the blast radius is bounded four independent
		// ways — the window expires, a CRITICAL firing is never suppressed, any
		// colleague's 👎 re-arms it, and every silence is attributed in the ledger.
		if s.silence == nil {
			msg = "⚠️ silencing not enabled (notify.slack.silence_button is off)"
			break
		}
		key, ok := strings.CutPrefix(act.BlockID, "sil:")
		if !ok || key == "" {
			msg = "⚠️ could not identify the incident to silence"
			s.log.Warn("slack silence: unexpected block_id", "block_id", act.BlockID)
			break
		}
		window, werr := time.ParseDuration(act.SelectedOption.Value)
		if werr != nil {
			msg = "⚠️ could not read the silence window"
			s.log.Warn("slack silence: bad window", "value", act.SelectedOption.Value, "err", werr)
			break
		}
		now := time.Now()
		if serr := s.silence.Silence(key, window, p.User.ID, now); serr != nil {
			msg = "⚠️ silencing failed: " + serr.Error()
			s.log.Warn("slack silence failed", "key", key, "window", window, "err", serr)
			break
		}
		msg = thread.SilenceAck(p.User.Username, window, now.Add(window))
		s.log.Info("slack silence recorded", "key", key, "window", window,
			"until", now.Add(window), "user_id", p.User.ID, "user", p.User.Username)
```

The action id is spelled as a **bare literal** here, exactly as `"runlore_approve"` and `"runlore_feedback_up"` already are in this switch — `internal/server` cannot import `internal/notify`, so the two spellings are kept in sync by convention. **Add a comment at each spelling pointing at the other** — `approveActionID`'s existing comment (`slack.go:304`, *"must match the server's `/slack/interactions` handler"*) is the precedent to follow. There is no automated guard on this pairing today; matching the existing convention is what keeps it findable.

The overflow payload carries the chosen option in `actions[0].selected_option.value`, **not** `actions[0].value` — extend `slackInteraction`'s action struct with `BlockID string \`json:"block_id"\`` and `SelectedOption struct{ Value string \`json:"value"\` } \`json:"selected_option"\``.

Add the ack builder — **in `internal/thread`, not in `internal/server`.** The Matrix path (Task 8) must post the identical text, the spec is explicit that a silence must not mean two different things depending on the room, and `internal/thread` cannot import `internal/server`. `internal/server` already imports `internal/thread` (for `thread.Dispatcher`), so putting it there creates no new dependency edge and needs no later move.

In `internal/thread/responder.go` (or a small new `internal/thread/silence.go`):

```go
// SilenceAck is the message posted back after a human silences an
// investigation, on EVERY transport. It carries an explicit WARNING, because a
// silence is the one feedback verdict that changes what RunLore does: a reader
// who clicked expecting "note my opinion" has in fact switched off
// investigation for this incident, and the escape hatches are only reassuring
// if they are stated at the point of the click.
//
// It lives here, shared, rather than being spelled once per transport: two
// copies would drift, and a silence meaning something subtly different in Slack
// than in Matrix is exactly the confusion the warning exists to prevent.
func SilenceAck(user string, window time.Duration, until time.Time) string {
	return fmt.Sprintf("🔕 Silenced by @%s until %s (%s).\n\n"+
		"⚠️ RunLore will NOT investigate this incident while the silence stands — "+
		"no model call, no notification, no record. A CRITICAL firing still breaks "+
		"through; a 👎 or a resolved alert re-arms it immediately.",
		user, until.Format("15:04 MST"), window)
}
```

Finally, extend the `replace` decision's comment — it already excludes feedback, and silence joins it for the same reason:

```go
	// Approve/reject replace the interaction message with the outcome; a feedback
	// or silence ack must NOT — replacing would wipe the investigation the rating
	// or the silence is about.
	replace := act.ActionID == "runlore_approve" || act.ActionID == "runlore_reject"
```

- [ ] **Step 8: Run the handler tests**

Run: `go test ./internal/server/ -run Silence -v`
Expected: PASS.

- [ ] **Step 9: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
Expected: all pass.

- [ ] **Step 10: Commit**

```bash
git add internal/notify/slack.go internal/notify/registry.go internal/notify/slack_silence_test.go internal/notify/card_golden_test.go internal/notify/testdata internal/server/server.go internal/server/silence_test.go internal/thread/silence.go internal/thread/silence_test.go
git commit -m "feat(slack): a third button that silences an investigation

The window rides in the overflow option value; the TriggerKey rides in the
actions block's block_id. Slack caps an option value at 75 characters and a
GitOps TriggerKey is namespace/name:Reason — routinely past that — so
encoding the key there would have made Slack reject the whole message,
taking out the notification rather than just the control. A key too long
even for block_id drops the silence element alone, never the card.

The ack carries an explicit warning: this is the one feedback verdict that
changes what RunLore does, so what was switched off, and what still breaks
through, are stated at the point of the click."
```

---

## Task 7: Matrix — the 🔕 reaction

**Files:**
- Modify: `internal/notify/matrix_feedback.go`
- Test: `internal/notify/matrix_silence_test.go` (create)

**Interfaces:**
- Consumes: `SilenceNotify.Default()` / `MaxWindow` (Task 4); the `BuildMatrixFeedback` call added in Task 5 Step 6.
- Produces: `notify.SilenceSink` interface; `WithSilenceReactions(defaultWindow, maxWindow time.Duration) MatrixFeedbackOption`; `MatrixFeedback.silenceReactions`, `.silenceWindow`, `.silenceSink`.

**Background — three existing invariants you must not break.**

1. **The `/sync` filter is a capability boundary**, not just an optimisation. `sync` (`matrix_feedback.go:354`) requests `m.reaction` **only** when `feedbackReactions` is on. Silencing also needs reactions, so the condition becomes `feedbackReactions || silenceReactions`. Read that function's doc comment before editing it.
2. **`handleReaction`'s opt-in guard is per capability.** It currently returns early on `!f.feedbackReactions`. A deployment with `silence_reactions` on and `feedback_reactions` off must record 🔕 and **must not** record 👍/👎 — so the guard moves from the top of the function to the individual emoji cases.
3. **The self-authorship check is the trust anchor.** A vote counts only when the reacted-to event was sent by the bot (`f.self`). This matters more for silence than for votes: without it, any room member could post a message carrying an `io.runlore.trigger_key` of their choosing and silence an arbitrary incident — a denial-of-investigation primitive. `triggerKeyFor` already enforces it; route through it and change nothing about it.

- [ ] **Step 1: Write the failing test**

Create `internal/notify/matrix_silence_test.go`. Read `internal/notify/matrix_test.go` and `matrix_feedback_test.go` first and reuse their homeserver-stub helpers. The behaviours to pin, each its own test:

```go
// 1. A 🔕 reaction on one of RunLore's own messages records
//    Silence(<triggerKey>, defaultWindow, <sender>, ~now) exactly once.
// 2. The variation selector is stripped: "🔕️" and "🔕" are the same action
//    (the existing 👍 handling does this with strings.ReplaceAll — mirror it).
// 3. With silenceReactions OFF, a 🔕 records nothing, even if the listener is
//    running for feedback reactions.
// 4. With feedbackReactions OFF but silenceReactions ON, a 👍 records nothing
//    and a 🔕 records a silence. This is the guard-placement test — it fails if
//    the opt-in check is left at the top of handleReaction.
// 5. A 🔕 on an event NOT sent by the bot records nothing (the trust anchor).
// 6. Any other emoji records nothing.
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/notify/ -run TestMatrixSilence -v`
Expected: FAIL — compile error, `WithSilenceReactions` undefined.

- [ ] **Step 3: Add the sink, the fields, and the option**

In `internal/notify/matrix_feedback.go`, beside `FeedbackSink` (~line 30):

```go
// SilenceSink records a human 🔕 silence on a delivered investigation
// (implemented by *outcome.Ledger — the same ledger the Slack path records
// through, so the window cap, the dedup and the gate's read are shared by
// construction). Kept separate from FeedbackSink rather than widening it: the
// two capabilities have separate config flags and must be independently nil.
type SilenceSink interface {
	Silence(triggerKey string, window time.Duration, user string, at time.Time) error
}
```

Add to the `MatrixFeedback` struct:

```go
	// silenceReactions, silenceWindow and silenceSink back the 🔕 reaction
	// (notify.matrix.silence_reactions). silenceWindow is notify.silence.windows[0]:
	// a reaction is a bare emoji and can carry no duration of its own, so the
	// default is the only window this path can offer. The `silence:` thread command
	// is what offers the full choice.
	silenceReactions bool
	silenceWindow    time.Duration
	silenceSink      SilenceSink
```

`NewMatrixFeedback` already takes the ledger as `sink FeedbackSink`. Rather than change its signature, have `WithSilenceReactions` accept the sink implicitly by type-asserting the existing one — the ledger satisfies both:

```go
// WithSilenceReactions turns on Matrix 🔕 silence reactions
// (notify.matrix.silence_reactions). Like WithFeedbackReactions it is threaded
// through both the /sync filter and the handler's own guard, so a listener that
// was never given this option never records a silence even if it somehow
// received the reaction.
//
// defaultWindow is notify.silence.windows[0] — the only window a bare emoji can
// express. maxWindow is carried for the `silence:` command path and is enforced
// by the ledger regardless; it is threaded here so a misconfiguration is visible
// at construction rather than at the first click.
func WithSilenceReactions(defaultWindow, maxWindow time.Duration) MatrixFeedbackOption {
	return func(f *MatrixFeedback) {
		if defaultWindow <= 0 || (maxWindow > 0 && defaultWindow > maxWindow) {
			return // a misconfigured window leaves the capability off rather than silently clamping
		}
		sink, ok := f.sink.(SilenceSink)
		if !ok {
			return // the configured sink cannot record silences; leave the capability off
		}
		f.silenceReactions = true
		f.silenceWindow = defaultWindow
		f.silenceSink = sink
	}
}
```

**Order matters:** `WithSilenceReactions` reads `f.sink`, which `NewMatrixFeedback` sets from its parameter *before* applying options. Verify that ordering in `NewMatrixFeedback` before relying on it; if options are applied first, set the sink first.

- [ ] **Step 4: Widen the `/sync` filter**

```go
	if f.feedbackReactions || f.silenceReactions {
		types = append(types, `"m.reaction"`)
	}
```

Update that function's doc comment, which currently says "m.reaction only when feedbackReactions is on", to name both capabilities.

- [ ] **Step 5: Move the guard and add the 🔕 case**

In `handleReaction`, delete the top-level `if !f.feedbackReactions { return }` and gate per emoji:

```go
	// The opt-in check is PER CAPABILITY, not per function: a deployment with
	// silence_reactions on and feedback_reactions off must record 🔕 and must not
	// record 👍/👎. A single early return at the top of this function would
	// silently grant one capability to anyone who enabled the other — the exact
	// class of bug the original guard was written to prevent.
	kind := ""
	switch strings.ReplaceAll(e.Content.RelatesTo.Key, "️", "") {
	case "👍":
		if !f.feedbackReactions {
			return
		}
		kind = "up"
	case "👎":
		if !f.feedbackReactions {
			return
		}
		kind = "down"
	case "🔕":
		if !f.silenceReactions {
			return
		}
		kind = "silence"
	default:
		return
	}
```

Then, after `triggerKeyFor` resolves the key (unchanged — it carries the self-authorship trust anchor), branch on `kind`:

```go
	if kind == "silence" {
		if err := f.silenceSink.Silence(key, f.silenceWindow, e.Sender, time.Now()); err != nil {
			f.log.Warn("matrix silence recording failed", "key", key, "err", err)
			return
		}
		f.log.Info("matrix silence recorded", "key", key, "window", f.silenceWindow, "user", e.Sender)
		return
	}
	if err := f.sink.Feedback(key, kind, e.Sender, time.Now()); err != nil {
		f.log.Warn("matrix feedback recording failed", "key", key, "err", err)
		return
	}
	f.log.Info("matrix feedback recorded", "key", key, "rating", kind, "user", e.Sender)
```

Update `handleReaction`'s doc comment to describe all three emoji.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/notify/ -run TestMatrix -v`
Expected: PASS — including every pre-existing Matrix test. If an existing feedback test now fails, the guard move in Step 5 changed 👍/👎 behaviour and must be corrected.

- [ ] **Step 7: Complete the Task 5 wiring**

If you deferred the `WithSilenceReactions` line from Task 5 Step 6, add it to `internal/app/notify.go` now:

```go
	if mc.SilenceReactions {
		opts = append(opts, notify.WithSilenceReactions(cfg.Notify.Silence.Default(), cfg.Notify.Silence.MaxWindow.Std()))
	}
```

- [ ] **Step 8: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
Expected: all pass.

- [ ] **Step 9: Commit**

```bash
git add internal/notify/matrix_feedback.go internal/notify/matrix_silence_test.go internal/app/notify.go
git commit -m "feat(matrix): record a silence from a bell reaction

A bare emoji carries no duration, so the reaction path uses the first
configured preset; the silence: thread command offers the full choice.

The opt-in check moves from the top of handleReaction to the individual
emoji cases: a deployment with silence_reactions on and feedback_reactions
off must record the bell and must not record ratings, which a single early
return could not express. The /sync filter widens on either capability.

Routed through the existing self-authorship check, which matters more here
than for ratings: without it, any room member could post a message
carrying a trigger key of their choosing and silence an arbitrary
incident."
```

---

## Task 8: Matrix — the `silence:` thread command

**Files:**
- Modify: `internal/thread/grammar.go`
- Modify: `internal/thread/responder.go`
- Test: `internal/thread/grammar_silence_test.go` (create)

**Interfaces:**
- Consumes: `Responder.Handle` (existing); the ledger's `Silence` (Task 1).
- Produces: `thread.IntentSilence`; `thread.SilenceRecorder`; `Responder.Silence` and `Responder.SilenceMax` fields.

**Background.** `Parse` is **pure** — it matches each prefix as a whole, colon-anchored token anywhere in the message and knows nothing about configuration. `prefixes` (`grammar.go:62`) is in **priority order**: the first entry whose token appears anywhere wins, regardless of position in the text. `reinvestigate:` leads so a note that also contains it is refused rather than recorded.

`silence:` belongs with `reinvestigate:` at high priority and for the identical reason: **it changes behaviour**. A note reading *"we agreed to silence: this until Thursday"* must be refused as an ambiguous command rather than silently filed as knowledge. Read `Parse`'s doc comment in full — it explains why erring toward "refuse" is the right side for this class of prefix.

- [ ] **Step 1: Write the failing test**

Create `internal/thread/grammar_silence_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import "testing"

func TestParseSilence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        string
		wantIntent Intent
		wantText   string
	}{
		{"anchored", "silence: 4h", IntentSilence, "4h"},
		{"with a leading mention", "@runlore silence: 24h", IntentSilence, "24h"},
		{"no duration", "silence:", IntentSilence, ""},
		{"unanchored still matches", "please silence: 1h", IntentSilence, "1h"},
		{
			// The priority rule: a note that also contains the command is refused
			// as ambiguous rather than filed, exactly as for reinvestigate:.
			name:       "a note containing the command loses to it",
			raw:        "note: we agreed to silence: this until Thursday",
			wantIntent: IntentSilence,
			wantText:   "this until Thursday",
		},
		{
			// Whole-token matching: a longer word ending in the prefix is prose.
			name:       "presilence: is not the command",
			raw:        "presilence: 4h",
			wantIntent: IntentFreeform,
			wantText:   "presilence: 4h",
		},
		{"plain prose is untouched", "can we silence this alert", IntentFreeform, "can we silence this alert"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Parse(tc.raw)
			if p.Intent != tc.wantIntent {
				t.Errorf("Intent = %v, want %v", p.Intent, tc.wantIntent)
			}
			if p.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", p.Text, tc.wantText)
			}
		})
	}
}

func TestSilenceIntentString(t *testing.T) {
	if got, want := IntentSilence.String(), "silence"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
```

**Confirm the `presilence:` expectation against `Parse`'s actual whole-token rule** (`grammar.go:152` — the character before the token must not be alphanumeric) before implementing; if the rule differs, fix the test's expectation, not the rule.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/thread/ -run TestParseSilence -v`
Expected: FAIL — compile error, `IntentSilence` undefined.

- [ ] **Step 3: Add the intent**

In `internal/thread/grammar.go`, add to the `Intent` const block after `IntentReinvestigate`:

```go
	// IntentSilence is the "silence:" prefix: suppress re-investigating this
	// incident for the duration that follows ("silence: 4h"). Like
	// IntentReinvestigate it CHANGES BEHAVIOUR rather than recording knowledge,
	// which is why it sits at high priority in prefixes — a note that merely
	// contains it is refused as ambiguous rather than filed as the human's words.
	IntentSilence
```

Add to `String()`:

```go
	case IntentSilence:
		return "silence"
```

Add to `prefixes`, **directly after** `reinvestigate:`:

```go
	{"silence:", IntentSilence},
```

Update the `prefixes` doc comment to explain that both behaviour-changing prefixes lead, and why.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/thread/ -run TestParseSilence -v`
Expected: PASS.

Run the whole grammar suite: `go test ./internal/thread/ -run 'TestParse|TestGrammar' -v`
Expected: PASS — the existing `note:` / `reinvestigate:` priority tests must be unaffected.

- [ ] **Step 5: Handle the intent in the responder**

Add to the `Responder` struct in `internal/thread/responder.go`:

```go
	// Silence records a `silence: <duration>` command; nil when silencing is not
	// enabled, in which case the command is answered with a short explanation
	// rather than silently ignored. SilenceMax is notify.silence.max_window,
	// carried only so the reply can state the bound — the LEDGER enforces it, and
	// remains the single place that does.
	Silence    SilenceRecorder
	SilenceMax time.Duration
```

```go
// SilenceRecorder records a human 🔕 silence (implemented by *outcome.Ledger).
// Declared here rather than imported because internal/thread cannot depend on
// internal/notify; each package declaring the narrow interface it consumes is
// the idiom this codebase already follows for feedback.
type SilenceRecorder interface {
	Silence(triggerKey string, window time.Duration, user string, at time.Time) error
}
```

Add the case to `Handle`'s switch, beside `IntentReinvestigate`:

```go
	case IntentSilence:
		return r.silence(tc, author, p.Text)
```

```go
// silence answers a `silence: <duration>` command. Every failure path REPLIES —
// a command that changes behaviour must never fail quietly, or the human walks
// away believing the incident is muted when it is not.
func (r *Responder) silence(tc Context, author, text string) (string, error) {
	if r.Silence == nil {
		return "Silencing isn't enabled here — ask an operator about `notify.matrix.silence_reactions`.", nil
	}
	if tc.TriggerKey == "" {
		return "I can't tell which incident this thread is about, so there's nothing to silence.", nil
	}
	window, err := time.ParseDuration(strings.TrimSpace(text))
	if err != nil {
		return fmt.Sprintf("I couldn't read %q as a duration — try `silence: 4h` (up to %s).",
			strings.TrimSpace(text), r.SilenceMax), nil
	}
	now := time.Now()
	if err := r.Silence.Silence(tc.TriggerKey, window, author, now); err != nil {
		return "Couldn't silence this: " + err.Error(), nil
	}
	return SilenceAck(author, window, now.Add(window)), nil
}
```

`SilenceAck` is already defined in this package by Task 6 Step 7, and `internal/server` already calls it — so the Matrix reply and the Slack ack are the same text by construction, with nothing to keep in sync.

- [ ] **Step 6: Wire the responder**

In whichever `internal/app` function builds the `*thread.Responder` (grep for `buildThreadResponder`), set `Silence` and `SilenceMax` when `cfg.Notify.SilenceEnabled() && ledger.Enabled()`.

- [ ] **Step 7: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
Expected: all pass.

- [ ] **Step 8: Commit**

```bash
git add internal/thread/grammar.go internal/thread/responder.go internal/thread/grammar_silence_test.go internal/server/server.go internal/app
git commit -m "feat(thread): a silence: command with an explicit duration

Sits beside reinvestigate: at high priority in the prefix table, for the
identical reason: it changes behaviour rather than recording knowledge, so
a note that merely contains it is refused as ambiguous rather than filed
as the human's words.

Every failure path replies. A command that mutes an incident must never
fail quietly, or the human walks away believing it is muted when it is
not. The acknowledgement text is shared with the Slack path so a silence
does not mean two different things depending on the room."
```

---

## Task 9: Documentation and chart

**Files:**
- Modify: `website/content/docs/integrations/notifications/slack.md`
- Modify: `website/content/docs/integrations/notifications/matrix.md` (if present; otherwise the Matrix section of the notifications index)
- Modify: `website/content/docs/operations/troubleshooting.md`
- Modify: `website/content/docs/concepts/learning-loop.md`
- Modify: `website/content/docs/configuration/configuration.md`
- Modify: `deploy/helm/runlore/values.yaml`, `deploy/helm/runlore/values-full.yaml`

**Interfaces:**
- Consumes: every symbol from Tasks 1-8. Nothing consumes this task.

**Background.** This repo pins documentation that restates code facts with reflection tests over the real parse target. Before writing, check whether the docs you are editing are covered by a guard in `internal/docsguard` — if the silence config block is restated in the docs, it may need to be added to whatever that guard enumerates. Run `go test ./internal/docsguard/ -v` after editing and fix anything it reports.

- [ ] **Step 1: Document the Slack button**

In `website/content/docs/integrations/notifications/slack.md`, in the feedback-buttons section, add the third control. Cover: what a click does (suppresses re-investigation — **no model call, no notification, no record**), that it is unprivileged and why that is safe, and all four escapes (expiry, CRITICAL, 👎, resolve). Include the config:

```yaml
notify:
  silence:
    windows: [1h, 4h, 24h]
    max_window: 24h
  slack:
    silence_button: true
    signing_secret_env: SLACK_SIGNING_SECRET
outcome:
  ledger_path: /data/outcome.jsonl
```

- [ ] **Step 2: Document the Matrix paths**

The 🔕 reaction (uses `windows[0]`, since an emoji carries no duration) and `silence: 4h`. Note `notify.matrix.silence_reactions` enables both.

- [ ] **Step 3: Add the troubleshooting row**

`website/content/docs/operations/troubleshooting.md` has a table for "why was RunLore quiet?". Add a row keyed on the log line from Task 3 Step 6 (`msg="silenced by a human: skipping re-investigation"`) and the metric `investigations_completed_total{result="silenced"}`.

This makes **five** suppression layers documented — dedup, debounce, coalesce, recurrence cooldown, silence. Say so in the section's intro: an operator debugging silence needs to know the other four exist.

- [ ] **Step 4: Document the third verdict**

In `website/content/docs/concepts/learning-loop.md`, alongside 👍/👎: 🔕 is the third verdict, and the only one that changes what RunLore *does* rather than how it weighs an entry. State that it does **not** currently feed the curator, and why (see the spec's "Open questions deferred").

- [ ] **Step 5: Document the config keys**

`website/content/docs/configuration/configuration.md` — one line per key: `notify.silence.windows`, `notify.silence.max_window`, `notify.slack.silence_button`, `notify.matrix.silence_reactions`. Mark each **opt-in**, and note the `outcome.ledger_path` requirement, matching how `recurrence_cooldown` is documented there.

- [ ] **Step 6: Update the chart**

`deploy/helm/runlore/values.yaml` — **commented out**, matching how `recurrence_cooldown` appears there:

```yaml
    # OPT-IN: a 🔕 control on the investigation card that suppresses
    # re-investigating the same incident for the chosen window. Requires
    # outcome.ledgerPath and a Slack signing secret.
    # silence:
    #   windows: [1h, 4h, 24h]
    #   max_window: 24h
```

`values-full.yaml` — active, with realistic values, matching how it sets `recurrence_cooldown: 30m`.

- [ ] **Step 7: Verify the docs guards**

Run: `go test ./internal/docsguard/ -v`
Expected: PASS. If a guard reports drift, the docs restate a code fact that has changed — fix the docs, not the guard.

- [ ] **Step 8: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`
Expected: all pass.

- [ ] **Step 9: Commit**

```bash
git add website deploy/helm/runlore
git commit -m "docs: the silence control, its escapes, and its config

Documents the third verdict across Slack, Matrix, the learning-loop
concept page and the config reference, and adds a troubleshooting row for
the log line and metric a silenced firing emits.

The 'why was RunLore quiet?' section now lists five suppression layers;
the intro says so, because an operator debugging one of them needs to know
the other four exist."
```

---

## Definition of Done

- [ ] `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh` — clean, `0 issues`.
- [ ] A silence recorded on one replica is enforced on the leader. Verified by reading the code path, not assumed: `POST /slack/interactions` is `work`-wrapped (`server.go:224`), so a follower proxies it single-hop and the fold happens in the leader's process.
- [ ] `hack/demo.sh` still renders a verdict card.
- [ ] `hack/demo-trigger-policy.sh` still prints its investigate/skip decisions.
- [ ] The compaction test genuinely bites — verified by temporarily breaking `snapshotCheckpointLocked` (Task 1 Step 8).
- [ ] The nil-gate regression test genuinely bites — verified by temporarily reverting `recurrenceGateWanted` to `cooldown > 0` (Task 5).
- [ ] PR description written in English, no AI attribution, no co-author trailer.

## Deliberately Not In This Plan

Carried from the spec's Non-goals so the scope cannot drift mid-execution:

- Broader mute scopes (per-workload, per-alert-class).
- A `lore silences` CLI.
- The inconclusive-retry backoff, and whether `recurrence_cooldown` should default on — **separate specs**.
- A modal with free-text reason capture.
- Feeding silences to the curator.
