# Mid-loop Tool-Output Compaction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Under budget pressure, elide superseded/old tool-output bodies from the loop's message history so a long investigation can finish instead of hitting the hard-kill — protecting the root-cause skeleton.

**Architecture:** A pure, provider-agnostic `compactHistory` in `internal/investigate` that rewrites the `[]providers.Message` history; called at the top of each loop step before the existing budget guard; two new metrics. No model-client changes.

**Tech Stack:** Go 1.26, standard library + OpenTelemetry (existing). No new deps.

## Global Constraints

- Go 1.26.0; standard library + existing deps only.
- No co-authored commits; no AI attribution.
- **Protected from elision (never elided):** the seed (first message), every assistant turn, keep-list tool results (`what_changed`, `kb_search`, `gitops_resource_status`, `gitops_tree`), and the most-recent `keepRecentToolOutputs = 3` tool results.
- **Elision order:** superseded first (a `(tool name, args)` re-issued by a later assistant turn), then largest-byte-first; stop once the estimate is `<= target` (= `0.7 × MaxTokensPerInvestigation`). Never elide more than necessary.
- Compaction is disabled when `MaxTokensPerInvestigation <= 0` (no reference point). Idempotent (an elided marker is never re-selected).
- The existing nudge → hard-kill backstop is unchanged; it runs after compaction on the recomputed estimate.
- Spec: `dev/superpowers/specs/2026-06-30-mid-loop-history-compaction-design.md`.

---

### Task 1: `compactHistory` + unit tests

**Files:**
- Create: `internal/investigate/compact.go`
- Test: `internal/investigate/compact_test.go`

