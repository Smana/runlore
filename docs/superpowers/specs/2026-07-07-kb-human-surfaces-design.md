# KB human surfaces — design

**Date:** 2026-07-07
**Status:** approved for planning

## Problem

RunLore's knowledge base is heavily engineered for *agent* consumption (3-gate
recall, Bayesian decay, deterministic dedup) but nearly invisible to *humans*:

- When a known incident recurs, the notification shows only an occurrence
  counter and a "previous" link. The valuable knowledge — the previously
  confirmed cause and the human-reviewed resolution — is behind a click to
  GitHub, at exactly the moment (on-call, incident open) when zero-click
  matters most.
- The BM25 index exists and is exposed over MCP for LLM clients, but a human
  has no search interface at all beyond GitHub code search over markdown.
- The KB PR reviewer — the human who pays the curation cost — gets no context
  to judge "is this a duplicate? what do we already know about this?".

This is also an adoption problem: the README's core promise is "it remembers
what it learns", but no human-visible moment ever demonstrates the memory
working. The humans who review and merge KB PRs never see the dividends.

## Scope

Three independent features, one PR each (parallel worktrees):

1. **"Seen before" block in notifications** — inline prior cause/resolution on
   recurring incidents.
2. **`lore kb search` / `lore kb show`** — human CLI over the existing index.
3. **"Related knowledge" section in drafted KB PRs** — reviewer context.

Out of scope (deliberately): Slack slash command, web UI, periodic digest.
These serve personas (cold-exploration engineer, lead) not prioritized in this
round, and each drags new surface area (exposed HTTP endpoint, authn). Revisit
after the three features above land.

## Feature 1 — "Seen before" block in notifications

### Data source decision

**Chosen: catalog lookup by fingerprint at delivery time.**

- The outcome ledger already stamps each `open` with the incident's
  `DupFingerprint`; curated entries carry the same fingerprint in their YAML
  frontmatter (`catalog.Entry.Fingerprint`).
- At delivery, a merged entry found by that fingerprint reflects the
  **human-edited resolution** added at review time — which is precisely the
  value curation adds. A ledger-denormalized snapshot (rejected) would freeze
  the model's pre-review prose; a forge fetch (rejected) puts a network call
  and rate limits on the delivery path.

### Components

- `catalog.Entry.Section(name string) string` — extract the first paragraph of
  a `## <name>` markdown section from the entry body, truncated (~300 chars).
  Returns `""` on missing/malformed sections. Shared by features 1 and (test
  fixtures aside) reusable anywhere entry bodies are summarized.
- `Catalog.ByFingerprint(fp string) (*Entry, bool)` — O(1) map built during
  indexing (fingerprint is already parsed from frontmatter), rebuilt with the
  index on git-sync. Entries without a fingerprint are simply absent.
- `providers.PriorKnowledge` struct on `Investigation`:
  `{Count int, LastSeen time.Time, Cause string, Resolution string,
  EntryURL string, PrevLink string, ResolveRate string}`.

### Flow (at `OnComplete`, where recurrence facts are already stamped)

1. Incident is recurring (`Occurrences() >= 2` for its trigger key) → its
   `DupFingerprint` is already computed for the ledger open.
2. `catalog.ByFingerprint(fp)` hit → fill `PriorKnowledge` with
   `Section("Cause")`, `Section("Resolution")`, the entry URL, and the
   resolve-rate from `OpenCounts()`.
3. No merged entry (PR still open, or entry lacks a fingerprint) → fill only
   `Count/LastSeen/PrevLink` — same information as today, rendered in the new
   block.
4. Merged entry found but both sections empty/malformed → treat as case 3
   (never render an empty knowledge block).

### Rendering

- Block: `📚 Seen before ×N · last seen <relative date>` + 2–3 lines of prior
  cause and resolution + entry link + resolve-rate.
- Placement: directly under the verdict/title, **before** the current root
  cause — history frames how the reader interprets what follows.
