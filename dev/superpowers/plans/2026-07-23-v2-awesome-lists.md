# V2 — Awesome-list listings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Get RunLore listed on the two category awesome-lists it is currently absent from: `last9/awesome-sre-agents` and `agamm/awesome-ai-sre`.

**Architecture:** Two small PRs on external repos. Not release-bound — can go out immediately. Forks and PRs must come from the maintainer's **personal** GitHub account (the agent's `gh` is a work account with read-only access to Smana repos; use `gh auth switch` or the web UI).

**Tech Stack:** GitHub forks/PRs, Markdown.

---

### Task 1: PR to last9/awesome-sre-agents

**Files:**
- Modify (in fork): `README.md` of `last9/awesome-sre-agents`

- [ ] **Step 1: Check the list's entry format and section layout**

Open https://github.com/last9/awesome-sre-agents and note: which section fits (open-source agents), the exact entry format (link + one-line description, alphabetical or append), and the CONTRIBUTING rules if present.

- [ ] **Step 2: Fork, branch, add the entry**

Draft entry (adapt punctuation/casing to the list's house style, keep under one line):

```markdown
- [RunLore](https://github.com/Smana/runlore) - Open-source, self-hosted SRE agent that investigates Kubernetes incidents (GitOps-exact "what changed" diffs for Flux/Argo CD) and writes what it learns as PR-reviewed markdown into a Git knowledge base you own, with outcome-weighted instant recall. Read-only by default, model-agnostic.
```

- [ ] **Step 3: Open the PR**

PR title: `Add RunLore`. PR body (one paragraph):

```text
Adds RunLore — an Apache-2.0, self-hosted SRE agent (single Go binary, runs in-cluster).
Distinguishing traits vs the agents already listed: it learns from its own investigations into a
PR-reviewed, user-owned Git knowledge base (OKF-compatible markdown), weights recall by real-world
resolve rate, and anchors RCA on GitOps-revision-exact diffs (Flux + Argo CD). Ships a public eval
harness. Disclosure: I'm the author.
```

- [ ] **Step 4: Record the PR URL in the roadmap issue** (tracking only; merge is not in our control)

### Task 2: PR to agamm/awesome-ai-sre

**Files:**
- Modify (in fork): `README.md` of `agamm/awesome-ai-sre`

- [ ] **Step 1: Check the list's entry format and section layout**

Open https://github.com/agamm/awesome-ai-sre — same checks as Task 1 Step 1. This list separates open-source vs commercial; RunLore goes in open-source.

- [ ] **Step 2: Fork, branch, add the entry**

Same draft entry as Task 1 Step 2, adapted to this list's style.

- [ ] **Step 3: Open the PR** — same title/body pattern as Task 1 Step 3 (keep the author disclosure).

- [ ] **Step 4: Record the PR URL in the roadmap issue**

## Acceptance criteria

- [ ] One PR open (or merged) on each list, entry accurate and in house style, author disclosure included.
- [ ] PR URLs recorded on the V2 roadmap issue.
- [ ] If a list maintainer requests changes, follow up within a week (these PRs are cheap goodwill).
