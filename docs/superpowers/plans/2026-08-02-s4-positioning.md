# S4 — Positioning — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lead with the position RunLore actually owns — portable, Git-owned, human-reviewed incident knowledge — and put the competitive comparison where people search for it.

**Architecture:** Content-only. The homepage hero, its social card and the site metadata are rewritten around knowledge ownership; the competitive research already in `prior-art.md` is projected into three standalone `/compare/<tool>` pages built for search intent, with `prior-art.md` remaining the sourced research they cite. MCP is promoted from a configuration subsection to the stated answer for toolset breadth.

**Tech Stack:** Hugo + Hextra (`website/`), SVG → PNG export for the social card.

**Spec:** [`docs/superpowers/specs/2026-08-02-s4-positioning-design.md`](../specs/2026-08-02-s4-positioning-design.md)

## Global Constraints

- **Every competitive claim must be sourced and dated.** A statement about another project carries a link to that project's own documentation or repository. No claim from memory.
- Every compare page carries `last_verified: 2026-08-02` in its front matter and renders that date on the page.
- Internal links use `{{< relref >}}`; `hugo.yaml` sets `refLinksErrorLevel: ERROR`, so a bad ref fails the build.
- The headline copy is fixed by the spec — use it verbatim:
  **H1:** `Your incident knowledge, in your Git. Not in a vendor's database.`
- Do not touch the hero flow diagram SVG in `_index.md:30` — it already terminates in the OKF knowledge base, which is now the headline claim.
- Conventional Commits. **Never** add co-author trailers or AI attribution.

---

### Task 1: Homepage hero and site metadata

**Files:**
- Modify: `website/content/_index.md:1-41`
- Modify: `website/hugo.yaml:57` (`params.description`)

**Interfaces:**
- Consumes: Hextra shortcodes already in use — `hextra/hero-badge`, `hextra/hero-headline`, `hextra/hero-subtitle`, `hextra/hero-button`, `hextra/feature-grid`, `hextra/feature-card`.
- Produces: the new hero copy that Task 2's social card must match.

- [ ] **Step 1: Rewrite the front matter and hero**

In `website/content/_index.md`, replace the `description` front-matter value and the headline/subtitle blocks:

```yaml
description: "Your incident knowledge, in your Git — not in a vendor's database. RunLore investigates alerts like the other agents do, then writes what it learned to a repo you own, as a pull request a human merges. Portable markdown, provenance-tracked, read-only by default, your models, one Go binary."
```

```markdown
{{< hextra/hero-headline >}}
  Your incident knowledge, in your Git. Not in a vendor's database.
{{< /hextra/hero-headline >}}

{{< hextra/hero-subtitle >}}
  RunLore investigates alerts like the other agents do — then writes what it
  learned to a repo you own, as a pull request a human merges. Portable markdown,
  provenance-tracked, trust that decays when it stops working. Read-only by
  default, your models, one Go binary.
{{< /hextra/hero-subtitle >}}
```

The subtitle's first clause names the category immediately, so a reader who arrived looking for "an SRE agent" is not left guessing before the differentiation lands.

- [ ] **Step 2: Reorder the feature cards to follow the claim**

Replace the `feature-grid` block (`_index.md:32-41`) so the ownership card leads:

```markdown
{{< hextra/feature-grid cols="2" >}}
  {{< hextra/feature-card icon="academic-cap" title="Knowledge you own"
    subtitle="Every investigation opens a pull request in a Git repo you control. A human merges it. The result is portable markdown with full provenance — exportable, greppable, and yours if you stop using RunLore tomorrow." >}}
  {{< hextra/feature-card icon="shield-check" title="Read-only by default"
    subtitle="Reads your cluster, metrics, logs and network flows. Its only writes go to Git, via reviewed PRs — and it would rather say “I don’t know” than guess." >}}
  {{< hextra/feature-card icon="cube-transparent" title="GitOps-native provenance"
    subtitle="Turns “what changed?” into an exact Git answer — the rendered-manifest diff of the revisions Flux or Argo CD reconciled. That provenance is what makes the knowledge trustworthy." >}}
  {{< hextra/feature-card icon="chip" title="Your models · one Go binary"
    subtitle="A single self-hosted Go binary running in your cluster on your own model providers. Any OpenAI-compatible endpoint, or native Anthropic. No lock-in, your data. Point it at any MCP server for tools it doesn’t ship." >}}
{{< /hextra/feature-grid >}}
```

