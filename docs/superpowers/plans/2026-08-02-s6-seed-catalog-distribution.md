# S6 — Commons seed catalog + distribution artifacts — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Kill the cold start. On day one a new adopter's catalog is empty, `kb_search` returns nothing, and RunLore is a slower HolmesGPT — for exactly the feature it is sold on.

**Architecture:** A set of generic, vendor-neutral OKF entries authored in-repo first as one reviewable PR, then split into a standalone `runlore-kb-commons` repository whose contribution barrier is a markdown pull request. An opt-in `catalog.commons` second source indexes it alongside the user's own catalog, with the user's entries always winning ties and the curator never writing to it.

**Spec:** [`docs/superpowers/specs/2026-08-02-s6-seed-catalog-distribution-design.md`](../specs/2026-08-02-s6-seed-catalog-distribution-design.md)

## The claim this plan makes — and the one it refuses to make

The improvement report says a commons catalog *"eliminates the cold start — day-1 instant recall value."* **That is false as the engine stands, and this plan does not repeat it.**

Instant recall applies a structural filter: a resource-less entry cannot match a workload-carrying incident (`internal/investigate/recall_test.go:427` — *"scopeless request vs named entry → none"*; `nearmiss_gate_test.go:131`). Generic commons entries carry no `resource` by nature, and real alerts almost always carry a namespace and workload. **Commons entries will not fire instant recall.**

What they *will* do is ground investigations: `kb_search` (`internal/investigate/kbsearch_tool.go`) applies **no** structural filter, so the model can find and cite any commons entry mid-loop. That is real day-one value — a better-reasoned investigation with citable prior art — and it is what this plan claims.

Instant recall stays earned by *your own* scoped entries. That is the correct trust boundary anyway, and every doc line this plan writes must say so.

## Global Constraints

- Every entry passes `lore validate-kb` — the same merge gate a curated PR faces.
- **No entry may make a wrong diagnosis plausible.** The poisoned-entry eval scenario exists precisely because a bad entry is worse than a missing one. Every entry states what it does *not* cover.
- No vendor-specific paths, cluster names, or org names.
- Commons entries are visibly marked as commons wherever they surface, so an on-call always knows whether they are reading their platform's truth or generic advice.
- SPDX header on new `.go` files; `golangci-lint` clean; no new third-party dependencies.
- Conventional Commits. **Never** add a co-author trailer or any AI attribution.

---

### Task 1: Author the commons entries

**Files:** create `examples/kb-commons/*.md` (15–25 entries) + `examples/kb-commons/index.md`
**Also:** a CI step running `lore validate-kb examples/kb-commons`

- [ ] **Step 1: Read the format contract first**

`plugins/kb-steward/skills/kb-steward/references/okf-format.md` is the field-by-field contract, and `examples/runbooks/helmrelease-upgrade-failure.md` is a worked example. Read both before writing an entry.

- [ ] **Step 2: Write the entries**

`type: Playbook`, no `resource` (they are platform-wide by construction), each with real `Symptom` / `Checks` / `Cause` / `Resolution` sections.

| Area | Entries |
|---|---|
| GitOps | HelmRelease upgrade failure · HelmRelease terminal state · Kustomization build failure · Argo CD `Application` Degraded · Argo CD OutOfSync stuck |
| Workloads | CrashLoopBackOff after deploy · OOMKilled · readiness-probe failure · image pull backoff · init-container failure |
| Storage | PVC unbound · volume node-affinity conflict · disk-pressure eviction |
| Networking | DNS resolution failure · NetworkPolicy denial · Service has no endpoints |
| Certificates | cert-manager order/challenge stuck · expiring certificate |
| Nodes | node NotReady · memory pressure · scheduler `Unschedulable` |

**Tags carry the alert names that fire for each** (`KubePodCrashLooping`, `KubePersistentVolumeFillingUp`, …). `lore kb import` and `data-sources.md` both establish that alert-name tokens are the recall signal that lets an alert find a runbook — an entry without them is much harder to retrieve.

- [ ] **Step 3: Validate and add the CI gate**

```bash
go run ./cmd/lore validate-kb examples/kb-commons
```
Add the same command to `.github/workflows/ci.yaml` so an entry that would fail the merge gate cannot land.

