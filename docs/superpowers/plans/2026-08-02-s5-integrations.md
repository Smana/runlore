# S5 — Integrations (GitLab forge · Grafana Alerting · Elasticsearch/OpenSearch) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the three integration gaps that sit directly across RunLore's stated audience — lock-in-averse, sovereignty-conscious, self-hosting teams — without widening the maintenance surface the improvement report warns about.

**Architecture:** Three independent additions behind existing interfaces, **one PR each**, each verified against a real backend before merge. GitLab implements `providers.CurationForge` + `ReinvestForge` over the v4 API. Grafana becomes a first-class `source.Descriptor` built on the proven custom-webhook mapper. Elasticsearch/OpenSearch becomes a third `LogsProvider` behind the same three log tools.

**Tech Stack:** Go 1.x, stdlib HTTP; Docker for the local verification backends.

**Spec:** [`docs/superpowers/specs/2026-08-02-s5-integrations-design.md`](../specs/2026-08-02-s5-integrations-design.md)

## Global Constraints

- Every new `.go` file starts with `// SPDX-License-Identifier: Apache-2.0`.
- `golangci-lint run` must pass; **no new third-party dependencies** — the GitLab and Elasticsearch clients are stdlib `net/http`, matching how `internal/forge/github` and `internal/logs/loki` are written.
- **Credentials by env-var indirection only.** Config stores the env var *name* (`token_env`), never the value — the pattern every existing integration follows. New secrets must reach `internal/redact`.
- **TLS verification stays on.** Do not add an `insecure_skip_verify` escape hatch.
- Config validation **fails closed and loudly** at startup: a missing credential or malformed target aborts `serve` rather than silently disabling the feature.
- Every integration ships a page under `website/content/docs/integrations/` — `internal/docsguard` fails CI otherwise, in both directions.
- Comments explain *why*; this codebase comments heavily and deliberately.
- Conventional Commits. **Never** add a co-author trailer or any AI attribution.

## Verified context — do not re-derive

- `providers.CurationForge` is three methods: `OpenPR(ctx, KBEntry) (Ref, error)`, `ListPRsByLabel(ctx, label) ([]CuratedIssue, error)`, `Comment(ctx, number, body) error`. `ReinvestForge` adds `ListIssuesByLabel` and `ReplaceLabel`. Both in `internal/providers/providers.go`.
- `internal/providers/curationforge_test.go` holds the compile-time assertion pattern: `var _ providers.CurationForge = (*github.Client)(nil)`.
- `internal/logs/detect.go` probes the backend once at startup and **fails safe to VictoriaLogs**. Its provider constants are the ids `internal/docsguard` reflects over.
- The `custom` source already implements dot-path extraction, `items` batching, `severity_map`, resolved-event handling and per-instance tokens — with startup-time mapping validation. Grafana reuses it rather than reimplementing it.
- `hack/e2e-local.sh` is the existing pattern for container-backed local verification.

---

### Task 1: GitLab forge

**Files:**
- Create: `internal/forge/gitlab/gitlab.go`, `gitlab_test.go`
- Modify: `internal/config/config.go` (a `Forge.Provider` selector + a `GitLab` block), `internal/app/forge.go` (wiring)
- Modify: `internal/providers/curationforge_test.go` (add the assertion)
- Create: `website/content/docs/integrations/gitlab.md`

**Interfaces:**
- Consumes: `providers.CurationForge`, `providers.ReinvestForge`, `providers.KBEntry`, `providers.Ref`, `providers.CuratedIssue`.
- Produces: `gitlab.New(cfg) *Client` satisfying both forge interfaces.

- [ ] **Step 1: Read the GitHub client first**

`internal/forge/github/github.go` is ~500 lines and already solves every problem you are about to meet: dedup via a hidden fingerprint marker in the body, label lifecycle, comment coalescing, retry/backoff. **Mirror its structure.** Read it before writing anything, and note in your report where you deliberately diverged.

- [ ] **Step 2: Write the failing tests**

`gitlab_test.go`, driven by an in-process `httptest` mock GitLab API. Cover the full curation lifecycle:
- `OpenPR` creates a branch, commits the entry, opens a merge request, applies labels
- `ListPRsByLabel` returns open MRs carrying a label
- `Comment` posts a note
- `ReplaceLabel` transitions a label
- the fingerprint marker round-trips through an MR description so dedup works
- a 429 or 5xx is retried; a 404 is not

Run: `go test ./internal/forge/gitlab/ -v` → FAIL (package does not exist).

- [ ] **Step 3: Implement**

Map the GitHub concepts onto GitLab v4: PR → merge request, PR comment → note, branch+commit → the Commits API. Auth is a **project or group access token** sent as the `PRIVATE-TOKEN` header — GitLab has no GitHub-App equivalent, and OAuth apps are the wrong shape for a bot.

`base_url` is optional (omit for gitlab.com); `kb_repo` carries the project path and must be URL-encoded into the API path (`%2F`), which is the single most common GitLab-client bug.

- [ ] **Step 4: Config + wiring**

```yaml
forge:
  provider: gitlab          # github (default) | gitlab
  kb_repo: your-group/runlore-kb
  base_branch: main
  gitlab:
    base_url: https://gitlab.example.com   # omit for gitlab.com
    token_env: GITLAB_TOKEN
```

Validation fails closed: `provider: gitlab` with no `token_env`, or a `kb_repo` that is not a valid project path, aborts `serve`. Add the token to `internal/redact`.

- [ ] **Step 5: Verify against a real GitLab project**

