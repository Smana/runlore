# RunLore Loop Token-Budget Hard-Kill — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation |
| **Date** | 2026-06-23 |
| **Scope** | Hard-stop the ReAct investigation loop when the estimated token count exceeds the configured budget after the nudge has already fired. Deliver findings gathered so far (or an explicit unresolved result). **Tool-result elision and repeated-(tool,args) loop detection are explicitly out of scope (follow-up slices).** |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | `internal/investigate/loop.go` (step loop, `budgetNudged` flag); `internal/investigate/budget.go` (`estimateTokens`, `overBudget`, `budgetNudge`); `internal/telemetry/metrics.go` (existing instruments). |

---

## 1. Why this exists

The current loop has a soft budget: when `estimateTokens(sys, messages) > MaxTokensPerInvestigation`, it injects a `budgetNudge` message asking the model to call `submit_findings` now. This fires once, then the loop continues forever (bounded only by `maxSteps`). A runaway model that keeps calling tools will consume unbounded context, potentially OOMing the process.

The fix: once the nudge has fired AND the estimate is still over budget on a subsequent step, **hard-kill** — stop the loop and deliver whatever findings were gathered.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Kill condition: `budgetNudged == true && overBudget(estimate, max)`** | The nudge gives the model one full turn to wind down. The next over-budget check fires after the model has seen the nudge and responded. This cleanly reuses the existing `budgetNudged` flag with zero new state. |
| D2 | **No `budgetHardKillFactor` multiplier** | A separate factor (e.g. 1.5×) adds configuration surface and complexity. The existing flag-based approach is sufficient: nudge fires at 1×, kill fires on the next over-budget check after the nudge. Simple and auditable. |
| D3 | **Guard: `MaxTokensPerInvestigation == 0` → no hard-kill** | `overBudget` already returns false when budget ≤ 0 (unlimited). The hard-kill check reuses the same guard, so the existing "no limit" behavior is unchanged. |
| D4 | **On hard-kill: deliver an unresolved investigation** | Panicking or returning an error would hide findings already gathered. A graceful termination with an explicit `Unresolved` entry is honest and usable. If root causes were already parsed (from a `submit_findings` call that errored), deliver them; otherwise deliver a synthetic unresolved result. |
| D5 | **Telemetry: log a warning; reuse `InvestigationsDropped`** | Adding a new counter (`investigations_budget_killed_total`) is not trivially justified for one slice. `InvestigationsDropped` is semantically close (an investigation that did not complete normally). Log a structured warning with estimate + budget for immediate observability. |

## 3. Design

### 3.1 Hard-kill check in the step loop (`internal/investigate/loop.go`)

The existing nudge injection sits at the top of the step loop:

```go
if !budgetNudged && overBudget(estimateTokens(sys, messages), li.MaxTokensPerInvestigation) {
    messages = append(messages, providers.Message{Role: "user", Content: budgetNudge})
    budgetNudged = true
}
```

Add a second, symmetric check immediately after:

```go
if budgetNudged && overBudget(estimateTokens(sys, messages), li.MaxTokensPerInvestigation) {
    est := estimateTokens(sys, messages)
    li.Log.Warn("investigation hard-stopped at token budget",
        "title", req.Title,
        "estimate_tokens", est,
        "budget_tokens", li.MaxTokensPerInvestigation)
    if li.Metrics != nil {
        li.Metrics.InvestigationsDropped.Add(ctx, 1)
    }
    li.deliver(req, budgetKillResult(req))
    return nil
}
```

`overBudget` is idempotent on the same message slice, so calling `estimateTokens` twice is fine (the estimate did not change between the two checks in the same iteration). Alternatively, call once and reuse:

```go
if est := estimateTokens(sys, messages); overBudget(est, li.MaxTokensPerInvestigation) {
    if !budgetNudged {
        messages = append(messages, providers.Message{Role: "user", Content: budgetNudge})
        budgetNudged = true
    } else {
        // Hard-kill: nudge already fired, model did not wind down.
        li.Log.Warn("investigation hard-stopped at token budget",
            "title", req.Title, "estimate_tokens", est,
            "budget_tokens", li.MaxTokensPerInvestigation)
        if li.Metrics != nil {
            li.Metrics.InvestigationsDropped.Add(ctx, 1)
        }
        li.deliver(req, budgetKillResult(req))
        return nil
    }
}
```

This single-estimate form is cleaner: one `estimateTokens` call, one `overBudget` check, two branches.

### 3.2 `budgetKillResult` helper (`internal/investigate/loop.go` or `budget.go`)

```go
// budgetKillResult synthesises an unresolved investigation for use when the
// token-budget hard-kill fires (nudge fired, model did not wind down).
func budgetKillResult(req Request) providers.Investigation {
    return providers.Investigation{
        Title:      req.Title,
        Resource:   req.Workload,
        Fingerprint: req.Fingerprint,
        Unresolved: []string{
            "investigation stopped: token budget exceeded after nudge (model did not submit findings in time)",
        },
    }
}
```

### 3.3 No changes to `budget.go` or `telemetry/metrics.go`

`overBudget`/`estimateTokens`/`budgetNudge` need no changes. The `InvestigationsDropped` counter is already defined in `telemetry.Metrics`.

## 4. Invariants

- `MaxTokensPerInvestigation == 0` → `overBudget` returns false → hard-kill never fires. Existing behavior preserved.
- Nudge fires at most once (`budgetNudged` flag), hard-kill fires at most once (function returns). The model always gets at least one turn after the nudge before the kill.
- Hard-kill delivers a result via the normal `deliver` path — `OnComplete` is called.

## 5. Tests

In `internal/investigate/loop_test.go`, add:

- **`TestLoopHardKillOnBudgetExhaustion`**: fake model returns an infinite sequence of tool calls (never `submit_findings`); `MaxTokensPerInvestigation` is set small enough to trigger at step 2 or 3. Assert: (a) loop terminates, (b) `OnComplete` called exactly once, (c) result has an `Unresolved` entry containing "budget", (d) model called ≤ N times (bounded by kill, not `maxSteps`).
- **`TestLoopHardKillDisabledWhenNoBudget`**: same runaway model, `MaxTokensPerInvestigation == 0`. Assert loop runs to `maxSteps` without hard-kill firing (model called exactly `maxSteps` times).

## 6. Out of scope (follow-up slices)

- **Tool-result elision**: truncating or dropping old tool messages from the history before re-sending.
- **Repeated-(tool,args) loop detection**: detecting the model calling the same tool with the same args in a cycle.
- **Provider-reported usage**: adding a `Usage` field to `CompletionResponse` and using real token counts rather than the estimate. The `estimateTokens` proxy is sufficient for this slice.
- **`InvestigationsDropped` vs a dedicated counter**: if the budget-kill metric needs its own instrument for alerting fidelity, add it in a follow-up alongside the tool-result elision metric.
