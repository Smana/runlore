# V4 — README: surface the hidden strengths Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make three currently code-only differentiators visible to a first-time README reader: the recall trust gates (outcome decay + margin gate + verify-on-recall), and the MCP server/client. (`source_diff` is already surfaced at README lines 40–41 — out of scope here.)

**Architecture:** Docs-only, one file, one commit. Additions are deliberately compact — the README is already long; each addition earns its place by being a differentiator no competitor can claim.

**Tech Stack:** Markdown.

---

### Task 1: Add "why a recall can be trusted" to the learning-loop section

**Files:**
- Modify: `README.md` (learning-loop section, after the paragraph ending "…knowledge that keeps failing decays." around line 107)

- [ ] **Step 1: Insert the trust-gates paragraph**

Insert after "…knowledge that keeps failing decays.":

```markdown
An instant recall is never a blind cache hit — three gates stand in front of it: the entry must
**structurally match** the incident (same workload/resource, retrieval score above a floor), it must
**win by a clear margin** over the runner-up entry (ambiguous matches fall through to a full
investigation), and its confidence is **weighted by its real-world track record** — an entry that
keeps resolving incidents gains trust, one that keeps failing decays toward re-investigation.
Even then, the recalled finding goes through the same adversarial verify pass as a fresh one; the
shipped eval suite includes a poisoned-entry scenario proving a bad entry is rejected at recall time.
```

- [ ] **Step 2: Render-check** — preview the README; the paragraph sits between the decay sentence and the "→ How the learning loop works" link line.

### Task 2: Add MCP to the integrations table

**Files:**
- Modify: `README.md` (integrations table, lines ~145–156)

- [ ] **Step 1: Add the row** (after the "Knowledge base (git forge)" row):

```markdown
| **MCP** | Server — query your KB from Claude Code / any MCP client · Client — wire external MCP tool servers into investigations (allowlist-gated) | `mcp.*` |
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs(readme): surface recall trust gates and MCP server/client"
```

## Acceptance criteria

- [ ] A README reader learns the three recall gates and the verify-on-recall behavior without reading code.
- [ ] MCP server + client appear in the integrations table with the `mcp.*` config pointer.
- [ ] No duplication with docs/learning-loop.md phrasing (README stays the summary; the doc keeps the detail).
