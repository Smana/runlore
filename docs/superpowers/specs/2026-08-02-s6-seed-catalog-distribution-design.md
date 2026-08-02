# Design — S6: Commons seed catalog + distribution artifacts

- **Date:** 2026-08-02
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Source:** improvement report §6 (the strategic product bet) and §7 (distribution)
- **Index:** [decomposition](2026-08-02-improvement-report-decomposition.md)

## Problem

**The cold start.** RunLore's differentiator only pays off *after* a team has had several incidents
and merged several PRs. On day one the catalog is empty, `kb_search` returns nothing, and RunLore is
a slower, less-integrated HolmesGPT. That is a brutal cold start for the exact feature the project
sells.

**A contribution surface that requires Go.** The repo is ~98% Go with 3 contributors. Markdown
runbooks are contributable by any SRE who reads the blog; Go internals are not.

**A moat only this architecture can have.** Komodor, Cleric, NeuBird and Datadog keep learned
knowledge closed and non-exportable — by construction they cannot ship a community catalog. Aurora's
RAG store is locked in Postgres/Weaviate/Memgraph. Only portable markdown-in-Git enables a commons.

**Distribution is cheap and entirely unspent.** RunLore is absent from `last9/awesome-sre-agents`,
has no Artifact Hub listing despite publishing an OCI chart, and no CNCF landscape entry.

### A correction to the report's premise

The report claims a commons catalog *"eliminates the cold start — day-1 instant recall value, which
is currently zero."* **That is not true as the engine stands.** Instant recall applies a structural
filter: a resource-less entry cannot match a workload-carrying incident
(`internal/investigate/recall_test.go:427` — *"scopeless request vs named entry → none"*;
`nearmiss_gate_test.go:131`). Generic commons entries carry no `resource` by nature, and real alerts
almost always carry a namespace and workload. So commons entries will **not** fire instant recall.

What they *will* do is ground investigations: `kb_search`
(`internal/investigate/kbsearch_tool.go`) applies **no** structural filter, so the model can find and
cite any commons entry mid-loop. That is real, day-one value — a better-reasoned investigation with
citable prior art — and it is what this spec claims. Instant recall stays earned by *your own*
scoped entries, which is the correct trust boundary anyway.

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Where the entries are authored | **In this repo first**, then split to a standalone repo | Owner's choice. One reviewable PR for the content, then the repo once the shape settles |
| Standalone repo | `Smana/runlore-kb-commons`, created via `gh` | The report's contribution-surface argument only works with a repo whose barrier to entry is a markdown PR |
| Value claim | "grounds investigations from day one", **not** "day-1 instant recall" | See the correction above. Claiming recall value we cannot deliver would be the exact dishonesty this project campaigns against |
| Chart seeding | Two PRs: content + `lore kb import` path first; `catalog.commons` git source second | The first needs no engine change and delivers value immediately; the second needs multi-root indexing (`catalog.New` takes one dir today) |
| Precedence | A user's own entry always outranks a commons entry on a tie | Your platform's truth beats generic advice, always |
| Curation writes | Never to the commons dir | The curator writes to the KB repo you own; commons is read-only input |
| Distribution PRs | **Prepared, not submitted** | Owner's choice — they are public actions under the owner's name |

## Scope

### In scope

1. 15–25 commons OKF entries authored in-repo under `examples/kb-commons/`, CI-validated.
2. `Smana/runlore-kb-commons` created and seeded from the settled content, with `CONTRIBUTING.md`
   and a validation workflow.
3. `catalog.commons` — an optional second catalog source, indexed alongside the user's, with
   commons-loses-ties precedence.
4. Chart wiring so the commons can be enabled with one value.
5. Distribution artifacts staged in `dev/distribution/`: the `awesome-sre-agents` entry, the
   `cncf/landscape` YAML block, the Artifact Hub listing file, each with submission instructions.

### Non-goals (YAGNI)

- Submitting the distribution PRs (owner does this).
- A curation path that writes *back* to the commons repo — human PRs only, by design.
- Changing the instant-recall structural filter to admit resource-less entries. It is a real
  question, but it trades precision for coverage and must be settled with eval evidence, in its own
  spec, with the poisoned-entry scenario as the gate. **Flagged as a follow-up.**
- Vector/embedding distribution of the commons — plain markdown indexed locally.

## Design

### The entries

Generic, vendor-neutral failure modes an SRE meets everywhere, `type: Playbook`, no `resource`
(they are platform-wide by construction), each with real `Symptom` / `Checks` / `Cause` /
`Resolution` sections and alert-name tags so retrieval can find them from an alert:

| Area | Entries |
|---|---|
| GitOps | HelmRelease upgrade failure · HelmRelease terminal state · Kustomization build failure · Argo CD `Application` Degraded · Argo CD OutOfSync stuck |
| Workloads | CrashLoopBackOff after deploy · OOMKilled · readiness-probe failure · image pull backoff · init-container failure |
| Storage | PVC unbound · volume node-affinity conflict · disk pressure eviction |
| Networking | DNS resolution failure · NetworkPolicy denial · Service has no endpoints |
| Certificates | cert-manager order/challenge stuck · expiring certificate |
| Nodes | node NotReady · memory pressure · scheduler `Unschedulable` |

