# Curation resolution join keyed on dedup fingerprint — design (item R6)

| | |
|---|---|
| **Date** | 2026-06-24 |
| **Roadmap item** | R6 — replace free-text-title resolution join with the deterministic dedup fingerprint |
| **Depends on** | #11 (`DupFingerprint`, merged), outcome ledger Episodes (merged) |
| **Effort** | S–M |

## Problem

`LedgerResolutionChecker.IsResolved` (`internal/curate/resolution.go:77-91`) decides a
curated PR's incident has resolved by joining the PR to a ledger episode on the
**LLM free-text title**:

```go
title := strings.TrimSpace(strings.TrimPrefix(pr.Title, "KB: "))
...
for _, e := range eps {
    if e.Resolved && e.Title == title { return true, nil }
}
```

Two fragilities:

1. **Title is the weakest key.** A reworded re-investigation of the same incident
   produces a different `inv.Title`, so the resolved episode's title no longer equals
   the PR's title and the resolved PR is **never auto-queued** — it sits unmerged
   waiting for a human `accepted` label. This is the exact failure the curation queue
   was built to avoid.
2. **`TrimSpace`-vs-raw mismatch.** The PR side `TrimSpace`es; the ledger side
   compares the raw `Episode.Title`, which is written verbatim from `found.Title`
   at `cmd/lore/main.go:1217`. A title the LLM emits with stray leading/trailing
   whitespace silently fails to match.

