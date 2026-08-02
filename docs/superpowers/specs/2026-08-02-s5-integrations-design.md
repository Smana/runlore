# Design — S5: Integrations (GitLab forge · Grafana Alerting · Elasticsearch/OpenSearch)

- **Date:** 2026-08-02
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Source:** improvement report §5 (ranked by adoption unlocked per unit of work)
- **Depends on:** [S2](2026-08-02-s2-getting-started-integrations-design.md) — each integration adds a page to `/docs/integrations/`
- **Index:** [decomposition](2026-08-02-improvement-report-decomposition.md)

## Problem

RunLore's stated audience is lock-in-averse, sovereignty-conscious, self-hosting teams. Three gaps
sit directly across that audience:

1. **The forge is GitHub-only.** `internal/forge/` contains exactly one implementation. A team that
   self-hosts GitLab — the archetype of the stated audience — cannot use the learning loop at all,
   because curation is the loop. No OSS competitor covers this.
2. **Grafana Alerting is a config exercise.** Many teams never touch Alertmanager directly. RunLore
   *can* ingest Grafana today, but only by hand-writing a nine-field `custom` webhook mapping
   (`data-sources.md:133`). That is a documented workaround presented as an integration.
3. **Elasticsearch/OpenSearch logs are unsupported.** Logs are VictoriaLogs + Loki
   (`internal/logs/`). ES/OpenSearch is the enterprise long tail.

Two report items are deliberately excluded: **Microsoft Teams** (dropped by the owner) and
**OpenTelemetry traces** (the report itself calls it a 2027 bet).

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Shipping unit | **One PR per integration** | Each is independently reviewable, revertable, and locally verifiable |
| Local verification | Mandatory before merge, and **documented on the integration's page** | Owner's rule. A recipe that ships with the integration is a recipe users can follow |
| GitLab auth | Project/group **access token** via `token_env` | Works identically on gitlab.com and self-hosted. GitLab has no GitHub-App equivalent; OAuth apps are the wrong shape for a bot |
| GitLab test strategy | In-process mock GitLab API for the suite, **plus one real end-to-end pass** on a live project before merge | A 4GB `gitlab-ce` container in CI buys fidelity nobody will wait for; the real pass buys it once, where it counts |
| Grafana source | Promote to a **first-class `sources.grafana`** built on the existing custom-mapping machinery | Reuses proven code; users get `sources: {grafana: {}}` instead of nine hand-copied field paths |
| ES/OpenSearch query API | Classic `_search` DSL (`query_string` + aggregations) | The one dialect both Elasticsearch 8.x and OpenSearch 2.x speak. ES\|QL would fork the implementation |
| ES/OpenSearch detection | Extend the existing `logs.Detect` probe | Same fail-safe shape already proven for Loki |

## Scope

### In scope

1. `internal/forge/gitlab` — `CurationForge` + `ReinvestForge`, config, wiring, docs page.
2. `sources.grafana` — first-class trigger source, config, wiring, docs page.
3. `internal/logs/elasticsearch` — `LogsProvider` + `LogFields` + `LogStats`, detection, docs page.
4. `hack/integration/<name>/` — the compose/manifest each local verification uses.
5. A security pass per PR (token handling, TLS defaults, redaction, NetworkPolicy egress).

### Non-goals (YAGNI)

- Microsoft Teams, OpenTelemetry traces, Azure/GCP "what changed".
- GitLab **source-repo** cloning for `source_diff` on private GitLab hosts — `data-sources.md:209`
  already documents that non-forge hosts clone anonymously. Making the GitLab token available to
  `source_diff` is a natural follow-up, not this spec.
- Gitea/Forgejo (the same interface makes it cheap later).
- ES\|QL, Elastic Agent-specific schemas beyond ECS defaults.

## Design

### 1 · GitLab forge

`providers.CurationForge` is three methods (`OpenPR`, `ListPRsByLabel`, `Comment`) and
`ReinvestForge` adds `ListIssuesByLabel` + `ReplaceLabel` — a small surface, all HTTP.

Config, additive and backwards-compatible (`forge.github_app` stays the default path):

```yaml
forge:
  provider: gitlab                  # github (default) | gitlab
  kb_repo: your-group/runlore-kb    # project path, same field as GitHub's owner/name
  base_branch: main
  gitlab:
    base_url: https://gitlab.example.com   # omit for gitlab.com
    token_env: GITLAB_TOKEN                # project or group access token
```

Mapping: PR → **merge request**, PR comment → **note**, labels → labels (GitLab supports them on
both MRs and issues), branch push → the Commits API (create a branch, commit the drafted entry). The
existing fingerprint marker in the MR description keeps dedup working unchanged — it is plain HTML
comment text.

Token scope documented as **`api`** on a project (or group) access token, with the least-privilege
note that a *group* token is only needed when the KB repo may move. The token is read by env
indirection like every other credential, never inlined, and added to the redaction set.

Validation fails closed at startup: `provider: gitlab` without `token_env`, or with a `kb_repo` that
isn't a valid project path, aborts `serve` rather than silently disabling curation.

**Local verification:** an in-process `httptest` mock covering the full curation lifecycle
(open MR → dedup list → comment → label transition → retire), then one manual pass against a real
GitLab project: trigger an investigation, confirm the MR appears with the right labels and body,
comment coalescing works on a second occurrence, and `reinvestigate` round-trips.

### 2 · Grafana Alerting as a first-class source

