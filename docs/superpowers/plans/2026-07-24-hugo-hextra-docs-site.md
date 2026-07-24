# Hugo + Hextra Docs Site Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish RunLore's docs as a Hugo + Hextra website at `https://runlore.io/`, opening on an overview with Getting Started and full-text search immediately reachable.

**Architecture:** A self-contained Hugo site under `website/` uses Hextra (installed as a Hugo Module → isolated `website/go.mod`). The 17 existing `docs/*.md` files are `git mv`'d into six weighted sections under `website/content/docs/`; internal cross-links become `relref` shortcodes so a broken ref fails the build. A GitHub Actions workflow builds with Hugo Extended and deploys to GitHub Pages with a `CNAME` of `runlore.io`.

**Tech Stack:** Hugo Extended, Hextra theme (`github.com/imfing/hextra`), Hugo Modules (Go), GitHub Actions, GitHub Pages, FlexSearch (built into Hextra).

## Global Constraints

- Hugo **Extended** required, version **≥ 0.134.0** (Hextra floor). CI pins `HUGO_VERSION: "0.140.0"`.
- Hugo Module path for the site: `github.com/Smana/runlore/website`.
- `baseURL: https://runlore.io/` (apex). `CNAME` file content: `runlore.io`.
- Hextra module import: `github.com/imfing/hextra`.
- All work on branch `docs/hugo-hextra-site` (already created; spec already committed there).
- Conventional-commit messages, **English**, **no AI attribution**, **no co-author** trailer.
- **No new prose invented** — page bodies are moved as-is; homepage copy is drawn from `README.md`.
- `refLinksErrorLevel: ERROR` in config — a dead internal ref must fail the build.
- The Go build (`go build ./...` at repo root) must stay green — `website/go.mod` is a separate module and must not affect it.

## File Structure

```
website/
  go.mod, go.sum              # Task 1 — Hextra module pin
  hugo.yaml                   # Task 1 — full site config (baseURL, module, search, navbar, theme)
  assets/images/logo.svg      # Task 1 — copied from repo assets/ (light + dark)
  assets/images/logo-dark.svg
  static/favicon.png          # Task 1
  static/CNAME                # Task 5 — runlore.io
  content/
    _index.md                 # Task 4 — homepage (hextra-home)
    docs/
      _index.md               # Task 2 — /docs landing
      getting-started.md      # Task 2 — leaf, weight 10
      concepts/_index.md      # Task 2  (+ design, architecture, learning-loop,
      concepts/*.md           #          data-sources, reviewing-knowledge, prior-art)
      configuration/_index.md # Task 2  (+ configuration, mcp)
      operations/_index.md    # Task 2  (+ observability, troubleshooting, upgrade-uninstall)
      security/_index.md      # Task 2  (+ security-model, security-architecture)
      reference/_index.md     # Task 2  (+ kb-steward, benchmarking, examples/harbor-registry-down)
.github/workflows/docs.yml    # Task 5 — build + deploy to Pages
.gitignore                    # Task 1 — ignore website build artifacts
README.md, CONTRIBUTING.md    # Task 6 — repoint docs links to runlore.io
```

Content moves via `git mv` (history preserved). Top-level `docs/` product pages are folded into `website/`; `docs/superpowers/` (specs/plans) stays and is **not** part of the site.

---

### Task 1: Scaffold the Hugo + Hextra site (config + assets, builds empty)

**Files:**
- Create: `website/hugo.yaml`, `website/go.mod`, `website/go.sum`
- Create: `website/content/_index.md` (temporary placeholder, replaced in Task 4)
- Create: `website/assets/images/logo.svg`, `website/assets/images/logo-dark.svg`, `website/static/favicon.png`
- Modify: `.gitignore` (append)

**Interfaces:**
- Produces: the site root `website/`, config key `params.search` (FlexSearch), navbar logo at `images/logo.svg`, module path `github.com/Smana/runlore/website`. Later tasks put content under `website/content/docs/`.

- [ ] **Step 1: Initialize the Hugo module and add Hextra**

```bash
cd website 2>/dev/null || mkdir website && cd website
hugo mod init github.com/Smana/runlore/website
hugo mod get github.com/imfing/hextra
cd ..
```

Expected: `website/go.mod` now contains `module github.com/Smana/runlore/website` and a `require github.com/imfing/hextra ...` line; `website/go.sum` created.

