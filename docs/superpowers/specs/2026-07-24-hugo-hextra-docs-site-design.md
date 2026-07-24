# Design — Hugo + Hextra documentation site for RunLore

- **Date:** 2026-07-24
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Target URL:** https://runlore.io/ (GitHub Pages, custom apex domain)

## Problem

RunLore has a book-sized documentation corpus — 17 Markdown files, ~4,450 lines under
`docs/` — but it is only browsable as raw files on GitHub: no search, no navigation
sidebar, no landing page, weak SEO. This undersells a project positioning itself as the
first-mover OKF-producing SRE agent.

Goal: publish a real documentation website that (a) opens on a quick **overview**, (b)
surfaces **Getting Started** immediately, and (c) provides a **search bar** — built with
Hugo + the Hextra theme, deployed to GitHub Pages at `runlore.io`.

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Generator | Hugo | Go-native, single binary, matches project ethos; author already runs Hugo for blog.ogenki.io |
| Theme | Hextra | Docs-oriented, built-in FlexSearch (satisfies the search-bar requirement natively), dark mode, no Node toolchain (unlike Docsy) |
| Hosting | GitHub Pages | Free, native to the repo, deploy via Actions, no external account |
| Domain | `runlore.io` (apex) | `baseURL = https://runlore.io/`, `CNAME` file = `runlore.io` |
| Content layout | Sectioned site under `website/` | `git mv` docs into grouped sections; cleanest sidebar/IA; Hugo confined to `website/` |
| Hextra install | Hugo Modules | Standard for Hextra; creates an isolated `website/go.mod` that the Go build ignores |
| Versioning | Deferred (non-goal) | Pre-1.0, active development; ship "latest" only, add a `mike`-equivalent later |
| Diagrams | draw.io, ogenki style | `.drawio` source is truth, exported `.svg` embedded |

## Scope

### In scope
1. Scaffold a Hugo + Hextra site under `website/` (Hextra via Hugo Modules → `website/go.mod`).
2. Migrate all 17 docs into 6 sections with front matter (`title`, `weight`) + section `_index.md` files, via `git mv` (history preserved).
3. Homepage (`content/_index.md`, `hextra-home` layout): overview + prominent Getting Started CTA.
4. Search bar via Hextra's built-in FlexSearch (config only).
5. Fix cross-section `.md` links during the move + add a `render-link` render hook as a safety net.
6. GitHub Actions workflow to build and deploy to GitHub Pages; `static/CNAME` = `runlore.io`.
7. Repoint user-facing `docs/*.md` links in `README.md` and `CONTRIBUTING.md` to `runlore.io`.
8. `.gitignore` Hugo build artifacts (`website/public/`, `website/resources/`, `website/.hugo_build.lock`).
9. Establish the draw.io diagram convention (ogenki style) and redraw the existing architecture
   diagram as the pattern-setter.

### Non-goals (YAGNI)
- Docs versioning (add later if/when releases diverge).
- Prose rewriting / copyediting — this is a **structural** migration; page bodies are moved as-is.
- i18n / translations.
- Blog section on the docs site.
- Custom theme development beyond Hextra config + logo + colors.
- Comments, analytics, search telemetry.

## Information architecture

Sidebar order is weight-driven; Getting Started sits at the top so it is visible immediately.

| Section | Weight | Pages migrated in |
|---|---|---|
| Getting Started | 10 | `getting-started` |
| Concepts | 20 | `design`, `architecture/runlore-architecture`, `learning-loop`, `data-sources`, `reviewing-knowledge`, `prior-art` |
| Configuration | 30 | `configuration`, `mcp` |
| Operations | 40 | `observability`, `troubleshooting`, `upgrade-uninstall` |
| Security | 50 | `security-model`, `security-architecture` |
| Reference | 60 | `kb-steward`, `benchmarking`, `examples/harbor-registry-down` |

Each section directory gets an `_index.md` with a one-line intro and its `weight`.

### Resulting tree (target)

```
website/
  go.mod                       # Hextra Hugo module (isolated from the Go build)
  hugo.yaml                    # baseURL https://runlore.io/, Hextra config, search on
  content/
    _index.md                  # homepage (hextra-home): hero + overview + cards + CTAs
    docs/
      _index.md
      getting-started/_index.md
      concepts/{_index, design, architecture, learning-loop, data-sources,
                reviewing-knowledge, prior-art}.md
      configuration/{_index, configuration, mcp}.md
      operations/{_index, observability, troubleshooting, upgrade-uninstall}.md
      security/{_index, security-model, security-architecture}.md
      reference/{_index, kb-steward, benchmarking}.md
      reference/examples/harbor-registry-down.md
  layouts/_default/_markup/render-link.html   # resolve internal .md links
  static/CNAME                 # runlore.io
  assets/                      # logo (reuse repo assets/logo*.png)
.github/workflows/docs.yml
```

