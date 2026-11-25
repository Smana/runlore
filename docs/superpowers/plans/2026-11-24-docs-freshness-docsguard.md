# Public Docs Freshness (`v0.20.0`) & `README.md` `docsguard` Implementation Plan

> **Status:** Implemented (PR #625, merged 2026-11-24)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring `README.md`, `ROADMAP.md`, `website/content/docs/concepts/design.md`, and `website/content/docs/integrations/_index.md` into sync with `v0.20.0`, remove duplicate unguarded integration/package lists from `design.md`, and add a `docsguard` test ensuring `README.md`'s integrations table can never silently drift again.

**Architecture:** `internal/docsguard/integration_pages_test.go` already walks every `integration: {kind, id}` page under `website/content/docs/integrations/`. Extend it with `TestReadmeListsAllIntegrationPages` which extracts `README.md`'s `Supported integrations` section and asserts every non-recipe integration page's canonical `https://runlore.io/docs/integrations/<rel>/` URL appears there. Deduplicate `design.md` §7 and §13 by linking to `integrations/_index.md` and `AGENTS.md`.

**Tech Stack:** Go 1.26 (`testing`, `filepath`, `strings`), Markdown / Hugo docs.

**Spec:** [`docs/superpowers/specs/2026-11-24-docs-freshness-docsguard-design.md`](../specs/2026-11-24-docs-freshness-docsguard-design.md)

## Global Constraints

- **TDD:** Write `TestReadmeListsAllIntegrationPages` before editing `README.md` and confirm it catches the existing omissions (Loki, Elasticsearch, OpenSearch, GitLab, Grafana, custom-webhook, templated, etc.).
- **Guard the guard:** Fail loudly if the `Supported integrations` heading disappears, if `checked == 0`, or if any entry in `readmeSkippedIntegrationIDs` stops matching a real page.
- **No codegen.** `README.md` remains hand-edited prose guarded by canonical URL presence.
- **Keep `ROADMAP.md` honest:** only remove items genuinely shipped in the tree; do not add untracked speculative items without GitHub issues.
- **Check cross-page anchors:** verify existing `internal/docsguard` tests (config fences, anchors, links) remain green.
- **Quality gate before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`.
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/docsguard/integration_pages_test.go` (modify) | Add `TestReadmeListsAllIntegrationPages` + helper to extract `README.md`'s `Supported integrations` section |
| `README.md` (modify) | Update `## 🔌 Supported integrations` with clickable canonical URLs for all shipped integrations + concise rows/links for `v0.15–v0.20` capabilities; refresh `## Project status & stability` |
| `ROADMAP.md` (modify) | Remove the 4 already-shipped items from `## Next` and `## Later` |
| `website/content/docs/concepts/design.md` (modify) | Replace §7 provider table with `relref` to `integrations/_index.md`; replace §13 ASCII tree with link to `AGENTS.md` |
| `website/content/docs/integrations/_index.md` (modify) | Fix stale Gemini card subtitle (line 45) |

---

## Task 1: Add failing `docsguard` test pinning `README.md` to integration pages

**Files:**
- Modify: `internal/docsguard/integration_pages_test.go`

- [x] **Step 1: Add `TestReadmeListsAllIntegrationPages` and `readmeSection` helper**

In `internal/docsguard/integration_pages_test.go`:
- Define `readmeSkippedIntegrationIDs` (`"grafana-mcp": true`, `"local-keyless": true`).
- Add helper `readmeSection(t *testing.T, content, headingSubstr string) string` that locates the `## ` line containing `headingSubstr` and returns everything up to the next `\n## ` heading (calling `t.Fatalf` if not found).
- Add `TestReadmeListsAllIntegrationPages(t *testing.T)`:
  - Load `pages := loadIntegrationPages(t, dir)` from `website/content/docs/integrations`.
  - Read `README.md` from `repoRoot(t)` and extract its `Supported integrations` section.
  - Track `seenSkipped := map[string]bool{}` and `checked := 0`.
  - For each `p` in `pages`:
    - If `readmeSkippedIntegrationIDs[p.ID]`, record `seenSkipped[p.ID] = true` and continue.
    - Compute `rel` relative to `dir`, strip `.md`, normalize slashes, and build `wantURL := "https://runlore.io/docs/integrations/" + strings.TrimSuffix(filepath.ToSlash(rel), ".md") + "/"`.
    - Assert `strings.Contains(section, wantURL)`; otherwise `t.Errorf(...)` naming the page, `kind/id`, and exact missing URL.
    - Increment `checked`.
  - Assert `checked > 0`.
  - Assert every key in `readmeSkippedIntegrationIDs` is in `seenSkipped` (stale-skip guard).

- [x] **Step 2: Run the new test to verify it fails on current `README.md`**

```bash
go test -v ./internal/docsguard/... -run TestReadmeListsAllIntegrationPages
```

Expected: FAIL reporting missing canonical URLs including `loki`, `elasticsearch`, `opensearch`, `gitlab`, `grafana`, `custom-webhook`, `templated`, etc.

---

## Task 2: Refresh `README.md` to `v0.20.0`

**Files:**
- Modify: `README.md`

- [x] **Step 1: Update `## 🔌 Supported integrations` and learning-loop summary in `README.md`**

- Update every row in `## 🔌 Supported integrations` so every non-skipped integration links to its canonical `https://runlore.io/docs/integrations/<path>/` page:
  - **GitOps:** Flux · Argo CD
  - **Metrics:** [Prometheus / VictoriaMetrics](https://runlore.io/docs/integrations/data-sources/prometheus/)
  - **Logs:** [VictoriaLogs](https://runlore.io/docs/integrations/data-sources/victorialogs/) · [Loki](https://runlore.io/docs/integrations/data-sources/loki/) · [Elasticsearch](https://runlore.io/docs/integrations/data-sources/elasticsearch/) · [OpenSearch](https://runlore.io/docs/integrations/data-sources/opensearch/)
  - **Network flows:** [Cilium Hubble](https://runlore.io/docs/integrations/data-sources/hubble/) · [AWS VPC Flow Logs](https://runlore.io/docs/integrations/data-sources/aws-vpc-flow-logs/) · [GCP Firewall Logs](https://runlore.io/docs/integrations/data-sources/gcp-firewall-logs/)
  - **Cloud:** [AWS](https://runlore.io/docs/integrations/data-sources/aws-cloud/) · [GCP](https://runlore.io/docs/integrations/data-sources/gcp-cloud/)
  - **Kubernetes:** [Kubernetes](https://runlore.io/docs/integrations/data-sources/kubernetes/) (pod status, events, specs, logs)
  - **Source repos:** [Source repos](https://runlore.io/docs/integrations/data-sources/source-repos/)
  - **LLM:** [Anthropic](https://runlore.io/docs/integrations/llm/anthropic/) · [Google Gemini](https://runlore.io/docs/integrations/llm/gemini/) · [OpenAI-compatible](https://runlore.io/docs/integrations/llm/openai-compatible/)
  - **Triggers:** [Alertmanager](https://runlore.io/docs/integrations/triggers/alertmanager/) · [GitOps failures](https://runlore.io/docs/integrations/triggers/gitops/) · [PagerDuty](https://runlore.io/docs/integrations/triggers/pagerduty/) · [Grafana Alerting](https://runlore.io/docs/integrations/triggers/grafana/) · [Custom webhook](https://runlore.io/docs/integrations/triggers/custom-webhook/)
  - **Notifiers:** [Slack](https://runlore.io/docs/integrations/notifications/slack/) (threaded summary, feedback buttons, silence, thread Q&A & reply-to-KB) · [Matrix](https://runlore.io/docs/integrations/notifications/matrix/) (reactions, silence, thread Q&A & reply-to-KB) · [Webhook](https://runlore.io/docs/integrations/notifications/webhook/) · [Templated](https://runlore.io/docs/integrations/notifications/templated/)
  - **Knowledge base (git forge):** [GitHub](https://runlore.io/docs/integrations/forge/github/) · [GitLab](https://runlore.io/docs/integrations/forge/gitlab/)
  - **MCP:** [MCP server & client](https://runlore.io/docs/integrations/data-sources/mcp/)
  - Add concise rows for **Decision model** (`decision_model.*`, optional System One rerank/dedup) and **Remediation handoff** (`factory.*`, optional external task API + post-soak verification).
- In `## 📚 The learning loop`, add a short mention of deterministic injection screening (`threatcheck` gating `ready-to-merge`), thread-to-KB replies, and KB-over-HTTP / `lore kb resolve`, linking to existing docs (`learning-loop/`, `reviewing-knowledge/`).
- In `## Project status & stability`, accurately reflect the broader integration surface (Loki/ES/OpenSearch, GitLab, Grafana/custom sources, templated notifiers).

- [x] **Step 2: Run `docsguard` to verify `TestReadmeListsAllIntegrationPages` passes**

```bash
go test -v ./internal/docsguard/...
```

Expected: PASS.

---

## Task 3: Clean up `ROADMAP.md`, `design.md`, and `integrations/_index.md`

**Files:**
- Modify: `ROADMAP.md`
- Modify: `website/content/docs/concepts/design.md`
- Modify: `website/content/docs/integrations/_index.md`

- [x] **Step 1: Update `ROADMAP.md`**

- Remove the 4 shipped bullets from `## Next` and `## Later`:
  - Templated webhook source (`internal/source/custom`)
  - Templated notifier (`internal/notify/templated`)
  - Persisted vector cache + incremental indexing (`internal/catalog/veccache.go`)
  - Hot-path benchmarks (12 `Benchmark*` functions across `internal/`)
- Keep all genuinely open items (`#392` until Item 1 lands, `#391`, `#394`, `#362`, hybrid recall graduation, multi-KB federation, frozen `auto` rung).

- [x] **Step 2: Deduplicate `design.md` §7 & §13 and fix `integrations/_index.md:45`**

- In `website/content/docs/concepts/design.md`:
  - §7: Replace lines 302–314 (the stale provider table) with a short paragraph + `relref` link to `/docs/integrations/_index.md` as the single maintained integrations catalog. Keep the GCP footnote, cloud native-SDK note, GitOps comparison table, and auto-discovery notes intact.
  - §13: Replace the stale ASCII tree (lines 483–508) with a brief pointer to [`AGENTS.md`](https://github.com/Smana/runlore/blob/main/AGENTS.md#layout) for the maintained package layout.
- In `website/content/docs/integrations/_index.md`:
  - Line 45: change `"Google's models over the OpenAI-compatible endpoint — version caveats apply."` to `"Google's Gemini models over the native streamGenerateContent API."`.

- [x] **Step 3: Run full quality gate**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected: all tests (including `internal/docsguard` cross-page anchor and link checks) PASS, `gofmt -l .` prints nothing, `hack/lint.sh` reports `0 issues`.

- [x] **Step 4: Commit**

Commit using Conventional Commits (English, no co-author line), e.g.:
`docs: refresh README, ROADMAP, and design for v0.20 and guard README integrations`
