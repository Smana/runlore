# Mid-loop tool-output compaction (T3)

Status: draft for review
Date: 2026-06-30
Scope: one PR, dedicated worktree (`feat/model-history-compaction`). Context-window management under budget pressure, with an explicit quality gate.

## Problem & framing

The investigation ReAct loop (`internal/investigate/loop.go`) resends the entire growing message history every step. Each tool output is already byte-truncated to `max_tool_output_bytes` (32KB) on entry, but ~19 of them still accumulate, and when the estimate crosses `max_tokens_per_investigation` the loop hard-kills (`result = "budget_exceeded"`, delivers an unresolved finding — *no root cause*).

**T1 (PR #182) already cached the growing history on Anthropic and OpenAI**, so T3's value is no longer primarily cost. It is:
1. **Context-window headroom** — compact superseded outputs so an investigation can finish instead of hard-killing.
2. **Gemini cost** — implicit caching is opportunistic (min-token thresholds).
3. **Less context bloat** — which can degrade answer quality on long runs.

**The right quality baseline is the hard-kill, not full history.** Compaction only fires under budget pressure, where today the loop dies with no findings. So where it fires, a complete investigation on compacted history is a quality *improvement*; where the threshold is never reached, T3 is inert.

**Residual quality risk (honest):** eliding old tool outputs means the model can't re-quote their raw bytes later, and naive recency-elision could drop a decisive early output. The design below minimizes this (supersession-aware + keep-list + recent-K window + kept assistant reasoning), and a **live-eval gate** proves no regression before merge.

## Design

A new `internal/investigate/compact.go` operating on the `[]providers.Message` history — provider-agnostic, before the history reaches any model client, so it composes with T1 regardless of merge order.

### Trigger

At the top of each loop step, after computing the existing `est := estimateTokens(sys, messages, specs)`: if `MaxTokensPerInvestigation > 0` and `est > compactionBudgetFraction × MaxTokensPerInvestigation` (`compactionBudgetFraction = 0.7`, package constant), run compaction, then recompute `est`. The existing nudge → hard-kill backstop then runs unchanged on the (possibly compacted) `est`. A no-op compaction falls straight through to that backstop — no infinite loop. Disabled when no budget is configured (no reference point).

### What is protected (never elided)

- The **seed** (first user message).
- **All assistant turns** (reasoning + tool-call JSON) — small, and they carry the model's *interpretation* of evidence, so conclusions survive even when raw bytes don't.
- Tool results from **keep-list tools: `what_changed` and `kb_search`** — the root-cause spine (the change timeline + the runbook hit). The keep-list is a package constant.
- The **most-recent `keepRecentToolOutputs = 3`** tool results (the model's active working set).

### Elision policy (hardened: supersession-aware, bounded)

Eligible = tool-result messages that are NOT protected above. Elide eligible outputs **only until `est` drops back below the trigger threshold** (never more than necessary), in this priority order:

1. **Superseded first** — an eligible output whose `(tool name, args)` is re-issued by a *later* assistant turn (a literal re-query returning refreshed data → the older one is genuinely stale).
2. **Then largest-first** — remaining eligible outputs by byte size descending (max token reduction per elision = fewest elisions = least information lost).

Each elided tool-result body is replaced with `[earlier <tool> output elided to bound context]`, where `<tool>` is resolved from the matching assistant `ToolCall` by `ToolCallID` (id→name map, as `gemini.toContents` already does). Idempotent: an already-elided marker is tiny, so it is never re-selected (it isn't large, and a marker isn't "superseded").

### Constants (no new config knobs — matches the "minimal config" decision)

```
compactionBudgetFraction = 0.7   // fraction of MaxTokensPerInvestigation that triggers compaction
keepRecentToolOutputs    = 3     // most-recent tool results kept verbatim
keepListTools            = {"what_changed", "kb_search"}  // never elided
```

Easy to promote to config later; kept internal now.

### T1 interaction

Compaction rewrites tool messages *below* T1's rolling breakpoint (which is on the last message — always a kept/recent one). So the Anthropic cached prefix is invalidated **once**, on the compaction step, then re-caches on the new compacted prefix. This one-time miss is the accepted cost. No code coordination needed; T3 branches from `main` independently of #182.

### Observability

Two counters in `internal/telemetry/metrics.go` (existing `ctr(...)` pattern): `HistoryCompactions` (compaction events) and `HistoryElidedBytes` (total bytes elided). A one-time `Info` log per investigation when compaction first fires (title + before/after estimate). Recorded in the loop.

## Quality gate (the "measure" half — a documented merge criterion)

Compaction's quality impact is measured with the existing live-eval harness, NOT taken on faith. **This gate runs on a real cluster with API keys + a judge model; it is a maintainer/CI step — it cannot run in the dev sandbox.** Procedure, wired into the PR's merge criteria:

1. On `main`: `lore eval --live --scenarios eval/scenarios-k3d` → baseline report (or reuse the committed `eval/reports/2026-06-22-k3d-baseline.md`).
2. On this branch: same command → branch report.
3. Compare with `LiveReport.RegressionsVS(baseline)` (already implemented, `internal/eval/report.go:102`). **Merge only if it returns zero regressions** (no scenario that passed on main fails/skips now), and RCA/coverage scores are not materially lower.

To make compaction actually exercise in the eval, run with a tightened budget (a scenario/config `max_tokens_per_investigation` low enough that long scenarios cross the 0.7 threshold) so the gate tests the compaction path, not just the inert path.

The PR description will state explicitly: **mechanism implemented + unit/integration-tested; quality gate (live eval, zero regressions) pending a maintainer run.**

## Files touched

- `internal/investigate/compact.go` (new) — `compactHistory(messages []providers.Message, maxEstTokens int, sys string, specs []providers.ToolSpec) (compacted []providers.Message, events CompactionStats)` (or similar signature returning bytes elided + whether it fired).
- `internal/investigate/compact_test.go` (new) — unit tests.
- `internal/investigate/loop.go` — call compaction at the top of the step loop before the budget guard; record metrics + the one-time log.
- `internal/investigate/loop_test.go` — integration test (reaches `submit_findings` instead of `budget_exceeded`).
- `internal/telemetry/metrics.go` — 2 counters.
- Possibly `eval/` docs or `eval/rubric.md` — note the compaction gate procedure (lightweight).

## Testing

**Unit (`compact_test.go`), all deterministic, no model:**
- Elides eligible old tool bodies beyond the recent-K window; keeps the most-recent K verbatim.
- Never elides: seed, assistant turns, `what_changed`/`kb_search` results.
- Superseded-first ordering: an output whose (tool,args) is re-issued later is elided before a larger non-superseded one.
- Largest-first among non-superseded.
- Stops once under threshold (does not over-elide when a partial elision suffices).
- Marker names the correct tool (id→name resolution).
- Idempotent (second pass on already-compacted history is a no-op / no further elision).
- No-op when nothing eligible (all within keep-list / recent-K) — returns history unchanged.

**Integration (`loop_test.go`):** a scripted model emitting many large tool outputs under a low `MaxTokensPerInvestigation`; assert the loop reaches `submit_findings` (`result = "resolved"`) where without compaction it would hit `budget_exceeded`. (Add a sibling assertion that with compaction disabled — budget so low even compacted history overflows — the hard-kill still fires, proving the backstop is intact.)

**Quality (live, maintainer/CI):** the eval-gate procedure above.

All existing loop/telemetry tests stay green; `go build ./... && go vet && gofmt -l`.

## Non-goals

- LLM-summarization compaction (rejected: extra call/latency, partly self-defeating).
- Headline-extraction hybrid (rejected: heuristic line-picking).
- Making the constants configurable (deferred; internal constants now).
- Re-fetching elided outputs on demand (the model re-calls the tool if it truly needs fresh data).
- Touching the model clients (compaction is entirely in `internal/investigate`).

## Open risk acknowledged

If the live eval shows a regression on a scenario where a decisive early output got elided, the mitigation order is: (1) raise `keepRecentToolOutputs`, (2) extend the keep-list (e.g. add `gitops_tree`/`gitops_resource_status`), (3) lower `compactionBudgetFraction` so compaction fires less often. The gate exists precisely to surface this before merge.
