# V1 — Prior-art refresh (July 2026 landscape) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Update `docs/prior-art.md` so its competitive claims match the July-2026 landscape: Komodor's Klaudia Memory launch, Aurora's real feature set, the Tracer-Cloud "opensre" name collision, Datadog's pricing collapse, and the ITBench-AA numbers.

**Architecture:** Docs-only. One file modified, edits grouped into one commit. Every claim below was verified against primary sources during the 2026-07-23 competitive analysis; source URLs are embedded in the text so future refreshes can re-verify.

**Tech Stack:** Markdown.

---

### Task 1: Update the open-source table (Aurora, OpenSRE collision)

**Files:**
- Modify: `docs/prior-art.md` (OSS table, lines ~11–18)

- [ ] **Step 1: Replace the Aurora row**

Replace the current one-line Aurora row:

```markdown
| **Aurora** (Arvo AI) | Hybrid RAG over docs; auto-generates postmortems. | Postmortems, not runbook self-update. | The RAG angle. We borrow hybrid retrieval. |
```

with:

```markdown
| [**Aurora**](https://github.com/Arvo-AI/aurora) (Arvo AI, Apache-2.0) | LangGraph agents running kubectl/aws/az/gcloud in sandboxed pods; **deployment diffs + Terraform/IaC analysis** as RCA input; RAG knowledge base "that grows over time" (Postgres + Weaviate + Memgraph); "Actions": auto-postmortems, **fix PRs**, Slack. Aggressive comparison-marketing vs Holmes/k8sgpt. | **Yes** — auto-ingested RAG KB. But locked in its databases: no review gate, no git export, no outcome signal. | **The fastest-moving OSS threat.** It has diffs, a KB, and PR machinery — combining them into knowledge-PRs is one feature away. Our structural answer: user-owned markdown, human review, outcome weighting. |
```

- [ ] **Step 2: Add the name-collision note to the OpenSRE row**

Append to the end of the OpenSRE row's last cell (after "…rejected at recall time."):

```markdown
*(Naming hazard: the unrelated [Tracer-Cloud "opensre"](https://github.com/tracer-cloud/opensre) framework (~9k stars) absorbs most of the "OpenSRE" search traffic — don't confuse the two, and expect evaluators to.)*
```

- [ ] **Step 3: Render-check the table**

Run: `grep -c '^|' docs/prior-art.md` and preview the file (any markdown previewer). Expected: table still renders, same column count per row.

### Task 2: Update the change-aware and commercial sections (Klaudia Memory, Datadog pricing)

**Files:**
- Modify: `docs/prior-art.md` (sections "Change-aware RCA" and "Commercial", lines ~22–52)

- [ ] **Step 1: Replace the Komodor bullet**

Replace:

```markdown
- **Komodor** — workload-scoped **manifest-diff RCA** with Argo CD/Flux integration; Gartner 2026
  "Representative Vendor". The closest commercial analogue to our diff spine.
```

with:

```markdown
- **Komodor** — workload-scoped **manifest-diff RCA** with Argo CD/Flux integration (the only
  commercial vendor with explicit Flux support); Gartner 2026 "Representative Vendor". As of
  [**Klaudia Memory** (2026-07-21)](https://komodor.com/platform/klaudia-ai-powered-troubleshooting/)
  it also keeps persistent incident memory — the closest commercial analogue to RunLore overall,
  now on both the diff spine *and* the learning loop. Its memory is closed and non-exportable;
  ours is the differentiator that remains.
```

- [ ] **Step 2: Update the Datadog bullet**

Replace:

```markdown
- **Datadog Change Tracking** — commit-level deploy correlation (no file-level source diff).
```

with:

```markdown
- **Datadog Change Tracking** — commit-level deploy correlation via APM deployment SHAs (`git log`
  between deployments; no GitOps-revision semantics). Bits AI SRE went GA Dec 2025 with closed
  investigation memory; its effective per-investigation price dropped ~75% in 2026 (from $25 to
  ~$6.50 via pooled ["AI Credits"](https://www.nobs.tech/blog/datadog-bits-ai-pricing-ai-credits-governance)) —
  per-investigation economics are racing to the bottom, which favors a BYO-model OSS entrant.
```

- [ ] **Step 3: Update the commercial memory line**

Replace:

```markdown
- **Memory/learning** (Cleric, Resolve, PagerDuty, Google) is **all closed**.
```

with:

```markdown
- **Memory/learning** is now the specialists' *whole pitch* (Cleric "first self-learning AI SRE",
  Komodor Klaudia Memory, NeuBird's vector-DB KB, AWS "Learned Skills", New Relic Knowledge) —
  and **all of it is closed and non-exportable, without exception**. Portability + review is the
  unoccupied position, not "learning".
```

### Task 3: Update the eval-reality section with ITBench-AA

**Files:**
- Modify: `docs/prior-art.md` (section "The eval reality", lines ~53–57)

- [ ] **Step 1: Replace the section body**

Replace:

```markdown
**ITBench** (IBM/ICML 2025) — frontier models identify the root cause **< 50%** of the time and fully
resolve only **~11–14%** of real K8s incidents. Treat sub-50% as the baseline; design for failure; make
honest uncertainty a feature, not a footnote.
```

with:

```markdown
**ITBench** (IBM/ICML 2025) — frontier models identify the root cause **< 50%** of the time and fully
resolve only **~11–14%** of real K8s incidents. The successor
[**ITBench-AA**](https://artificialanalysis.ai/evaluations/itbench-aa) (Artificial Analysis + IBM,
May 2026) confirms it: the best frontier model scores **47%** on agentic IT tasks, with "confusing
symptoms for root causes" the classic failure — while vendors market 92–94% accuracy. Treat sub-50%
as the baseline; design for failure; make honest uncertainty a feature, not a footnote — and publish
reproducible numbers (see the eval scorecard) where vendors publish claims.
```

- [ ] **Step 2: Proofread and commit**

Read the full file once for flow and date consistency ("mid-2026" in the intro can stay).

```bash
git add docs/prior-art.md
git commit -m "docs(prior-art): refresh to July 2026 — Klaudia Memory, Aurora, opensre collision, Datadog pricing, ITBench-AA"
```

## Acceptance criteria

- [ ] `docs/prior-art.md` names Klaudia Memory with its date and draws the "closed vs portable memory" conclusion.
- [ ] Aurora's row reflects diffs + RAG KB + fix PRs and names it the fastest-moving OSS threat.
- [ ] The Tracer-Cloud "opensre" collision is noted.
- [ ] Datadog pricing reflects the AI-Credits era; ITBench-AA numbers are cited with a source link.
- [ ] No stale claim contradicts the 2026-07-23 competitive analysis.
