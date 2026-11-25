# Design — Decouple `internal/factory` auth & split core god-files

- **Date:** 2026-11-24
- **Status:** Implemented (PR #624 + PR #626, merged 2026-11-24)
- **Owner:** Smaine Kahlouch
- **Touches:** `internal/factory/{client.go,client_test.go}`, `internal/config/config*.go`, `internal/providers/providers*.go`, `internal/docsguard/decision_backends_test.go`, `website/content/docs/configuration/configuration.md`, `website/content/docs/integrations/remediation-factory.md` (or under `reference/`)

## Problem

Two structural maintenance and adoption issues have accumulated as RunLore reached `v0.20.0`:

1. **`internal/factory` auth is coupled to a single identity provider.** The `POST /v1/tasks` + signed-webhook remediation-handoff protocol is clean and vendor-neutral, yet `internal/factory/client.go:130-135` hardcodes ZITADEL's audience scope format (`openid urn:zitadel:iam:org:project:id:<id>:aud`) and makes `project_id_env` mandatory for `Client.Enabled()`. Any OSS team running Keycloak, Auth0, Okta, Dex, Entra ID, or a simple Bearer-authenticated internal broker cannot use `factory` without patching Go code.
2. **Core contract/config files have become god-files.** `internal/config/config.go` (3,033 LOC) and `internal/providers/providers.go` (2,610 LOC) concentrate unrelated subsystems in two monolithic files, causing unnecessary merge conflicts and review overhead whenever any provider or config section changes.

## Decisions (locked during grilling)

| # | Decision | Choice | Rationale |
|---|---|---|---|
| **Q1** | `internal/factory` decoupling | **Generalize `factory` auth with 100% backwards compatibility** (keep ZITADEL `project_id_env`, add generic OAuth2 client-credentials with optional `oauth_scope`/`oauth_audience`, and add static Bearer `token_env`) + **publish protocol docs** for `POST /v1/tasks` and signed webhooks | Unblocks every non-ZITADEL identity provider and simple webhook brokers with minimal code, zero breaking config changes, and no premature interface over-abstraction. |
| **Q2** | God-file splitting scope | **Pure same-package domain splits for `internal/config/config.go` and `internal/providers/providers.go` only**, leaving stateful files (`outcome/ledger.go`, `investigate/loop.go`, `thread/responder.go`) untouched; update `internal/docsguard/decision_backends_test.go` to read `internal/config/*.go` | Maximizes maintainability and cuts merge friction with zero public API changes and zero risk of introducing subtle state bugs in well-tested runtime files. |

## Non-goals

- **Breaking existing ZITADEL `factory:` configs.** Existing deployments setting `token_url` + `client_id_env` + `client_secret_env` + `project_id_env` must continue working byte-for-byte with zero config edits.
- **Extracting a multi-protocol `TaskFiler` plugin registry.** The HTTP `POST /v1/tasks` contract stays the single wire protocol.
- **Splitting `internal/outcome/ledger.go`, `internal/investigate/loop.go`, or `internal/thread/responder.go`.** Deliberately deferred to avoid churning stateful concurrency/compaction logic.

## Detailed design

### 1. Generalized `factory` authentication (`internal/factory/client.go` & `internal/config`)

Add three optional fields to `config.Factory`:

```yaml
factory:
  url: https://broker.example.com
  # Mode A — static Bearer token (mutually exclusive with OAuth2 fields below):
  token_env: FACTORY_API_TOKEN
  # Mode B — generic OAuth2 client credentials (Keycloak, Auth0, Okta, Dex, Entra ID, …):
  token_url: https://idp.example.com/oauth/token
  client_id_env: FACTORY_CLIENT_ID
  client_secret_env: FACTORY_CLIENT_SECRET
  oauth_scope: "tasks:write"        # optional; omitted if empty
  oauth_audience: "https://broker"  # optional; sent as form field `audience` if non-empty
  # Mode C — ZITADEL shorthand (existing, 100% backwards-compatible):
  #   token_url + client_id_env + client_secret_env + project_id_env
  #   emits scope: "openid urn:zitadel:iam:org:project:id:<project>:aud"
```

Validation & auth rules:
- **Mutual exclusion:** `token_env` cannot be combined with `token_url`, `client_id_env`, `client_secret_env`, `project_id_env`, `oauth_scope`, or `oauth_audience` (fail loud at `config.Validate`).
- **ZITADEL vs generic scope:** `project_id_env` and `oauth_scope` cannot both be set (ambiguous scope source — fail loud at `config.Validate`).
- **Endpoint security:** `checkSecureKeyEndpoint` enforces `https://` (or private/loopback) on `factory.url` for all 3 modes and on `factory.token_url` for OAuth2 modes.
- **Runtime behavior (`Client`):**
  - Mode A (`staticToken != ""`): sends `Authorization: Bearer <staticToken>`, skips OAuth2 token exchange, does not retry 401 refresh loops (a revoked static token is permanent until restarted/reloaded).
  - Mode B (`token_url` + `client_id` + `client_secret`, no `project_id`): posts `grant_type=client_credentials`, `client_id`, `client_secret`, plus `scope` if `oauth_scope != ""` and `audience` if `oauth_audience != ""`; caches token with 60s skew and single 401 refresh exactly as today.
  - Mode C (`project_id != ""`): identical to today's ZITADEL scope format.
- **Open protocol doc:** publish a clear, concise page documenting the `POST /v1/tasks` request/response status codes (`201`/`200`/`202`/`429`), `Idempotency-Key` header, HMAC-signed inbound webhook payload, post-soak verification, and the three auth modes.

### 2. Same-package file splits (zero API/symbol changes)

#### `internal/config/` (from 3,033 LOC in `config.go`)

| File | Contents |
|---|---|
| `config.go` | Package doc, top-level `Config`, `Logging`, `ServerConfig`, `ServerMCP`, `Telemetry`, `LeaderElection`, `Duration` |
| `config_sources.go` | `Endpoint`, `MetricsConfig`, `LogsConfig`, `LogFields`, `Cloud`, `GCPCloudCfg`, `MCP`, `MCPServer`, `SourceRepos`, `Network`, `HubbleCfg`, `AWSFlowCfg`, `GCPFlowCfg`, `GitOps`, `GitOpsMirror`, `Sources` / `Triggers` |
| `config_model.go` | `Model`, `Pricing`, `Embeddings`, `ModelOverride`, `DecisionModel`, `Investigation`, `ProgressUpdates`, `Coalesce`, `RateLimit` |
| `config_catalog.go` | `Catalog`, `CatalogCommons`, `Outcome`, `InstantRecall`, `CatalogGit`, `Forge` |
| `config_notify.go` | `Notify`, `ThreadNotify`, `AnnounceMode`, `SlackNotify`, `MatrixNotify`, `SilenceNotify` |
| `config_actions.go` | `ActionPolicy`, `ActionMode`, `ActionAllow`, `ActionAuto` |
| `config_factory.go` | `Factory`, `FactoryCuration`, `FactoryVerify`, `FactoryWebhook` |
| `config_validate.go` | `ApplyDefaults`, `Validate`, `ValidateEffort`, `ValidateThinking`, `checkSecureKeyEndpoint`, and validation helpers |

#### `internal/providers/` (from 2,610 LOC in `providers.go`)

| File | Contents |
|---|---|
| `providers.go` | Package doc, core enums (`Engine`, `ChangeType`, `ResourceScope`), `TimeWindow`, `Selector` |
| `providers_workload.go` | `Workload`, identity normalization, key/matching helpers |
| `providers_gitops.go` | `SourceRef`, `Change`, `Diff`, `FileDiff`, `FailureEvent`, `Action`, `OpSafety`, `Ops`, `GitOpsProvider`, `GitOpsInspector`, `Lookup*`, `ResourceStatus`, `DepNode`, `Executor` |
| `providers_cluster.go` | `ResourceSpec*`, `ResourceList*`, `ResourceReader`, `KindScoper`, `LogReader`, `PodLogQuery`, `PodStatus`, `KubeEvent` |
| `providers_telemetry.go` | `MetricsProvider`, `AlertRule*`, `LogsProvider`, `LogFields`, `LogStats`, `NetworkProvider`, `CloudProvider` |
| `providers_model.go` | `ModelProvider`, `CompletionRequest`, `CompletionResponse`, `Message`, `ToolSpec`, `ToolCall`, `Usage`, `UsageTotals`, `EstimateTokens`, `Decider`, `Question`, `Answer`, `Answers` |
| `providers_investigation.go` | `Investigation`, `Hypothesis`, `Verdict`, `MatchedKB`, `AppliedResolution`, `EvidenceGrounding`, `ProvenanceLink` |
| `providers_forge.go` | `IssueProvider`, `KBEntry`, `CuratedIssue`, `EntryPRState`, `PullSummary`, `Notifier`, `ProgressNotifier`, `PossibleDuplicateHeading`, card evidence parser |

#### `internal/docsguard/decision_backends_test.go`
- Update `readDoc(t, filepath.Join(root, "internal", "config", "config.go"))` to concatenate all non-test `*.go` files in `internal/config/` (sorted deterministically) so field-doc checks keep working seamlessly across the split files.

## Verification plan

- **Mechanical split commit FIRST (zero behavior change):** split `config.go` and `providers.go`, update `decision_backends_test.go`, run full `go test -race ./... && hack/lint.sh` — proving zero regressions before touching any behavior.
- **`factory` auth tests in `internal/factory/client_test.go` and `internal/config/config_test.go`:**
  - Mode A (static bearer `token_env`): sends `Bearer <tok>`, makes zero token-endpoint calls, does not loop on 401.
  - Mode B (generic OAuth2): posts client credentials with optional `scope` and `audience` (and omits them when empty), caches + refreshes on 401.
  - Mode C (ZITADEL `project_id_env`): byte-identical form payload to existing tests.
  - Mutual-exclusion validation errors (`token_env` + `token_url`; `project_id_env` + `oauth_scope`; cleartext public `url` with `token_env`).
- Full quality gate: `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`.