- [ ] **Step 2: Write `website/hugo.yaml`**

```yaml
baseURL: https://runlore.io/
title: RunLore
languageCode: en-us
enableRobotsTXT: true
enableGitInfo: true

# Drift guard: any unresolved internal ref (relref/ref) fails the build.
refLinksErrorLevel: ERROR

module:
  imports:
    - path: github.com/imfing/hextra

markup:
  goldmark:
    renderer:
      unsafe: true
  highlight:
    noClasses: false

menu:
  main:
    - name: Docs
      pageRef: /docs
      weight: 1
    - name: GitHub
      weight: 2
      url: "https://github.com/Smana/runlore"
      params:
        icon: github

params:
  description: "An open-source SRE agent that investigates incidents — and remembers what it learns."
  navbar:
    displayTitle: true
    displayLogo: true
    logo:
      path: images/logo.svg
      dark: images/logo-dark.svg
      link: /
      width: 32
      height: 32
  search:
    enable: true
    type: flexsearch
    flexsearch:
      index: content
      tokenize: forward
  editURL:
    enable: true
    base: "https://github.com/Smana/runlore/edit/main/website/content"
  footer:
    enable: true
    displayCopyright: true
  theme:
    default: system
    displayToggle: true
```

- [ ] **Step 3: Copy logo + favicon assets from the repo**

```bash
mkdir -p website/assets/images website/static
cp assets/logo.svg      website/assets/images/logo.svg
cp assets/logo-dark.svg website/assets/images/logo-dark.svg
cp assets/favicon-256.png website/static/favicon.png
```

- [ ] **Step 4: Add a temporary homepage placeholder**

`website/content/_index.md`:

```markdown
---
title: RunLore
---

Placeholder — replaced by the hextra-home layout in Task 4.
```

- [ ] **Step 5: Append build artifacts to `.gitignore`**

Append to `.gitignore`:

```gitignore
# Hugo docs site (website/)
website/public/
website/resources/
website/.hugo_build.lock
```

- [ ] **Step 6: Build the empty site and confirm it renders**

Run: `cd website && hugo mod get && hugo --gc && cd ..`
Expected: `Total in ... ms`, `0` errors, `website/public/index.html` created. No `WARN` lines about missing modules.

- [ ] **Step 7: Confirm the Go build is unaffected by the nested module**

Run: `go build ./...`
Expected: exits 0 (Go skips `website/` because it has its own `go.mod`).

- [ ] **Step 8: Commit**

```bash
git add website/hugo.yaml website/go.mod website/go.sum website/content/_index.md \
        website/assets website/static .gitignore
git commit -m "feat(docs): scaffold Hugo + Hextra site under website/"
```

---

### Task 2: Migrate the 17 docs into sectioned content (git mv + front matter)

**Files:**
- Create (section landings): `website/content/docs/_index.md` and `website/content/docs/{concepts,configuration,operations,security,reference}/_index.md`
- Move (via `git mv`): all 17 product docs from `docs/` into `website/content/docs/…` (see mapping)
- Delete (after moves): empty `docs/architecture/`, `docs/examples/`

**Interfaces:**
- Consumes: site root from Task 1.
- Produces: content tree with unique page basenames (relied on by Task 3 `relref`) and section weights driving the sidebar. Getting Started is a leaf page at weight 10 (top of sidebar).

- [ ] **Step 1: Create the section directories and `_index.md` files**

```bash
mkdir -p website/content/docs/{concepts,configuration,operations,security,reference/examples}
```

`website/content/docs/_index.md`:

```markdown
---
title: Documentation
weight: 1
---

RunLore investigates incidents on your platform and remembers what it learns.
Start with **Getting Started**, then explore the concepts, configuration,
operations, and security sections.
```

Create each section `_index.md` with this exact content (title + weight + one-liner):

| File | title | weight | one-liner body |
|---|---|---|---|
| `concepts/_index.md` | Concepts | 20 | How RunLore is designed and how its learning loop works. |
| `configuration/_index.md` | Configuration | 30 | Configure the agent, its data sources, and MCP. |
| `operations/_index.md` | Operations | 40 | Run, observe, troubleshoot, and upgrade RunLore. |
| `security/_index.md` | Security | 50 | The read-only default, the action gate, and the trust model. |
| `reference/_index.md` | Reference | 60 | Tools, benchmarks, and worked examples. |

