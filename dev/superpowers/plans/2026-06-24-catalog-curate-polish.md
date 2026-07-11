# Plan — Catalog / Curate polish (Item R22)

Spec: `dev/superpowers/specs/2026-06-24-catalog-curate-polish-design.md`.
Test-first per nit; gate green before each commit; commit incrementally.

## Commit 1 — Nit 2: index `Resource` for BM25 (smallest, isolated)
- Test: `internal/catalog/catalog_test.go` — entry whose unique term is only in
  `Resource` ranks first for a resource-term query.
- Change: `catalog.go:88` append `e.Resource` to the indexed `text` join.
- Gate → commit.

## Commit 2 — Nit 3: emit `timestamp` in curated frontmatter
- Test: `internal/forge/github/github_test.go` — `renderEntry` output contains a
  `timestamp:` line parseable as RFC3339.
- Change: add `Timestamp string \`yaml:"timestamp,omitempty"\`` to
  `kbFrontmatter`; set `time.Now().UTC().Format(time.RFC3339)` in `renderEntry`.
- Gate → commit.

## Commit 3 — Nit 1: derive `KBEntry.Type` (default Incident, Playbook on
  resource-less + actionable findings)
- Test: `internal/curator/draft_test.go` — resource-less + suggested-action →
  `Playbook`; resource-pinned → `Incident`.
- Change: `draft.go` add `entryType(inv)`; set `Type: entryType(inv)`.
  Fix stale comment in `providers.go:379`.
- Gate → commit.

## Commit 4 — Nit 4: fingerprint-first Phase-2 dedup
- Test: `internal/curate/dedup_test.go` — reworded-title PRs sharing a marker
  collapse; same-title PRs with different markers don't; markerless → Jaccard.
- Change: `dedup.go` — pairwise duplicate test = equal parsed fingerprints when
  both present, else title-Jaccard. Add `providers` import.
- Gate → commit.

## Final
- Full gate incl. `golangci-lint run --enable gosec ./...`.
- Report: per-nit verdict, changes, commits, gate output, branch, deviations.
