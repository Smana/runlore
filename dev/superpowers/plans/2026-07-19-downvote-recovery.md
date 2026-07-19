# 👎 Recovery Path — Confirmation Evidence (N5) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** End the permanent full-cost re-investigation loop a single mistaken 👎 creates today:
when the re-investigation a standing 👎 forces reaches the SAME conclusion as the contested entry,
that confirmation becomes recovery evidence — while a human 👎 still outranks any single machine
confirmation.

**The loop being closed:** a standing 👎 (a) rejects recall via `outcomeFactor` and (b) bypasses the
recurrence cooldown (`TriggerRecurrence.Contested()`), so every recurrence runs a full paid
investigation. When that fresh investigation reaches the same RCA, `Curator.Curate`'s
catalog-fingerprint dedup (`internal/curator/curator.go:82-87`) produces NO artifact and records
NOTHING — the entry is never vindicated nor superseded, so the loop never converges.

**Architecture:** a new append-only ledger event kind `"confirm"` (old binaries ignore unknown
kinds by the documented forward-compat property of `foldLocked`). The curator's fingerprint-dedup
branch — reachable ONLY by fresh investigations, since recalls early-return at the top of `Curate`
— reports the match through a new nil-safe `ConfirmationSink` the app wires to the ledger.
Confirmations fold into `Aggregate.Confirms` (per-entry, drives `outcomeFactor` recovery at HALF
the weight of a human observation) and a per-TriggerKey index surfaced on the curate `Contested`
pass's reviewer comment. `TriggerRecurrence.Contested()` is deliberately UNCHANGED: the human
override on the cooldown stands until the voter changes their vote — but once the factor recovers,
recall fires again and each recurrence costs a recall (2 model calls), not a full investigation,
which is what ends the cost loop without weakening the human signal.

**Weighting (the fail-safe contract):** `confirmWeight = 0.5` — a machine confirmation is half a
Bernoulli observation in the same Beta posterior. With the defaults (k=2, floor 0.5): one standing
👎 alone → factor 1/3 (rejected); +1 confirmation → 1.5/3.5 ≈ 0.43 (STILL rejected — a human
outranks a single machine confirmation); +2 confirmations → 2/4 = 0.5 (recovered, since rejection
is strictly `f < floor`).

**Tech Stack:** Go (toolchain go1.26.5), stdlib testing; no new dependencies.

## Global Constraints

- Quality gate before EVERY commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — `0 issues`, empty `gofmt -l .`.
- Additionally `go test -race ./internal/outcome/ ./internal/curator/ ./internal/curate/` before the final commit.
- New `.go` files start with `// SPDX-License-Identifier: Apache-2.0` on line 1.
- Conventional Commits; NO co-author trailer, no AI attribution.
- Human feedback outranks machine evidence: one 👎 must survive one confirmation (pinned by test).
- Ledger events are append-only and durable-first (append before fold, mirroring `Feedback`); every new derived state must round-trip compaction checkpoints.
- **Conflicts:** `2026-07-19-recall-runner-up.md` (N3) also touches the `outcomeFactor` call in `internal/investigate/recall.go`. Execute sequentially; the later plan rebases the one call site.

## File Structure

- Modify: `internal/outcome/ledger.go` — event kind `"confirm"`, `Confirm()`, `applyConfirmLocked`, `Aggregate.Confirms`, `triggerConfirms` index, checkpoint round-trip, `ContestedTrigger.Confirms`.
- Modify: `internal/investigate/recall.go` — `outcomeFactor` gains `confirms` + `confirmWeight`.
- Modify: `internal/curator/curator.go` — `ConfirmationSink` + the dedup-branch hook.
- Modify: `internal/curate/contested.go` — reviewer comment renders the confirmation count.
- Modify: `internal/app/investigate.go:382` — wire `cur.Confirmations = ledger`.
- Modify: `docs/learning-loop.md` — recovery paragraph.
- Tests: `internal/outcome/ledger_confirm_test.go` (new), plus additions to `internal/investigate/recall_test.go`, `internal/curator/curator_test.go`, `internal/curate/contested_test.go`.

---

