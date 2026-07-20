# Multi-KB Federation — Design

| | |
|---|---|
| **Status** | Draft `v0.1` — awaiting maintainer review; **do not plan/implement until the adoption triggers in §9 fire** |
| **Date** | 2026-07-20 |
| **Scope** | One RunLore instance reading N knowledge bases — an org-wide KB, a team KB, read-only vendor runbook bundles — with one writable KB for curation/retirement, stable cross-KB entry identity for the outcome loop, and zero config breakage for single-KB deployments. |
| **Author** | Claude (audit Later-wave L3), for Smana's review |
| **Related** | `2026-07-19-audit-roadmap.md` (Later horizon); `internal/catalog` (single `Dir`/`Git` today); `internal/outcome` ledger keyed by entry path; N4 retirement (`internal/curate/retirement.go`); N6 staleness (`status`/`last_validated`); N2 vector cache (content-hash, survives federation rebuilds) |

---

## 1. Why this exists

Today the catalog is exactly one source: `config.Catalog{Dir, Git}` feeds one
`BuildCatalog` → one `*catalog.Catalog` (`internal/app/catalog.go:21-120`), one
`Syncer`, one bleve index. A multi-team org wants layering: a **team KB** (their
incidents), an **org KB** (platform-wide constraints and patterns), and **vendor
bundles** (e.g. an upstream OKF runbook set for an ingress controller) — consumed
read-only, never curated into.

Federation is NOT about serving many consumers (MCP `kb_search`/`kb_get` already
does that). It is about one agent *reading* many KBs while the learning loop keeps
working: recall must rank across all of them, outcome decay must attribute to the
right entry, and the write path (curation, retirement) must know where PRs go.

The hard problem is **identity**, not search: the outcome ledger keys aggregates by
the entry's bundle-relative path (`event.Entry`, `OpenCounts() map[string]Aggregate`,
`internal/outcome/ledger.go:67,989`). Two KBs can both contain `runbooks/oom.md`;
without namespacing, their track records merge silently and decay poisons the wrong
entry.

## 2. Decisions (recommended; alternatives in §-notes)

| # | Decision | Rationale |
|---|---|---|
| F1 | **Config = ordered `catalog.sources` list; legacy keys become an implicit `default` source** | Zero breakage (§8); names are the identity namespace. |
| F2 | **One merged index + ranked pool; per-source `weight` multiplier; NO strict tiers** | BM25 scores are not comparable across separate indexes — per-KB indexes would need cross-index score normalization, which is exactly the corpus-dependent-magnitude trap the reranker was built to escape. One pool composes with every existing gate unchanged. |
| F3 | **Entry identity = `<source>/<path>`; ledger reads legacy bare paths as `default/<path>`** | Stable across federation; single-KB history survives with a read-shim, no data migration. |
| F4 | **Exactly one `writable: true` source; curator + retirement target it; validated at startup** | One write path keeps curation the load-bearing gate; per-rule routing is YAGNI. |
| F5 | **Decayed read-only entries are suppressed by the outcome gate (already per-instance), never by PR** | Retirement pass filters to the writable source; the recall decay floor needs no write access — this is where per-KB-scoped outcome identity earns its keep. |
| F6 | **Same-fingerprint cross-KB duplicate: index the writable KB's copy, skip others (log it)** | Deterministic winner when an org forks a vendor runbook; margin-gate self-suppression between near-identical entries is avoided. |

## 3. Config shape (F1)

```yaml
catalog:
  sources:
    - name: team            # required, unique, [a-z0-9-], stable — becomes the ID prefix
      git: {url: ..., branch: main, interval: 5m, token_env: TEAM_KB_TOKEN}
      writable: true        # exactly one source; curation + retirement PRs go here
    - name: org
      git: {url: ..., token_env: ORG_KB_TOKEN}
    - name: vendor-ingress
      dir: /var/lib/runlore/bundles/ingress   # mounted read-only bundle
      weight: 0.9           # optional retrieval-score multiplier, default 1.0
  instant_recall: {...}     # unchanged, instance-wide
```

- `sources` and the legacy `dir`/`git` keys are mutually exclusive (config error).
- Legacy keys ⇒ one implicit source `{name: default, writable: true}` — the whole
  federation machinery runs with N=1 and is behaviorally identical (§8).
- `weight` applies to the retrieval score (BM25 or cosine) BEFORE ranking/fusion;
  it never touches the fire gates' thresholds (a weighted-down entry can still fire
  if it wins the pool). Alternative considered — per-source confidence caps:
  rejected, overlaps with outcome decay's job.
- Deliberately absent (YAGNI until asked): per-source namespace eligibility
  scoping, per-source recall thresholds, >1 writable source.

## 4. Recall semantics (F2)

One merged pool: `Load` walks every source into a single entry slice (each entry
stamped `Source`), one bleve index, one vector slice. `SearchScored`/`SearchHybrid`
are unchanged except the per-source weight multiplication at scoring time. All
existing gates — structural agreement, reranker/margin, outcome decay (per
namespaced ID), N3 runner-up fallback, N6 status/age — apply unchanged, which is
the point: federation adds a dimension to *retrieval*, not new gate logic.

