# Catalog / Curate polish — 4 small correctness/quality nits (Item R22)

Date: 2026-06-24
Branch: `worktree-agent-af2f1c94780af7ac6`

Four small correctness/quality nits across the curate/catalog path. Each is
**challenged against current code** below: real vs overstated, with a verdict and
`file:line`, before any change.

---

## Nit 1 — `KBEntry.Type` hardcoded `"Incident"`

**Claim:** Playbook/Postmortem are advertised but never produced
(`internal/curator/draft.go:61`). Derive the type from the finding; default
Incident.

**CHALLENGE.**

- `Postmortem` is **not a real type**. The validator's vocabulary is
  `{Incident, Playbook, Concept}` (`internal/kbvalidate/kbvalidate.go:39`). The
  `KBEntry.Type` doc comment `// e.g. Incident | Playbook | Postmortem`
  (`internal/providers/providers.go:379`) is **stale** — emitting `Postmortem`
  would fail `lore validate-kb`. So the literal "produce Postmortem" half of the
  nit is **wrong**, not just overstated.
- `draftKBEntry` always renders the OKF Incident sections — `## Symptom`,
  `## Investigate`, `## Cause`, `## Resolution` (`draft.go:26-52`) — and
  `kbvalidate` requires *exactly* those sections **only** for `Incident`
  (`kbvalidate.go:128-138`). A drafted entry is therefore an Incident *by
  construction*; blindly stamping it `Playbook` would mislabel a section-bearing
  incident card and silently relax the validator that those sections satisfy.
- A `Playbook` is a *generalized, reusable* runbook keyed to a resource pattern
  (e.g. `resource: helmrelease://*`), authored/merged by a human — the lifecycle
  comment is explicit: a *solved* entry is what "should be merged as a Playbook"
  (`internal/forge/github/github.go:93`). The curator drafts a *specific*
  incident, not a generalized playbook.

**Verdict: REAL but mis-stated.** The hardcode is a real smell (the field exists
to vary; `Type` is plumbed all the way to frontmatter), and one legitimately
non-Incident shape exists. A finding with **no concrete affected resource**
(`inv.Resource.Ref() == ""` — alert named a namespace or nothing) yet a
**reusable suggested action and change provenance** is generalized,
resource-pattern knowledge → reads as a `Playbook`, not a point-in-time Incident.
That is the only defensible non-Incident the curator can emit today without
inventing data.

**Decision.** Add `entryType(inv)` deriving the type:
- default `Incident`;
- `Playbook` when the finding has **no concrete resource ref** but **does** carry
  a top root cause with a suggested action (generalized, not pinned to one
  object). `meetsBar` already guarantees a top cause + evidence + provenance, so
  this only ever fires on quality findings.
Fix the stale doc comment to `Incident | Playbook | Concept`. Do **not** emit
`Postmortem` (not a valid type). Note: a Playbook drafted this way still renders
the OKF sections; `kbvalidate` relaxes section checks for non-Incident, so this
is strictly safe — a Playbook with extra structure validates fine.

---

## Nit 2 — `Resource` not indexed for BM25

**Claim:** `Resource` drives the recall structural filter but isn't in the
indexed `text`, so it gets no lexical lift (`internal/catalog/catalog.go:88`).

**CHALLENGE.** Confirmed by reading `buildIndex`: the indexed `text` is
`Title + Description + Tags + Body` (`catalog.go:88`) — `e.Resource` is absent.
`Resource` is a real, populated frontmatter field (`entry.go:10`,
`load.go:64,74`) carrying e.g. `tooling/harbor-core` or `helmrelease://*`. A
query mentioning the resource (`"harbor-core CreateContainerConfigError"`) gets
no lexical contribution from the matching entry's resource. **Verdict: REAL.**

**Decision.** Append `e.Resource` to the indexed `text` join. Minimal, additive,
no schema change. (The separate `title` field is untouched.)

---

## Nit 3 — OKF `timestamp` omitted from curated entries

**Claim:** seed entries carry `timestamp`; curated ones don't
(`internal/forge/github/github.go:279-286`).