Example (`concepts/_index.md`):

```markdown
---
title: Concepts
weight: 20
---

How RunLore is designed and how its learning loop works.
```

- [ ] **Step 2: Move each doc into its section (`git mv`)**

```bash
git mv docs/getting-started.md                     website/content/docs/getting-started.md
git mv docs/design.md                              website/content/docs/concepts/design.md
git mv docs/architecture/runlore-architecture.md   website/content/docs/concepts/architecture.md
git mv docs/learning-loop.md                       website/content/docs/concepts/learning-loop.md
git mv docs/data-sources.md                        website/content/docs/concepts/data-sources.md
git mv docs/reviewing-knowledge.md                 website/content/docs/concepts/reviewing-knowledge.md
git mv docs/prior-art.md                           website/content/docs/concepts/prior-art.md
git mv docs/configuration.md                       website/content/docs/configuration/configuration.md
git mv docs/mcp.md                                 website/content/docs/configuration/mcp.md
git mv docs/observability.md                       website/content/docs/operations/observability.md
git mv docs/troubleshooting.md                     website/content/docs/operations/troubleshooting.md
git mv docs/upgrade-uninstall.md                   website/content/docs/operations/upgrade-uninstall.md
git mv docs/security-model.md                      website/content/docs/security/security-model.md
git mv docs/security-architecture.md               website/content/docs/security/security-architecture.md
git mv docs/kb-steward.md                          website/content/docs/reference/kb-steward.md
git mv docs/benchmarking.md                        website/content/docs/reference/benchmarking.md
git mv docs/examples/harbor-registry-down.md       website/content/docs/reference/examples/harbor-registry-down.md
rmdir docs/architecture docs/examples 2>/dev/null || true
```

- [ ] **Step 3: Add front matter and remove the leading H1 from each moved page**

For each moved page: **prepend** the front matter block below, then **delete the first `# …` heading line** in the body (Hextra renders `title` as the page H1 — leaving the old H1 would double the heading).

| Page (under `website/content/docs/`) | title | weight |
|---|---|---|
| `getting-started.md` | Getting Started | 10 |
| `concepts/design.md` | Design | 10 |
| `concepts/architecture.md` | Architecture | 20 |
| `concepts/learning-loop.md` | Learning Loop | 30 |
| `concepts/data-sources.md` | Data Sources | 40 |
| `concepts/reviewing-knowledge.md` | Reviewing Knowledge | 50 |
| `concepts/prior-art.md` | Prior Art | 60 |
| `configuration/configuration.md` | Configuration Reference | 10 |
| `configuration/mcp.md` | MCP | 20 |
| `operations/observability.md` | Observability | 10 |
| `operations/troubleshooting.md` | Troubleshooting | 20 |
| `operations/upgrade-uninstall.md` | Upgrade & Uninstall | 30 |
| `security/security-model.md` | Security Model | 10 |
| `security/security-architecture.md` | Security Architecture | 20 |
| `reference/kb-steward.md` | kb-steward | 10 |
| `reference/benchmarking.md` | Benchmarking | 20 |
| `reference/examples/harbor-registry-down.md` | Harbor registry down | 30 |

Front-matter template to prepend (fill title/weight from the table):

```markdown
---
title: <Title>
weight: <Weight>
---
```

- [ ] **Step 4: Build and confirm every page is present in the sidebar**

Run: `cd website && hugo --gc && cd ..`
Expected: `0` errors. Then confirm 17 doc pages + section indexes rendered:

Run: `find website/public/docs -name index.html | wc -l`
Expected: `≥ 23` (17 pages + 6 section/docs indexes).

> Note: internal `](foo.md)` links still point at raw filenames here — they will 404 at runtime until Task 3. The build still passes because plain markdown links are not validated (only `relref` is).

- [ ] **Step 5: Commit**

```bash
git add website/content/docs docs
git commit -m "feat(docs): migrate docs into sectioned Hextra content tree"
```

---

### Task 3: Convert internal cross-links to `relref` (fail-closed)

**Files:**
- Modify: every page under `website/content/docs/` that contains an internal `](name.md…)` link (full list below)

**Interfaces:**
- Consumes: unique page basenames from Task 2 (`relref` resolves by basename across sections).
- Produces: all internal links resolvable; `refLinksErrorLevel: ERROR` now actively guards them.

- [ ] **Step 1: Rewrite each internal link to a `relref` shortcode**

