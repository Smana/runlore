# Zero-Config Day-0 CLI Ergonomics Implementation Plan

> **Status:** Implemented (PR #631, merged 2026-11-24)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `lore` into an effortless laptop-to-cluster CLI by extending `lore investigate` (positional `<ns>/<name>` or `<kind>/<ns>/<name>` targets, Gemini/OpenAI/Anthropic env auto-detection, `--provider`/`--effort`/`--kube-context`/`--json`/`--quiet`, live stderr tool trace) and adding a zero-LLM `lore status` cluster triage command.

**Architecture:** Reuse RunLore's existing `parseInterleaved`, `config.Validate`, `LoopInvestigator`, and Flux/Argo CRD parsing. Add `RestConfigForContext` in `internal/app/kube.go` so CLI commands can switch kubeconfig contexts cleanly, add an optional `providers.GitOpsFailureLister` for one-shot GitOps failure listing without starting informers, and add a nil-safe `OnToolCall` progress hook to `LoopInvestigator`.

**Tech Stack:** Go 1.26, stdlib `flag` + `encoding/json`, `k8s.io/client-go` (`fake.NewSimpleClientset`), Flux/Argo dynamic clients.

**Spec:** [`docs/superpowers/specs/2026-11-24-day0-cli-ergonomics-design.md`](../specs/2026-11-24-day0-cli-ergonomics-design.md)

## Global Constraints

- **No changes to `lore serve` config resolution.** Env auto-detection and `--kube-context` stay strictly in CLI entrypoints.
- **Validate AFTER applying CLI overrides.** `--effort` and `--base-url` must pass `cfg.Validate()` (including cleartext-key protection).
- **Stdout vs stderr separation.** Stdout carries ONLY the final verdict/report (text or JSON); progress lines and env-detection notices go to stderr and are suppressed by `--quiet` or `--json`.
- **Redact progress previews.** Any tool argument snippet printed to stderr passes through `redact.Secrets`.
- **Zero-LLM `lore status`.** Must never require a model key or config file.
- **Quality gate before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`.
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/app/investigate_config.go`, `investigate_config_test.go` (modify) | Expand `configFromEnv` for `GEMINI_API_KEY` (`gemini-4-neon`), bare `OPENAI_API_KEY`, `--provider` override, per-provider `*_MODEL` env vars, and resolution provenance string |
| `internal/app/kube.go`, `kube_test.go` (modify/create) | Add `RestConfigForContext(kubeContext string)` and wire optional kube-context through CLI builders |
| `internal/investigate/loop.go`, `loop_test.go` (modify) | Add nil-safe `OnToolCall func(step int, call providers.ToolCall)` hook invoked before tool dispatch |
| `internal/app/investigate_cmd.go`, `investigate_cmd_test.go` (modify/create) | Positional target parsing (`parseTarget`), `parseInterleaved`, `--provider`/`--effort`/`--kube-context`/`--json`/`--quiet`, stderr progress output, post-override `cfg.Validate()` |
| `internal/providers/providers.go` (modify) | Add optional `GitOpsFailureLister` interface (`ListFailures(ctx, namespace) ([]FailureEvent, error)`) |
| `internal/providers/gitops/flux/flux.go`, `flux_test.go` (modify) | Implement `ListFailures` over Kustomizations + HelmReleases |
| `internal/providers/gitops/argocd/argocd.go`, `argocd_test.go` (modify) | Implement `ListFailures` over Argo CD Applications |
| `internal/app/status_cmd.go`, `status_cmd_test.go` (create) | Implement `RunStatus` (`lore status`): unhealthy pods + failing GitOps objects + recent Warning events, compact table + `lore investigate` suggestions, `--json`, `--exit-code` |
| `cmd/lore/main.go`, `website/content/docs/reference/cli.md` (modify) | Register `status` subcommand, update usage text and CLI docs |

---

## Task 1: Expand `configFromEnv` and post-override validation

**Files:**
- Modify: `internal/app/investigate_config.go`
- Modify: `internal/app/investigate_config_test.go`

- [x] **Step 1: Write failing table-driven tests in `internal/app/investigate_config_test.go`**

Test cases:
- `GEMINI_API_KEY` alone → `provider: "gemini"`, `model: "gemini-4-neon"`, `api_key_env: "GEMINI_API_KEY"`; with `GEMINI_MODEL=gemini-4-carbon` uses Carbon.
- Bare `OPENAI_API_KEY` (no `OPENAI_BASE_URL`) → `provider: "openai"`, `base_url: "https://api.openai.com/v1"`, `model: "gpt-4o-mini"`, `api_key_env: "OPENAI_API_KEY"`.
- Precedence when multiple keys are exported: `OPENAI_BASE_URL` > `ANTHROPIC_API_KEY` > `GEMINI_API_KEY` > bare `OPENAI_API_KEY`.
- Explicit `providerOverride`: `"gemini"` picks `GEMINI_API_KEY` even when `ANTHROPIC_API_KEY` is also set; returns a clear error naming `GEMINI_API_KEY` if unset.
- `ANTHROPIC_MODEL` override respected alongside `OPENAI_MODEL` and `GEMINI_MODEL`.

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/app/... -run TestConfigFromEnv
```

Expected: FAIL.

- [x] **Step 3: Implement expanded `configFromEnv` in `internal/app/investigate_config.go`**

- Add constants `envGeminiKey = "GEMINI_API_KEY"`, `envGeminiModel = "GEMINI_MODEL"`, `envAnthropicModel = "ANTHROPIC_MODEL"`, `defaultGeminiModel = "gemini-4-neon"`, `defaultOpenAIEndpoint = "https://api.openai.com/v1"`.
- Return `(*config.Config, string, error)` (or small result struct) naming the resolution source (e.g. `"GEMINI_API_KEY"`, `"./runlore.yaml"`) so `RunInvestigate` can log one concise stderr line.

- [x] **Step 4: Run tests**

```bash
go test -race ./internal/app/... -run 'TestConfigFromEnv|TestResolveInvestigateConfig'
```

Expected: PASS.

---

## Task 2: Positional targeting, new flags, `--kube-context`, and live stderr progress in `lore investigate`

**Files:**
- Modify: `internal/app/kube.go` (and create `internal/app/kube_test.go`)
- Modify: `internal/investigate/loop.go`, `internal/investigate/loop_test.go`
- Modify: `internal/app/investigate_cmd.go` (and create `internal/app/investigate_cmd_test.go`)

- [x] **Step 1: Write failing tests**

- `investigate_cmd_test.go`:
  - `parseTarget("apps/checkout-api")` → `Workload{Namespace: "apps", Name: "checkout-api"}`.
  - `parseTarget("Deployment/apps/checkout-api")` → `Workload{Kind: "Deployment", Namespace: "apps", Name: "checkout-api"}`.
  - Rejects empty segments (`apps/`, `/foo`), 4+ segments (`a/b/c/d`), or conflicting `--namespace other` + `apps/checkout-api`.
  - Interleaved flags: `apps/checkout-api --provider gemini --effort high --json` parses both positional target and flags.
  - Post-override validation rejects invalid `--effort` or cleartext public `--base-url`.
- `loop_test.go`:
  - `OnToolCall` fires once per dispatched tool call with step number and `ToolCall`.
- `kube_test.go`:
  - `RestConfigForContext("nonexistent-context")` returns a clear error rather than silently falling back to in-cluster config.

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/app/... ./internal/investigate/... -run 'TestParseTarget|TestOnToolCall|TestRestConfigForContext'
```

Expected: FAIL.

- [x] **Step 3: Implement changes**

- `internal/app/kube.go`: add `RestConfigForContext(kubeContext string) (*rest.Config, error)` and context-aware builder variants used by `RunInvestigate` and `RunStatus`.
- `internal/investigate/loop.go`: add `OnToolCall func(step int, call providers.ToolCall)` to `LoopInvestigator`; call it in the loop right before `dispatchTools` (when non-nil).
- `internal/app/investigate_cmd.go`:
  - Switch `fs.Parse(args)` to `parseInterleaved(fs, args)`.
  - Add `--provider`, `--effort`, `--kube-context`, `--json`, `--quiet`.
  - Parse positional target into `req.Workload` (`Kind`, `Namespace`, `Name`) and synthesize default title when `--alert` is empty.
  - Run `cfg.Validate()` after applying all CLI flag overrides.
  - Wire `OnToolCall` to print `→ <name>(<redacted-brief-args>)` to `os.Stderr` when `!quiet && !jsonOut`.
  - When `--json` is set, marshal `*result` to `os.Stdout` with `json.MarshalIndent`.

- [x] **Step 4: Run package tests**

```bash
go test -race ./internal/investigate/... ./internal/app/...
```

Expected: PASS.

---

## Task 3: One-shot `GitOpsFailureLister` on Flux and Argo CD

**Files:**
- Modify: `internal/providers/providers.go`
- Modify: `internal/providers/gitops/flux/flux.go`, `flux_test.go`
- Modify: `internal/providers/gitops/argocd/argocd.go`, `argocd_test.go`

- [x] **Step 1: Write failing tests for Flux & Argo `ListFailures`**

- `flux_test.go`: seed fake dynamic client with one Ready Kustomization, one `Ready=False` Kustomization, and one `Ready=False` HelmRelease across two namespaces; assert `ListFailures(ctx, "")` returns both failures and `ListFailures(ctx, "apps")` returns only the `apps` failure.
- `argocd_test.go`: seed fake dynamic client with one Healthy/Synced Application, one Degraded Application, and one OutOfSync Application; assert `ListFailures` returns the unhealthy ones cleanly.

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/providers/gitops/... -run TestListFailures
```

Expected: FAIL.

- [x] **Step 3: Implement `GitOpsFailureLister` in `providers.go`, `flux.go`, and `argocd.go`**

- Define `GitOpsFailureLister` in `internal/providers/providers.go` alongside `GitOpsInspector`.
- Implement `ListFailures` on both `*flux.Provider` and `*argocd.Provider` (with compile-time `var _ providers.GitOpsFailureLister = (*Provider)(nil)` assertions), reusing the existing failure extraction helpers used by `WatchFailures`.

- [x] **Step 4: Run tests**

```bash
go test -race ./internal/providers/gitops/...
```

Expected: PASS.

---

## Task 4: Implement `lore status`, wire CLI, update docs & run quality gate

**Files:**
- Create: `internal/app/status_cmd.go`
- Create: `internal/app/status_cmd_test.go`
- Modify: `cmd/lore/main.go`
- Modify: `website/content/docs/reference/cli.md`

- [x] **Step 1: Write failing tests in `internal/app/status_cmd_test.go`**

Test with `fake.NewSimpleClientset` + fake `GitOpsFailureLister` + injected clock (`now`):
- Healthy cluster → `0` failures, prints healthy summary, exits `0` even with `--exit-code`.
- Unhealthy cluster (CrashLoopBackOff pod, Pending pod, high-restart pod, failing Flux Kustomization, 1 fresh Warning event + 1 stale Warning event >15m old) → stale event excluded, unhealthy pods and GitOps failure listed, concrete `lore investigate <kind>/<ns>/<name>` commands printed, `--json` produces valid structured JSON, `--exit-code` returns a non-zero exit sentinel.
- Warning events alone (0 unhealthy pods, 0 GitOps failures) → exits `0` even with `--exit-code`.

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/app/... -run TestStatus
```

Expected: FAIL.

- [x] **Step 3: Implement `internal/app/status_cmd.go`, register in `cmd/lore/main.go`, and update docs**

- Implement `RunStatus(args []string) error` with `--namespace`, `--kube-context`, `--json`, `--exit-code` using `parseInterleaved`.
- Wire `case "status":` in `cmd/lore/main.go` and update the `usage` constant.
- Update `website/content/docs/reference/cli.md` with `lore status` and the new `lore investigate` target/flags/env precedence.

- [x] **Step 4: Run full quality gate**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected: all tests PASS (including `internal/docsguard`), `gofmt -l .` prints nothing, `hack/lint.sh` reports `0 issues`.

- [x] **Step 5: Commit**

Commit using Conventional Commits (English, no co-author line), e.g.:
`feat(cli): add zero-config target/env ergonomics to lore investigate and add lore status`
