# Deterministic curation dedup fingerprint — design (roadmap #11)

| | |
|---|---|
| **Date** | 2026-06-23 |
| **Roadmap item** | #11 — replace title-equality open-PR dedup with a deterministic fingerprint stored in the PR/entry |
| **Depends on** | #2 (discovered `Resource`, merged) |
| **Effort** | M |

## Problem

The curator's open-PR dedup keys on the **LLM free-text title**. `duplicateOpenPR`
(`internal/curator/curator.go:83-95`) normalizes the incoming `inv.Title` and each
open PR's title and compares them for equality:

```go
want := normTitle(inv.Title)
for _, pr := range prs {
    if normTitle(strings.TrimPrefix(pr.Title, "KB: ")) == want { ... }
}
```

Two investigations of the **same incident** routinely produce different prose
titles, so both pass the dedup and **both file a PR** — the 5×-DependencyNotReady
flood the curation spec was written to prevent is not prevented. The reliable,
structured fields now exist on the `Investigation` (`inv.Resource.Ref()` from #2,
the ranked `RootCauses`) but neither the open-PR dedup nor the catalog dedup uses
them deterministically. (The existing `Fingerprint()` in `fingerprint.go:14-25` is a
**fuzzy BM25 query string** for catalog search — a different mechanism, left as-is.)

## Design

A deterministic fingerprint identifies "the same problem on the same resource,"
computed from structured signals, stored in the PR so future investigations match
against it.

### The fingerprint

New `DupFingerprint(inv providers.Investigation) string` in
`internal/curator/fingerprint.go`:

- Canonical input string: `normRef + "|" + tokenSet(topCauseSummary)` where
  - `normRef` = `strings.ToLower(inv.Resource.Ref())` (the `namespace/name` from #2),
  - `topCauseSummary` = `inv.RootCauses[0].Summary` when present, else `""`,
  - `tokenSet(s)` = lowercase `s`, split on non-alphanumeric, drop tokens shorter
    than 3 chars, dedupe, **sort**, join with spaces — order-independent so two
    phrasings of the same cause normalize alike.
- Return `sha256` hex of the canonical input.
- **Guard:** if both `normRef == ""` and the token set is empty, return `""` — an
  empty fingerprint never matches (avoids false-coalescing unrelated findings that
  carry neither a resource nor a cause). The quality gate (`meetsBar`) already
  blocks cause-less findings from filing, so this only affects the dedup pre-check.

Why not include the alert title / alertname: that is the LLM free-text fragility
this item removes, and `alertname` is not a clean field on `Investigation` (only the
LLM `Title` and the per-series `inv.Fingerprint` hash). `Resource.Ref()` +
cause-token-set is the deterministic spine and a strict improvement; the per-series
alert fingerprint is deliberately excluded so genuinely-recurring problems from
different alert series still dedup.

### Storing & matching it

The fingerprint must travel with the PR so a later investigation can match it
without fetching file contents. Two carriers:

1. **PR/entry frontmatter (durable record):** add `Fingerprint string` to
   `providers.KBEntry` and to `kbFrontmatter` (`internal/forge/github/github.go:278`,
   `yaml:"fingerprint,omitempty"`). `draftKBEntry` (`internal/curator/draft.go`) sets
   `Fingerprint: DupFingerprint(inv)`; `renderEntry` serializes it into the entry's
   YAML frontmatter.
2. **PR body marker (matchable from the listing):** `ListPRsByLabel` already returns
   each open PR's `Body` (`CuratedIssue.Body`). `prBody`
   (`internal/forge/github/github.go:290`) appends a hidden HTML-comment marker
   `<!-- runlore-fingerprint: <hex> -->` when `e.Fingerprint != ""`.

Marker format/parse helpers live in `providers` (both `curator` and `forge/github`
import it — no import cycle):

```go
// providers
func FingerprintMarker(fp string) string             // "" when fp==""
func ParseFingerprintMarker(body string) string      // "" when absent
```

### The new dedup

`duplicateOpenPR` (`curator.go`) matches on the fingerprint marker instead of the
title:

```go
want := DupFingerprint(inv)
if want == "" {
    return 0, false, nil // nothing deterministic to match on
}
prs, err := c.Forge.ListPRsByLabel(ctx, "runlore")
...
for _, pr := range prs {
    if providers.ParseFingerprintMarker(pr.Body) == want {
        return pr.Number, true, nil
    }
}
return 0, false, nil
```

`normTitle` becomes unused and is removed. Open PRs filed before this change carry
no marker, so they simply won't match (acceptable: the fix prevents *future* floods;
it does not retro-dedup history). The downstream coalesce-comment behavior on a
match is unchanged.

### Out of scope

- The catalog (on-disk, main-branch) dedup via `Novelty`/`Fingerprint` BM25 query —
  unchanged; it is the fuzzy first gate and a different problem.
- Retro-fingerprinting existing open PRs.
- `meetsBar` provenance changes — that is item #16.

## Testing

- `internal/curator/fingerprint_test.go`
  - `TestDupFingerprintStableAcrossTitlePhrasing` — two investigations with the
    **same** `Resource` and cause but **different** `Title` produce the **same**
    `DupFingerprint`.
  - `TestDupFingerprintDiffersByResource` — same cause, different `Resource.Ref()` →
    different fingerprint.
  - `TestDupFingerprintDiffersByCause` — same resource, disjoint cause token-set →
    different fingerprint.
  - `TestDupFingerprintEmptyWhenNoResourceOrCause` — no resource and no cause → `""`.
  - `TestFingerprintMarkerRoundTrip` — `ParseFingerprintMarker(FingerprintMarker(x)) == x`; empty body → `""`; `FingerprintMarker("") == ""`.
- `internal/curator/curator_test.go`
  - Update `TestCurateDuplicateCoalescesNoPR`: the fake open PR's `Body` carries the
    matching fingerprint marker (not a matching title) → coalesces, no new PR.
  - `TestCurateDistinctTitleSameFingerprintCoalesces` — incoming finding with a
    **different title** but the same resource+cause as an open PR (marker in body) →
    coalesces (the regression the item fixes).
  - `TestCurateDifferentFingerprintOpensSecondPR` — open PR with a non-matching
    marker → a genuinely different finding still opens its own PR.
- `internal/curator/draft_test.go`
  - `TestDraftKBEntrySetsFingerprint` — `draftKBEntry(inv).Fingerprint == DupFingerprint(inv)`.
- `internal/forge/github/github_test.go`
  - `TestRenderEntryIncludesFingerprintFrontmatter` — `renderEntry` emits
    `fingerprint:` in the YAML frontmatter when set, omits when empty.
  - `TestPRBodyIncludesFingerprintMarker` — `prBody` contains the hidden marker when
    the entry has a fingerprint.

## Files touched

- `internal/curator/fingerprint.go` — `DupFingerprint`.
- `internal/providers/providers.go` — `KBEntry.Fingerprint`; `FingerprintMarker` / `ParseFingerprintMarker`.
- `internal/curator/draft.go` — set `Fingerprint` on the drafted entry.
- `internal/curator/curator.go` — fingerprint-based `duplicateOpenPR`; drop `normTitle`.
- `internal/forge/github/github.go` — `kbFrontmatter.Fingerprint`; `renderEntry`; `prBody` marker.
- corresponding `_test.go` files above.