Tags carry the alert names that fire for each (`KubePodCrashLooping`, `KubePersistentVolumeFillingUp`,
…) — `data-sources.md` and `lore kb import` both establish that alert-name tokens are the recall
signal that lets an alert find a runbook.

Quality bar: every entry must pass `lore validate-kb`, cite no vendor-specific paths, and state
what it does **not** cover. An entry that would make a wrong diagnosis plausible is worse than no
entry — the poisoned-entry eval scenario exists precisely because of this.

### `catalog.commons`

```yaml
catalog:
  dir: /var/lib/runlore/catalog          # your KB (unchanged)
  commons:
    url: https://github.com/Smana/runlore-kb-commons
    branch: main
    interval: 24h                         # commons changes slowly
    dir: /var/lib/runlore/commons         # separate mount — never mixed with your mirror
```

Kept in a **separate directory** from the git-sync mirror so it can never dirty the user's checkout.
`catalog.New` takes a single dir today, so this adds multi-root loading: entries are loaded from both
roots and carry a provenance flag. Precedence rule: on equal score, the user's entry wins; commons
entries are visually marked as commons in `kb_search` results and in any notification that cites
them, so an on-call always knows whether they are reading their platform's truth or generic advice.
The curator's write path is unchanged and never targets the commons root.

Chart: `catalog.commons.enabled` renders the extra volume + the config block. Off by default —
adopting a shared catalog is a choice, not something that happens to you on upgrade.

### The standalone repo

`Smana/runlore-kb-commons`, public, Apache-2.0, containing the entries, an `index.md` human listing,
`CONTRIBUTING.md` (what a good entry looks like, the OKF field contract, the "no vendor specifics"
rule), and a CI workflow running `lore validate-kb` on every PR. The `kb-steward` skill already
exists and applies directly — the repo's contribution guide points at it.

RunLore's README and `/docs/integrations/` gain a link, and Getting Started's step 1b mentions it as
an alternative to starting empty.

### Distribution artifacts

`dev/distribution/` holds one file per target, each containing the exact content to submit and where
to submit it: the `awesome-sre-agents` list entry, the `cncf/landscape` `landscape.yml` block (with
the required logo asset prepared), and the Artifact Hub repository metadata for the existing GHCR
OCI chart. Each names the upstream contribution rules it must satisfy.

## Testing

| Test | Guards |
|---|---|
| `lore validate-kb examples/kb-commons` in CI | every entry passes the same merge gate as a curated PR |
| `TestCommonsEntriesHaveAlertTags` | each entry carries at least one alert-name tag, so retrieval can reach it |
| `TestCommonsPrecedence` | a user entry and a commons entry at equal score resolve in the user's favour |
| `TestCommonsNeverWritten` | the curator's write path cannot target the commons root |
| Manual | with commons enabled and an empty user KB, an investigation cites a commons entry; with a competing user entry, it cites the user's |

## Risks

| Risk | Mitigation |
|---|---|
| Generic entries mislead an investigation | Every entry states its limits; the adversarial verify pass and the poisoned-entry scenario already guard this path; commons entries are visibly marked |
| A commons entry contradicts a team's platform | User entries win ties and are marked distinctly; the whole feature is opt-in |
| The commons repo attracts no contributors and looks dead | 20 solid seeded entries are useful on their own; the repo's value does not depend on inbound PRs |
| Splitting the content into a second repo doubles maintenance | The in-repo copy is a **seed, not a fork**. Sequence: PR1 authors the entries under `examples/kb-commons/` with CI validation; PR2 creates and seeds `runlore-kb-commons`, moves validation to that repo's CI, and deletes `examples/kb-commons/` — leaving exactly one source of truth |
| Scope creep into changing recall's structural filter | Explicitly out of scope and flagged as its own spec, gated on eval evidence |

## Acceptance criteria

1. 15–25 entries exist, every one passing `lore validate-kb` in CI, each carrying alert-name tags —
   under `examples/kb-commons/` after PR1, in the standalone repo after PR2.
2. `Smana/runlore-kb-commons` exists, public, seeded, with a contribution guide and validation CI;
   `examples/kb-commons/` is gone and nothing references it.
3. `catalog.commons` indexes the commons alongside a user catalog; user entries win ties; the
   curator can never write there; the feature is off by default.
4. An investigation against an **empty** user KB visibly cites a commons entry — demonstrated
   manually and covered by a test.
5. Docs claim "grounds investigations from day one" and explicitly state that instant recall stays
   earned by your own scoped entries.
6. `dev/distribution/` contains submission-ready artifacts for awesome-sre-agents, the CNCF
   landscape, and Artifact Hub — unsubmitted.
</content>