Transform every `](<name>.md)` and `](<name>.md#<anchor>)` into `]({{< relref "<name>.md" >}})` / `]({{< relref "<name>.md#<anchor>" >}})` — write these shortcodes verbatim into the content files (no `/* */` escaping). Basenames are globally unique, so no path is needed. The complete set to convert (source → target):

```
getting-started.md      -> configuration.md, design.md, security-model.md, troubleshooting.md, upgrade-uninstall.md
concepts/design.md      -> learning-loop.md, prior-art.md, security-architecture.md
concepts/learning-loop.md -> configuration.md#notify--where-findings-go, design.md
concepts/reviewing-knowledge.md -> configuration.md#forge--the-git-host-for-curation,
                            getting-started.md#step-7--the-learn-loop-kb-lifecycle--re-runs,
                            kb-steward.md, learning-loop.md
concepts/data-sources.md -> (none)
concepts/prior-art.md   -> (none)
configuration/configuration.md -> learning-loop.md, learning-loop.md#6-the-feedback-edge--outcome-driven-decay-what-makes-it-learn,
                            reviewing-knowledge.md#expected-triage-volume, security-model.md,
                            security-model.md#the-feedback-channels--exposure--trust-model, upgrade-uninstall.md
configuration/mcp.md    -> (none)
operations/observability.md -> troubleshooting.md
operations/troubleshooting.md -> observability.md
security/security-model.md -> configuration.md, configuration.md#actions--the-autonomy-ladder-off-by-default,
                            design.md, getting-started.md, security-architecture.md
security/security-architecture.md -> design.md#9-safety--trust-model, security-model.md,
                            security-model.md#honest-limitations, security-model.md#least-privilege-rbac,
                            security-model.md#tamper-evident-audit-log
reference/kb-steward.md -> mcp.md, reviewing-knowledge.md
reference/benchmarking.md -> prior-art.md
```

Example edit (in `security/security-architecture.md`):

```diff
-See the [security model](security-model.md#honest-limitations) for the honest limitations.
+See the [security model]({{< relref "security-model.md#honest-limitations" >}}) for the honest limitations.
```

> `relref` outputs the resolved URL as a bare string, so it drops straight into the link's `(…)`. A wrong basename or missing target aborts the build under `refLinksErrorLevel: ERROR`.

- [ ] **Step 2: Build with the drift guard and confirm all refs resolve**

Run: `cd website && hugo --gc && cd ..`
Expected: `0` errors. A wrong basename or missing anchor would abort with `REF_NOT_FOUND` (that is the guard working).

- [ ] **Step 3: Confirm no raw `.md` links remain**

Run: `grep -rnE "\]\((\.?/)?[a-z0-9-]+\.md" website/content/docs || echo "clean"`
Expected: `clean` (no plain `.md` links left).

- [ ] **Step 4: Commit**

```bash
git add website/content/docs
git commit -m "fix(docs): resolve internal cross-links via relref"
```

---

### Task 4: Homepage — hextra-home hero, overview, feature grid

**Files:**
- Modify (replace placeholder): `website/content/_index.md`

**Interfaces:**
- Consumes: `docs/getting-started.md` route (`docs/getting-started`) for the primary CTA.
- Produces: the landing page; CTA links must resolve to real pages (verified in Step 2).

- [ ] **Step 1: Replace `website/content/_index.md` with the home layout**

Copy is drawn from `README.md` (no new prose):

Write the shortcodes verbatim (no `/* */` escaping — this file is Hugo content):

```markdown
---
title: RunLore
layout: hextra-home
---

{{< hextra/hero-badge >}}
  Free, open source · Apache-2.0
{{< /hextra/hero-badge >}}

{{< hextra/hero-headline >}}
  An open-source SRE agent that investigates incidents
{{< /hextra/hero-headline >}}

{{< hextra/hero-subtitle >}}
  …and remembers what it learns. Read-only by default — it reads your cluster,
  metrics, logs and network flows, and its only writes go to Git via reviewed PRs.
  Runs on your models, as a single Go binary in your cluster.
{{< /hextra/hero-subtitle >}}

{{< hextra/hero-button text="Get Started" link="docs/getting-started" >}}

{{< hextra/feature-grid cols="2" >}}
  {{< hextra/feature-card title="Learns your platform"
    subtitle="Every investigation opens a PR in a Git repo you own. A human merges it, building a knowledge base of your incidents — the same pattern next time gets an instant answer." >}}
  {{< hextra/feature-card title="Read-only by default"
    subtitle="Reads your cluster, metrics, logs and network flows. Its only writes go to Git, via reviewed PRs — and would rather say “I don’t know” than guess." >}}
  {{< hextra/feature-card title="GitOps-native"
    subtitle="Turns “what changed?” into an exact Git answer — the rendered-manifest diff of the revisions Flux or Argo CD reconciled." >}}
  {{< hextra/feature-card title="Your models · one Go binary"
    subtitle="A single self-hosted Go binary running in your cluster on your own model providers. Portable, no lock-in, your data." >}}
{{< /hextra/feature-grid >}}
```