**This step needs the maintainer** — it requires a real project and token. Run one end-to-end pass: trigger an investigation, confirm the MR appears with the right labels and body, confirm a second occurrence *comments* rather than opening a duplicate, and confirm `reinvestigate` round-trips. Paste the MR URL into the report.

- [ ] **Step 6: Docs page, tests, lint, commit**

`gitlab.md` with front matter `integration: {kind: forge, id: gitlab}`, a minimal config that **parses under `config.Load`** (`internal/docsguard/minimal_config_test.go` enforces this), the token scope (`api` on a project or group access token), and the minimum GitLab version tested.

---

### Task 2: Grafana Alerting as a first-class source

**Files:**
- Create: `internal/source/grafana/grafana.go`, `grafana_test.go`
- Create: `website/content/docs/integrations/grafana.md`
- Create: `hack/integration/grafana/docker-compose.yml` + a provisioned alert rule

**Interfaces:**
- Consumes: the `custom` source's mapping machinery (`internal/source/custom`), `source.Register(Descriptor)`.
- Produces: a registered source named `grafana` serving `POST /webhook/grafana`.

- [ ] **Step 1: Write the failing test**

The defining test: **the built-in mapping must equal the one currently documented** in `data-sources.md` (now `website/content/docs/integrations/custom-webhook.md`) — `items: alerts`, `title: labels.alertname`, `message: annotations.summary`, `severity: labels.severity`, `namespace: labels.namespace`, `workload_name: labels.pod`, `fingerprint: fingerprint`, `resolved: status`, `labels: labels`.

Assert it as a table, so a change to the default is a visible test diff rather than a silent behaviour change.

Also test: a batch payload with several alerts; a resolved event recording a resolution rather than triggering an investigation; `token_env` falling back to `server.webhook_token_env`; every field remaining overridable.

- [ ] **Step 2: Implement as a thin wrapper**

Register a `source.Descriptor` named `grafana` that delegates to the custom mapper with the defaults baked in. **Do not reimplement extraction** — the point is that Grafana users stop hand-copying nine field paths, not that Grafana gets its own parser. It inherits the 1MiB body cap, per-delivery request cap and startup mapping validation for free.

- [ ] **Step 3: Verify against a real Grafana**

`hack/integration/grafana/` — compose file with Grafana and a provisioned alert rule whose contact point posts to a local `lore serve`. Fire the rule; confirm an investigation starts with the right namespace and workload. Resolve it; confirm a resolution is recorded rather than a second investigation.

- [ ] **Step 4: Docs page, tests, lint, commit**

---

### Task 3: Elasticsearch / OpenSearch logs

**Files:**
- Create: `internal/logs/elasticsearch/elasticsearch.go`, `elasticsearch_test.go`
- Modify: `internal/logs/detect.go` (extend the probe), `internal/config/config.go`, `internal/app/` wiring
- Create: `website/content/docs/integrations/elasticsearch.md`
- Create: `hack/integration/elasticsearch/docker-compose.yml` + a seeding script

**Interfaces:**
- Consumes: `providers.LogsProvider` (`Query`), plus the optional `LogFields` and `LogStats` capabilities.
- Produces: a `logs` provider registered under the id added to `internal/logs/detect.go`.

- [ ] **Step 1: Read the Loki client**

`internal/logs/loki` is the closest sibling and already solves field-convention defaults, token/header auth, and the capability type-assertions. Mirror it.

- [ ] **Step 2: Write the failing tests**

Mock `_search` responses covering all three tools on **both** distributions:

| Tool | Mechanism |
|---|---|
| `query_logs` | `_search` with `query_string` + a `range` filter on the timestamp field, newest-first, size-capped |
| `logs_error_summary` | `date_histogram` split by the level field; top messages from a `terms` aggregation, falling back to client-side aggregation when the field is `text`-only |
| `discover_log_fields` | `_field_caps` (present on ES 8 and OpenSearch 2) |

Plus: detection distinguishes OpenSearch (`version.distribution: opensearch`) from Elasticsearch, and **anything unrecognised still fails safe to VictoriaLogs** — that existing guarantee must not regress.

- [ ] **Step 3: Implement**

Use the classic `_search` DSL, the one dialect both ES 8.x and OpenSearch 2.x speak. ES|QL would fork the implementation.

ECS field defaults (`kubernetes.namespace`, `kubernetes.pod.name`, `kubernetes.container.name`, `log.level`, `@timestamp`), all overridable through the existing `logs.fields` block. Tell the model the right query language for this provider, exactly as LogsQL and LogQL already do.

- [ ] **Step 4: Verify against real containers**

`hack/integration/elasticsearch/` — single-node ES **and** OpenSearch, a seeding script writing ECS-shaped documents including a CrashLoopBackOff-style error burst, then all three tools exercised against both. Paste the output.

- [ ] **Step 5: Docs page, tests, lint, commit**

Document the parity caveats honestly, the way the Loki page does — particularly the top-messages fallback when the message field is `text`-only.

---

## Final verification

- [ ] `go test ./... && golangci-lint run` clean
- [ ] `go test ./internal/docsguard/` — all three new pages present and their configs parse
- [ ] Each integration verified against a **real** backend, with output in its report
- [ ] Credentials env-indirected, redacted, and absent from logs
- [ ] Existing GitHub / Alertmanager / VictoriaLogs / Loki behaviour unchanged — proven by their suites passing untouched
- [ ] A per-PR security pass on each integration (token handling, TLS, redaction, NetworkPolicy egress)
- [ ] Three separate PRs, English titles and descriptions, no AI attribution
</content>
