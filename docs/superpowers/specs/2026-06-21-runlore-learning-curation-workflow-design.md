# RunLore Learning / Curation Workflow — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-21 |
| **Scope** | Make RunLore's learning loop actually *compound* the knowledge catalog: dedup + a real merge gate + a curator agent |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | `docs/design.md` §6 "Learn" (the intended loop, largely unenforced); `internal/curator`, `internal/catalog`, `internal/forge/github`; the eval-harness initiative (`2026-06-21-runlore-eval-harness-design.md`, workstream A) |

---

## 1. Why this exists — the loop produces but never compounds

Observed on the live `runlore-kb` repo (2026-06-21):

- **0 of ~12 KB PRs were ever merged** — every closed one was closed *unmerged*. The catalog's only entry is the hand-authored **seed** playbook (`helmrelease-upgrade-failure.md`); the "Incidents" section is still empty, and `log.md` has no curation entries.
- After dozens of investigations, the catalog has grown by **zero** learned entries.
- **Heavy duplication**: PRs #12/#20/#22/#27/#29 are all the *same* "Kustomization DependencyNotReady / missing GitRepository" incident; issues #40/#41 both "NodeTerminatedCheckAWS"; #37/#39 both "Harbor install failing."
- **7 issues** sit open, all `triggered`, **0 promoted, 0 resolved** — a second pile-up queue.

Root problems (user-confirmed): **(1) no process/criteria** for what's worth keeping or when to merge; **(2) too much noise/duplication** burying the signal; **(3) draft quality** too low to keep. (The *unit* of knowledge — a per-incident entry — is fine; PR #48 "HarborRegistryDown" is a genuinely mergeable example and is the **quality anchor** for this design.)

This design makes the **curation gate actually work** so the catalog compounds, while keeping the human as the (now trivial) merge approver.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **Human stays the merge approver**, but the system delivers a short, deduped, high-quality, **decision-ready** queue | RunLore's design treats human merge review as the load-bearing quality gate; the fix is to make that decision trivial, not to automate it away. |
| D2 | **Drop the per-investigation issue.** Uncertain findings → **chat alert only**, no KB-repo artifact | Issues never enter recall (only merged entries do); per-finding issues are a redundant pile-up nobody works (7 open, 0 promoted). |
| D3 | **The only issues that exist** are rare **knowledge-gap** issues opened when a pattern *recurs unresolved* | The one genuinely useful signal an issue carries: "RunLore is blind here" → author seeded knowledge or fix RunLore. Requires the cross-incident view (Phase 2). |
| D4 | **Both layers, sequenced**: Phase 1 file-time gate (stop new noise) → Phase 2 `lore curate` agent (groom backlog + ongoing) | File-time stops the flood cheaply; only a groomer can clean the standing backlog and do cross-incident dedup/upgrade/recurrence. |
| D5 | **Per-incident entry is an acceptable unit** to merge | User explicitly did not want pattern-synthesis as a precondition; #48-style incident entries are mergeable as-is. |
| D6 | **Curator agent writes to the forge only — never auto-merges, never auto-closes human-touched artifacts** | The curator is itself a fallible LLM; the human gate and reversibility are the backstop. |
| D7 | **Quality anchor = PR #48** (HarborRegistryDown): correct root cause + evidence + concrete fix | A concrete, agreed standard for "mergeable," not an abstract bar. |

## 3. The curation policy & lifecycle (the core)

Every investigation finding flows through this state machine. It *operationalizes* the labels RunLore already applies (`triggered`/`investigating`/`solved`), which today are decorative.

```
investigation finding
   ├─ DUPLICATE (matches an open PR or a catalog entry)
   │     → coalesce: comment "seen again @T, investigation <link>" + bump; file NOTHING new
   │
   ├─ NOVEL + confident + passes the MERGE BAR
   │     → draft PR (OKF entry), labels: runlore, solved, ready-to-merge
   │     → YOU approve → merged (catalog grows, log.md appended)
   │     → YOU reject  → closed: wont-fix / rejected
   │
   └─ uncertain / below the bar
         → chat alert ONLY (Slack/Matrix) — NO KB-repo artifact

curator agent (Phase 2) detects a RECURRING unresolved pattern (N× same fingerprint, never solved)
   → opens ONE knowledge-gap issue, labels: runlore, knowledge-gap
   → you: author seeded knowledge / fix RunLore / close wont-fix
```