- Surfaces: Slack blocks (channel message, not the thread — this is the
  zero-click information) **and** `notify.Format` (so Matrix/webhook carry it
  too). The existing mrkdwn-escape invariant applies: no `&`, `<`, `>` in
  `Format` scaffolding; all model/entry strings escaped in Slack blocks.
- **Recall path excluded**: a recalled answer already presents the KB entry;
  the block applies only to *fresh* investigations of a recurring trigger.
- **Best-effort**: any lookup/parse error degrades to the counter+link form or
  drops the block; it must never fail or delay delivery.

## Feature 2 — `lore kb search` / `lore kb show`

New CLI namespace `lore kb` — the human *read* surface. The existing
`lore catalog sync` (a machine/ops write operation) is untouched.

- `lore kb search "<query>" [--config <path>] [-k 10] [--json] [--ledger <path>]`
  - Loads the catalog the same way `lore mcp`/`BuildCatalog` does (local dir
    or git clone per config), runs the same BM25 search the agent uses.
  - Output: aligned text table — `SCORE · ENTRY · TITLE · RESOURCE · LAST
    SEEN`. `LAST SEEN` derives from the entry's `timestamp` frontmatter
    (blank when absent). `--json` emits the hits as JSON for scripting.
  - `--ledger <jsonl>`: the outcome ledger lives in-cluster, not on
    workstations, so resolve-rate is opt-in — when the flag points at a
    readable ledger file, a `RESOLVE` column (e.g. `3/3`) is added via
    `OpenCounts()`; otherwise the column is omitted entirely.
- `lore kb show <entry>` — full entry: frontmatter card then markdown body.
  Argument is a path or slug; if no exact match, fall back to a search — a
  unique hit is shown, multiple hits are listed as disambiguation.
- Constraints: stdlib only (no table/color deps); ANSI color only when stdout
  is a TTY; exit non-zero on no results (scripting-friendly).

Side benefit: a 30-second demo path for adopters —
`git clone <their-kb> && lore kb search "oom"`.

## Feature 3 — "Related knowledge" section in drafted KB PRs

At draft time the curator **already runs** a BM25 search for dedup; its hits
are currently discarded after the duplicate check. Reuse them:

- Append a `## Related knowledge` section to the **PR body** (not the entry
  file): the top 3–5 nearest entries as linked titles with score and resource,
  plus — when the trigger is recurring — one line of history via
  `Occurrences()`: `Trigger seen ×N · previous entry: <link>`.
- Rendered below the existing fingerprint marker, same mechanics as the
  decision card.
- Omitted entirely when no hit clears a score floor (no noise on genuinely
  novel incidents).
- `kbvalidate` is unaffected: the section lives in the PR body; the merged
  entry file is unchanged.

## Error handling (cross-cutting)

Every feature is best-effort relative to its host path:

- Feature 1: lookup/parse failure → degrade to counter+link or omit; delivery
  never blocks.
- Feature 2: unreadable catalog/config → clear error message, non-zero exit;
  unreadable `--ledger` → warn and omit the column.
- Feature 3: search failure at draft time → section omitted; the PR still
  opens.

## Testing

- `Section()`: table-driven over malformed/missing/nested-heading fixtures.
- `ByFingerprint`: absent fingerprint, duplicate fingerprints (last-indexed
  wins, deterministically), rebuild-on-sync.
- Notification: Slack block rendering with the escape invariant
  (`TestSlackMessageFallbackEscaped` pattern), `Format` output for
  Matrix/webhook, recall-path exclusion, fallback cases.
- CLI: golden-output tests over a fixture catalog; `--json` schema; ledger
  present/absent.
- Curator: golden PR-body tests with/without related hits and recurrence.

## Sequencing

Three independent PRs from parallel worktrees. The shared `Section()` helper
ships in the Feature 1 PR (Feature 3 uses entry titles/links only, so there is
no cross-PR dependency; if that changes, extract `Section()` as a tiny base
PR).
