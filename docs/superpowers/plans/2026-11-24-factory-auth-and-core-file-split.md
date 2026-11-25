# Decouple `internal/factory` Auth & Split Core God-Files Implementation Plan

> **Status:** Implemented (PR #624 + PR #626, merged 2026-11-24)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** (1) Split `internal/config/config.go` (3,033 LOC) and `internal/providers/providers.go` (2,610 LOC) into cohesive same-package domain files with zero behavior/API changes, and (2) generalize `internal/factory` authentication to support static Bearer tokens (`token_env`) and generic OAuth2 client-credentials (`oauth_scope` / `oauth_audience`) alongside 100% backwards-compatible ZITADEL `project_id_env`, with protocol documentation.

**Architecture:** Execute the mechanical file split in its own isolated task/commit first (updating `internal/docsguard/decision_backends_test.go` to read `internal/config/*.go`), then extend `config.Factory` and `internal/factory/client.go` with TDD tests covering all three auth modes and validation rules.

**Tech Stack:** Go 1.26, stdlib `net/http` + `net/url`, `httptest`.

**Spec:** [`docs/superpowers/specs/2026-11-24-factory-auth-and-core-file-split-design.md`](../specs/2026-11-24-factory-auth-and-core-file-split-design.md)

## Global Constraints

- **Separate pure refactor from behavior change.** Task 1 (file split) must pass the entire test suite and linter with zero behavioral diff before Task 2 touches `factory` auth.
- **Zero breaking API or YAML changes.** Every exported Go symbol stays in package `config` or package `providers`; existing ZITADEL `factory:` YAML works unchanged.
- **Do not touch `internal/outcome/ledger.go`, `internal/investigate/loop.go`, or `internal/thread/responder.go`.**
- **Keep `internal/docsguard` 100% green**, including `decision_backends_test.go` and `config_fences_test.go`.
- **Quality gate before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`.
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` + `config_{sources,model,catalog,notify,actions,factory,validate}.go` (split) | Same-package domain split of `internal/config/config.go` |
| `internal/providers/providers.go` + `providers_{workload,gitops,cluster,telemetry,model,investigation,forge}.go` (split) | Same-package domain split of `internal/providers/providers.go` |
| `internal/docsguard/decision_backends_test.go` (modify) | Read all non-test `internal/config/*.go` files instead of hardcoding `config.go` |
| `internal/factory/client.go`, `client_test.go` (modify) | Support static Bearer (`token_env`), generic OAuth2 (`oauth_scope`, `oauth_audience`), and existing ZITADEL (`project_id_env`) |
| `internal/config/config_factory.go`, `config_validate.go`, `config_test.go` (modify) | Add `TokenEnv`, `OAuthScope`, `OAuthAudience` fields + mutual-exclusion & HTTPS validation |
| `website/content/docs/configuration/configuration.md`, `website/content/docs/reference/remediation-factory.md` (modify/create) | Document the open `POST /v1/tasks` + webhook contract and all 3 auth modes |

---

## Task 1: Mechanical same-package split of `internal/config/config.go` and `internal/providers/providers.go`

**Files:**
- Modify/Create: `internal/config/config*.go`
- Modify/Create: `internal/providers/providers*.go`
- Modify: `internal/docsguard/decision_backends_test.go`

- [x] **Step 1: Split `internal/config/config.go` into domain files**

Move declarations verbatim (preserving all doc comments, SPDX headers, and `package config`):
- `config.go`: `Config`, `Logging`, `ServerConfig`, `ServerMCP`, `Telemetry`, `LeaderElection`, `Duration`
- `config_sources.go`: `Endpoint`, `MetricsConfig`, `LogsConfig`, `LogFields`, `Cloud`, `GCPCloudCfg`, `MCP`, `MCPServer`, `SourceRepos`, `Network`, `HubbleCfg`, `AWSFlowCfg`, `GCPFlowCfg`, `GitOps`, `GitOpsMirror`, `Sources`, `Triggers`
- `config_model.go`: `Model`, `Pricing`, `Embeddings`, `ModelOverride`, `DecisionModel`, `Investigation`, `ProgressUpdates`, `Coalesce`, `RateLimit`
- `config_catalog.go`: `Catalog`, `CatalogCommons`, `Outcome`, `InstantRecall`, `CatalogGit`, `Forge`
- `config_notify.go`: `Notify`, `ThreadNotify`, `AnnounceMode`, `SlackNotify`, `MatrixNotify`, `SilenceNotify`
- `config_actions.go`: `ActionPolicy`, `ActionMode`, `ActionAllow`, `ActionAuto`
- `config_factory.go`: `Factory`, `FactoryCuration`, `FactoryVerify`, `FactoryWebhook`
- `config_validate.go`: `ApplyDefaults`, `Validate`, `ValidateEffort`, `ValidateThinking`, `checkSecureKeyEndpoint`, and validation helpers

- [x] **Step 2: Update `internal/docsguard/decision_backends_test.go` to read all non-test `internal/config/*.go`**

Add a tiny helper `readPackageGoDocs(t, dir)` that sorts and concatenates all non-test `*.go` files in `internal/config/`, replacing the single hardcoded `config.go` read at line 31.

- [x] **Step 3: Split `internal/providers/providers.go` into domain files**

Move declarations verbatim (preserving all doc comments, SPDX headers, and `package providers`):
- `providers.go`: core enums (`Engine`, `ChangeType`, `ResourceScope`), `TimeWindow`, `Selector`
- `providers_workload.go`: `Workload` and identity/normalization helpers
- `providers_gitops.go`: `SourceRef`, `Change`, `Diff`, `FileDiff`, `FailureEvent`, `Action`, `OpSafety`, `Ops`, `GitOpsProvider`, `GitOpsInspector`, `Lookup*`, `ResourceStatus`, `DepNode`, `Executor`
- `providers_cluster.go`: `ResourceSpec*`, `ResourceList*`, `ResourceReader`, `KindScoper`, `LogReader`, `PodLogQuery`, `PodStatus`, `KubeEvent`
- `providers_telemetry.go`: `MetricsProvider`, `AlertRule*`, `LogsProvider`, `LogFields`, `LogStats`, `NetworkProvider`, `CloudProvider`
- `providers_model.go`: `ModelProvider`, `CompletionRequest`, `CompletionResponse`, `Message`, `ToolSpec`, `ToolCall`, `Usage`, `UsageTotals`, `EstimateTokens`, `Decider`, `Question`, `Answer`, `Answers`
- `providers_investigation.go`: `Investigation`, `Hypothesis`, `Verdict`, `MatchedKB`, `AppliedResolution`
- `providers_forge.go`: `IssueProvider`, `KBEntry`, `CuratedIssue`, `EntryPRState`, `PullSummary`, `Notifier`, `ProgressNotifier`, `PossibleDuplicateHeading`, card evidence parser

- [x] **Step 4: Run full quality gate to prove zero regressions**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected: 100% PASS, `gofmt -l .` empty, `hack/lint.sh` `0 issues`.

- [x] **Step 5: Commit pure refactor separately**

Commit using Conventional Commits (English, no co-author line):
`refactor: split config.go and providers.go into same-package domain files`

---

## Task 2: Decouple `internal/factory` authentication (static Bearer + generic OAuth2 + ZITADEL)

**Files:**
- Modify: `internal/config/config_factory.go`, `internal/config/config_validate.go`, `internal/config/config_test.go`
- Modify: `internal/factory/client.go`, `internal/factory/client_test.go`

- [x] **Step 1: Write failing tests in `internal/config/config_test.go` and `internal/factory/client_test.go`**

- `config_test.go`:
  - Static bearer mode (`url` + `token_env` + `repository` + `max_cost_usd`) validates clean; public `http://` URL with `token_env` rejected by `checkSecureKeyEndpoint`.
  - Generic OAuth2 mode (`url` + `token_url` + `client_id_env` + `client_secret_env` + optional `oauth_scope` / `oauth_audience`, no `project_id_env`) validates clean.
  - Existing ZITADEL mode (`project_id_env`) validates clean.
  - Rejects `token_env` combined with `token_url` / `client_id_env` / `client_secret_env` / `project_id_env` / `oauth_scope` / `oauth_audience`.
  - Rejects `project_id_env` combined with `oauth_scope`.
- `client_test.go`:
  - Static bearer: `Enabled() == true`, posts `POST /v1/tasks` with `Authorization: Bearer <static>`, makes zero calls to any token URL, does not trigger an OAuth refresh retry on `401`.
  - Generic OAuth2: posts `grant_type=client_credentials`, `client_id`, `client_secret`, optional `scope` and `audience` (omitted when empty), caches token, refreshes once on `401`.
  - ZITADEL (`project_id_env` set): continues posting `"openid urn:zitadel:iam:org:project:id:<id>:aud"`.

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/config/... ./internal/factory/... -run 'Test.*Factory'
```

Expected: FAIL.

- [x] **Step 3: Implement auth generalization in `config_factory.go`, `config_validate.go`, and `factory/client.go`**

- Add `TokenEnv`, `OAuthScope`, and `OAuthAudience` (`yaml:"token_env"`, `yaml:"oauth_scope"`, `yaml:"oauth_audience"`) to `config.Factory`.
- Update `Validate()` for mutual exclusion and `checkSecureKeyEndpoint` on `factory.url` and `factory.token_url`.
- Update `factory.Client`:
  - Read `staticToken: getenv(cfg.TokenEnv)` in `NewClient`.
  - `Enabled()` returns true when `URL != ""` AND either `staticToken != ""` OR (`TokenURL != "" && clientID != "" && secret != ""`).
  - In `accessToken`: return `(c.staticToken, false, nil)` immediately when `c.staticToken != ""`; otherwise build OAuth2 form using ZITADEL scope when `c.project != ""`, else `c.cfg.OAuthScope` (when non-empty) and `audience: c.cfg.OAuthAudience` (when non-empty).
  - In `CreateTask`: only perform the single `401` token-drop retry when `c.staticToken == ""`.

- [x] **Step 4: Run tests with race detector**

```bash
go test -race ./internal/config/... ./internal/factory/... ./internal/app/...
```

Expected: PASS.

---

## Task 3: Document the open remediation-factory protocol & run quality gate

**Files:**
- Create: `website/content/docs/reference/remediation-factory.md` (and link from `website/content/docs/reference/_index.md`)
- Modify: `website/content/docs/configuration/configuration.md`

- [x] **Step 1: Document protocol and auth modes**

- Update `configuration.md` with `factory.token_env`, `factory.oauth_scope`, and `factory.oauth_audience`.
- Write `website/content/docs/reference/remediation-factory.md` describing the `POST /v1/tasks` request/response contract, `Idempotency-Key`, signed inbound webhook schema, post-soak verification, deferred curation, and all three auth modes (static Bearer, generic OAuth2, ZITADEL).

- [x] **Step 2: Run full quality gate (including `internal/docsguard` config-fence and link checks)**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected: all tests PASS, `gofmt -l .` empty, `hack/lint.sh` `0 issues`.

- [x] **Step 3: Commit**

Commit using Conventional Commits (English, no co-author line), e.g.:
`feat(factory): support static bearer and generic OAuth2 auth alongside ZITADEL`
