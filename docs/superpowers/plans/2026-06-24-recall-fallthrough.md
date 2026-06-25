# Recall Fall-Through on Empty Finding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **This is a STUB plan** but the design is closed (no open decisions). Safe to implement as written.

**Goal:** When the confirm/verify pass empties a recalled finding's root causes, fall through to the full ReAct loop instead of delivering an empty "recall" result — fixing the P0 where a verify-rejected recall publishes nothing.

**Architecture:** Guard the recall block's `deliver`/`return` (and the tokens-saved metric) on `len(rec.RootCauses) > 0`. On the empty case, log and fall through to the loop below; leave `result` unset so the loop's normal completion records it. `verified`/`downgraded` paths unchanged.

**Tech Stack:** Go 1.26, stdlib `testing`. Tests follow the scripted-`ModelProvider` pattern in `internal/investigate/loop_test.go` + the verify/verdict fakes in the verify tests.

**Spec:** `docs/superpowers/specs/2026-06-24-recall-fallthrough-design.md`

**Branch:** `fix/recall-fallthrough-on-empty`

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/investigate/loop.go` | the recall short-circuit | Guard deliver/return + tokens-saved on non-empty; log + fall through on empty |
| `internal/investigate/loop_test.go` | loop tests | `TestRecallRejectedByVerifyFallsThrough` + the metric assertion |

Order: T1 writes the failing fall-through test; T2 implements; T3 verifies.

---

### Task 1: Test — verify-rejected recall runs the full loop

**Files:** `internal/investigate/loop_test.go`

- [ ] **Step 1: Write the failing test.** `TestRecallRejectedByVerifyFallsThrough`: build a `LoopInvestigator` with `Recall` wired to a strong-hit fake and `Verify: true`, plus a scripted model that (1) answers the verify call by rejecting **all** root causes, then (2) drives the full loop to a `submit_findings` with a real (non-empty) finding. Assert the **delivered** result has `len(RootCauses) > 0` (or populated `Unresolved`) and is **not** the empty recall (`result != "recall"`). Reuse the verdict-rejection fake from the existing verify tests and the loop-script pattern.
- [ ] **Step 2: Run to verify it fails.** `go test ./internal/investigate/ -run TestRecallRejectedByVerifyFallsThrough` → FAIL today (an empty "recall" is delivered and the loop never runs; the scripted loop responses go unused / the assertion on a non-empty finding fails).

---

### Task 2: Implement the fall-through

**Files:** `internal/investigate/loop.go` (recall block)

- [ ] **Step 1:** In the recall block, wrap the `RecallTokensSaved.Add(...)` in `if len(rec.RootCauses) > 0 { ... }` (keep `RecallHits.Add(... result ...)` unconditional).
- [ ] **Step 2:** Replace the unconditional `result = "recall"; li.deliver(req, rec); return nil` with a `if len(rec.RootCauses) > 0 { result = "recall"; li.deliver(req, rec); return nil }` guard, followed by a `li.Log.Info("instant recall rejected by verify; running full investigation", ...)` and **no return** — so execution falls through to the `byName := map[string]Tool{}` loop below. (See spec §3 for the exact block.)
- [ ] **Step 3: Run to verify it passes.** `go test ./internal/investigate/ -run TestRecallRejectedByVerifyFallsThrough` → PASS.
- [ ] **Step 4: Commit.** `git commit -m "fix(investigate): fall through to full loop when verify empties a recall"`

---

### Task 3: Whole-tree verification

- [ ] `go build ./... && go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` → all green, `0 issues`. Run `go test -race ./internal/investigate/` too (goroutine in the recall block).
- [ ] Flip **R2** Status in `docs/roadmap.md` to the PR number.

---

## Notes for the implementer

- Only the **all-rejected** (`len(rec.RootCauses) == 0`) case changes. `verified` and `downgraded`
  (≥1 surviving cause) still deliver + return exactly as today.
- Leave `result` unset on the fall-through — the loop's normal completion path sets it; setting it to
  `"recall"` would mislabel the outcome ledger and metrics.
- `RecallHits{result:"rejected"}` must still fire (we want to see how often the guard catches a bad
  recall); `RecallTokensSaved` must **not** (no tokens were saved).