**Interfaces:**
- Produces (consumed by Task 3): `compactHistory(messages []providers.Message, sys string, specs []providers.ToolSpec, target int) ([]providers.Message, int)`; `compactionTarget(budget int) int`; constants `compactionBudgetFraction`, `keepRecentToolOutputs`; `keepListTools`.
- Consumes: existing `estimateTokens(system string, msgs []providers.Message, tools []providers.ToolSpec) int` (`budget.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/investigate/compact_test.go`:

```go
package investigate

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// toolMsg builds an assistant tool-call turn followed by its tool-result message.
func callAndResult(id, name, args, result string) []providers.Message {
	return []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: id, Name: name, Args: args}}},
		{Role: "tool", ToolCallID: id, Content: result},
	}
}

func buildHistory(seedSize int, calls ...[]providers.Message) []providers.Message {
	msgs := []providers.Message{{Role: "user", Content: strings.Repeat("s", seedSize)}}
	for _, c := range calls {
		msgs = append(msgs, c...)
	}
	return msgs
}

func toolResultByID(msgs []providers.Message, id string) string {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

func TestCompactElidesOldBeyondRecentK(t *testing.T) {
	big := strings.Repeat("x", 4000)
	// 5 pod_logs calls; with K=3, the oldest 2 (ids 1,2) are eligible.
	msgs := buildHistory(10,
		callAndResult("1", "pod_logs", `{"ns":"a"}`, big),
		callAndResult("2", "pod_logs", `{"ns":"b"}`, big),
		callAndResult("3", "pod_logs", `{"ns":"c"}`, big),
		callAndResult("4", "pod_logs", `{"ns":"d"}`, big),
		callAndResult("5", "pod_logs", `{"ns":"e"}`, big),
	)
	target := estimateTokens("", msgs, nil) - 1500 // force some elision
	out, elided := compactHistory(msgs, "", nil, target)
	if elided == 0 {
		t.Fatal("expected some bytes elided")
	}
	// recent 3 (ids 3,4,5) kept verbatim
	for _, id := range []string{"3", "4", "5"} {
		if toolResultByID(out, id) != big {
			t.Fatalf("recent tool result %s must be kept verbatim", id)
		}
	}
	// oldest (id 1) elided to a marker
	if !isElidedMarker(toolResultByID(out, "1")) {
		t.Fatalf("oldest tool result should be elided, got %q", toolResultByID(out, "1"))
	}
}

func TestCompactProtectsSeedAssistantAndKeepList(t *testing.T) {
	big := strings.Repeat("y", 5000)
	msgs := buildHistory(20,
		callAndResult("1", "what_changed", `{}`, big),         // keep-listed
		callAndResult("2", "kb_search", `{}`, big),            // keep-listed
		callAndResult("3", "gitops_tree", `{}`, big),          // keep-listed
		callAndResult("4", "gitops_resource_status", `{}`, big), // keep-listed
		callAndResult("5", "pod_logs", `{}`, big),
		callAndResult("6", "pod_logs", `{}`, big),
		callAndResult("7", "pod_logs", `{}`, big),
		callAndResult("8", "pod_logs", `{}`, big),
	)
	target := 1 // force maximum elision
	out, _ := compactHistory(msgs, "", nil, target)
	// keep-list tools never elided
	for _, id := range []string{"1", "2", "3", "4"} {
		if toolResultByID(out, id) != big {
			t.Fatalf("keep-list tool %s must never be elided", id)
		}
	}
	// seed untouched
	if out[0].Content != strings.Repeat("y", 20) {
		t.Fatal("seed must never be elided")
	}
	// assistant turns untouched (still carry their tool calls)
	for _, m := range out {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			t.Fatal("assistant tool-call turn must be preserved")
		}
	}
}

func TestCompactSupersededFirst(t *testing.T) {
	big := strings.Repeat("z", 3000)
	small := strings.Repeat("z", 3200)
	// id 1 (pod_logs ns=a) is superseded by id 4 (same name+args, later). id 2 is a
	// larger, non-superseded one-off. Supersession must elide id 1 before id 2 even
	// though id 2 is larger. Use K=3 so ids 1 and 2 are eligible (ids 3,4 are recent).
	msgs := buildHistory(10,
		callAndResult("1", "pod_logs", `{"ns":"a"}`, big),  // superseded by id 4
		callAndResult("2", "controller_logs", `{"c":"x"}`, small), // larger, one-off
		callAndResult("3", "pod_logs", `{"ns":"b"}`, big),
		callAndResult("4", "pod_logs", `{"ns":"a"}`, big),  // re-query of id 1
	)
	// target that allows exactly one elision to drop under it
	target := estimateTokens("", msgs, nil) - 600
	out, _ := compactHistory(msgs, "", nil, target)
	if !isElidedMarker(toolResultByID(out, "1")) {
		t.Fatal("superseded id 1 should be elided first")
	}
	if isElidedMarker(toolResultByID(out, "2")) {
		t.Fatal("non-superseded id 2 should NOT be elided when one elision sufficed")
	}
}

func TestCompactNoopWhenUnderTarget(t *testing.T) {
	msgs := buildHistory(10, callAndResult("1", "pod_logs", `{}`, "small"))
	target := estimateTokens("", msgs, nil) + 1000 // already under
	out, elided := compactHistory(msgs, "", nil, target)
	if elided != 0 {
		t.Fatalf("expected no-op, elided %d", elided)
	}
	if &out[0] == nil {
		t.Fatal("should return the history")
	}
}

func TestCompactDisabledTargetZero(t *testing.T) {
	msgs := buildHistory(10, callAndResult("1", "pod_logs", `{}`, strings.Repeat("x", 9000)))
	_, elided := compactHistory(msgs, "", nil, 0)
	if elided != 0 {
		t.Fatal("target<=0 must be a no-op")
	}
}

func TestCompactIdempotent(t *testing.T) {
	big := strings.Repeat("x", 5000)
	msgs := buildHistory(10,
		callAndResult("1", "pod_logs", `{}`, big),
		callAndResult("2", "pod_logs", `{}`, big),
		callAndResult("3", "pod_logs", `{}`, big),
		callAndResult("4", "pod_logs", `{}`, big),
	)
	target := estimateTokens("", msgs, nil) - 1000
	once, _ := compactHistory(msgs, "", nil, target)
	twice, elided2 := compactHistory(once, "", nil, target)
	if elided2 != 0 {
		t.Fatalf("second compaction pass should be a no-op, elided %d", elided2)
	}
	_ = twice
}

func TestCompactionTarget(t *testing.T) {
	if compactionTarget(0) != 0 || compactionTarget(-5) != 0 {
		t.Fatal("budget<=0 disables compaction")
	}
	if got := compactionTarget(100000); got != 70000 {
		t.Fatalf("compactionTarget(100000)=%d, want 70000", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/investigate/ -run 'TestCompact' -v`
Expected: compile error (compact.go doesn't exist), then FAIL.

- [ ] **Step 3: Implement `compact.go`**

Create `internal/investigate/compact.go`:

```go
package investigate

import (
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

const (
	// compactionBudgetFraction is the fraction of MaxTokensPerInvestigation at which
	// compaction triggers and the size it elides back down to (headroom for more steps).
	compactionBudgetFraction = 0.7
	// keepRecentToolOutputs is the number of most-recent tool results kept verbatim
	// (the model's active working set).
	keepRecentToolOutputs = 3
)

// keepListTools are tools whose outputs are the structural root-cause skeleton and are
// never elided: the change timeline, the runbook hit, the failing resource's status, and
// the dependency-cascade root. The gitops_* pair are engine-agnostic (Flux + ArgoCD).
var keepListTools = map[string]bool{
	"what_changed":           true,
	"kb_search":              true,
	"gitops_resource_status": true,
	"gitops_tree":            true,
}

const (
	elidedPrefix = "[earlier "
	elidedSuffix = " output elided to bound context]"
)

func elidedMarker(tool string) string {
	if tool == "" {
		tool = "tool"
	}
	return elidedPrefix + tool + elidedSuffix
}

func isElidedMarker(s string) bool {
	return strings.HasPrefix(s, elidedPrefix) && strings.HasSuffix(s, elidedSuffix)
}

// compactionTarget returns the estimate at/below which compaction stops: 0.7 * budget.
// budget <= 0 disables compaction (returns 0).
func compactionTarget(budget int) int {
	if budget <= 0 {
		return 0
	}
	return int(float64(budget) * compactionBudgetFraction)
}

// compactHistory elides the bodies of eligible tool-result messages — superseded ones
// first, then largest-first — until estimateTokens(sys, messages, specs) drops to or
// below target, or no eligible output remains. Protected: the seed (index 0), every
// assistant turn, keep-list tool results, and the most-recent keepRecentToolOutputs tool
// results. Returns a new slice (the caller's messages are never mutated) and the number
// of body bytes elided (0 when nothing was compacted). target <= 0 is a no-op.
func compactHistory(messages []providers.Message, sys string, specs []providers.ToolSpec, target int) ([]providers.Message, int) {
	if target <= 0 || estimateTokens(sys, messages, specs) <= target {
		return messages, 0
	}
	// Resolve each tool-call id -> (name, args) so a tool RESULT (which carries only
	// ToolCallID) is attributable to its tool and dedupable by (name, args).
	type call struct{ name, args string }
	byID := map[string]call{}
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			byID[tc.ID] = call{tc.Name, tc.Args}
		}
	}
	// Positions of tool-result messages, in order.
	var toolIdx []int
	for i, m := range messages {
		if m.Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	recentCut := len(toolIdx) - keepRecentToolOutputs // list-positions >= this are recency-protected
	// Last list-position per (name, args) — earlier ones are superseded.
	lastPosFor := map[call]int{}
	for pos, mi := range toolIdx {
		lastPosFor[byID[messages[mi].ToolCallID]] = pos
	}
	type cand struct {
		mi         int
		size       int
		superseded bool
	}
	var cands []cand
	for pos, mi := range toolIdx {
		if pos >= recentCut {
			continue // most-recent K
		}
		c := byID[messages[mi].ToolCallID]
		if keepListTools[c.name] || isElidedMarker(messages[mi].Content) {
			continue
		}
		cands = append(cands, cand{mi: mi, size: len(messages[mi].Content), superseded: lastPosFor[c] != pos})
	}
	// Superseded first, then largest-first.
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].superseded != cands[b].superseded {
			return cands[a].superseded
		}
		return cands[a].size > cands[b].size
	})
	// Copy so the caller's slice contents are never mutated.
	out := make([]providers.Message, len(messages))
	copy(out, messages)
	elided := 0
	for _, cd := range cands {
		if estimateTokens(sys, out, specs) <= target {
			break
		}
		name := byID[out[cd.mi].ToolCallID].name
		before := len(out[cd.mi].Content)
		out[cd.mi].Content = elidedMarker(name)
		elided += before - len(out[cd.mi].Content)
	}
	if elided == 0 {
		return messages, 0
	}
	return out, elided
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/investigate/ -run 'TestCompact' -v`
Expected: PASS — all compaction unit tests green.

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/compact.go internal/investigate/compact_test.go
git commit -m "feat(investigate): compactHistory — elide superseded/old tool outputs

Provider-agnostic history compaction: under budget pressure, elide the bodies of
eligible tool-result messages (superseded first, then largest-first) until the estimate
drops below target, protecting the seed, assistant turns, the keep-list root-cause
skeleton (what_changed/kb_search/gitops_resource_status/gitops_tree), and the most-recent
K outputs. Pure and idempotent; not yet wired into the loop."
```

---

### Task 2: Telemetry counters

**Files:**
- Modify: `internal/telemetry/metrics.go`
- Test: `internal/telemetry/metrics_test.go`

**Interfaces:**
- Produces (consumed by Task 3): `Metrics.HistoryCompactions`, `Metrics.HistoryElidedBytes` (both `metric.Int64Counter`).

- [ ] **Step 1: Write the failing test**

Add to `internal/telemetry/metrics_test.go` (match the file's existing assertion style):

```go
func TestHistoryCompactionCountersConstructed(t *testing.T) {
	m := NewMetrics()
	if m.HistoryCompactions == nil || m.HistoryElidedBytes == nil {
		t.Fatal("NewMetrics must construct HistoryCompactions and HistoryElidedBytes")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/telemetry/ -run TestHistoryCompactionCountersConstructed -v`
Expected: compile error — fields don't exist.

- [ ] **Step 3: Add the fields + construction**

In `internal/telemetry/metrics.go`, add to the `Metrics` struct near `ToolOutputTruncatedBytes`:

```go
	HistoryCompactions metric.Int64Counter // mid-loop history compaction events
	HistoryElidedBytes metric.Int64Counter // tool-output bytes elided by compaction
```

And in `NewMetrics`'s returned literal, near `ToolOutputTruncatedBytes`:

```go
		HistoryCompactions: ctr("history_compactions_total", "mid-loop tool-output history compaction events"),
		HistoryElidedBytes: ctr("history_elided_bytes_total", "tool-output bytes elided by mid-loop compaction"),
```

- [ ] **Step 4: Run test to verify pass**

Run: `go test ./internal/telemetry/ -run TestHistoryCompactionCountersConstructed -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/telemetry/metrics.go internal/telemetry/metrics_test.go
git commit -m "feat(telemetry): history compaction counters"
```

---

### Task 3: Wire compaction into the loop + integration test

**Files:**
- Modify: `internal/investigate/loop.go`
- Test: `internal/investigate/loop_test.go`

**Interfaces:**
- Consumes: `compactHistory`, `compactionTarget` (Task 1); `Metrics.HistoryCompactions`, `Metrics.HistoryElidedBytes` (Task 2).

- [ ] **Step 1: Write the failing integration test**

Add to `internal/investigate/loop_test.go` (uses the existing `scriptModel` + `bigTool`):

```go
// TestCompactionLetsInvestigationFinish drives a model that calls a big-output tool many
// times under a low token budget; with compaction the loop reaches submit_findings
// instead of the budget hard-kill.
func TestCompactionLetsInvestigationFinish(t *testing.T) {
	var resp []providers.CompletionResponse
	for i := 1; i <= 6; i++ {
		resp = append(resp, providers.CompletionResponse{
			ToolCalls: []providers.ToolCall{{ID: fmtID(i), Name: "big_tool", Args: `{}`}},
		})
	}
	resp = append(resp, providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "f", Name: submitFindingsName, Args: `{"confidence":0.7,"root_causes":[{"summary":"found it"}]}`},
	}})

	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:                     &scriptModel{responses: resp},
		Tools:                     []Tool{bigTool{size: 4000}},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		MaxTokensPerInvestigation: 6000, // low: without compaction, 6 x ~1000-token outputs hard-kill
		OnComplete:                func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "compaction finish test"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) == 0 {
		t.Fatalf("expected a resolved investigation via compaction, got %+v", got)
	}
}