- [ ] **Step 2: Build and verify the homepage + CTA target**

Run: `cd website && hugo --gc && cd ..`
Expected: `0` errors.
Run: `grep -o 'href="[^"]*getting-started[^"]*"' website/public/index.html | head -1`
Expected: a link resolving to `/docs/getting-started/` (proves the CTA target exists).
Run: `grep -c "feature-card\|hextra" website/public/index.html` → non-zero (cards rendered).

- [ ] **Step 3: Commit**

```bash
git add website/content/_index.md
git commit -m "feat(docs): landing page with overview and feature cards"
```

---

### Task 5: Deploy workflow (GitHub Pages) + CNAME

**Files:**
- Create: `.github/workflows/docs.yml`
- Create: `website/static/CNAME`

**Interfaces:**
- Consumes: the buildable site from Tasks 1–4.
- Produces: a Pages deployment on push to `main` touching `website/**`.

- [ ] **Step 1: Add the CNAME**

`website/static/CNAME` (single line, no trailing content):

```
runlore.io
```

- [ ] **Step 2: Create `.github/workflows/docs.yml`**

```yaml
name: Docs site

on:
  push:
    branches: [main]
    paths:
      - 'website/**'
      - '.github/workflows/docs.yml'
  workflow_dispatch:

permissions:
  contents: read
  pages: write
  id-token: write

concurrency:
  group: pages
  cancel-in-progress: false

jobs:
  deploy:
    runs-on: ubuntu-latest
    environment:
      name: github-pages
      url: ${{ steps.deployment.outputs.page_url }}
    env:
      HUGO_VERSION: "0.140.0"
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version: stable
      - name: Install Hugo Extended
        run: |
          wget -O /tmp/hugo.deb \
            "https://github.com/gohugoio/hugo/releases/download/v${HUGO_VERSION}/hugo_extended_${HUGO_VERSION}_linux-amd64.deb"
          sudo dpkg -i /tmp/hugo.deb
      - name: Configure Pages
        id: pages
        uses: actions/configure-pages@v5
      - name: Build
        working-directory: website
        run: |
          hugo mod get
          hugo --minify --gc --baseURL "${{ steps.pages.outputs.base_url }}/"
      - uses: actions/upload-pages-artifact@v3
        with:
          path: website/public
      - name: Deploy
        id: deployment
        uses: actions/deploy-pages@v4
```

