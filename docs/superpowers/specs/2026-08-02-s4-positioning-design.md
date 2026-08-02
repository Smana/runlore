# Design — S4: Positioning

- **Date:** 2026-08-02
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Source:** improvement report §3
- **Index:** [decomposition](2026-08-02-improvement-report-decomposition.md)

## Problem

RunLore leads with the commodity and buries the wedge.

`website/content/_index.md:16` reads **"The SRE agent that speeds up investigations"**. RunLore's
own `prior-art.md` states the opposite in plain terms: *"the autonomous alert → RCA → Slack runtime
is a commodity, and change-diff RCA is no longer unique."* The headline is the commodity, stated as
the headline.

The unoccupied position — and prior-art proves nobody else holds it — is **portability plus human
review**: every commercial tool that learns keeps the knowledge; every open tool that is portable
doesn't learn. That claim appears nowhere above the fold.

Three supporting problems:

- **The competitive comparison is filed as "prior art"** — an academic label nobody searches. People
  search *"runlore vs holmesgpt"*. Aurora publishes comparison content against this space and it
  works for them.
- **The eval scorecard** — "the only SRE agent that publishes its own eval scorecard" — sits on page
  four of the docs (S3 makes it real; S4 makes it visible).
- **MCP is buried under Configuration.** RunLore cannot out-toolset HolmesGPT's ~60 toolsets and
  shouldn't try, but it ships an MCP client. *"No native integration? Point RunLore at any MCP
  server"* converts the narrowest weakness into a one-line answer.

The social card (`website/static/images/og-card.svg`, shipped 2026-08-02) hard-codes the old
headline, so every share link would keep advertising it.

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| H1 direction | **Knowledge ownership** — "Your incident knowledge, in your Git. Not in a vendor's database." | Owner's choice. It is the claim prior-art shows is unoccupied |
| Comparison URLs | Three standalone pages at `/compare/<tool>` | Matches search intent; one combined page gives one URL to rank instead of three |
| Competitors covered | HolmesGPT, k8sgpt, Aurora | Owner's choice — including the commercial vendor already marketing against this space |
| Tone | Factual, sourced, dated, with an explicit "pick them instead if…" on every page | A comparison that never concedes anything is read as marketing and believed by nobody |
| `prior-art.md` | Stays, as the research source the three pages cite | It is genuinely good research; it is just not a landing page |
| Staleness | `last_verified` front matter on each compare page + a docs-check reminder | Competitor facts rot fast, and a wrong claim about a competitor is the most expensive kind |

## Scope

### In scope

1. Rewrite the homepage hero (H1, subtitle, feature cards, meta description).
2. Regenerate `og-card.svg` → `og-card.png` with the new headline.
3. `content/compare/_index.md` + `/compare/holmesgpt`, `/compare/k8sgpt`, `/compare/aurora`.
4. Add **Compare** to the site's top-level menu.
5. Promote the eval scorecard above the fold (hero-adjacent link to `/eval`, once S3 lands).
6. Promote MCP: README section, `/docs/integrations/` card, and a homepage feature card line.
7. Repoint `prior-art.md` to cite the compare pages, and vice versa.

### Non-goals (YAGNI)

- Rewriting the README's body (its framing is already the correct one — the site is what lags).
- A pricing page, a "customers" section, or testimonials.
- Benchmarks *of* competitors — the compare pages state documented capabilities, they do not
  run other people's tools and grade them.
- Redesigning the theme, colors, or the hero diagram (the diagram already shows the OKF KB).

## Design

### Homepage hero

```
H1   Your incident knowledge, in your Git. Not in a vendor's database.

Sub  RunLore investigates alerts like the other agents do — then writes what it
     learned to a repo you own, as a pull request a human merges. Portable
     markdown, provenance-tracked, trust that decays when it stops working.
     Read-only by default, your models, one Go binary.
```

The four feature cards reorder to follow the claim: **Knowledge you own** → **Read-only by default**
→ **GitOps-native** → **Your models, one binary**. The `description` front matter and the
`params.description` in `hugo.yaml` are updated to match, since they feed search results and the
social card. `og-card.svg` gets the new headline and is re-exported to PNG at the same dimensions
and path, so no `images:` front matter changes.

The hero keeps the existing flow diagram — it already terminates in the OKF knowledge base, which is
now the headline claim rather than a footnote.

### Compare pages

One shared structure so the three read as a set:

```markdown
---
title: RunLore vs HolmesGPT
last_verified: 2026-08-02
---

One-paragraph honest summary.

## At a glance          # table: capability × RunLore / them, sourced
## What they do better  # first, and specifically
## What RunLore does that they don't
## Pick them instead if…
## Sources              # links + the date each was checked
```

Substance per page:

| Page | RunLore's claim | The honest concession |
|---|---|---|
| HolmesGPT | Portable Git-owned knowledge; human-reviewed curation; published eval scorecard | ~60 toolsets vs RunLore's narrower set; CNCF Sandbox; 2,205 contributors vs 3 |
| k8sgpt | Investigation loop with evidence + memory, not one-shot analysis | Far larger community, CNCF Sandbox since 2023, simpler to adopt |
| Aurora | Knowledge is exportable markdown in your repo, not a vendor RAG store; self-hosted, your models | Commercial support, a funded team, a broader integration surface |

Every capability row links to the competitor's own documentation, so a reader can check the claim
without trusting us. The "at a glance" tables are written from published docs only, and the
`last_verified` date is rendered on the page.

MCP appears on all three pages as the toolset-breadth answer: RunLore's native tool set is narrower
by design, and any MCP server closes a specific gap.

### MCP promotion

`configuration/mcp.md` stays the reference. What changes is reach: a README section under the
integration list, a card on the `/docs/integrations/` index (S2), a line in the compare pages, and
one homepage feature-card sentence.

## Testing

| Test | Guards |
|---|---|
| `hugo --gc --minify` in `docs-check.yml` | `refLinksErrorLevel: ERROR` catches every broken internal ref |
| External link check on the compare pages | a competitor doc URL that 404s is a credibility bug |
| Manual | og-card renders correctly in a social-preview validator at 1200×630 |
| Manual | homepage reads coherently top to bottom against the new H1 |

## Risks

| Risk | Mitigation |
|---|---|
| Comparison pages read as attack marketing | "What they do better" comes *first* on every page, and every claim is sourced and dated |
| Competitor facts go stale | `last_verified` rendered on the page; refresh is a standing item alongside `prior-art.md` |
| A vendor disputes a claim | Only published documentation is cited, with links; corrections are a PR away |
| The new H1 confuses readers who came for "SRE agent" | The subtitle's first clause is "RunLore investigates alerts like the other agents do" — the category is stated immediately, then differentiated |

## Acceptance criteria

1. The homepage H1 states knowledge ownership; the subtitle names the category in its first clause.
2. `og-card.png` shows the new headline; the meta description matches.
3. `/compare/holmesgpt`, `/compare/k8sgpt`, `/compare/aurora` exist, each with a sourced at-a-glance
   table, a "what they do better" section, and a rendered `last_verified` date.
4. **Compare** is reachable from the top-level menu.
5. MCP is presented as the toolset-breadth answer in the README, the integrations index, and each
   compare page.
6. `hugo` builds clean; every external link on the compare pages resolves.
</content>