**CHALLENGE.** The shipped seed Playbook has `timestamp: 2026-06-20T00:00:00Z`
(`examples/runbooks/helmrelease-upgrade-failure.md:7`). `kbFrontmatter`
(`github.go:279-286`) has **no** `timestamp` field, so `renderEntry` never emits
one. OKF recommends it. The catalog loader doesn't parse it (`load.go:60-66`), so
it's purely advisory metadata today — but absent-on-curated vs present-on-seed is
a real inconsistency, and it's cheap human/provenance context in the diff.
`time.Now()` is used freely across the repo (e.g. `auth.go:54`,
`server.go:375`), so there is no prohibition. **Verdict: REAL.**

**Decision.** Add `Timestamp string \`yaml:"timestamp,omitempty"\`` to
`kbFrontmatter` and emit it from `renderEntry` using `time.Now().UTC()` in
RFC3339 (the format the seed uses, and the format `flux.Executor` already uses
at `flux.go:27`). Keep it on the *render* boundary (not on `KBEntry`) so the
deterministic `draftKBEntry` stays time-free and unit-testable; `renderEntry` is
already an I/O-adjacent serializer. `omitempty` keeps existing
`renderEntry(KBEntry{...})` test outputs that don't set it unaffected — but the
new field is always set in production, so the test asserts presence.

---

## Nit 4 — Phase-2 dedup uses title-Jaccard

**Claim:** `internal/curate/dedup.go:45` reintroduces title fragility that
`DupFingerprint` (`internal/curator/fingerprint.go`) was built to kill. Switch
to compare `DupFingerprint`. **Confirm `DupFingerprint` is computable here.**

**CHALLENGE — computability.** `DupFingerprint(inv)` needs an `Investigation`;
Phase-2 dedup only has `[]providers.CuratedIssue` (PR number/title/body/labels),
**not** the originating Investigation. So `DupFingerprint` is **NOT** directly
computable in Phase-2 — *this is the load-bearing subtlety.*

**BUT** the fingerprint is already *persisted* in the PR body: the curator writes
`FingerprintMarker(e.Fingerprint)` into every drafted PR's body
(`internal/forge/github/github.go:298-300` via `prBody`), and the file-time
`Curator.duplicateOpenPR` already reads it back with `ParseFingerprintMarker`
(`internal/curator/curator.go:104,113`). `CuratedIssue.Body` is available in
Phase-2 (`providers.go:295`; `forge.go:15`). So the *deterministic* fingerprint
**is** recoverable in Phase-2 — by parsing the marker, exactly as the file-time
gate does. **Verdict: REAL, and the fix is the marker, not recomputation.**

**Decision.** Make Phase-2 dedup **fingerprint-first**: when *both* PRs in a
candidate pair carry a parseable fingerprint marker, treat them as duplicates iff
the fingerprints are **equal** (deterministic, phrasing-proof — mirrors
`duplicateOpenPR`). Fall back to the existing title-Jaccard **only when a
fingerprint is absent** on either side (legacy/hand-filed PRs without a marker).
This kills the title fragility for the common path (all curator-drafted PRs carry
the marker) while staying robust for markerless PRs. Protected-label and
canonical/lowest-number rules are unchanged.

---

## Tests (test-first, stdlib `testing`, table-driven, NO testify)

1. **Nit 1** — `draftKBEntry` of a finding with no concrete resource but a
   suggested action yields `Type == "Playbook"`; a resource-pinned finding stays
   `Incident`.
2. **Nit 2** — an entry whose distinguishing term lives *only* in `Resource`
   ranks first for a resource-term query (no lift without the fix).
3. **Nit 3** — `renderEntry` frontmatter contains a `timestamp:` line with a
   parseable RFC3339 value.
4. **Nit 4** — two reworded-title PRs sharing one fingerprint marker collapse
   (title-Jaccard alone would miss them); two same-title PRs with *different*
   markers do **not** collapse (fingerprint beats coincidental title overlap);
   markerless PRs still fall back to title-Jaccard.

## Gate (before each commit)

`go build ./... && go vet ./... && go test ./... && gofmt -l . &&
golangci-lint run ./...` (+ `--enable gosec` where available).
