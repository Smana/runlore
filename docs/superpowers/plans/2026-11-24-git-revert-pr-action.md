# Native Deterministic Git-Revert PR Action Implementation Plan

> **Status:** Implemented (PR #634, merged 2026-11-24)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a pure-Go, `approve`-only `revert_pr` action that deterministically constructs an inverse commit for a single server-observed GitOps commit and opens a Draft PR (GitHub) or Draft MR (GitLab) against an allowlisted GitOps repository.

**Architecture:** Register `revert_pr` in `providers.Ops` with `ApproveOnly: true` (hard-blocked from `auto`). Resolve `Action.Revert` strictly server-side from `observedChanges` (`what_changed` only, never `source_diff` or model-invented SHAs). Implement `internal/executor/revertpr` over a new `providers.RevertForge` interface backed by atomic GitHub Git Data API and GitLab Commits API calls, guarded by credential separation, repo allowlist, protected path denylist, single-parent check, and clean-head overlap check.

**Tech Stack:** Go 1.26, GitHub REST/Git Data API, GitLab REST API v4, `internal/action`, `internal/audit`, `httptest`.

**Spec:** [`docs/superpowers/specs/2026-11-24-git-revert-pr-action-design.md`](../specs/2026-11-24-git-revert-pr-action-design.md)

## Global Constraints

- **Depends on Item 6** (`internal/investigate/observedchanges.go` must exist so `RevertTarget` is derived strictly from server-observed `what_changed` records).
- **Hard-blocked from `actions.mode: auto`.** `ApproveOnly: true` cannot be bypassed by config.
- **Draft PR/MR only — never auto-merge.**
- **Zero `git` CLI binary dependency.** All operations run pure-Go over forge APIs (`httpx.SecureClient`).
- **Fail closed everywhere:** empty `allowed_repos`, unobserved commit, merge commit, >50 files, any file matching built-in or operator `protected_paths`, any overlapping later commit on `HEAD`, or missing credentials all refuse safely.
- **Every execution and refusal is audit-logged** via `internal/audit`.
- **Quality gate before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`.
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/providers/providers.go` (modify) | Add `ApproveOnly` to `OpSafety`, register `"revert_pr"` in `Ops`, add `RevertTarget` onto `Action`, define `CommitDetail`, `CommitFile`, `RevertPRRequest`, and `RevertForge` interface |
| `internal/config/config.go`, `config_test.go` (modify) | Add `actions.revert_pr` config block (`enabled`, `allowed_repos`, `protected_paths`, `reuse_forge_credentials`, dedicated `forge`) + fail-closed `Validate()` rules |
| `internal/action/{safety.go,policy.go,auto.go,approvals.go}` + tests (modify) | Enforce `ApproveOnly` hard-deny in `auto.go`; validate `RevertTarget` + `allowed_repos` in `policy.go` and at `Approvals.Approve` execution boundary |
| `internal/investigate/{tools.go,loop.go,observedchanges.go}` + tests (modify) | Allow `revert_pr` in `submit_findings` schema; resolve `Action.Revert` strictly from observed GitOps `what_changed` commits (strip `Op` when unobserved or `source_repo`) |
| `internal/executor/revertpr/revertpr.go`, `revertpr_test.go` (create) | Pure-Go deterministic revert engine (protected paths, single-parent, overlap check, blob restoration, idempotent PR lookup, Draft PR creation) |
| `internal/forge/github/revert.go`, `revert_test.go` (create) | GitHub `RevertForge` implementation (atomic Git Data API blobs/tree/commit/ref + Draft PR) |
| `internal/forge/gitlab/revert.go`, `revert_test.go` (create) | GitLab `RevertForge` implementation (atomic Repository Commits API batch actions + Draft MR) |
| `internal/app/{action.go,serve.go}` + tests (modify) | Wire `revertpr.Executor` with dedicated or opted-in credentials alongside Flux/Argo cluster executor; post opened Draft PR URL back to incident thread |
| `website/content/docs/concepts/design.md`, `website/content/docs/security/security-architecture.md` (modify) | Document `revert_pr` action, credential separation, and safety invariants |

---

## Task 1: Provider contracts, config, and `internal/action` policy gates

**Files:**
- Modify: `internal/providers/providers.go`
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `internal/action/{safety.go,policy.go,auto.go,approvals.go}` + tests

- [x] **Step 1: Write failing tests for config validation and action gating**

- `internal/config/config_test.go`:
  - `actions.revert_pr.enabled: true` with `mode: auto` or `mode: off` → error.
  - `actions.revert_pr.enabled: true` with empty `allowed_repos` → error.
  - `actions.revert_pr.enabled: true` with neither `actions.revert_pr.forge` nor `reuse_forge_credentials: true` → error.
  - Valid config with dedicated `actions.revert_pr.forge` passes; valid config with `reuse_forge_credentials: true` + top-level `forge` passes.
- `internal/action/policy_test.go` & `auto_test.go`:
  - `auto` hard-rejects `revert_pr` regardless of confidence, rate-limit, or allowlist.
  - `approve` rejects `revert_pr` when `Revert == nil`, when `Revert.Repo` is not in `allowed_repos`, or when `actions.revert_pr.enabled == false`.
  - `approve` accepts `revert_pr` when enabled + repo allowlisted + `Revert.CommitSHA != ""`, and re-validates on `Approve()`.

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/config/... ./internal/action/... -run 'Test.*RevertPR'
```

Expected: FAIL.

- [x] **Step 3: Implement provider types, config validation, and action gate rules**

- Add `ApproveOnly bool` to `OpSafety`, register `"revert_pr": {Reversible: true, Blast: 2, ApproveOnly: true}` in `providers.Ops`, add `Revert *RevertTarget` to `providers.Action`, and define `RevertForge`.
- Add `RevertPR` config struct + fail-closed `Validate()` in `internal/config/config.go`.
- Enforce `ApproveOnly` in `internal/action/auto.go` and `revert_pr` repo-allowlist checks in `internal/action/policy.go`.

- [x] **Step 4: Run tests**

```bash
go test -race ./internal/providers/... ./internal/config/... ./internal/action/...
```

Expected: PASS.

---

## Task 2: Pure-Go revert engine (`internal/executor/revertpr`)

**Files:**
- Create: `internal/executor/revertpr/revertpr.go`
- Create: `internal/executor/revertpr/revertpr_test.go`

- [x] **Step 1: Write failing unit tests with a fake `providers.RevertForge`**

Cover every invariant:
- Clean revert of commit with 1 added file, 1 modified file, 1 removed file, 1 renamed file → exact inverse file actions submitted to `CreateRevertDraftPR` with `Draft: true`, branch `runlore/revert-<sha[:12]>`.
- Merge commit (`len(Parents) == 2`) → refused with clear error.
- Root commit (`len(Parents) == 0`) → refused.
- Commit touching >50 files → refused.
- Built-in protected paths: `clusters/prod/flux-system/gotk-sync.yaml`, `manifests/kube-system/coredns.yaml`, `.github/workflows/deploy.yaml`, `.gitlab-ci.yml` → entire commit refused even if another file in the commit is normal.
- Operator `protected_paths` glob (e.g. `infra/iam/**`) → refused.
- Overlapping later edit between `C` and `HEAD` (`CompareChangedFiles` includes a file from `C`) → refused naming the conflicting file.
- Non-overlapping later edit (`HEAD` moved on unrelated files) → succeeds cleanly against current `headSHA`.
- Idempotent dedup: `FindOpenPRByHead` returns existing open PR → returns that URL without calling `CreateRevertDraftPR`.

- [x] **Step 2: Run test to verify failure**

```bash
go test ./internal/executor/revertpr/...
```

Expected: FAIL.

- [x] **Step 3: Implement `internal/executor/revertpr/revertpr.go`**

Implement protected path matching (`path.Match` + segment checks for `flux-system`/`kube-system`/`.github`/`.gitlab`), parent/size/overlap verification, blob materialization, branch naming, PR body rendering (redacted via `redact.Secrets`), and idempotent execution.

- [x] **Step 4: Run tests with race detector**

```bash
go test -race -v ./internal/executor/revertpr/...
```

Expected: PASS.

---

## Task 3: GitHub and GitLab `RevertForge` implementations

**Files:**
- Create: `internal/forge/github/revert.go`, `internal/forge/github/revert_test.go`
- Create: `internal/forge/gitlab/revert.go`, `internal/forge/gitlab/revert_test.go`

- [x] **Step 1: Write failing `httptest` fixture tests for GitHub and GitLab `RevertForge`**

- `internal/forge/github/revert_test.go`:
  - `DefaultBranch`, `CommitDetail`, `CompareChangedFiles`, `FileBlob`, `FindOpenPRByHead`, and `CreateRevertDraftPR` (verifies base tree + blob creation + tree deletion (`sha: null`) + commit + ref creation + `POST /pulls` with `"draft": true`).
- `internal/forge/gitlab/revert_test.go`:
  - `DefaultBranch`, `CommitDetail`, `CompareChangedFiles`, `FileBlob`, `FindOpenPRByHead`, and `CreateRevertDraftPR` (verifies `POST /repository/commits` with `start_branch`, `branch`, atomic `actions[]` (`create`/`update`/`delete`), and `POST /merge_requests` with `"Draft: "` title prefix).

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/forge/... -run TestRevert
```

Expected: FAIL.

- [x] **Step 3: Implement `github/revert.go` and `gitlab/revert.go`**

Reuse existing client auth (`github_app` installation token / GitLab private token), `httpx.SecureClient`, bounded `httpx.Drain`, and compile-time `var _ providers.RevertForge = (*Client)(nil)` checks.

- [x] **Step 4: Run forge tests**

```bash
go test -race ./internal/forge/...
```

Expected: PASS.

---

## Task 4: Wire server-side `RevertTarget` resolution, app executor dispatch, audit & docs

**Files:**
- Modify: `internal/investigate/{tools.go,loop.go,observedchanges.go}` + tests
- Modify: `internal/app/{action.go,serve.go}` + tests
- Modify: `website/content/docs/concepts/design.md`, `website/content/docs/security/security-architecture.md`

- [x] **Step 1: Write failing tests for server-derived `RevertTarget` & composite action executor**

- `internal/investigate/`:
  - Proposed `revert_pr` matching a single observed `what_changed` GitOps commit gets `Action.Revert` stamped server-side.
  - Proposed `revert_pr` citing an unobserved SHA, a `source_diff` commit, or a multi-commit range has `Op` stripped (downgraded to advisory suggestion).
- `internal/app/`:
  - Composite executor dispatches `suspend`/`resume`/`reconcile` to Flux/Argo and `revert_pr` to `revertpr.Executor`, audit-logging every outcome and posting the opened Draft PR URL back to the incident thread.

- [x] **Step 2: Implement investigation wiring, app dispatch, and docs updates**

- Update `submit_findings` tool schema to include `revert_pr` when enabled, resolve `Action.Revert` server-side from `observedChanges` (GitOps only).
- Build dedicated `RevertForge` client from `cfg.Actions.RevertPR.Forge` (or reuse top-level forge when `ReuseForgeCredentials` is true) and wrap in a composite executor in `internal/app/action.go`.
- Update `website/content/docs/concepts/design.md` §6 and `website/content/docs/security/security-architecture.md` §1 to document `revert_pr`, its `approve`-only + Draft-PR boundary, protected paths, and credential separation.

- [x] **Step 3: Run full quality gate**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected: all tests PASS, `gofmt -l .` prints nothing, `hack/lint.sh` reports `0 issues`.

- [x] **Step 4: Commit**

Commit using Conventional Commits (English, no co-author line), e.g.:
`feat(actions): add pure-Go approve-only deterministic git revert_pr action`