func fmtID(i int) string { return string(rune('0' + i)) }
```

(If `bigTool` is not in scope or `MaxToolOutputBytes` is needed to shape sizes, mirror the existing big-output test setup nearby; keep `MaxToolOutputBytes` unset so the 4000-byte outputs are not pre-truncated.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestCompactionLetsInvestigationFinish -v`
Expected: FAIL — without compaction wired in, the loop hits `budget_exceeded` and delivers a budget-kill result (no root causes), so `got.RootCauses` is empty.

- [ ] **Step 3: Add the compaction-logged flag**

In `internal/investigate/loop.go`, alongside the existing `nudged`/`budgetNudged`/`truncationNudged` flags (around the loop setup), add:

```go
	compactionLogged := false // set when the one-time compaction log has fired
```

- [ ] **Step 4: Wire compaction before the budget guard**

In `internal/investigate/loop.go`, replace the existing budget-guard block at the top of the `for step := ...` loop:

```go
		if est := estimateTokens(sys, messages, specs); overBudget(est, li.MaxTokensPerInvestigation) {
```

with compaction first, then the guard on the recomputed estimate:

```go
		est := estimateTokens(sys, messages, specs)
		// Mid-loop compaction: before the budget guard, elide superseded/old tool outputs
		// to stay under budget so a long investigation can finish instead of hard-killing.
		if target := compactionTarget(li.MaxTokensPerInvestigation); target > 0 && est > target {
			if compacted, elided := compactHistory(messages, sys, specs, target); elided > 0 {
				messages = compacted
				est = estimateTokens(sys, messages, specs)
				if !compactionLogged {
					li.Log.Info("compacted investigation history to bound context",
						"title", req.Title, "elided_bytes", elided, "estimate_tokens", est)
					compactionLogged = true
				}
				if li.Metrics != nil {
					li.Metrics.HistoryCompactions.Add(ctx, 1)
					li.Metrics.HistoryElidedBytes.Add(ctx, int64(elided))
				}
			}
		}
		if overBudget(est, li.MaxTokensPerInvestigation) {
```