> **Not in scope here:** the "publishes its own eval scorecard" line above the fold belongs to S3, which creates `/eval`. Adding it from this PR would ship a dead link if this branch merges first. Do not add it.

- [ ] **Step 3: Update the site-wide description**

In `website/hugo.yaml:57`, replace `params.description` with the same sentence used in the front matter.

- [ ] **Step 4: Build and review**

Run: `cd website && hugo --gc --minify && hugo server -D`
Open `http://localhost:1313/` and read the page top to bottom. Check: does the H1 → subtitle → cards → diagram sequence tell one coherent story? Is the category clear within the first two lines?

- [ ] **Step 5: Commit**

```bash
git add website/content/_index.md website/hugo.yaml
git commit -m "docs(website): lead with knowledge ownership, not investigation speed"
```

---

### Task 2: Regenerate the social card

**Files:**
- Modify: `website/static/images/og-card.svg`
- Regenerate: `website/static/images/og-card.png`

**Interfaces:**
- Consumes: the H1 copy from Task 1.
- Produces: a 1200×630 PNG at the same path (`hugo.yaml:60` and `_index.md:5` reference `images/og-card.png`, so neither changes).

**Why:** `og-card.svg` hard-codes `The SRE agent that speeds up investigations` and `…and learns from your context`. Every shared link would keep advertising the headline Task 1 just replaced.

- [ ] **Step 1: Read the current card**

Run: `cat website/static/images/og-card.svg`
Note the exact `<text>` elements, their `x`/`y`/`font-size`/`class` attributes, and the 1200×630 canvas. Keep the layout, gradient, and logo untouched — only the copy changes.

- [ ] **Step 2: Edit the copy**

Replace the two headline text elements. The new headline is longer than the old one, so it needs two lines:

- line 1: `Your incident knowledge, in your Git.`
- line 2: `Not in a vendor's database.`
- subtitle: `Investigates incidents · curates what it learns to a repo you own`

Reduce the headline `font-size` as needed so neither line exceeds roughly 1080px of the 1200px canvas, and adjust the two `y` values so the block stays vertically centered against the logo. Keep `runlore.io` and the `Free & open source · Apache-2.0` footer as they are.

Use a typographic apostrophe (`’`) in `vendor’s` to match the rest of the site's copy, and escape nothing else — SVG text does not need entity escaping for that character in UTF-8.

- [ ] **Step 3: Export the PNG**

Try, in order of preference, whichever is available:

```bash
# Inkscape (best text rendering fidelity)
inkscape website/static/images/og-card.svg \
  --export-type=png --export-filename=website/static/images/og-card.png \
  --export-width=1200 --export-height=630

# or rsvg-convert
rsvg-convert -w 1200 -h 630 website/static/images/og-card.svg \
  -o website/static/images/og-card.png

# or ImageMagick
magick -background none -density 144 website/static/images/og-card.svg \
  -resize 1200x630 website/static/images/og-card.png
```

- [ ] **Step 4: Verify the render**

Run: `file website/static/images/og-card.png`
Expected: `PNG image data, 1200 x 630`.

Open the PNG and read it. Confirm: no clipped text, no overlap with the logo, both headline lines legible at thumbnail size (social previews render small — view it at ~25% zoom to check).

- [ ] **Step 5: Commit**

