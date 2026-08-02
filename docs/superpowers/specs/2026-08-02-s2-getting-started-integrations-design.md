# Design — S2: Getting Started restructure + integrations browser

- **Date:** 2026-08-02
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Source:** improvement report §2.3, §9.2, §9.4, §9.5 + owner correction
- **Depends on:** [S1](2026-08-02-s1-funnel-demo-cli-design.md) (tier 1 is S1's keyless demo)
- **Index:** [decomposition](2026-08-02-improvement-report-decomposition.md)

## Problem

`getting-started.md` is 624 lines and presents the **hardest** path as the only path.

- Prerequisites open with "**Required:** a Kubernetes cluster running Flux or Argo CD" — which the
  README directly contradicts ("GitOps isn't required: every data source is pluggable"). Report §9.2.
- The production path needs a cluster **+** Flux/Argo **+** an LLM endpoint **+** a GitHub App **+** a
  KB repo **+** a notifier. Six multiplicative prerequisites; at 70% retention per step that is 12%.
- Step 4's "complete production-style example" `values.yaml` runs ~180 lines (`getting-started.md:247–425`).
  It is presented as the golden path. Report §9.5.
- Two links point at repo paths through Hugo, so they render as broken site URLs:
  `getting-started.md:243` (`../deploy/helm/runlore/values-minimal.yaml`) and `benchmarking.md:38`
  (`../eval/compare.example.yaml`). Report §9.4.
- Every non-default integration — Loki, Matrix, PagerDuty, Hubble, AWS, GCP, Gemini, Anthropic,
  custom webhooks — is inlined as commented YAML inside that one page. There is no way to *browse*
  what RunLore integrates with, and nothing for a search engine to rank for "runlore loki".

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Page structure | Three escalating tiers, each independently complete and useful | Report §2.3 — a reader who stops at tier 1 still got value |
| Tier-3 stack | **Prometheus + GitHub + OpenAI-compatible LLM + Slack only** | Owner's correction. One opinionated path is followable; a matrix of options is not |
| Everything else | Moves to `/docs/integrations/` | Owner's correction |
| Integrations IA | Section index with card grids + **one page per integration** | Owner's choice. Each integration gets a landing page worth ranking |
| Values profiles | `values-minimal.yaml` / `values-standard.yaml` / `values-full.yaml`, all schema-validated in CI | Report §9.5. The chart's own `values.yaml` stays the chart default — it is not an example |
| GitOps prerequisite | `Required` → `Recommended` | The README's framing is the correct one for adoption, and it matches the code (an unset source disables its tool) |
| Repo links | Absolute GitHub URLs | The site is not the repo; `relref` cannot reach files outside `content/` |
| Doc/code drift | A reflection test over the real registries | Owner's standing preference: pin docs that restate code facts with a reflection test, then mutation-test the guard |

## Scope

### In scope

1. Rewrite `website/content/docs/getting-started.md` into three tiers.
2. New `/docs/integrations/` section: `_index.md` with five card grids + 23 integration pages
   (S5 adds 3 more: Grafana, Elasticsearch, GitLab).
3. `deploy/helm/runlore/values-standard.yaml` and `values-full.yaml`; trim `values-minimal.yaml`
   to the ~15-line shape the report asks for.
4. Extend `internal/config/minimal_values_test.go` to validate all three profiles against
   `values.schema.json`.
5. Fix report §9.2 and §9.4.
6. `internal/docsguard` — the integration-page coverage test.
7. Sidebar weights so Integrations sits directly under Getting Started.

### Non-goals (YAGNI)

- Rewriting `configuration.md` — it stays the exhaustive key reference every integration page
  deep-links into.
- Per-integration screenshots.
- Translating any page.
- Merging or splitting the Concepts / Operations / Security sections.

## Design

### The three tiers

| Tier | Needs | Gets | Where |
|---|---|---|---|
| **1 · Try** | Go **or** `curl \| sh` | A real RCA on recorded evidence, keyless, ~60s | Top of Getting Started, above prerequisites |
| **2 · Investigate** | kubeconfig + one API key | `lore investigate` against your live cluster, output in the terminal | Second section |
| **3 · Learn** | Helm + KB repo + GitHub App + Slack | The full loop — the differentiator | The rest of the page |

Tier 1 and 2 are S1's deliverables; this spec only places and documents them. Tier 3 keeps the
existing step numbering so external links survive, but every non-standard-stack option is replaced
with a one-line pointer into `/docs/integrations/`.

### Tier 3's `values.yaml`

The page shows **`values-minimal.yaml` (~15 lines) first**, then a collapsed/linked
`values-standard.yaml` for the KB + curation loop, and links `values-full.yaml` rather than
inlining it. Profiles:

| Profile | Contains |
|---|---|
| `values-minimal` | image, model, one notifier. Investigate + notify. No KB, no curation |
| `values-standard` | + catalog git-sync, forge/GitHub App, webhook token, metrics + logs URLs |
| `values-full` | + HA, persistence, network policy, actions ladder, cloud, network flows, instant recall |

### `/docs/integrations/`

Index page: five `hextra/feature-grid` card grids — **Triggers · LLM providers · Data sources ·
Notifications · Forge** — each card linking to its own page.

| Group | Pages (at S2 merge) | Added by S5 |
|---|---|---|
| Triggers | alertmanager, gitops, pagerduty, custom-webhook | grafana |
| LLM providers | openai-compatible, anthropic, gemini, local-keyless (vLLM/Ollama) | — |
| Data sources | prometheus-victoriametrics, victorialogs, loki, kubernetes, hubble, aws-vpc-flow-logs, gcp-firewall-logs, aws-cloud, source-repos, mcp | elasticsearch |
| Notifications | slack, matrix, webhook, templated | — |
| Forge | github | gitlab |

Every page follows one template, deliberately short:

```markdown
---
title: Grafana Loki
weight: 30
integration: {kind: logs, id: loki}     # consumed by the drift guard
---

**What it gives you** — which tools this enables, in one sentence.

## Minimal config          # the smallest block that works
## Verify it locally       # the exact commands, incl. a container recipe where relevant
## Notes                   # gotchas, field defaults, parity caveats
## Reference               # deep link into configuration.md / data-sources.md
```

The "Verify it locally" block is what makes the owner's *test each integration locally* rule
durable: the recipe an integration ships with is the recipe a user follows.

### The drift guard

`internal/docsguard/integration_pages_test.go` reflects over the **real** parse targets, not a
hand-maintained list:

- `source.Registered()` → one page per registered source descriptor.
- `notify.Registered()` → one page per notifier (this spec adds `Registered()` to
  `internal/notify/registry.go`, mirroring `internal/source`).
- The logs provider constants in `internal/logs/detect.go` → one page each.

It asserts both directions: every registered integration has a page, and every page's
`integration:` front-matter id resolves to something registered. A new notifier without a docs page
fails CI; a page for a deleted provider fails CI. The guard is mutation-tested during
implementation — add a fake registry entry, confirm red, revert.

The guard covers exactly what has a runtime registry — sources, notifiers, logs providers. LLM,
cloud, network and MCP pages have no equivalent registry to reflect over and are therefore
maintained by hand; the guard ignores pages whose `integration.kind` is not one of the three
registered kinds, so it never produces a false failure on them.

## Testing

| Test | Guards |
|---|---|
| `TestIntegrationPagesCoverRegistries` | the drift guard, both directions |
| `TestShippedValuesProfilesValidate` | all three profiles parse and satisfy `values.schema.json` |
| `hugo --gc --minify` in `docs-check.yml` | `refLinksErrorLevel: ERROR` already fails the build on any unresolved `relref` |
| Link check | no site-relative link resolves to a repo path |
| Manual | read tier 1 → tier 3 top to bottom on a clean browser; each tier stands alone |

## Risks

| Risk | Mitigation |
|---|---|
| 22 thin pages read as filler | Each carries a *working* minimal config and a *runnable* local verification — content a dense reference page cannot give |
| Restructure breaks inbound links to Getting Started anchors | Keep existing step anchors; add new ones rather than renaming |
| The integrations section drifts from the code | The reflection guard, mutation-tested |
| Tier 3 gets thinner and loses production guidance | `values-full.yaml` + the Harden-for-production section stay; only *options* move out, not *guidance* |

## Amendments from wave-1 execution (added 2026-08-02, after S1/S3/S4 shipped)

Five things surfaced during wave 1 that S2 must incorporate. They were not knowable when this
spec was written.

1. **`getting-started.md:19-22` is now factually wrong.** It describes `hack/demo.sh` as running
   `lore serve` with mocked Alertmanager alerts through the trigger policy ("just Go + `curl`").
   S1 replaced that: `hack/demo.sh` now replays a recorded investigation and renders a real
   verdict card, and the trigger-policy demo moved to `hack/demo-trigger-policy.sh`. Two S1
   reviewers flagged this as correctly out of S1's scope and explicitly S2's to fix. **Tier 1 of
   the new Getting Started is exactly this command**, so the rewrite fixes it by construction —
   but do not leave the old wording anywhere.

2. **The LLM integration pages must carry verified compatibility, not just config.** Two providers
   are broken against RunLore, both found by running them:
   - **glm-4.6** stalls indefinitely on forced `tool_choice` ([#391]) — it emits one
     `reasoning_content` delta and never completes. This silently degrades the adversarial verify
     pass, the recall reranker, the eval judge, KB semantic validation and `kb import --model`.
     **glm-4.5-air** answers the identical request in ~5s and is the verified-working choice.
   - **Gemini 3.x** is unusable ([#392]) — the client never replays `thought_signature` on
     functionCall parts, so investigations fail with a 400 on the *second* turn. Gemini 2.5 works.
   A "Verify it locally" block that does not surface these would send readers straight into them.
   The openai-compatible page should state the forced-`tool_choice` requirement explicitly, since
   that is the capability that separates a working endpoint from a broken one.

3. **`lore investigate` changed underneath tier 2.** It now runs with no `runlore.yaml`,
   synthesizing config from `OPENAI_BASE_URL`/`OPENAI_API_KEY`/`OPENAI_MODEL` or
   `ANTHROPIC_API_KEY`, and accepts `--model`/`--base-url`/`--metrics-url`/`--logs-url`. It also
   prints a stderr notice naming which signals are disabled. Tier 2 should lead with the
   zero-config form — it is now genuinely one command — and explain the notice, so a thin answer
   reads as under-configured rather than as the product being weak.

4. **The eval scorecard is live.** `runlore.io/eval` renders the published nightly and the README
   badge is green. Anywhere Getting Started or the integrations pages gesture at "how good is
   it", link `/eval` rather than describing it.

5. **Hugo version floor.** `hugo.yaml` requires ≥ 0.146.0 and Hextra needs the `try` template
   func. A stale `hugo` on PATH fails with `function "try" not defined`. Worth a line in
   CONTRIBUTING or the docs-contribution notes, since every website task in wave 1 hit it.

## Acceptance criteria

1. Getting Started opens with a keyless tier-1 path; nothing above it requires a cluster.
2. Prerequisites list Flux/Argo CD under **Recommended**, and the page no longer contradicts the README.
3. No site-relative link points at a repo path.
4. The first `values.yaml` a reader sees is ≤ 20 lines.
5. `/docs/integrations/` lists every registered trigger, LLM, data source, notifier and forge, each
   linking to its own page with a runnable local-verification recipe.
6. `TestIntegrationPagesCoverRegistries` fails when an integration is added without a page (proven
   by mutation, reverted).
7. `hugo` builds clean; `go test ./...` and `golangci-lint` pass.
</content>