Shadowing emerges from the loop, not from static tiers: if a team entry and an org
entry both agree structurally, the one with the better track record survives the
outcome gate; static tier precedence (alternative considered) would freeze that
judgment at config time and required a cross-KB fingerprint contract we don't have.
The ONE static rule is F6: byte-identical intent (same `fingerprint` frontmatter)
indexes only the writable copy.

## 5. Outcome & feedback identity (F3) — the critical seam

- `catalog.Entry` gains `Source string`; `Entry.ID() = Source + "/" + Path`.
- Every ledger consumer switches from `Path` to `ID()`: the recall gate's
  `counts[e.Path]` lookup, open-event stamping, feedback attribution, retirement
  candidacy, kbmcp result identity.
- Ledger load shims legacy events: a bare `entry` (no `/`-prefixed known source —
  disambiguated by a one-time check against configured source names) is read as
  `default/<path>`. New events are always written namespaced. Single-KB users who
  later federate keep their entire trust history (their source IS `default`).
- Feedback (👍/👎) and N5 confirms attribute through `byTrigger` exactly as today —
  the credited value is now the namespaced ID. No schema change beyond the string's
  content; checkpoint/compaction untouched.

## 6. Write path (F4) and retirement on read-only KBs (F5)

- `Curator.Forge` and `Retirement.Forge` stay single-valued, bound to the writable
  source's repo. Config validation: `sources` requires exactly one `writable: true`
  (the implicit `default` is writable).
- Retirement candidacy filters `OpenCounts` to `writable-source/` IDs. A decayed
  vendor entry gets no PR — and needs none: the outcome gate already rejects it at
  recall time on this instance, scoped to this instance's ledger. Surface it
  (`kb_readonly_entry_decayed` log + metric) so the operator can report upstream.
- N6 read-side staleness (`status: retired`, age step-down) applies to every source
  as-is — a vendor bundle that ships `status: retired` is honored with zero code.

## 7. Index, memory, reload (interacts with N2)

- **One merged index** (F2). Per-source `Syncer`s (git sources) and static dirs each
  own a local mirror dir; any source's HEAD move (or startup) triggers ONE merged
  rebuild, debounced (e.g. 10s) so simultaneous syncs coalesce.
- The N2 content-hash vector cache makes merged rebuilds cheap: a change in one
  source re-embeds only that source's changed entries — the cache is keyed by entry
  text, source-agnostic, and survives federation unchanged.
- Memory is the same corpus total as today (no duplication); `Ready()` stays
  all-or-nothing on the first successful merged build, per-source failures degrade
  to "index what loaded" with the existing skip/warn pattern (one bad bundle never
  empties the catalog).

## 8. Migration & compatibility

- Single-KB configs: byte-identical behavior, no config edit, ledger read-shim
  covers history. The only observable change is namespaced entry IDs in NEW ledger
  events and logs.
- `lore kb` / kbmcp: results gain a `source` field (additive).
- Rollback: turning federation off (back to legacy keys) keeps `default/` history;
  non-default sources' ledger history becomes inert (correct — those entries are
  gone from the index).

## 9. When NOT to build this

Do not build on speculation. Triggers (any one):
1. A real multi-team adopter asks for org/team layering (issue/discussion, not a
   hypothetical).
2. A vendor/community OKF bundle worth consuming read-only actually exists.
3. The maintainer's own deployment needs a second KB.

Until then this spec is a parking orbit: the identity work (§5) is the only part
worth doing early IF the ledger is being touched for other reasons, because the
legacy-path shim gets harder the longer bare paths accumulate. Counter-argument to
building early: N=1 federation is pure added complexity in the hottest, most
safety-critical path (recall). Single-maintainer scope says wait.

## 10. Phased breakdown (for later plan-writing)

- **P1 — identity groundwork** (shippable dark): `Entry.Source` + `ID()`,
  namespaced ledger keys + legacy read-shim, `catalog.sources` parsing/validation
  (exactly-one-writable, unique names, mutual exclusion with legacy keys), merged
  load from N static dirs. Single-source behavior proven byte-identical by test.
- **P2 — git federation**: per-source Syncers, debounced merged reload, per-source
  sync metrics/logs, per-source auth.
- **P3 — loop polish**: `weight` in scoring, F6 fingerprint dedup at index time,
  retirement source-filter + readonly-decay surfacing, kbmcp `source` field, docs.

## 11. Open questions for the maintainer

1. Default `weight` for read-only/vendor sources — 1.0 (neutral, recommended) or a
   built-in haircut (e.g. 0.9) reflecting "not our incidents"?
2. F6 dedup: writable-copy-wins is deterministic — but should a NEWER vendor copy
   ever win (upstream fixed their runbook)? Recommended: no; the org copy was a
   deliberate fork, surface a "vendor copy changed" log instead.
3. Should a 👎 on a vendor entry be visible upstream somehow (issue template link in
   the notification)? Out of scope here; noting the ask.
4. Per-source namespace eligibility (`sources[].namespaces:` — a team KB only
   eligible for its namespaces): defer to demand, or is this actually the FIRST
   thing a multi-team org asks for? If the latter, it belongs in P1's config shape
   as a reserved key.
5. Bound on N (source count)? Recommended: soft-cap 8 with a startup warning;
   merged-index memory is the real limit, not the loop.
