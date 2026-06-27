# RunLore Recall Fall-Through on Empty Finding — Design

| | |
|---|---|
| **Status** | **Implemented** (2026-06-24) — this **reverses a prior, deliberately-tested behavior** (withdraw-to-empty); the fall-through approach was a product decision, not a bug fix (see D4) |
| **Date** | 2026-06-24 |
| **Scope** | When confirm/verify empties a recalled finding's root causes, **fall through to the full ReAct loop** instead of delivering an empty "recall" result. Touches the recall block in `internal/investigate/loop.go`, `internal/investigate/loop_test.go`, the poisoned-entry test `internal/eval/live_test.go`, and a line in `docs/learning-loop.md`. |
| **Author** | Smana (drafted with Claude) |
| **Related** | [`docs/roadmap.md`](../../roadmap.md) **R2** (P0); builds on recall-confirmatory (`2026-06-23-recall-confirmatory-design.md`) and the verify pass (`2026-06-23-loop-hardkill-design.md` neighbourhood) |

---

## 1. Why this exists

The recall short-circuit is meant to be *guarded* by the adversarial verify pass: "catalog content is
untrusted, so verify a recalled finding too, and a crafted high-recall entry can't bypass review." But
when verify (or the confirm step) **rejects every root cause**, the code at
`internal/investigate/loop.go` (the recall block ending ~`:155-157`) unconditionally does:

```go
result = "recall"
li.deliver(req, rec)   // rec.RootCauses is now empty
return nil
```

So exactly when the guard fires on a stale/poisoned/wrong recall, RunLore **publishes nothing** — zero
root causes, zero confidence, recorded as a `"recall"` outcome — and never runs the real investigation
it should have fallen back to. The safety story ("verify guards recall") is inverted into a silent
blackhole. The metric already computes `recallResult = "rejected"` in this branch, proving the rejection
is detected; only the control flow is wrong.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | On `len(rec.RootCauses) == 0` after confirm/verify, **do not deliver and do not return** — fall through to the full ReAct loop (treat the recall as a miss). | Restores the intended fail-safe: a rejected recall costs a full investigation, never a blank answer. |
| D2 | Still record `RecallHits{result:"rejected"}` (the recall *was* attempted and rejected), but **skip `RecallTokensSaved`** in that case. | No tokens were saved — we run the full loop. Keeps the savings metric honest. |
| D3 | Leave the `len > 0` paths (`verified` / `downgraded`) exactly as today: deliver + return. | Only the empty case changes. |
| D4 | **Reverses a deliberate prior behavior.** `internal/eval/live_test.go` `TestRunOnceRecallPoisonedRejected` previously asserted "a rejected recall is still a recall (loop skipped): Recalled must stay true" — withdraw-to-empty + surface the rejected hypothesis in `Unresolved`. The user chose **fall-through** (2026-06-24): a rejected recall on a real, ongoing incident should yield a fresh root-cause attempt, not an empty result. That test now asserts the agent re-investigates and never delivers the poisoned cause. | The prior behavior was coherent (honest-uncertainty + outcome-decay self-heal), so this is a product trade-off, not a defect. Recorded here so the reversal is intentional and traceable. |

## 3. Design

Restructure the tail of the recall block so deliver/return are guarded on a non-empty finding:

```go
if m := li.Recall.Metrics; m != nil {
	recallResult := "verified"
	switch {
	case len(rec.RootCauses) == 0:
		recallResult = "rejected"
	case li.Verify && rec.Confidence < initialConfidence:
		recallResult = "downgraded"
	}
	m.RecallHits.Add(ctx, 1, metric.WithAttributes(attribute.String("result", recallResult)))
	if len(rec.RootCauses) > 0 { // tokens are only "saved" when we actually short-circuit
		saved := int64(li.MaxTokensPerInvestigation)
		if saved == 0 {
			saved = defaultRecallTokensSavedEstimate
		}
		m.RecallTokensSaved.Add(ctx, saved)
	}
}
if len(rec.RootCauses) > 0 {
	result = "recall"
	li.deliver(req, rec)
	return nil
}
// Recall was rejected by confirm/verify: fall through to a full investigation
// (do not deliver an empty finding). This is the intended fail-safe.
li.Log.Info("instant recall rejected by verify; running full investigation",
	"title", req.Title, "entry", entry.Path)
// execution continues to the ReAct loop below (byName := ...)
```

Nothing else in the loop changes; `result` is left unset here so the loop's normal completion sets it.

## 4. Components / seams

| Change | Location |
|---|---|
| Guard deliver/return + tokens-saved on `len(rec.RootCauses) > 0`; log + fall through otherwise | `internal/investigate/loop.go` (recall block) |
| Test: recall hit → verify rejects all → full loop runs and delivers a fresh finding | `internal/investigate/loop_test.go` (or `recall_test.go`) |

## 5. Trade-offs accepted in v1

- A rejected recall now costs a full investigation (more tokens) — but that is the whole point: a blank
  "recall" answer is strictly worse than re-investigating. The cost is bounded by the existing
  max-steps / token-budget / per-investigation deadline guards.
- We keep delivering on `downgraded` (confidence lowered but ≥1 cause survives) — only the *all-rejected*
  case falls through.

## 6. Testing

- **`TestRecallRejectedByVerifyFallsThrough`**: a scripted `ModelProvider` where (a) recall hits a strong
  entry, (b) the verify call rejects all root causes (reuse the `contentRejectModel`/verdict pattern from
  the existing verify tests), then (c) the full loop runs (`what_changed`/`kb_search` → `submit_findings`)
  and a fresh, non-empty finding (or honest `unresolved`) is delivered. Assert the delivered result is
  **not** the empty recall (root-cause count > 0 or `unresolved` populated; `result != "recall"`).
- **Metric**: assert `recall_hits_total{result="rejected"}` increments and `recall_tokens_saved_total`
  does **not** in the rejected case (extend the existing recall-metric test harness).
- Existing recall tests (`verified`/`downgraded`/hit-skips-loop) must stay green.

## 7. Out of scope (later slices)

- Changing the confirm/verify logic itself (only the control flow after it).
- Carrying the rejected recall as *context* into the fall-through investigation (a possible enhancement —
  noted, not done here).

This restores the recall safety invariant: when the adversarial guard rejects a catalog hit, RunLore
investigates for real instead of publishing an empty answer.
