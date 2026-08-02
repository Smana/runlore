# S2 — Getting Started restructure + integrations browser — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn a 624-line page that presents the hardest path as the only path into three escalating tiers, and give every integration a page worth landing on.

**Architecture:** Getting Started becomes Try → Investigate → Learn, each independently complete. Tier 3 documents exactly one opinionated stack (Prometheus + GitHub + an OpenAI-compatible LLM + Slack); every other option moves to a new `/docs/integrations/` section with one short page per integration. A reflection test over the runtime registries keeps that section from drifting out of sync with the code.

**Tech Stack:** Hugo + Hextra (`website/`), Go (the drift guard), Helm values profiles.

**Spec:** [`docs/superpowers/specs/2026-08-02-s2-getting-started-integrations-design.md`](../specs/2026-08-02-s2-getting-started-integrations-design.md) — **read its "Amendments from wave-1 execution" section**, which carries five findings this plan depends on.

## Global Constraints

- **Use `/usr/bin/hugo`** — the `hugo` on PATH is mise-global 0.139.0 and cannot build this site (`hugo.yaml` sets `hugoVersion.min: 0.146.0`; Hextra needs the `try` func). `/usr/bin/hugo` is v0.164.0.
- Internal links use `{{< relref >}}`; `refLinksErrorLevel: ERROR` fails the build on a bad ref. Repo files use absolute GitHub URLs — `relref` cannot reach outside `content/`.
- Every new `.go` file starts with `// SPDX-License-Identifier: Apache-2.0`; `golangci-lint run` must pass; no new third-party dependencies.
- Comments explain *why*; this codebase comments heavily and deliberately.
- **Do not weaken any published claim to make a page shorter.** Where a caveat exists it moves, it does not vanish.
- Conventional Commits. **Never** add a co-author trailer or any AI attribution.

## Wave-1 facts this plan depends on (verified, do not re-derive)