Leave the body of the `overBudget` block (nudge / hard-kill) exactly as-is — it now reads the pre-computed/recomputed `est`.

- [ ] **Step 5: Run tests to verify pass + no regressions**

Run: `go test ./internal/investigate/ -v 2>&1 | tail -30`
Expected: PASS — `TestCompactionLetsInvestigationFinish` passes; the existing budget tests (nudge, hard-kill) still pass (a budget so low that even compacted history overflows still hard-kills, since compaction returns the irreducible protected set and `overBudget` then fires).

- [ ] **Step 6: Commit**

```bash
git add internal/investigate/loop.go internal/investigate/loop_test.go
git commit -m "feat(investigate): wire mid-loop compaction before the budget guard

Elide superseded/old tool outputs once the estimate crosses 0.7x the token budget, so a
long investigation can finish instead of hard-killing; the nudge -> hard-kill backstop is
unchanged and still fires when even the compacted, protected history overflows. Records
history_compactions_total / history_elided_bytes_total and logs once per investigation."
```

---

### Task 4: Full verification

**Files:** none.

- [ ] **Step 1: Build, full suite, vet, fmt**

Run: `go build ./... && go test ./... && go vet ./... && gofmt -l internal/investigate internal/telemetry`
Expected: all green; `gofmt -l` prints nothing. If anything fails, stop and report BLOCKED with the exact output.

