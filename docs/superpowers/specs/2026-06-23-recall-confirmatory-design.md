# Confirmatory evidence on the recall short-circuit — design (roadmap #13)

| | |
|---|---|
| **Date** | 2026-06-23 |
| **Roadmap item** | #13 — feed real cluster state to verify on the instant-recall path (or cap confidence when unavailable) |
| **Depends on** | #2 (discovered `Resource`, merged) |
| **Effort** | M |

## Problem

On the instant-recall short-circuit, `recalledInvestigation`
(`internal/investigate/recall.go:199-219`) sets the finding's sole evidence to a
**tautological string**: `"instant recall: matched knowledge-base entry %q"`. The
verify pass (`verifyFindings`, `verify.go:59-87`) judges each root cause **"ONLY on
the evidence given"** and can only *lower* confidence. Given only that string,
verify is a no-op on the recall path: a confidently-worded but **wrong or stale**
catalog entry sails through. (Bounded today: verify is the 3rd gate, recall is off
under `auto`, and recalled confidence is capped at 0.90 — but the gate that should
catch a stale belief cannot.)

All the parts to fix it already exist: the `pod_status` and `kube_events` tools
(`kube_tools.go`, backed by `providers.KubeReader`) return current pod health and
the Kubernetes event stream for a namespace, and `req.Workload` (namespace + optional
name, reliable since #2) is available at recall time.

## Design

On a recall hit, run a **minimal, non-LLM confirmatory step** — call the existing
`pod_status` and `kube_events` tools scoped to the alert's workload — and append
their output to the recalled finding's evidence **before** verify. Verify then judges
the recalled cause against current cluster state and can downgrade/reject a stale
entry. When confirmatory evidence cannot be gathered, fall back to a lower confidence
cap.

### Confirmatory step (`internal/investigate/confirm.go`, new)

```go
const recallUnconfirmedCap = 0.70 // recall confidence ceiling when current state could not be gathered

// recallConfirmTools are the read-only, namespace-scoped checks used to confirm a
// recalled finding against current cluster state, in priority order.
var recallConfirmTools = []string{"pod_status", "kube_events"}

// confirmRecall gathers current cluster state for the recalled workload and appends
// it to the finding's top hypothesis evidence, so the verify pass can judge the
// recalled cause against reality rather than a tautology. It reuses the investigation
// tools already wired into the loop (no new dependency). Best-effort: a missing
// namespace, absent tools, or a tool error simply yields gathered=false. gathered is
// true when at least one confirmatory tool returned output (including "no pods" /
// "no events" — that is still real current state).
func (li *LoopInvestigator) confirmRecall(ctx context.Context, req Request, inv providers.Investigation) (providers.Investigation, bool)
```

- Scope: `pod_status` is called with `{"namespace": ns}`; `kube_events` with
  `{"namespace": ns, "object": name}` (object omitted when the name is unknown),
  where `ns/name = req.Workload`. If `ns == ""`, return `(inv, false)`.
- Tool resolution: build a name→tool map from `li.Tools`; for each name in
  `recallConfirmTools` that is present, `Call` it. Swallow per-tool errors (log at
  debug); skip empty output.
- On success, append each rendered block to `inv.RootCauses[0].Evidence` as
  `"current state — <tool>:\n<output>"`, and return `(inv, true)`. If nothing was
  gathered, return `(inv, false)` unchanged.

### Wiring in the loop (`internal/investigate/loop.go`)

Replace the recall block's build→verify sequence:

```go
rec := recalledInvestigation(req, *entry, conf)
rec, confirmed := li.confirmRecall(ctx, req, rec)
if !confirmed {
    // Could not confront the entry with current state — be less assertive so an
    // unverifiable recall does not present at full recall confidence.
    rec = capRecallConfidence(rec, recallUnconfirmedCap)
}
initialConfidence := rec.Confidence
if li.Verify {
    rec = li.verifyFindings(ctx, req, rec) // now judges against real evidence when confirmed
}
```

`capRecallConfidence(inv, cap)` lowers `inv.Confidence` and every
`inv.RootCauses[i].Confidence` to at most `cap` (only ever lowers). Applying the cap
**before** verify keeps verify's max-of-survivors recomputation consistent.

The existing recall metrics block (`result = verified|downgraded|rejected` by
comparing to `initialConfidence`) is unchanged and now also reflects a
confirmatory-driven downgrade.

### Why this shape

- **Reuses the agent's own tools** — no new `KubeReader` plumbing through `main.go`;
  the confirmatory check renders exactly what the model would see.
- **Deterministic + cheap** — 1–2 in-cluster read calls, no extra LLM round-trip, so
  recall stays the fast path.
- **Combines both roadmap options** — confirmatory evidence when the workload is
  known and tools are present; a lower confidence cap when it is not.
- **Fail-safe** — every failure mode (no namespace, no tools, tool error) degrades to
  the lower-cap path, never to a crash or a lost finding.

### Out of scope

- Adding new confirmatory tools (metrics/logs) — `pod_status` + `kube_events` are the
  fast, universally-available kube checks; widen later if needed.
- Changing `recalledInvestigation`'s tautological label line (kept as an explicit
  "this was a recall" marker; the real evidence is appended alongside it).
- Recall behavior under `auto` (still disabled there by design).

## Testing

- `internal/investigate/confirm_test.go`
  - `TestConfirmRecallAppendsCurrentState` — a fake `pod_status` tool returning
    "web CrashLoopBackOff" → evidence gains the block, `gathered==true`.
  - `TestConfirmRecallScopesToWorkload` — assert `pod_status` receives the request's
    namespace and `kube_events` receives the workload name as `object` (a fake tool
    records the args it was called with).
  - `TestConfirmRecallNoNamespaceSkips` — empty `req.Workload.Namespace` →
    `gathered==false`, evidence unchanged, no tool called.
  - `TestConfirmRecallToolsAbsentSkips` — `li.Tools` without the confirm tools →
    `gathered==false`.
  - `TestConfirmRecallToolErrorTolerated` — a confirm tool returning an error is
    skipped; if the other yields output, `gathered==true`; if both fail, `false`.
  - `TestCapRecallConfidenceOnlyLowers` — caps inv + per-hypothesis confidence to the
    ceiling; a value already below is untouched.
- `internal/investigate/loop_test.go`
  - `TestInstantRecallUnconfirmedLowersConfidence` — recall hit with no confirm tools
    → delivered confidence ≤ `recallUnconfirmedCap`.
  - `TestInstantRecallConfirmedEvidenceReachesVerify` — recall hit + a confirm tool
    whose output contradicts the entry + a `scriptModel` verify that rejects →
    delivered finding is rejected/downgraded (proves real evidence reached verify).
- Existing recall/verify/loop tests stay green.

## Files touched

- `internal/investigate/confirm.go` — **new**: `confirmRecall`, `capRecallConfidence`, `recallConfirmTools`, `recallUnconfirmedCap`.
- `internal/investigate/loop.go` — wire the confirmatory step + cap into the recall block.
- `internal/investigate/confirm_test.go` — **new** tests above.
- `internal/investigate/loop_test.go` — two recall-path tests above.
