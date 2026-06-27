# R18 — Clamp model confidence + fix the token-budget estimate

Date: 2026-06-24
Status: implemented
Scope: `internal/investigate` (two correctness nits)

## Problem

Two independent correctness defects in `internal/investigate`, both verified against current code.

### Nit 1 — model confidence is never clamped to [0,1]

Model-emitted confidence flows into the system unclamped from three call sites:

- `internal/investigate/tools.go:98` — `inv.Confidence = f.Confidence` (overall, copied straight from `submit_findings` JSON)
- `internal/investigate/tools.go:102` — `rc.Confidence = rc.Confidence` (per root-cause, ditto)
- `internal/investigate/verify.go:103,108` — `rc.Confidence = v.Confidence` (verdict confidence from the verify pass)

A model emitting `1.7`, `-0.2`, or `NaN` reaches:

- the **auto-action confidence gate**: `internal/action/auto.go:86` (`inv.Confidence < a.minConfidence`). `NaN < x` is always false, so a `NaN` overall confidence would *pass* the gate; `1.7` trivially passes; `-0.2` would wrongly skip.
- **Slack / chat rendering**: `internal/notify/slack.go:143`, `internal/notify/format.go:15,17`, `internal/curator/draft.go`, `internal/forge/github/github.go:266` — all print `Confidence*100` (e.g. `170%`, `NaN%`).
- **recall-trust / verify math**: `internal/investigate/verify.go:120-126` takes `max` over per-cause confidence; `confirm.go` caps it. `NaN` poisons the `max` comparison.

### Nit 2 — token-budget estimate undercounts

`internal/investigate/budget.go` `estimateTokens` (lines 21-27) sums only `len(system) + Σ len(m.Content)`. The loop (`internal/investigate/loop.go:205`) sends, on **every** step:

- `Tools: specs` — the full tool schemas (name + description + JSON Schema), re-sent each step. Counted: **never**.
- `Messages` carrying assistant `ToolCalls[].Args` (the tool-call JSON the model emitted, `internal/investigate/loop.go:238`). Counted: **never** (only `m.Content` is summed).

So the estimate is systematically low; the hard-kill guard (`internal/investigate/loop.go:186-202`) fires later than intended, or — for a tool-heavy investigation whose bytes live mostly in tool-call args + schemas — effectively never.

## Decision

### `clamp01` helper (shared in the package)

```go
// clamp01 constrains a model-emitted confidence to [0,1]; NaN -> 0 (a NaN
// score must never pass the auto-action gate, where NaN < x is always false).
func clamp01(x float64) float64 {
	switch {
	case math.IsNaN(x):
		return 0
	case x < 0:
		return 0
	case x > 1:
		return 1
	default:
		return x
	}
}
```

Apply in:
- `parseFindings` (tools.go): overall `Confidence` and each root cause `Confidence`.
- `applyVerdicts` (verify.go): wherever `rc.Confidence = v.Confidence` (the keep and downgrade branches). The `rc.Confidence /= 2` downgrade fallback operates on an already-clamped value, so the result stays in range.

`+Inf`/`-Inf` are handled by the `>1`/`<0` arms. The recomputed `inv.Confidence = maxc` in `applyVerdicts` is then a max over clamped values, so it needs no separate clamp.

### `estimateTokens` — add tool-call args + a one-time tool-spec byte count

```go
func estimateTokens(system string, msgs []providers.Message, tools []providers.ToolSpec) int {
	n := len(system)
	for _, t := range tools { // tool schemas are re-sent every step
		n += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls { // assistant tool-call JSON goes over the wire
			n += len(tc.Args)
		}
	}
	return n / 4
}
```

Signature changes to take `tools`; both callers in `loop.go` (the budget check at :186 and the metric at :256) pass `specs`. The `budget_test.go` `TestEstimateTokens` updates to the new signature.

This stays an under-estimate of the true wire size (it ignores JSON envelope/role overhead) but is now within the same order of magnitude as what is actually sent, which is the point: the hard-kill must fire on a runaway, tool-heavy investigation.

`budgetKillResult` / `timeoutResult` are untouched.

## Tests (test-first, stdlib, table-driven)

- `tools_test.go`: `parseFindings` clamps overall + per-cause confidence — `1.7→1`, `-0.2→0`, `NaN→0`, in-range unchanged.
- `verify_test.go`: `applyVerdicts` clamps verdict confidence on keep and downgrade — out-of-range and NaN.
- `budget_test.go`: `estimateTokens` includes tool-call args and a tool-spec count; assert it exceeds the old content-only sum for the same input.

## Out of scope

Recall-derived confidence (`recall.go`) is computed internally from bounded signals, not copied from model JSON — not a clamp target.