```bash
git add website/static/images/og-card.svg website/static/images/og-card.png
git commit -m "docs(website): social card matches the new headline"
```

---

### Task 3: The `/compare` section and the HolmesGPT page

**Files:**
- Create: `website/content/compare/_index.md`
- Create: `website/content/compare/holmesgpt.md`
- Modify: `website/hugo.yaml:31-45` (menu)

**Interfaces:**
- Consumes: the sourced research in `website/content/docs/concepts/prior-art.md` (HolmesGPT row, "Where RunLore is different", the ITBench figures).
- Produces: the page structure Task 4's two pages copy exactly.

- [ ] **Step 1: Create the section index**

Create `website/content/compare/_index.md`:

```markdown
---
title: Compare
weight: 5
---

How RunLore differs from the tools it is most often weighed against — and when you
should pick one of them instead.

Every page states what the other tool does better **first**, cites the project's own
documentation, and carries the date the claims were last checked. If something here
is out of date or wrong, [open an issue](https://github.com/Smana/runlore/issues) —
a wrong claim about someone else's project is the most expensive kind of mistake we
can make.

{{< hextra/feature-grid cols="3" >}}
  {{< hextra/feature-card link="holmesgpt/" title="RunLore vs HolmesGPT" subtitle="The strongest OSS investigation agent. Broader toolset, no learning loop." >}}
  {{< hextra/feature-card link="k8sgpt/" title="RunLore vs k8sgpt" subtitle="A deterministic detector with LLM explanations, not an investigation loop." >}}
  {{< hextra/feature-card link="aurora/" title="RunLore vs Aurora" subtitle="Diffs, a RAG knowledge base and fix PRs — but the knowledge stays in its databases." >}}
{{< /hextra/feature-grid >}}

The long-form research behind these pages — including the commercial landscape and the
eval reality — is in [Prior art]({{< relref "/docs/concepts/prior-art.md" >}}).
```

- [ ] **Step 2: Write the HolmesGPT page**

Create `website/content/compare/holmesgpt.md` with front matter:

```yaml
---
title: RunLore vs HolmesGPT
description: "HolmesGPT has the broader toolset; RunLore keeps what it learns in a Git repo you own. An honest, sourced comparison."
last_verified: 2026-08-02
---
```

Body sections, in this order:

1. **One-paragraph summary.** HolmesGPT is the strongest open-source investigation agent and the one to beat on breadth. RunLore does not try to out-toolset it. The difference is what happens *after* the investigation.

2. **What HolmesGPT does better** — first, and specifically. Source each from prior-art's HolmesGPT row and the project's own docs:
   - ~60 toolsets spanning Datadog, New Relic, Splunk, Sentry, Elasticsearch/OpenSearch, VictoriaMetrics and more.
   - CNCF Sandbox project with 2,205 contributors versus RunLore's 3 — a materially different bus factor and support surface.
   - Approval-gated remediation with signed approval tickets since v0.34.
   - Mature, widely deployed, backed by Robusta.

3. **What RunLore does that HolmesGPT does not.**
   - HolmesGPT is stateless: human-authored runbooks, every incident starts from zero (its commercial parent sells the intelligence layer). RunLore curates each verified finding into a PR against a Git repo you own; a merged entry answers the same incident instantly next time.
   - Flux support and revision-exact "what changed" — HolmesGPT has an Argo CD toolset but no Flux and no rendered-diff-between-reconciled-revisions.
   - A published nightly eval scorecard.

4. **At a glance** — a table with columns `| | RunLore | HolmesGPT |` and rows: investigation breadth, learning loop, knowledge portability, human review gate, GitOps what-changed, autonomy ceiling, published eval numbers, project maturity, MCP client. Every cell either links a source or states "not documented".

5. **Toolset breadth, honestly** — RunLore's native tool set is narrower by design. Any MCP server closes a specific gap; link `{{< relref "/docs/configuration/mcp.md" >}}`.