The deterministic dedup fingerprint (item #11) already solves "is this the same
incident, regardless of the LLM's prose" for the curator's open-PR dedup. The
resolution join should reuse the **same** stable key.

## Two fingerprints — why only one is usable

The codebase has two distinct fingerprints; conflating them is the trap:

- **Alert fingerprint** (`outcome.Event.Fingerprint`, `Investigation.Fingerprint`):
  the Alertmanager firing↔resolved hash. The ledger keys its open-index on it
  (`ledger.go:42`) and `Resolve(fp, …)` matches it. It is **per alert series** and is
  the *wrong granularity* for the PR→incident join: one curated PR (one
  `DupFingerprint`) can subsume many alert fingerprints (recurrence, plus coalesced
  batches — `main.go:1212` records one open per constituent alert fingerprint). It is
  also **not carried on the PR**, and `Episode` does not even copy it out of `Event`
  (`ledger.go:155-158`). Re-keying on it is both wrong and infeasible.

- **DupFingerprint** (`KBEntry.Fingerprint`, the hidden `<!-- runlore-fingerprint -->`
  PR-body marker): the curator's deterministic hash of `resource + cause-token-set`
  (`internal/curator/fingerprint.go:80`). It is **stable across LLM phrasing** (that
  is its whole purpose, `2026-06-23-curate-dedup-fingerprint-design.md`) and is
  **already on the PR** (`draft.go:67` → `prBody`, `github.go:298`). The dedup design
  *deliberately excludes* the alert fingerprint so genuinely-recurring incidents from
  different alert series dedup to one PR — exactly the property the resolution join
  needs.

So `DupFingerprint` is the correct stable key. It is present on the PR side already.
It is **not** on the ledger side — `Episode`/`Event` carry `Title`, `Resource`,
`Entry`, `Kind`, the alert `Fingerprint`, but not the dedup fingerprint. The fix
plumbs it onto the ledger.

## Design

### Carry `DupFingerprint` on the ledger event/episode

Add a pure data field — `outcome` stays a leaf package (it imports no internal
package, and must not start importing `curator`):

- `outcome.Event`: `DupFingerprint string \`json:"dup_fingerprint,omitempty"\``.
- `outcome.Episode`: `DupFingerprint string` (copied through in `Episodes()` and
  `Resolve()` exactly like `Title`/`Resource`).

The producer already imports `curator` and `outcome` (`cmd/lore/main.go:46,61`) and
already records the open from the same `found` Investigation it later curates
(`main.go:1213` then `main.go:1252`). So the open site computes the dup fingerprint
once and stamps it:

```go
dupFP := curator.DupFingerprint(found)
...
ledger.Open(outcome.Event{
    Fingerprint:    fp,
    DupFingerprint: dupFP,
    Kind:           kind, Entry: found.RecalledEntry,
    Title: found.Title, Resource: found.Resource.Ref(), At: now,
})
```

`DupFingerprint` is the *same* value `draftKBEntry` writes into the PR-body marker
(`draft.go:67`), so the open episode and the curated PR carry an identical key. (A
`""` dup fingerprint — finding with neither resource nor cause — is recorded as
absent and never matches, mirroring the curator's empty-fingerprint guard.)

### Re-key the join, with a title fallback

`IsResolved` matches the PR-body dup-fingerprint marker against the resolved
episodes' `DupFingerprint`; only when *neither* the PR carries a marker *nor* a
fingerprint match is found does it fall back to the normalized title join — so the
`TrimSpace`-vs-raw fragility is also fixed by normalizing **both** sides:

```go
func (c LedgerResolutionChecker) IsResolved(_ context.Context, pr providers.CuratedIssue) (bool, error) {
    wantFP := providers.ParseFingerprintMarker(pr.Body)            // "" when no marker
    title := strings.TrimSpace(strings.TrimPrefix(pr.Title, "KB: "))
    if wantFP == "" && title == "" {
        return false, nil
    }
    eps, err := c.Ledger.Episodes()
    if err != nil { return false, err }
    for _, e := range eps {
        if !e.Resolved { continue }
        if wantFP != "" && e.DupFingerprint == wantFP {            // primary: stable key
            return true, nil
        }
        if wantFP == "" && title != "" && strings.TrimSpace(e.Title) == title { // legacy fallback
            return true, nil
        }
    }
    return false, nil
}
```

- **Primary join (fingerprint).** When the PR carries a marker, match *only* on the
  dup fingerprint — a reworded re-investigation of the same incident resolves the PR
  regardless of title (the acceptance criterion). An episode with an empty
  `DupFingerprint` never equals a non-empty `wantFP`, so empty never false-matches.
- **Legacy fallback (title).** A PR filed before this change carries no marker
  (`wantFP == ""`); it still resolves via the title join — now whitespace-robust on
  both sides (`strings.TrimSpace(e.Title) == title`). This keeps already-open PRs and
  ledgers written before R6 working; it is *not* used when a marker is present, so a
  fingerprint mismatch does not silently fall through to a brittle title match.

### Why fingerprint-only when a marker is present

If a PR has a marker but no episode matches its fingerprint, the honest answer is
"not resolved" — falling through to a title match there would resurrect the exact
fragility R6 removes (and could false-resolve a *different* incident that happens to
share the reworded title). Fingerprint presence is the signal that the deterministic
path applies.

### Out of scope

- The `Recurrence` knowledge-gap join (`recurrence.go`) groups by `Resource`/`Title`,
  not by the curated PR — it is a different mechanism and not part of R6.
- Retro-stamping ledgers/PRs written before R6 (the title fallback covers them).
- The alert-fingerprint open-index / `Resolve` path — unchanged.

## Testing

- `internal/outcome/ledger_test.go`
  - `TestEpisodeCarriesDupFingerprint` — an open with `DupFingerprint` set surfaces it
    on the paired `Episode` (open→resolve round trip and the `Episodes()` replay).
- `internal/curate/resolution_test.go`
  - `TestLedgerResolutionRekeyedTitleDiffers` (acceptance) — a curated PR whose body
    carries a dup-fingerprint marker resolves against a resolved episode that shares
    the **fingerprint** but has a **different title**; a PR whose marker matches *no*
    resolved episode does **not** resolve.
  - `TestLedgerResolutionLegacyTitleFallback` — a PR with **no** marker still resolves
    on the (whitespace-normalized) title join; the raw-vs-trimmed mismatch no longer
    breaks it.
  - `TestLedgerResolutionFingerprintMismatchNoTitleFallthrough` — a PR *with* a marker
    that matches no episode fingerprint stays unresolved even if a resolved episode
    shares its title (no fall-through to the brittle title path).
  - Existing title-only tests keep passing (they carry no marker → legacy path).

## Files touched

- `internal/outcome/ledger.go` — `Event.DupFingerprint`, `Episode.DupFingerprint`,
  copy-through in `Episodes()` and `Resolve()`.
- `cmd/lore/main.go` — compute `curator.DupFingerprint(found)` once and stamp it on
  the recorded open event.
- `internal/curate/resolution.go` — fingerprint-primary join with a whitespace-robust
  title fallback.
- corresponding `_test.go` files above.