The `custom` source already does the work: dot-path extraction, batch `items`, `severity_map`,
resolved-event handling, per-instance tokens. What is missing is a **named default**.

```yaml
sources:
  grafana:
    token_env: GRAFANA_WEBHOOK_TOKEN   # optional; falls back to server.webhook_token_env
```

registers `POST /webhook/grafana` with the mapping currently documented at `data-sources.md:133`
baked in as the default (`items: alerts`, `title: labels.alertname`, `message:
annotations.summary`, `severity: labels.severity`, `namespace: labels.namespace`, `workload_name:
labels.pod`, `fingerprint: fingerprint`, `resolved: status`, `labels: labels`). Every field stays
overridable for teams whose Grafana rules use different labels — the default is a starting point,
not a cage.

Implemented as a `source.Descriptor` registered like the others, delegating to the custom mapper, so
it inherits the 1MiB body cap, per-delivery request cap, and startup mapping validation.

**Local verification:** `hack/integration/grafana/` — a compose file with Grafana + a provisioned
alert rule and a contact point pointing at a local `lore serve`. Fire the rule, confirm an
investigation starts with the right namespace/workload; resolve it, confirm the resolution is
recorded rather than a second investigation.

### 3 · Elasticsearch / OpenSearch logs

Third implementation behind `LogsProvider`, plus the two optional capabilities the other backends
implement (`LogFields`, `LogStats`), so all three log tools work identically:

| Tool | ES/OpenSearch mechanism |
|---|---|
| `query_logs` | `_search` with `query_string` + a `range` filter on the timestamp field, newest-first, size-capped |
| `logs_error_summary` | `date_histogram` aggregation split by the level field; top messages from a `terms` aggregation over the normalized message keyword, falling back to client-side aggregation when the field is `text`-only |
| `discover_log_fields` | `_field_caps` (present on both ES 8 and OpenSearch 2) |

Config, mirroring Loki's shape exactly:

```yaml
logs:
  url: https://es.observability.svc:9200
  provider: elasticsearch      # elasticsearch | opensearch — optional, auto-detected
  index: logs-*                # index pattern (default logs-*)
  token_env: ES_TOKEN          # bearer / API key
  # basic_auth_env: ES_BASIC   # user:pass, for clusters that only speak basic auth
```

ECS field defaults (`kubernetes.namespace`, `kubernetes.pod.name`,
`kubernetes.container.name`, `log.level`, `@timestamp`), all overridable through the existing
`logs.fields` block — the same escape hatch Loki and VictoriaLogs use.

Detection extends `logs.Detect`: `GET /` returns `version.distribution: opensearch` for OpenSearch
and a plain `version.number` for Elasticsearch; anything else keeps the existing fail-safe to
VictoriaLogs. TLS verification stays **on** by default; a `insecure_skip_verify` escape hatch is
deliberately *not* added.

The query-language hint given to the model is set per provider, exactly as LogsQL/LogQL are today,
so the model writes Lucene `query_string` syntax against ES.

**Local verification:** `hack/integration/elasticsearch/` — single-node ES and OpenSearch
containers, a seeding script writing ECS-shaped log documents (including a CrashLoopBackOff-style
error burst), then all three tools exercised against both.

## Testing

| Integration | Tests |
|---|---|
| GitLab | mock-API lifecycle tests; config validation (missing token, bad project path, gitlab.com vs self-hosted base URL); redaction of the token; `providers.CurationForge`/`ReinvestForge` compile-time assertions like `curationforge_test.go` already does for GitHub; one manual real-project pass |
| Grafana | mapping-default test asserting the built-in mapping equals the documented one; batch payload; resolved-event path; token fallback to `server.webhook_token_env`; container-based manual pass |
| Elasticsearch | mock-`_search` tests for all three tools on both distributions; detection tests incl. fail-safe; field-override tests; container-based manual pass on ES **and** OpenSearch |
| All three | the S2 integration-page drift guard fails until each ships its docs page |

## Risks

| Risk | Mitigation |
|---|---|
| GitLab API differences between self-hosted versions | Target the v4 API surface that has been stable for years (MRs, notes, labels, commits); state the minimum tested GitLab version on the docs page |
| The one manual GitLab pass rots | The mock suite covers the contract; the manual pass is re-run when the docs page's "tested against" version is bumped |
| Grafana rule labels vary wildly between teams | Every mapped field stays overridable; the docs page shows both the default and an override example |
| ES vs OpenSearch divergence | Restricted to the classic `_search` DSL both speak; both are exercised in the manual pass |
| `logs_error_summary` top-messages fidelity on `text`-only message fields | Same client-side-aggregation caveat Loki already documents, stated on the page rather than hidden |
| Three integrations widen the maintenance surface the report warns about (§8.2) | Each is behind a config key that is off by default, and each ships with its own tests and local recipe |

## Acceptance criteria

Per integration (three separate PRs):

1. Works end-to-end in a real local environment, by the recipe published on its docs page.
2. Config validation fails closed and loudly on a missing credential or a malformed target.
3. Credentials are env-indirected, never logged, and covered by redaction.
4. `/docs/integrations/<name>` exists with minimal config, local verification, notes, and a
   reference deep-link; the S2 drift guard passes.
5. `go test ./...`, `golangci-lint`, and a per-PR security pass are clean.
6. Nothing about existing GitHub / Alertmanager / VictoriaLogs / Loki behaviour changes — proven by
   the existing suites staying green without edits.
</content>
