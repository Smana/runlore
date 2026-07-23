# V3 — OKF ecosystem listing + README positioning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Claim the first-mover position as the first SRE agent that *produces* OKF entries: get RunLore listed in the OKF ecosystem, and state the claim in RunLore's own README.

**Architecture:** One external contribution (OKF ecosystem/tools listing) + one small README edit in this repo. The external part is not release-bound. External PRs from the maintainer's personal account (agent `gh` is read-only on the relevant repos).

**Tech Stack:** GitHub PRs, Markdown.

---

### Task 1: Find the right OKF listing surface and submit

**Files:**
- Modify (in fork): the OKF ecosystem/tools list source (locate first — see Step 1)

- [ ] **Step 1: Locate the canonical listing surface**

Check, in order: (a) https://okf.md/tools/ — find its source repo (footer/GitHub link); (b) `GoogleCloudPlatform/knowledge-catalog` — look for an ECOSYSTEM/ADOPTERS/tools file or a "who's using OKF" section in the README/docs. Pick whichever accepts community entries (both if both do).

- [ ] **Step 2: Draft the entry**

Adapt to the target's format; the substance:

```markdown
- [RunLore](https://github.com/Smana/runlore) — open-source SRE agent that **produces** OKF entries:
  every incident investigation is drafted as an OKF-compatible markdown entry and opened as a PR in
  a Git knowledge base the user owns; merged entries are re-indexed for instant recall, weighted by
  real-world resolve rate. (Producer + consumer.)
```

- [ ] **Step 3: Open the PR** with the author disclosure, and record the URL on the V3 roadmap issue.

### Task 2: State the claim in RunLore's README

**Files:**
- Modify: `README.md` (the "Why RunLore" section, the OKF-linking sentence around line 226)

- [ ] **Step 1: Sharpen the OKF sentence**

Replace (in the paragraph starting "The wedge is the **combination**…"):

```markdown
catalog you own ([OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog)-compatible markdown,
not a proprietary store), from an agent that's **honest about the sub-50% reality**:
```

with:

```markdown
catalog you own — [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog)-compatible
markdown, not a proprietary store; as far as we know RunLore is the **first agent that *produces*
OKF entries from its own investigations** — from an agent that's **honest about the sub-50% reality**:
```

- [ ] **Step 2: Proofread and commit**

```bash
git add README.md
git commit -m "docs(readme): claim first-mover OKF-producing agent positioning"
```

## Acceptance criteria

- [ ] RunLore appears (PR open or merged) on at least one OKF ecosystem/tools listing, described as a producer.
- [ ] README states the OKF-producer claim, hedged with "as far as we know" (honesty posture).
- [ ] Claim is re-verified against the OKF ecosystem list at PR time (if another agent-producer appeared meanwhile, soften to "among the first").