The top-level `docs/` product pages are folded into `website/`. `docs/superpowers/` (specs)
and any non-product docs stay put and are **not** part of the site.

## Homepage

`content/_index.md` using Hextra's `hextra-home` layout:

```
┌────────────────────────────────────────────────┐
│                 [RunLore logo]                   │
│   An open-source SRE agent that investigates     │
│   incidents — and remembers what it learns.      │
│        [ Get Started → ]    [ GitHub ]           │
├────────────────────────────────────────────────┤
│  What is RunLore  (2–3 sentence overview)        │
├────────────────────────────────────────────────┤
│ ┌───────┐┌───────┐┌────────┐┌────────┐          │
│ │Learns ││Read-  ││GitOps- ││Your     │  cards   │
│ │your   ││only,  ││native  ││models,  │          │
│ │platfrm││Git PRs││Flux+Argo││1 binary │          │
│ └───────┘└───────┘└────────┘└────────┘          │
├────────────────────────────────────────────────┤
│  How it works:  Investigate → Open PR → Recall   │
└────────────────────────────────────────────────┘
```

All copy is pulled from the existing `README.md` — no new marketing prose is invented.
Primary CTA links to Getting Started; secondary to the GitHub repo.

## Cross-links & search

- **Search:** Hextra's built-in FlexSearch enabled via config → navbar search box, full-text,
  client-side, zero external dependencies.
- **Links:** the finite set of cross-section `](foo.md)` links (heaviest: 12 → `security-model`)
  is fixed during the move. A `layouts/_default/_markup/render-link.html` render hook resolves
  any remaining internal `.md` link to its built page.
- **Drift guard:** Hugo builds with `refLinksErrorLevel = ERROR` so a broken internal reference
  **fails the build** (and thus CI) rather than shipping a dead link.

## Diagrams (draw.io, ogenki style)

- Convention: the `.drawio` source lives beside its page; a theme-aware `.svg` is exported into
  the same directory and embedded; the `.drawio` is the tracked source of truth and is cited
  in-page.
- This work redraws the existing `runlore-architecture` diagram in ogenki style as the
  pattern-setter. Additional diagrams are incremental follow-ups (need an ogenki `.drawio`
  reference for palette/shapes).

## Deployment

`.github/workflows/docs.yml`:
- **Triggers:** push to `main` touching `website/**` or the workflow file; `workflow_dispatch`.
- **Permissions:** `pages: write`, `id-token: write`, `contents: read`; concurrency group so
  deploys don't overlap.
- **Steps:** checkout → setup-go → setup-hugo (extended) → `hugo mod get` → `hugo --minify --gc`
  (cwd `website/`) → `actions/upload-pages-artifact` (`website/public`) → `actions/deploy-pages`.
- `static/CNAME` carries `runlore.io` into the published site.

### One-time manual steps (owner)
1. Repo Settings → Pages → Source = **GitHub Actions**.
2. DNS for `runlore.io` → GitHub Pages:
   - Apex `A` records: `185.199.108.153`, `185.199.109.153`, `185.199.110.153`, `185.199.111.153`
   - Apex `AAAA` (optional): `2606:50c0:8000::153`, `…8001::153`, `…8002::153`, `…8003::153`
   - (or a single `ALIAS`/`ANAME` to `Smana.github.io` if the DNS provider supports it)

## Verification

- `hugo --minify --gc` (in `website/`) builds clean with **zero** broken-ref warnings.
- Every migrated page is reachable from the sidebar; search returns hits; homepage CTAs resolve.
- `go build ./...` at the repo root stays green — confirms `website/go.mod` is isolated from the
  Go module and does not affect the binary build or `golangci-lint`.
- README/CONTRIBUTING links resolve to live `runlore.io` pages.

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| Nested `website/go.mod` confuses Go tooling | Go stops at module boundaries; verify `go build ./...` still green in Verification |
| Reorg breaks cross-links | `refLinksErrorLevel = ERROR` fails the build on any dead internal ref; render hook resolves `.md` links |
| README links go stale after `git mv` | Explicit task to repoint README/CONTRIBUTING to `runlore.io` |
| DNS/Pages not yet configured → 404 on custom domain | Documented one-time manual steps; site still builds and can be validated on the Actions artifact first |