6. **Pick HolmesGPT instead if…** — you need a specific vendor toolset today, you want a CNCF-governed project with a large contributor base, or you do not want a knowledge-review workflow at all.

7. **Sources** — a bulleted list of every URL cited, each with "checked 2026-08-02".

Render the verification date in the body, e.g.:

```markdown
*Claims about HolmesGPT last checked {{< param "last_verified" >}} against the project's own documentation.*
```

If that shortcode does not resolve page front matter in this Hextra version, write the date literally and keep it in sync with the front matter.

- [ ] **Step 3: Add Compare to the menu**

In `website/hugo.yaml`, add to `menu.main`, keeping weights ordered:

```yaml
    - name: Compare
      pageRef: /compare
      weight: 2
```

and bump the existing GitHub (2) and Search (3) entries to 3 and 4.

- [ ] **Step 4: Verify every external link resolves**

Run:
```bash
grep -oE 'https?://[^ )"]+' website/content/compare/holmesgpt.md | sort -u | \
  while read -r u; do printf '%s %s\n' "$(curl -sIo /dev/null -w '%{http_code}' -L --max-time 15 "$u")" "$u"; done
```
Expected: every line starts `200`. A `404` on a competitor's documentation is a credibility bug — fix the link or drop the claim.

- [ ] **Step 5: Build**

Run: `cd website && hugo --gc --minify`
Expected: builds; `/compare/` and `/compare/holmesgpt/` render; the menu shows Compare.

- [ ] **Step 6: Commit**

```bash
git add website/content/compare/ website/hugo.yaml
git commit -m "docs(website): /compare section with the HolmesGPT comparison"
```

---

### Task 4: The k8sgpt and Aurora comparisons

**Files:**
- Create: `website/content/compare/k8sgpt.md`
- Create: `website/content/compare/aurora.md`

**Interfaces:**
- Consumes: the page structure from Task 3 — same seven sections, same front matter keys (`title`, `description`, `last_verified`), same at-a-glance table shape.

- [ ] **Step 1: Write the k8sgpt page**

Same structure as Task 3. Substance, sourced from prior-art's k8sgpt row and the project's docs:

- **What k8sgpt does better:** CNCF project since 2023 with ~7.8k stars and a far larger community; deterministic analyzers that need no LLM at all for many checks; trivial to adopt (`k8sgpt analyze`); `Result`-as-CRD fits a Kubernetes-native workflow.
- **What RunLore does that k8sgpt does not:** k8sgpt is a *detector* with optional LLM explanation, not a multi-step investigation loop — it does not correlate metrics, logs, network flows and GitOps history into a ranked, evidence-backed root cause; it has no memory; it does not curate knowledge.
- **Pick k8sgpt instead if…** you want fast deterministic cluster analysis without an LLM in the path, or you want the lightest possible thing to run.
- Note honestly that the two are complementary rather than mutually exclusive.

- [ ] **Step 2: Write the Aurora page**

Same structure. Substance, sourced from prior-art's Aurora row:

- **What Aurora does better:** commercial backing from Arvo AI and a funded team; sandboxed execution of `kubectl`/`aws`/`az`/`gcloud`; Terraform/IaC analysis as an RCA input, which RunLore does not have; auto-postmortems and fix PRs.
- **What RunLore does that Aurora does not:** Aurora's knowledge base is auto-ingested RAG locked in Postgres + Weaviate + Memgraph — no review gate, no Git export, no outcome signal. RunLore's is human-reviewed markdown in a repo you own, with trust that decays on real-world resolve-rate, and an eval scenario proving a poisoned entry is rejected at recall time.
- **Be scrupulously fair here.** Aurora is Apache-2.0 and moving fast; prior-art calls it "the fastest-moving OSS threat". State that plainly rather than downplaying it.
- **Pick Aurora instead if…** you want IaC-level analysis, sandboxed command execution, or commercial support.