### Task 1: Ledger `"confirm"` event + per-entry/per-trigger fold + checkpoint round-trip

**Files:**
- Modify: `internal/outcome/ledger.go`
- Create: `internal/outcome/ledger_confirm_test.go`

**Interfaces:**
- Produces: `func (l *Ledger) Confirm(entry, triggerKey, dupFP string, at time.Time) error`; `Aggregate.Confirms int`; `ContestedTrigger.Confirms int`. Task 2 consumes `Aggregate.Confirms`; Task 3 calls `Confirm` (via the curator's sink); Task 4 consumes `ContestedTrigger.Confirms`.

- [ ] **Step 1: Write the failing tests** — create `internal/outcome/ledger_confirm_test.go`.
Mirror the construction style of the existing `ledger_test.go` (temp-file ledger via `New` /
`NewWithMaxEvents`; check that file first and reuse its helpers where they exist):

```go
// SPDX-License-Identifier: Apache-2.0

package outcome

import (
	"path/filepath"
	"testing"
	"time"
)

func TestConfirmFoldsIntoAggregateAndTriggerIndex(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := l.Confirm("kb/worker-oom.md", "trig-1", "fp-abc", at); err != nil {
		t.Fatal(err)
	}
	if err := l.Confirm("kb/worker-oom.md", "trig-1", "fp-abc", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	counts, err := l.OpenCounts()
	if err != nil {
		t.Fatal(err)
	}
	if got := counts["kb/worker-oom.md"].Confirms; got != 2 {
		t.Fatalf("Aggregate.Confirms = %d, want 2", got)
	}
}

func TestConfirmSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Confirm("kb/e.md", "trig-1", "fp", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A fresh ledger over the same file replays the confirm line.
	l2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	counts, _ := l2.OpenCounts()
	if got := counts["kb/e.md"].Confirms; got != 1 {
		t.Fatalf("replayed Confirms = %d, want 1", got)
	}
}

func TestConfirmSurvivesCompactionCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := NewWithMaxEvents(path, 5) // tiny cap forces compaction on reload
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := l.Confirm("kb/e.md", "trig-1", "fp", at); err != nil {
		t.Fatal(err)
	}
	// Push the confirm past the compaction horizon with filler opens.
	for i := 0; i < 10; i++ {
		if err := l.Open(Event{Fingerprint: DeriveFingerprint(GitOpsFingerprintPrefix, string(rune('a'+i))), Kind: "fresh", At: at.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	// Reload triggers compaction (file > maxEvents): the confirm is folded into the
	// checkpoint. Its contribution must survive into a THIRD load that only ever
	// sees the checkpoint.
	l2, err := NewWithMaxEvents(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	counts, _ := l2.OpenCounts()
	if got := counts["kb/e.md"].Confirms; got != 1 {
		t.Fatalf("post-compaction Confirms = %d, want 1 (lost by the checkpoint)", got)
	}
	l3, err := NewWithMaxEvents(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	counts3, _ := l3.OpenCounts()
	if got := counts3["kb/e.md"].Confirms; got != 1 {
		t.Fatalf("checkpoint-only Confirms = %d, want 1", got)
	}
}

func TestContestedTriggersCarryConfirmCount(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	// An open with a KB link makes the trigger contest-eligible…
	if err := l.Open(Event{Fingerprint: "fp1", Kind: "fresh", TriggerKey: "trig-1", CuratedURL: "https://github.com/o/r/pull/7", At: at}); err != nil {
		t.Fatal(err)
	}
	// …a standing 👎 contests it, and a confirmation is recorded against it.
	if err := l.Feedback("trig-1", "down", "U1", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := l.Confirm("kb/e.md", "trig-1", "fp-dup", at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	cts := l.ContestedTriggers()
	if len(cts) != 1 {
		t.Fatalf("ContestedTriggers = %v, want exactly one", cts)
	}
	if cts[0].Confirms != 1 {
		t.Fatalf("ContestedTrigger.Confirms = %d, want 1", cts[0].Confirms)
	}
}

func TestConfirmDisabledLedgerAndEmptyEntry(t *testing.T) {
	disabled, err := New("") // path "" ⇒ disabled ledger, mirroring Feedback's no-op contract
	if err != nil {
		t.Fatal(err)
	}
	if err := disabled.Confirm("kb/e.md", "t", "fp", time.Now()); err != nil {
		t.Fatalf("disabled ledger must no-op, got %v", err)
	}
	if err := disabled.Confirm("", "t", "fp", time.Now()); err == nil {
		t.Fatal("empty entry path must be an error (an unattributable confirm is a bug)")
	}
}
```

(Adjust `New`/`NewWithMaxEvents`/`GitOpsFingerprintPrefix` call shapes to the actual
constructors in `ledger.go` — verify before writing; the semantics above are the contract.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/outcome/ -run Confirm -v`
Expected: FAIL — `l.Confirm undefined`, `Aggregate` has no field `Confirms`.

- [ ] **Step 3: Implement in `internal/outcome/ledger.go`.**

3a. `Aggregate` (line ~952) gains the field:

```go
	// Confirms counts machine confirmations: fresh investigations that independently
	// reached this entry's conclusion (same DupFingerprint) — the recovery evidence a
	// standing 👎 forces into existence. Weighted at half a human observation in
	// outcomeFactor (confirmWeight); see Ledger.Confirm.
	Confirms int
```

3b. `Ledger` struct gains the trigger index (next to `votes`), reset in
`resetStateLocked` (line ~366):

```go
	// triggerConfirms counts confirmations per TriggerKey — the ContestedTriggers
	// join that lets the curate Contested pass tell a reviewer "N re-investigations
	// reached this same conclusion". Rebuilt on load, checkpointed on compaction.
	triggerConfirms map[string]int
```

```go
	l.triggerConfirms = map[string]int{}
```

3c. `Event` doc comment (line 62): extend the kind list to `"open" | "resolve" | "feedback" | "checkpoint" | "confirm"`. Add to `foldLocked` (line ~383):

```go
	case "confirm":
		l.applyConfirmLocked(e)
```

3d. Append + fold, mirroring `Feedback` (durable-first, mutex, disabled no-op):

```go
// Confirm appends a machine confirmation: a FRESH investigation independently
// reached the same deterministic identity (DupFingerprint) as an existing catalog
// entry — exactly the re-derivation a standing 👎 forces. It is folded as recovery
// evidence into the entry's aggregate (at confirmWeight, see outcomeFactor) and
// into the per-trigger confirmation count the Contested curate pass surfaces.
// entry must be non-empty (an unattributable confirm is a caller bug); triggerKey
// may be empty (human `lore investigate` has none — entry credit still applies).
// Recalls must never reach this: only the curator's fingerprint-dedup branch calls
// it, and Curate returns before that branch for recalled findings.
func (l *Ledger) Confirm(entry, triggerKey, dupFP string, at time.Time) error {
	if entry == "" {
		return fmt.Errorf("confirm: empty entry path")
	}
	if !l.enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Event{Event: "confirm", Entry: entry, TriggerKey: triggerKey, DupFingerprint: dupFP, At: at}
	if err := l.appendLocked(e); err != nil {
		return err // durable-first: leave the fold untouched, like Open/Feedback
	}
	l.applyConfirmLocked(e)
	return nil
}

// applyConfirmLocked folds one confirm event into the per-entry aggregate and the
// per-trigger index. Must be called with mu held (or during single-threaded load).
func (l *Ledger) applyConfirmLocked(e Event) {
	if e.Entry == "" {
		return // malformed replayed line: never folded
	}
	a := l.agg[e.Entry]
	a.Confirms++
	l.agg[e.Entry] = a
	if e.TriggerKey != "" {
		l.triggerConfirms[e.TriggerKey]++
	}
}
```

3e. Checkpoint round-trip. `checkpointData` (line ~201) gains:

```go
	TriggerConfirms map[string]int `json:"trigger_confirms,omitempty"`
```

(`Aggregate.Confirms` rides along automatically — `checkpointData.Agg` serializes
`Aggregate` wholesale, and old binaries ignore the unknown JSON field.)
`seedCheckpointLocked` (line ~403) copies it:

```go
	for k, v := range cd.TriggerConfirms {
		l.triggerConfirms[k] = v
	}
```

`snapshotCheckpointLocked` (line ~465) captures it:

```go
	if len(l.triggerConfirms) > 0 {
		cd.TriggerConfirms = make(map[string]int, len(l.triggerConfirms))
		for k, v := range l.triggerConfirms {
			cd.TriggerConfirms[k] = v
		}
	}
```

3f. `ContestedTrigger` (line ~834) gains:

```go
	Confirms int // machine confirmations recorded for this trigger since (recovery evidence)
```

and `ContestedTriggers()` sets it in the assembly loop:

```go
		out = append(out, ContestedTrigger{TriggerKey: trigger, CuratedURL: a.curatedURL, Downs: n, Last: a.last, Confirms: l.triggerConfirms[trigger]})
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/outcome/ -v && go test -race ./internal/outcome/`
Expected: all PASS, including every pre-existing ledger/compaction test.

- [ ] **Step 5: Commit**

```bash
git add internal/outcome/ledger.go internal/outcome/ledger_confirm_test.go
git commit -m "feat(outcome): confirm events fold recovery evidence through checkpoints"
```

---

### Task 2: `outcomeFactor` recovery weighting (human outranks one machine confirm)

**Files:**
- Modify: `internal/investigate/recall.go:476-497` (`outcomeFactor` + its one call site)
- Test: `internal/investigate/recall_test.go` (extend the existing `outcomeFactor` table at line ~247)

- [ ] **Step 1: Write the failing tests** — extend the table-driven `outcomeFactor` test in
`internal/investigate/recall_test.go` (match its exact table shape — it currently calls
`outcomeFactor(c.recalls, c.resolved, c.up, c.down, k)`; add a `confirms` column) with these
pinned cases, keeping every existing row (`confirms: 0` must reproduce today's values —
adding weight-0.5×0 changes nothing):

```go
		// Recovery contract (k=2, floor 0.5 in prod):
		{recalls: 0, resolved: 0, up: 0, down: 1, confirms: 0, want: 1.0 / 3.0},  // one 👎 alone: rejected
		{recalls: 0, resolved: 0, up: 0, down: 1, confirms: 1, want: 1.5 / 3.5},  // one confirm: STILL rejected (human wins)
		{recalls: 0, resolved: 0, up: 0, down: 1, confirms: 2, want: 2.0 / 4.0},  // two confirms: exactly at the floor → recovered
		{recalls: 0, resolved: 0, up: 0, down: 0, confirms: 2, want: 2.0 / 3.0},  // confirms alone build trust gently
```

Also add, near the table, the explicit floor-semantics pin:

```go
	// The recovery threshold is exactly the floor: rejection is strictly f < floor,
	// so factor == 0.5 passes. One 👎 + two confirms is the minimum recovery.
	if f := outcomeFactor(0, 0, 0, 1, 2, 2.0); f < 0.5 {
		t.Fatalf("one down + two confirms = %v, must reach the 0.5 floor", f)
	}
	if f := outcomeFactor(0, 0, 0, 1, 1, 2.0); f >= 0.5 {
		t.Fatalf("one down + ONE confirm = %v, must stay below the floor (human outranks one machine confirm)", f)
	}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/investigate/ -run OutcomeFactor -v`
Expected: FAIL — wrong argument count.

- [ ] **Step 3: Implement** in `recall.go`:

```go
// confirmWeight is how much of one human observation a machine confirmation is
// worth in the Beta posterior. Half: a fresh investigation re-deriving the same
// conclusion is real evidence, but a human 👎 must outrank any SINGLE machine
// confirmation — with k=2 and floor 0.5, one 👎 needs TWO confirmations to recover.
const confirmWeight = 0.5
```

`outcomeFactor` gains the parameter (extend the doc comment's formula to
`factor = (resolved + up + confirmWeight·confirms + k/2) / (recalls + up + down + confirmWeight·confirms + k)`
and a sentence on the recovery contract):

```go
func outcomeFactor(recalls, resolved, up, down, confirms int, k float64) float64 {
	c := confirmWeight * float64(confirms)
	return (float64(resolved+up) + c + k/2) / (float64(recalls+up+down) + c + k)
}
```

Update the one production call site (`recall.go:242`, or inside `outcomeGate` if
`2026-07-19-recall-runner-up.md` landed first):

```go
			f := outcomeFactor(agg.Recalls, agg.Resolved, agg.FeedbackUp, agg.FeedbackDown, agg.Confirms, r.OutcomePrior)
```

Update the two test-file call sites flagged by the compiler
(`recall_decay_integration_test.go:86` gains a `0` confirms argument, and the
`recall_test.go` table from Step 1).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/investigate/ -v`
Expected: all PASS — every `confirms: 0` row unchanged, recovery rows exact.

- [ ] **Step 5: Full gate + commit**

```bash
git add internal/investigate/recall.go internal/investigate/recall_test.go internal/investigate/recall_decay_integration_test.go
git commit -m "feat(recall): confirmations recover outcome decay at half human weight"
```

---

### Task 3: Curator hook — the fingerprint-dedup match reports a confirmation

**Files:**
- Modify: `internal/curator/curator.go` (struct field + the dedup branch, lines 82-87)
- Test: `internal/curator/curator_test.go`

**Interfaces:**
- Consumes: nothing new (the sink is defined here; `*outcome.Ledger` satisfies it structurally — curator does NOT import outcome).
- Produces: `type ConfirmationSink interface { Confirm(entry, triggerKey, dupFP string, at time.Time) error }`; `Curator.Confirmations ConfirmationSink`. Task 5 wires it.

- [ ] **Step 1: Write the failing tests.** In `internal/curator/curator_test.go`, reuse the
file's existing fakes for `Forge`/catalog (read them first; the catalog fake must implement
`FindFingerprint` — the `fingerprintFinder` type-assert — for the dedup branch to run). Add:

```go
type recordingSink struct{ calls []string }

func (s *recordingSink) Confirm(entry, triggerKey, dupFP string, _ time.Time) error {
	s.calls = append(s.calls, entry+"|"+triggerKey+"|"+dupFP)
	return nil
}

func TestFingerprintDedupRecordsConfirmation(t *testing.T) {
	// Arrange a curator whose catalog fake returns a fingerprint match for the
	// investigation's DupFingerprint (same construction as the existing
	// fingerprint-dedup test in this file), plus the sink.
	sink := &recordingSink{}
	c := newFingerprintDedupCurator(t) // the existing test's arrangement, extracted or inlined
	c.Confirmations = sink
	inv := fingerprintMatchingInvestigation() // ditto — the inv whose DupFingerprint the fake matches

	ref, err := c.Curate(context.Background(), inv)
	if err != nil || ref.URL != "" {
		t.Fatalf("dedup branch must still file nothing: ref=%v err=%v", ref, err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("exactly one confirmation must be recorded, got %v", sink.calls)
	}
}

func TestRecalledFindingNeverConfirms(t *testing.T) {
	sink := &recordingSink{}
	c := newFingerprintDedupCurator(t)
	c.Confirmations = sink
	inv := fingerprintMatchingInvestigation()
	inv.Recalled = true // Curate early-returns before the dedup branch

	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("a recall must NEVER record recovery evidence, got %v", sink.calls)
	}
}

func TestNilSinkIsSafe(t *testing.T) {
	c := newFingerprintDedupCurator(t) // Confirmations left nil
	if _, err := c.Curate(context.Background(), fingerprintMatchingInvestigation()); err != nil {
		t.Fatalf("nil sink must be a no-op, got %v", err)
	}
}
```

(`newFingerprintDedupCurator` / `fingerprintMatchingInvestigation` are extraction targets
from the existing fingerprint-dedup test in this file — factor them out rather than
duplicating the arrangement; if no such test exists, build the minimal fake catalog whose
`FindFingerprint` returns `(catalog.Entry{Path: "kb/e.md", Title: "e"}, true)`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/curator/ -run 'Confirm|NilSink' -v`
Expected: FAIL — `Curator` has no field `Confirmations`.

- [ ] **Step 3: Implement** in `internal/curator/curator.go`:

3a. Add `"time"` to imports; add the interface above the `Curator` struct and the field to it:

```go
// ConfirmationSink records that a FRESH investigation independently reached an
// existing catalog entry's conclusion (identical DupFingerprint) — the recovery
// evidence that lets a contested entry's outcome factor climb back (see
// outcome.Ledger.Confirm, which satisfies this). Optional and nil-safe, like
// Metrics: a curator without a ledger simply keeps today's behavior.
type ConfirmationSink interface {
	Confirm(entry, triggerKey, dupFP string, at time.Time) error
}
```

```go
	Confirmations ConfirmationSink // optional; nil-safe — dedup matches are recovery evidence
```

3b. Extend the fingerprint-dedup branch (lines 82-87), computing the fingerprint once:

```go
	if ff, ok := c.Catalog.(fingerprintFinder); ok {
		fp := DupFingerprint(inv)
		if e, found := ff.FindFingerprint(fp); found {
			c.Log.Info("finding matches a catalog entry's fingerprint; not filing", "entry", e.Title, "path", e.Path)
			// Recovery evidence: a fresh investigation independently re-derived this
			// entry's conclusion — exactly what a standing 👎 forces. Recalls never
			// reach here (Curate returns on inv.Recalled above). Best-effort: a sink
			// error is logged, never blocks the (artifact-free) dedup outcome.
			if c.Confirmations != nil {
				if err := c.Confirmations.Confirm(e.Path, inv.TriggerKey, fp, time.Now()); err != nil {
					c.Log.Warn("confirmation record failed", "entry", e.Path, "err", err)
				}
			}
			return providers.Ref{}, nil
		}
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/curator/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/curator/curator.go internal/curator/curator_test.go
git commit -m "feat(curator): fingerprint-dedup matches record confirmation evidence"
```

---

### Task 4: Contested pass surfaces the confirmation count to the reviewer

**Files:**
- Modify: `internal/curate/contested.go:113-121` (`contestedComment`)
- Test: `internal/curate/contested_test.go`

- [ ] **Step 1: Write the failing test** (match the existing `contestedComment` test style in
`contested_test.go`):

```go
func TestContestedCommentRendersConfirmations(t *testing.T) {
	ct := outcome.ContestedTrigger{TriggerKey: "trig-1", CuratedURL: "https://github.com/o/r/pull/7", Downs: 1, Confirms: 2}
	body := contestedComment(ct, contestedMarker(ct.TriggerKey))
	if !strings.Contains(body, "2 fresh re-investigations independently reached this same conclusion") {
		t.Fatalf("comment must surface the confirmation count, got:\n%s", body)
	}
	// Zero confirmations: the line is absent (no noise on the common case).
	ct.Confirms = 0
	if body := contestedComment(ct, contestedMarker(ct.TriggerKey)); strings.Contains(body, "independently reached") {
		t.Fatalf("no-confirmation comment must not mention confirmations, got:\n%s", body)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/curate/ -run ContestedCommentRendersConfirmations -v`
Expected: FAIL — the string is absent.

- [ ] **Step 3: Implement** — in `contestedComment`, after the "re-arms re-investigation"
paragraph and before the marker:

```go
	if ct.Confirms > 0 {
		fmt.Fprintf(&b, "Since the contest, %d fresh re-investigation%s independently reached this same conclusion (recorded as recovery evidence in the outcome ledger).\n\n",
			ct.Confirms, pluralS(ct.Confirms))
	}
```

with the tiny helper (next to `pluralVote`):

```go
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
```

(Grammar check: `1 fresh re-investigation … reached` / `2 fresh re-investigations …
reached` — the test pins the plural form.)

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/curate/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/curate/contested.go internal/curate/contested_test.go
git commit -m "feat(curate): contested comment surfaces confirmation evidence"
```

---

### Task 5: App wiring + end-to-end recovery test + docs

**Files:**
- Modify: `internal/app/investigate.go:382` (after `cur := BuildCurator(...)`)
- Modify: `docs/learning-loop.md`
- Test: `internal/investigate/recall_decay_integration_test.go` (one new case)

- [ ] **Step 1: Wire the sink.** In `internal/app/investigate.go`, immediately after
`cur := BuildCurator(cfg, BuildForgeTokenSource(cfg, log), cat, metrics, log)` (line 382):

```go
	if cur != nil {
		// Fingerprint-dedup matches become recovery evidence for contested entries
		// (👎 recovery). A disabled ledger (no outcome.ledger_path) no-ops inside
		// Confirm, so this wiring is unconditional.
		cur.Confirmations = ledger
	}
```

(`ledger *outcome.Ledger` is already a parameter of `BuildInvestigator` — verify the
variable name at the call site.)

- [ ] **Step 2: Write the end-to-end recovery test** — append to
`internal/investigate/recall_decay_integration_test.go`, reusing its fixture:

```go
// TestRecallRecoversAfterConfirmations pins the 👎 recovery contract end to end
// over the real Recall gate: one standing 👎 rejects the recall; one machine
// confirmation still rejects (human outranks a single confirm); the second
// confirmation recovers it.
func TestRecallRecoversAfterConfirmations(t *testing.T) {
	dir := t.TempDir()
	entry := `---
type: Incident
title: worker OOMKilled after memory limit drop
description: apps/worker pods OOMKilled; raise the container memory limit
resource: apps/worker
---

# Symptom
apps/worker pods are OOMKilled shortly after a values change lowered the memory limit.
`
	if err := os.WriteFile(filepath.Join(dir, "worker-oom.md"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := cat.Entries()[0].Path
	req := Request{Title: "WorkerOOM", Message: "apps/worker pods OOMKilled after memory limit drop",
		Workload: providers.Workload{Namespace: "apps", Name: "worker"}}
	newRecall := func(agg outcome.Aggregate) *Recall {
		return &Recall{Catalog: cat, MinScore: 0.001, SoloFloor: 0.001, MarginGap: 0.001,
			Outcome:      fakeOutcome{counts: map[string]outcome.Aggregate{path: agg}},
			OutcomePrior: 2.0, OutcomeFloor: 0.5}
	}
	if e, _ := newRecall(outcome.Aggregate{FeedbackDown: 1}).lookup(context.Background(), req); e != nil {
		t.Fatal("one standing 👎 must reject the recall")
	}
	if e, _ := newRecall(outcome.Aggregate{FeedbackDown: 1, Confirms: 1}).lookup(context.Background(), req); e != nil {
		t.Fatal("one machine confirmation must NOT overcome a human 👎")
	}
	if e, _ := newRecall(outcome.Aggregate{FeedbackDown: 1, Confirms: 2}).lookup(context.Background(), req); e == nil {
		t.Fatal("two confirmations must recover the recall (factor back at the floor)")
	}
}
```

- [ ] **Step 3: Document.** In `docs/learning-loop.md`, in the feedback/decay section (near
the standing-👎 / recurrence-cooldown discussion), add:

```markdown
**👎 recovery.** A standing 👎 forces re-investigation — and when that fresh
investigation independently reaches the same conclusion (identical dedup
fingerprint), the curator records a *confirmation* in the outcome ledger instead of
silently deduping it away. Confirmations are recovery evidence at **half** the
weight of a human observation: one 👎 needs two independent confirmations before
the entry's outcome factor climbs back to the floor and recall fires again (a
recall costs two model calls instead of a full investigation — this is what ends
the re-investigation loop). The human override itself is untouched: the recurrence
cooldown stays broken while the 👎 stands, and only the voter changing their vote
clears it. The open KB PR's contested warning shows the confirmation count so the
reviewer sees both signals.
```

- [ ] **Step 4: Full verification**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./... && go test -race ./internal/outcome/ ./internal/curator/ ./internal/curate/ ./internal/investigate/`
Expected: clean, `0 issues`, all green.

- [ ] **Step 5: Commit**

```bash
git add internal/app/investigate.go internal/investigate/recall_decay_integration_test.go docs/learning-loop.md
git commit -m "feat(app): wire confirmation recovery; pin the 👎 recovery contract end to end"
```
