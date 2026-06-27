# `Verified`/provenance in the curation merge bar — design (roadmap #16)

| | |
|---|---|
| **Date** | 2026-06-23 |
| **Roadmap item** | #16 — gate KB drafting on a passed adversarial review + provenance |
| **Depends on** | — (verify pass + `ChangeRef`/`SuggestedAction` already exist) |
| **Effort** | M |

## Problem

`meetsBar` (`internal/curator/curator.go:103-109`) — the quality gate before the
curator drafts a merge-ready KB PR — checks only confidence, a non-empty top-cause
summary, and ≥1 evidence item:

```go
func meetsBar(inv providers.Investigation, minConf float64) bool {
	if inv.Confidence < minConf || len(inv.RootCauses) == 0 {
		return false
	}
	top := inv.RootCauses[0]
	return top.Summary != "" && len(top.Evidence) > 0
}
```

It **ignores the verify pass and any provenance**. So an unverified or symptom-only
finding can still draw a "merge-ready" decision card into the shared, communal
catalog — the exact thing the adversarial review exists to prevent. The verify pass
already runs on every investigation (it is hardcoded `Verify: true` at all three
`LoopInvestigator` construction sites in `cmd/lore/main.go`; there is no config
toggle), but its result never reaches the curator.

## Design

Surface the verify outcome on the `Investigation`, and require it — plus an
actionable-provenance anchor — at the merge bar.

### 1. `Verified` on the Investigation (`providers.go`)

Add `Verified bool` to `providers.Investigation`. Default `false`.

### 2. Set it in the verify pass (`verify.go`)

In `applyVerdicts`, after recomputing the surviving causes, set:

```go
inv.Verified = len(kept) > 0
```

`Verified` is therefore `true` only when the adversarial review **actually ran and at
least one root cause survived it**. Every best-effort early return in `verifyFindings`
(no root causes, verifier model error, no verdicts parsed — `verify.go:65,74,84`)
leaves `Verified` at its `false` default, which is correct: a finding the reviewer
could not confirm is not verified. (Delivery to chat is unaffected — the finding still
goes to Slack/Matrix via the notifier; only *curation* is gated.)

### 3. Require it (and provenance) at the merge bar (`curator.go`)

```go
func meetsBar(inv providers.Investigation, minConf float64) bool {
	// Only adversarially-reviewed findings may enter the shared catalog.
	if !inv.Verified {
		return false
	}
	if inv.Confidence < minConf || len(inv.RootCauses) == 0 {
		return false
	}
	top := inv.RootCauses[0]
	if top.Summary == "" || len(top.Evidence) == 0 {
		return false
	}
	// Provenance: the entry must be actionable knowledge, not a symptom restatement
	// — anchored to a causing change (ChangeRef) or a fixing action (SuggestedAction).
	return top.ChangeRef != "" || top.SuggestedAction != ""
}
```

**Why `ChangeRef` OR `SuggestedAction` (not both):** requiring a causing-change
reference for every entry would wrongly exclude legitimate non-GitOps incidents
(saturation, cert expiry, capacity) that have no deploy to point at; requiring a
fixing action would exclude findings whose remediation is genuinely unknown. Either
anchor makes the entry actionable, durable knowledge; a finding with **neither** is a
bare symptom restatement and is correctly kept out of the communal catalog. This
faithfully delivers the roadmap's "causing/fixing provenance" intent (the
`changeRefs` helper at `draft.go:80` already calls these "the causing/fixing-change
provenance the merge bar requires") without over-filtering.

### Scope notes (verified against the code)

- **Recalled findings** are unaffected: `Curate` early-returns on `inv.Recalled`
  before `meetsBar` (`curator.go:35`). They are verified (loop.go:125) but never
  curated.
- **Eval** is unaffected: the replay `Runner` builds its `LoopInvestigator` without
  `Verify` and scores findings directly (it never calls `meetsBar`/`Curate`).
- **Fresh production investigations** always run verify (`loop.go:232`,
  `Verify: true`), so a genuine, confirmed finding still curates exactly as before —
  the gate only newly excludes unverified or symptom-only findings.

### Out of scope

- Making verify configurable / a config toggle (it is intentionally always-on).
- Changing what the investigator populates into `ChangeRef`/`SuggestedAction`.
- Reworking `applyVerdicts`' confidence math.

## Testing

- `internal/investigate/verify_test.go`
  - Extend `TestVerifyRejectsCorrelationFinding` (all rejected) to assert
    `got.Verified == false` (no cause survived).
  - Extend `TestVerifyDowngradesUnproven` (a cause survives) to assert
    `got.Verified == true`.
- `internal/curator/curator_test.go`
  - Update the `goodFinding()` fixture to set `Verified: true` (in production the
    loop sets it; the fixture must represent a verified finding so the existing
    happy-path test still passes).
  - `TestCurateUnverifiedDropsNoArtifact` — a finding identical to `goodFinding()`
    but `Verified: false` → `meetsBar` fails → no PR, no error.
  - `TestCurateSymptomOnlyDropsNoArtifact` — `Verified: true`, high confidence,
    summary + evidence present, but top cause has **no** `ChangeRef` and **no**
    `SuggestedAction` → dropped (provenance gate).
  - `TestCurateVerifiedWithSuggestedActionOnlyOpensPR` — `Verified: true`, no
    `ChangeRef` but a `SuggestedAction` set → passes (proves the OR, so non-GitOps
    incidents still curate).
  - Existing `TestCurateNovelHighQualityOpensPR` and `TestCurateLowQualityDropsNoArtifact` stay green.

## Files touched

- `internal/providers/providers.go` — add `Verified bool`.
- `internal/investigate/verify.go` — set `inv.Verified` in `applyVerdicts`.
- `internal/curator/curator.go` — `meetsBar` requires `Verified` + provenance.
- `internal/investigate/verify_test.go`, `internal/curator/curator_test.go` — tests above (incl. the `goodFinding` fixture update).