---

## Self-Review

**Spec coverage:**
- Trigger at 0.7 × budget, disabled when budget ≤ 0 → `compactionTarget` + Task 3 Step 4. ✓
- Protected set (seed, assistant, keep-list incl. gitops_*, recent-K) → `compactHistory` + Tasks 1 tests. ✓
- Elision order superseded-then-largest, stop at target → `compactHistory` sort + loop. ✓
- Idempotent, no-op when nothing eligible / under target → tests. ✓
- Backstop unchanged, runs after on recomputed est → Task 3 Step 4. ✓
- Metrics + one-time log → Task 2 + Task 3. ✓
- Provider-agnostic, no model-client changes → all in `internal/investigate` + telemetry. ✓
- Eval gate is a deferred maintainer/EKS step (out of code scope) → noted in spec, not a task. ✓

**Placeholder scan:** No TBD/TODO; complete code in every step; commands have expected output. The one conditional ("if `bigTool` not in scope…") in Task 3 Step 1 is a known-harness note, not a placeholder — `bigTool` exists in loop_test.go (used by the existing big-output metric test).

**Type consistency:** `compactHistory`/`compactionTarget` signatures match between Task 1 definition and Task 3 use; `HistoryCompactions`/`HistoryElidedBytes` defined (Task 2) and used (Task 3) with matching names; `estimateTokens` signature matches `budget.go`.