- [ ] **Step 3: Validate the workflow YAML**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/docs.yml')); print('yaml ok')"`
Expected: `yaml ok`. (Full deploy is exercised on first push to `main` after merge, or via **workflow_dispatch**.)

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/docs.yml website/static/CNAME
git commit -m "ci(docs): build and deploy the docs site to GitHub Pages"
```

---

### Task 6: Repoint README + CONTRIBUTING links to runlore.io

**Files:**
- Modify: `README.md` (lines with `](docs/…md…)` — 17 targets across lines 17, 90, 117, 136, 151, 222, 235, 274–276)
- Modify: `CONTRIBUTING.md` (lines 4, 133)

**Interfaces:**
- Consumes: the section routes established in Task 2.

- [ ] **Step 1: Map each `docs/*.md` link to its runlore.io URL**

Use this exact mapping (anchors, where present, are preserved):

```
docs/getting-started.md                   -> https://runlore.io/docs/getting-started/
docs/design.md                            -> https://runlore.io/docs/concepts/design/
docs/architecture/runlore-architecture.md -> https://runlore.io/docs/concepts/architecture/
docs/learning-loop.md                     -> https://runlore.io/docs/concepts/learning-loop/
docs/data-sources.md                      -> https://runlore.io/docs/concepts/data-sources/
docs/reviewing-knowledge.md               -> https://runlore.io/docs/concepts/reviewing-knowledge/
docs/prior-art.md                         -> https://runlore.io/docs/concepts/prior-art/
docs/configuration.md                     -> https://runlore.io/docs/configuration/configuration/
docs/mcp.md                               -> https://runlore.io/docs/configuration/mcp/
docs/observability.md                     -> https://runlore.io/docs/operations/observability/
docs/troubleshooting.md                   -> https://runlore.io/docs/operations/troubleshooting/
docs/upgrade-uninstall.md                 -> https://runlore.io/docs/operations/upgrade-uninstall/
docs/security-model.md                    -> https://runlore.io/docs/security/security-model/
docs/security-architecture.md             -> https://runlore.io/docs/security/security-architecture/
docs/kb-steward.md                        -> https://runlore.io/docs/reference/kb-steward/
docs/benchmarking.md                      -> https://runlore.io/docs/reference/benchmarking/
docs/examples/harbor-registry-down.md     -> https://runlore.io/docs/reference/examples/harbor-registry-down/
```

Apply the replacements in `README.md` and `CONTRIBUTING.md` (the two `docs/` links in CONTRIBUTING are `getting-started.md` and `benchmarking.md`).

- [ ] **Step 2: Confirm no repo-relative `docs/*.md` links remain in either file**

Run: `grep -nE "\]\((\.?/)?docs/[a-z0-9/_-]+\.md" README.md CONTRIBUTING.md || echo "clean"`
Expected: `clean`.

- [ ] **Step 3: Commit**

```bash
git add README.md CONTRIBUTING.md
git commit -m "docs: point README and CONTRIBUTING links to runlore.io"
```

---

### Task 7 (gated): Architecture diagram in draw.io, ogenki style

> **Gate:** needs an ogenki `.drawio` reference (palette/shapes) from the owner. **Not required for launch** — Hextra renders the existing Mermaid diagram in `concepts/architecture.md` natively, so the page is complete without this task. Do this task only once the reference is provided.

**Files:**
- Create: `website/content/docs/concepts/architecture/runlore-architecture.drawio` (source of truth)
- Create: `website/content/docs/concepts/architecture/runlore-architecture.svg` (exported)
- Modify: `website/content/docs/concepts/architecture.md` (embed the SVG in place of the Mermaid block; keep the "Reading it" prose)

- [ ] **Step 1: Redraw the React → Investigate → Learn flow** (nodes/edges are the existing Mermaid graph in the migrated `architecture.md`) as a `.drawio`, matching the ogenki palette/shapes from the provided reference.

- [ ] **Step 2: Export a theme-friendly SVG** next to the source (draw.io desktop CLI or the drawio-skill), embed it, and cite the `.drawio` source path in-page.

- [ ] **Step 3: Build and verify** — `cd website && hugo --gc && cd ..` → `0` errors; the SVG renders on `/docs/concepts/architecture/`.

- [ ] **Step 4: Commit**

```bash
git add website/content/docs/concepts/architecture*
git commit -m "docs: architecture diagram in draw.io (ogenki style)"
```

---

## Final verification (after Tasks 1–6)

- [ ] `cd website && hugo --minify --gc && cd ..` → `0` errors, `0` `REF_NOT_FOUND` warnings.
- [ ] `go build ./...` at repo root → exits 0 (nested `website/go.mod` isolated).
- [ ] `grep -rE "\]\([a-z0-9-]+\.md" website/content/docs || echo clean` → `clean`.
- [ ] `grep -nE "\]\((\.?/)?docs/[a-z0-9/_-]+\.md" README.md CONTRIBUTING.md || echo clean` → `clean`.
- [ ] Local smoke test: `cd website && hugo server` → homepage shows hero + 4 cards; sidebar lists Getting Started (top) + 5 sections; search box returns hits for e.g. "redaction".

## Owner one-time manual steps (outside this plan)

1. Repo **Settings → Pages → Source = "GitHub Actions"**.
2. DNS for `runlore.io` → GitHub Pages apex `A` records `185.199.108.153`, `185.199.109.153`, `185.199.110.153`, `185.199.111.153` (optional `AAAA` `2606:50c0:8000::153`…`8003::153`).
3. Settings → Pages → **Custom domain = `runlore.io`**, enable **Enforce HTTPS** once DNS propagates.