- `hack/demo.sh` now replays a recorded investigation and renders a real verdict card with **no cluster, no API key, no network**. The trigger-policy demo moved to `hack/demo-trigger-policy.sh`.
- `lore investigate` runs with **no `runlore.yaml`**, synthesizing from `OPENAI_BASE_URL`/`OPENAI_API_KEY`/`OPENAI_MODEL` or `ANTHROPIC_API_KEY`; flags `--model`, `--base-url`, `--metrics-url`, `--logs-url`; and it prints a stderr notice naming disabled signals.
- `install.sh` is served at `runlore.io/install.sh` and verifies the release checksum (plus the cosign signature when cosign is present).
- **glm-4.6 stalls on forced `tool_choice`** (#391) — breaks verify, the recall reranker, the eval judge. **glm-4.5-air works.**
- **Gemini 3.x is unusable** (#392) — the client omits `thought_signature`; investigations fail on turn 2. Gemini 2.5 works.
- The nightly scorecard is live at `runlore.io/eval`.

---

### Task 1: The `/docs/integrations/` section scaffold and its drift guard

**Files:**
- Create: `website/content/docs/integrations/_index.md`
- Create: `internal/notify/registry.go` — add `Registered()` (mirroring `internal/source`)
- Create: `internal/docsguard/integration_pages_test.go`
- Create: `internal/docsguard/doc.go`

**Interfaces:**
- Consumes: `source.Registered() []source.Descriptor` (`internal/source/registry.go:108`), the logs provider constants in `internal/logs/detect.go`.
- Produces: `notify.Registered() []notify.Descriptor`; the `integration:` front-matter contract every page in Task 2/3 must satisfy.

- [ ] **Step 1: Add `notify.Registered()`**

`internal/source/registry.go:108` already exposes `Registered()`. `internal/notify/registry.go` has `Register` but no accessor. Add the mirror image, with a comment saying why it exists (a docs guard needs to enumerate what is wired):

```go
// Registered returns every registered notifier descriptor, sorted by name.
// It exists so a docs drift guard can enumerate what is actually wired rather
// than trusting a hand-maintained list — the same reason source.Registered does.
func Registered() []Descriptor { ... }
```

Match `source.Registered`'s shape exactly (read it first — sorted? copy-on-return?).

- [ ] **Step 2: Write the failing guard**

Create `internal/docsguard/integration_pages_test.go`. It walks `website/content/docs/integrations/**.md`, parses each page's `integration:` front matter (`kind` + `id`), and asserts **both directions**:

- every registered source / notifier / logs provider has a page;
- every page whose `kind` is one of those three resolves to something registered.

Pages whose `kind` is not a registered kind (`llm`, `cloud`, `network`, `mcp` — no runtime registry to reflect over) are ignored, so the guard never produces a false failure on them. Say that in a comment.

Run it: `go test ./internal/docsguard/`
Expected: FAIL — no pages exist yet.

- [ ] **Step 3: Write the section index**

`website/content/docs/integrations/_index.md` with `title: Integrations`, `weight: 15` (directly under Getting Started at 10), and five `hextra/feature-grid` card grids: **Triggers · LLM providers · Data sources · Notifications · Forge**. Cards link to the pages Tasks 2–3 create.

Add a short lead paragraph stating the rule that makes this section coherent: *presence is enablement* — a key under `sources.<name>` turns that source on, and an unset data source simply disables its tool.

- [ ] **Step 4: Commit**

```bash
git add internal/notify/registry.go internal/docsguard/ website/content/docs/integrations/_index.md
git commit -m "feat(docs): integrations section scaffold with a registry-reflection drift guard"
```

(The guard stays red until Task 3 completes. That is expected and stated in the commit body — this is the one task in this plan that lands red, because splitting a guard from the thing it guards across two commits is worse than a short red window. If you would rather it be green, write the guard in Task 3 instead and say so in your report.)

---

### Task 2: Integration pages — triggers, notifications, forge

**Files:** create under `website/content/docs/integrations/`:
`alertmanager.md`, `gitops.md`, `pagerduty.md`, `custom-webhook.md`, `slack.md`, `matrix.md`, `webhook.md`, `templated.md`, `github.md`

**Interfaces:** each page carries front matter `title`, `weight`, and `integration: {kind: <source|notifier|forge>, id: <registered id>}`.

- [ ] **Step 1: Confirm the registered ids**

```bash
grep -rn "Register(Descriptor{" -A 3 internal/source/*.go internal/source/*/*.go | grep -i "name" | head
grep -rn "Register(Descriptor{" -A 3 internal/notify/*.go internal/notify/*/*.go | grep -i "name" | head
```
The `id` in front matter must equal the registered name exactly, or the guard fails. Use what you find, not what this plan guesses.

- [ ] **Step 2: Write the pages**

One template, deliberately short — a reader should be able to wire an integration from this page alone:

```markdown
---
title: Grafana Loki
weight: 30
integration: {kind: logs, id: loki}
---

**What it gives you** — one sentence naming the tools this enables.

## Minimal config
```yaml
# the smallest block that works
```

## Verify it locally
Exact commands. Include a container recipe where one applies.

## Notes
Gotchas, field defaults, parity caveats.

## Reference
Deep link into configuration.md / data-sources.md via {{< relref >}}.
```

Source the content by **moving** it out of `getting-started.md` and `data-sources.md` rather than paraphrasing — those texts are already accurate and reviewed. Where you move a caveat, it must survive intact.

- [ ] **Step 3: Build and commit**

```bash
cd website && /usr/bin/hugo --gc --minify
git commit -m "docs(integrations): trigger, notification and forge pages"
```

---

### Task 3: Integration pages — LLM providers and data sources

**Files:** create under `website/content/docs/integrations/`:
`openai-compatible.md`, `anthropic.md`, `gemini.md`, `local-keyless.md`,
`prometheus.md`, `victorialogs.md`, `loki.md`, `kubernetes.md`, `hubble.md`,
`aws-vpc-flow-logs.md`, `gcp-firewall-logs.md`, `aws-cloud.md`, `source-repos.md`, `mcp.md`

- [ ] **Step 1: The LLM pages must carry verified compatibility, not just config**

This is the substantive part. Two providers are broken against RunLore and a reader wiring one up will hit it within minutes:

- **`openai-compatible.md`** must state that the endpoint has to support **forced `tool_choice`** (the named-function form). That single capability separates a working endpoint from a broken one: RunLore uses it for the adversarial verify pass, the recall reranker, the eval judge, KB semantic validation and `kb import --model`. Name **glm-4.6 as a known-broken example** (it emits one `reasoning_content` delta then stalls indefinitely — [#391]) and **glm-4.5-air as verified working**.
- **`gemini.md`** must state that **Gemini 3.x does not work** — the client omits the `thought_signature` that Gemini 3 requires on function-call parts, so investigations fail with a 400 on the second turn ([#392]) — and that **Gemini 2.5 works**.

Link the issues. Do not soften these into "may not work"; they were reproduced.

- [ ] **Step 2: Write the data-source pages**

Move content from `data-sources.md`, preserving its caveats — particularly the Loki parity notes (`logs_error_summary` client-side aggregation, `detected_level` absence on Loki 2.x) and the IP-based caveat on the AWS/GCP network providers.

- [ ] **Step 3: The guard must now pass**

Run: `go test ./internal/docsguard/ -v`
Expected: PASS.

Then **mutation-test it**: add a fake descriptor to one registry, confirm the guard fails naming the missing page, revert. Report both outcomes.

- [ ] **Step 4: Build and commit**

---

### Task 4: Rewrite Getting Started into three tiers

**Files:** rewrite `website/content/docs/getting-started.md`

- [ ] **Step 1: Tier 1 — Try (no cluster, no keys)**

Opens the page, above the prerequisites. `curl -fsSL https://runlore.io/install.sh | sh` (or `go install`), then `lore demo investigate --offline default`. Show the real card. State that the model turns are recorded and the card discloses when and with which model.

**This replaces the current `getting-started.md:19-22` block**, which still describes the old `hack/demo.sh` (`lore serve` + mocked Alertmanager + "just Go + curl"). That text is now factually wrong.

- [ ] **Step 2: Tier 2 — Investigate (kubeconfig + one API key)**

`lore investigate --alert "<symptom>" --namespace <ns>` with **no config file**. Document the env vars, the four flags, and the stderr degradation notice — explaining that it means *under-configured*, not *broken*.

- [ ] **Step 3: Tier 3 — Learn (the full loop)**

Keep the existing step numbering so inbound anchors survive. Document exactly one stack: **Prometheus + GitHub + an OpenAI-compatible LLM + Slack**. Every other option becomes a one-line pointer into `/docs/integrations/`.

Fix the two §9 bugs here:
- Flux/Argo CD moves from **Required** to **Recommended** (the README's framing is the correct one and matches the code — an unset source disables its tool).
- `../deploy/helm/runlore/values-minimal.yaml` and any other repo-relative link become absolute GitHub URLs.

- [ ] **Step 4: Build, link-check, commit**

```bash
cd website && /usr/bin/hugo --gc --minify
grep -rn "](\.\./" website/content/docs/getting-started.md   # must return nothing
```

---

### Task 5: Values profiles

**Files:**
- Modify: `deploy/helm/runlore/values-minimal.yaml` (trim to ~15 lines)
- Create: `deploy/helm/runlore/values-standard.yaml`, `values-full.yaml`
- Modify: `internal/config/minimal_values_test.go`

- [ ] **Step 1: Extend the schema test first**

`internal/config/minimal_values_test.go` validates one profile against `values.schema.json`. Generalize it to a table over all three, so a profile that drifts from the schema fails CI. Run it — it should fail on the two files that do not exist yet.

- [ ] **Step 2: Write the profiles**

| Profile | Contains |
|---|---|
| `values-minimal` | image, model, one notifier. Investigate + notify. **~15 lines.** |
| `values-standard` | + catalog git-sync, forge/GitHub App, webhook token, metrics + logs |
| `values-full` | + HA, persistence, NetworkPolicy, actions ladder, cloud, network flows, instant recall |

- [ ] **Step 3: Point Getting Started at them**

Tier 3 shows `values-minimal` **inline** (it is short enough), links `values-standard`, and links `values-full` rather than inlining ~180 lines.

- [ ] **Step 4: Run the tests and commit**

---

### Task 6: Cross-link and retire stale references

**Files:** `website/content/docs/concepts/data-sources.md`, `website/content/docs/configuration/configuration.md`, `README.md`, `CONTRIBUTING.md`

- [ ] **Step 1: Leave forwarding pointers**

Where content moved out of `data-sources.md`, leave a one-line pointer to the integration page rather than a gap. `configuration.md` stays the exhaustive key reference every integration page deep-links into — do not trim it.

- [ ] **Step 2: Sweep for stale text**

```bash
grep -rn "hack/demo.sh" --include="*.md" . | grep -v CHANGELOG
grep -rn "Required" website/content/docs/getting-started.md | head
```
Every hit must describe current behaviour.

- [ ] **Step 3: Add the Hugo version note**

Every website task in wave 1 hit the stale-`hugo` failure. Add a line to `CONTRIBUTING.md`: the site needs Hugo ≥ 0.146.0 (Hextra uses the `try` template func); an older binary fails with `function "try" not defined`.

- [ ] **Step 4: Final build and commit**

---

## Final verification

- [ ] `cd website && /usr/bin/hugo --gc --minify` builds clean
- [ ] `go test ./... && golangci-lint run` clean
- [ ] The drift guard fails when an integration is added without a page (proven by mutation, reverted)
- [ ] No `](../` repo-relative link remains in `getting-started.md`
- [ ] The first `values.yaml` a reader sees is ≤ 20 lines
- [ ] Tier 1 requires no cluster and no key; tier 2 requires no config file
- [ ] `/docs/integrations/` lists every registered trigger, LLM, data source, notifier and forge
- [ ] Open the PR — English title and description, no AI attribution, no co-author trailers
</content>