- [ ] **Step 3: Verify external links on both pages**

Run the same link check as Task 3 Step 4 against each file.
Expected: all `200`.

- [ ] **Step 4: Cross-check the claims**

Open each cited source and confirm the specific claim it is attached to. Any claim you cannot verify from a public page must be **deleted**, not softened — an unverifiable statement about a competitor is exactly the failure mode these pages exist to avoid.

- [ ] **Step 5: Build and commit**

```bash
cd website && hugo --gc --minify && cd ..
git add website/content/compare/
git commit -m "docs(website): k8sgpt and Aurora comparisons"
```

---

### Task 5: Promote MCP and cross-link prior art

**Files:**
- Modify: `README.md` (add an MCP section)
- Modify: `website/content/docs/concepts/prior-art.md` (link out to the compare pages)
- Modify: `website/content/docs/configuration/mcp.md` (front matter description for search)

**Interfaces:**
- Consumes: `/compare/*` from Tasks 3–4; `website/content/docs/configuration/mcp.md`.

- [ ] **Step 1: Add the MCP section to the README**

Place it after the integration/data-source discussion, phrased as the answer to toolset breadth:

```markdown
## No native integration? Point RunLore at any MCP server

RunLore ships a **narrow, deliberate native tool set** — cluster, metrics, logs, network
flows, GitOps history, cloud control plane, knowledge search. It does not try to match the
~60 vendor toolsets of the largest OSS agent, and it shouldn't.

Instead it ships an **MCP client**: point it at any Model Context Protocol server and those
tools join the investigation loop, governed by the same allowlist and the same read-only
posture as everything else. Whatever your stack has that RunLore doesn't, MCP closes it.

RunLore is also an MCP **server** — `lore mcp` exposes what-changed and knowledge search to
Claude Code, HolmesGPT, or any other MCP client.

→ [MCP configuration](https://runlore.io/docs/configuration/mcp/)
```

- [ ] **Step 2: Cross-link prior art and the compare pages**

At the top of `website/content/docs/concepts/prior-art.md`, after the intro paragraph, add:

```markdown
> Looking for a head-to-head? See [RunLore vs HolmesGPT]({{< relref "/compare/holmesgpt.md" >}}),
> [vs k8sgpt]({{< relref "/compare/k8sgpt.md" >}}), and [vs Aurora]({{< relref "/compare/aurora.md" >}}).
> This page is the underlying research — the full landscape, including the commercial
> vendors and the eval reality.
```

- [ ] **Step 3: Give the MCP page a search-facing description**

Add to `website/content/docs/configuration/mcp.md`'s front matter:

```yaml
description: "Point RunLore at any MCP server to add tools it doesn't ship natively — and expose RunLore's own what-changed and knowledge search to any MCP client."
```

- [ ] **Step 4: Build**

Run: `cd website && hugo --gc --minify`
Expected: builds with no unresolved refs (`refLinksErrorLevel: ERROR` catches any typo in the new `relref`s).

- [ ] **Step 5: Commit**

```bash
git add README.md website/content/docs/concepts/prior-art.md website/content/docs/configuration/mcp.md
git commit -m "docs: lead with MCP as the answer to toolset breadth"
```

---

## Final verification

- [ ] `cd website && hugo --gc --minify` builds clean
- [ ] Every external URL on the three compare pages returns 200, and every claim traces to a source you personally opened
- [ ] `og-card.png` is 1200×630, shows the new headline, and is legible at thumbnail size
- [ ] The homepage reads coherently top to bottom: category named in the first two lines, then the differentiation
- [ ] **Compare** appears in the top-level menu and all three pages are reachable from it
- [ ] `grep -rn "speeds up investigations" website/ README.md` returns nothing — the old headline is fully retired
- [ ] Open the PR — English title and description, no AI attribution, no co-author trailers. Note in the description that the `/eval` link on the homepage depends on S3
</content>