- [ ] **Step 4: Commit**

---

### Task 2: `catalog.commons` — a second, read-only catalog source

**Files:**
- Modify: `internal/catalog/catalog.go`, `load.go` (multi-root loading + provenance)
- Modify: `internal/config/config.go` (the `commons` block)
- Modify: `internal/app/catalog.go` (wiring)
- Modify: `deploy/helm/runlore/values.yaml` + templates
- Create: `website/content/docs/integrations/kb-commons.md`

**Interfaces:**
- Consumes: `catalog.New(dir)` (single-root today), `catalog.Load(dir)`, `catalog.Entry`.
- Produces: `catalog.Entry.Commons bool`; a `catalog.commons` config block.

- [ ] **Step 1: Write the failing tests**

Three properties, each a test:
1. **Both roots are indexed** — an entry from either is findable by `kb_search`.
2. **The user's entry wins a tie** — with two entries at equal score, the non-commons one ranks first. This is the important one: your platform's truth beats generic advice, always.
3. **The curator can never write to the commons root** — assert the write path rejects a commons-rooted target.

- [ ] **Step 2: Implement**

`catalog.New` takes one dir today. Add multi-root loading with a provenance flag on each entry. Keep the commons in a **separate directory** from the git-sync mirror so it can never dirty the user's checkout:

```yaml
catalog:
  dir: /var/lib/runlore/catalog          # your KB, unchanged
  commons:
    url: https://github.com/Smana/runlore-kb-commons
    branch: main
    interval: 24h                         # commons changes slowly
    dir: /var/lib/runlore/commons         # separate mount
```

Off by default — adopting a shared catalog is a choice, not something that happens on upgrade.

- [ ] **Step 3: Mark commons entries where they surface**

`kb_search` results and any notification citing one must show it is commons, not the user's own. An on-call must never mistake generic advice for their platform's recorded truth.

- [ ] **Step 4: Chart wiring, docs page, tests, commit**

The docs page must state plainly that commons entries **ground investigations via `kb_search`** and **do not fire instant recall**, and why (the structural filter). Do not let this page repeat the report's overclaim.

---

### Task 3: The standalone repository

> **Requires the maintainer** for repo creation, or explicit delegation.

- [ ] **Step 1: Create `Smana/runlore-kb-commons`** — public, Apache-2.0.
- [ ] **Step 2: Seed it** from `examples/kb-commons/`, preserving history where practical.
- [ ] **Step 3: Add `CONTRIBUTING.md`** — what a good entry looks like, the OKF field contract, the no-vendor-specifics rule, and a pointer to the `kb-steward` skill, which already exists and applies directly.
- [ ] **Step 4: Add validation CI** running `lore validate-kb` on every PR.
- [ ] **Step 5: Delete `examples/kb-commons/`** from this repo and repoint the CI gate, README and Getting Started at the new repo. **One source of truth** — the in-repo copy was a seed, not a fork.

---

### Task 4: Distribution artifacts — prepared, not submitted

**Files:** create `dev/distribution/{awesome-sre-agents,cncf-landscape,artifact-hub}.md`

- [ ] **Step 1: Write each artifact**

Exact submission-ready content plus where it goes and the upstream contribution rules it must satisfy:
- the `last9/awesome-sre-agents` list entry;
- the `cncf/landscape` `landscape.yml` block, with the required logo asset prepared;
- the Artifact Hub repository metadata for the existing GHCR OCI chart.

- [ ] **Step 2: Do NOT submit them.** These are public actions under the maintainer's name. Stage them and stop.

---

## Final verification

- [ ] 15–25 entries, every one passing `lore validate-kb` in CI, each carrying alert-name tags
- [ ] An investigation against an **empty** user KB visibly cites a commons entry — demonstrated, not assumed
- [ ] A user entry beats a commons entry at equal score — proven by test
- [ ] The curator cannot write to the commons root — proven by test
- [ ] `catalog.commons` is off by default
- [ ] Docs claim "grounds investigations from day one" and explicitly state that instant recall stays earned by your own scoped entries
- [ ] `dev/distribution/` holds submission-ready artifacts, unsubmitted
</content>