**Label semantics (load-bearing, not cosmetic):**

| Label | Meaning |
|---|---|
| `triggered` | filed, untriaged |
| `investigating` | being worked (reinvestigate running or human engaged) |
| `solved` | root cause **confirmed + resolution captured** — the *only* state eligible to merge |
| `ready-to-merge` | `solved` + passed the bar + deduped → in the decision-ready queue |
| `duplicate` / `wont-fix` / `stale` | terminal-closed |
| `knowledge-gap` | a recurring pattern RunLore can't resolve (the only issue type) |

**The merge bar** (what makes an entry mergeable — the #48 standard, explicit):
1. `solved` — root cause confirmed, not symptom-only (`must_reach_root`);
2. passed the adversarial **verify** pass (not correlation-only);
3. cites the **causing change** and the **fixing change / resolution** (provenance);
4. **novel** — not a duplicate of catalog or open artifacts;
5. well-formed OKF — frontmatter + **Symptom / Investigate / Cause / Resolution** sections.

**Duplicate identity.** "Same incident" = a fingerprint of `alertname + namespace + workload + root-cause signature`, matched via the existing bleve `kb_search` over the catalog **and** a forge search over open KB PRs. Near-matches **coalesce** rather than re-file — this is what kills the 5×"DependencyNotReady" problem.

**Decay.** Merged entries carry `status` (draft/verified), `confidence`, `last_validated`. The curator flags stale entries for re-validation or down-weights them in recall, so wrong or aging knowledge doesn't silently poison future investigations.

**Reinvestigate.** The existing `reinvestigate` outbound-poll (`internal/investigate/reinvestigate.go`), today tied to per-finding issues, is **re-pointed at `knowledge-gap` issues**: a human adds context to a gap issue and labels it `reinvestigate` → RunLore re-runs with that context → if it now reaches `solved`, the finding **promotes to a PR** (and the gap issue closes). It no longer fires on per-finding issues (those no longer exist).

**Thresholds** (`recurrence count N`, `stale window`, `novelty match score`) are **configurable** with defaults set at implementation time — named here as tunables, not left undefined.

## 4. Phase 1 — the file-time gate (in the existing curator)

Enhance `internal/curator/curator.go` (today: `confidence ≥ 0.75 → OpenPR, else OpenIssue`) into a three-step gate. **The `else → OpenIssue` branch is deleted.**

1. **Novelty / dedup check** — fingerprint the finding; query (a) bleve `kb_search` over the catalog and (b) a **new** forge "list open KB PRs" call. Above the match threshold → **coalesce** (comment + bump the canonical artifact) and stop; file nothing new.
2. **Quality gate** — only findings meeting the **merge bar** (§3) get drafted as a PR. Everything below the bar → **chat alert only, no repo artifact**.
3. **Merge-ready PR body** — the drafted PR leads with a **decision card**: one-line *why-keep*, confidence, root cause, causing change → fixing change, evidence trail, then the OKF entry diff. Labels `runlore, solved, ready-to-merge`.

**What this needs (implementation surface):**
- a `fingerprint(finding)` + novelty scorer (reuses the catalog index; adds a small threshold);
- one **new method on the GitHub forge client** to list/search open KB PRs (today it can only *open* them — `internal/forge/github/github.go`);
- the OKF-entry drafting upgraded from today's thin "## Summary / Root causes" dump to the full **Symptom/Investigate/Cause/Resolution** shape (the #48 standard);
- the decision-card PR-body template.

This is the cheap, immediate-relief layer: after it ships, what reaches the human is novel and merge-ready — never again a wall of duplicate low-quality PRs.

## 5. Phase 2 — the `lore curate` agent (the groomer)

A new `lore curate` mode with the **cross-incident view** the file-time gate structurally lacks. Five jobs:

1. **Backlog dedup** — cluster existing open PRs (and legacy issues) by fingerprint; close duplicates with a reversible back-ref to the canonical one. (Immediately cleans today's mess: keep 1 "DependencyNotReady", close 4 with a pointer.)
2. **Draft upgrade** — take promising-but-thin PRs and re-shape toward the #48 bar (re-investigate if needed, rewrite into full OKF). Sub-bar drafts that can't be lifted → close `wont-fix`.
3. **Recurrence → knowledge-gap issues** — track unresolved findings across incidents; when a fingerprint recurs N× never-solved, open **one** `knowledge-gap` issue. This is the only path that creates issues.
4. **Decision-ready queue** — apply `ready-to-merge` to PRs that pass the bar, deduped and ranked by value — the single surface the human approves from.
5. **Lifecycle + decay** — advance labels, close stale (no progress in N days), flag aging catalog entries for re-validation / recall down-weighting (`last_validated`).

**Mechanism.** A `lore curate` subcommand operating over the KB git repo + forge API + catalog index. Two run modes:
- **on-demand**: `lore curate` (human or CI);
- **scheduled**: in-cluster CronJob (or a serve-loop timer) on a cadence.

**Write boundary (guardrails — the curator is a fallible LLM):**
- Writes to the **forge only** (comments, labels, close-dups, gap issues, draft-branch commits). **Never auto-merges. Never auto-closes a `ready-to-merge` or any human-labeled/human-touched artifact.**
- Dedup-close requires a **high** match threshold + leaves a reversible back-ref (reopen is one click).
- Draft upgrades are commits to the PR branch, not merges.
- Every curator action goes through the existing append-only **audit log** (`internal/audit`).

## 6. Guardrails, observability, and tie to the eval harness

**KB-poisoning defense.** Frontier RCA is <50% accurate (ITBench, `docs/design.md` §10), so a wrong merged entry is a real risk. Defenses: the merge bar requires the verify pass; the human approves every merge; entries carry `status`/`confidence`/`last_validated`; the verify pass treats recalled catalog content as untrusted; decay down-weights stale entries.

**Observability (so we can tell if learning is actually improving).** Track and expose:
- **catalog growth** (learned entries merged over time — today: 0);
- **merge rate** (drafted PRs that merge vs close-unmerged);
- **dedup rate** (coalesced/closed-dup vs total findings);
- **recall hit rate** (investigations short-circuited or grounded by a learned entry);
- **time-to-merge** for `ready-to-merge` PRs.

**Tie to the eval harness (workstream A).** The eval harness's **scenario 9 (instant-recall)** is the closed-loop validation: once a curated entry for an incident exists, re-firing that incident's symptom should short-circuit via `kb_search` instead of re-investigating. That scenario goes from SKIP → PASS exactly when this curation workflow produces its first mergeable, recall-able entry — so workstream A *measures* whether workstream B is working.

## 7. Phasing

| Phase | Scope | Deliverable |
|---|---|---|
| **Phase 1** (file-time gate) | dedup + quality gate + merge-ready PR body in the curator; delete the issue branch; forge "list open PRs" method; OKF drafting upgrade | fewer, novel, merge-ready PRs; the decision card |
| **Phase 2** (`lore curate` agent) | backlog dedup, draft upgrade, recurrence→gap-issue, decision-ready queue, lifecycle/decay; on-demand + scheduled | the groomer; a clean backlog and a single approve-from surface |

Each phase is its own implementation plan (à la the eval-harness engine/catalog split).

## 8. Out of scope

- **Auto-merge** of any kind (human stays the gate, D1).
- **Cross-incident pattern *synthesis*** into higher-order playbooks (per-incident entries are fine, D5) — a possible later phase, not now.
- Vector/embedding retrieval (the catalog is BM25 today; orthogonal).
- GitLab forge support (GitHub only, as today).
- The autonomy ladder / cluster-mutating remediation (separate track).

## 9. Success criteria

- The catalog **grows**: learned entries get merged (from a baseline of 0), and `log.md` records them.
- A human approving the queue sees **few, deduped, merge-ready** candidates, each with a decision card — no duplicate walls.
- Duplicate findings **coalesce** instead of re-filing (the 5×"DependencyNotReady" case produces 1 artifact, not 5).
- The **only** issues that appear are rare `knowledge-gap` issues for genuinely recurring blind spots.
- The curator agent never merges and never closes a human-touched artifact; every action is audited and reversible.
- Eval scenario 9 (instant-recall) flips SKIP → PASS once the first relevant entry is merged — proving the loop compounds.
