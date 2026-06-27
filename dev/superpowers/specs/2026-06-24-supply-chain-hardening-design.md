# Design — Supply-chain hardening (R13)

Date: 2026-06-24 · Status: approved (autonomous) · Worktree: `worktree-agent-aa9c8de35e0ffe2bd`

## Problem

RunLore's CI/CD lacked several standard supply-chain controls. Audit of the
current repo state confirmed each gap:

- **No dependency automation.** No `.github/dependabot.yml` → Go modules and
  GitHub Actions never get automated update PRs; pinned digests (below) would
  rot without a bot to bump them.
- **Actions referenced by mutable tags.** Every `uses:` in `ci.yaml`,
  `build-image.yml`, `eval.yaml` used a floating tag (`@v4`, `@v8`, …). A
  compromised or retagged action would execute with the workflow's token.
- **No `govulncheck`.** CI built/tested/linted but never checked the Go module
  graph against the Go vulnerability database.
- **`gosec` not enabled.** `.golangci.yml` ran correctness/style linters but no
  security analyzer.
- **Trivy ran only post-publish.** `build-image.yml`'s Trivy + SARIF-upload
  steps are gated `if: github.event_name != 'pull_request'`, so a vulnerable
  base image or dependency could merge before any image scan ran. PRs had no
  scan gate.

## Changes

### Dependabot (`.github/dependabot.yml`) — config only
Two ecosystems, weekly: `gomod` (root) and `github-actions` (root). Grouped
minor/patch updates to keep PR volume low. Dependabot understands SHA-pinned
actions and bumps the digest + the trailing `# vX.Y.Z` comment together.

### SHA-pinned actions — all three workflows
Every `uses:` pinned to a full 40-char commit SHA with a `# vX.Y.Z` comment
recording the human-readable version. SHAs resolved from the GitHub API at
authoring time (each major-alias tag resolved to its current patch release):

| Action | Version | SHA |
|---|---|---|
| actions/checkout (ci, eval) | v4.3.1 | 34e114876b0b11c390a56381ad16ebd13914f8d5 |
| actions/checkout (build-image) | v6.0.3 | df4cb1c069e1874edd31b4311f1884172cec0e10 |
| actions/setup-go | v5.6.0 | 40f1582b2485089dde7abd97c1529aa768e1baff |
| actions/upload-artifact | v4.6.2 | ea165f8d65b6e75b540449e92b4886f43607fa02 |
| golangci/golangci-lint-action | v8.0.0 | 4afd733a84b1f43292c63897423277bb7f4313a9 |
| docker/setup-buildx-action | v4.1.0 | d7f5e7f509e45cec5c76c4d5afdd7de93d0b3df5 |
| docker/login-action | v4.2.0 | 650006c6eb7dba73a995cc03b0b2d7f5ca915bee |
| docker/metadata-action | v6.1.0 | 80c7e94dd9b9319bd5eb7a0e0fe9291e23a2a2e9 |
| docker/build-push-action | v7.2.0 | f9f3042f7e2789586610d6e8b85c8f03e5195baf |
| aquasecurity/trivy-action | v0.36.0 | ed142fd0673e97e23eac54620cfb913e5ce36c25 |
| github/codeql-action/upload-sarif | v4.36.2 | 8aad20d150bbac5944a9f9d289da16a4b0d87c1e |

(checkout intentionally kept at the version each workflow already used — v4 in
ci/eval, v6 in build-image — to avoid an unrelated upgrade in this slice.)

### `govulncheck` step (`ci.yaml`)
A step runs `govulncheck ./...` via `go run golang.org/x/vuln/cmd/govulncheck@latest`
(no new go.mod dependency; pinned by Go's module cache + checksum DB). Fails the
build on a known-vulnerable, reachable call path.

### `gosec` (`.golangci.yml`)
`gosec` added to `linters.enable`. golangci-lint v2 bundles gosec, so the local
gate exercises it with no extra install. Triage outcome below.

### Trivy on PRs (`build-image.yml`)
Added a PR-time `trivy-action` filesystem scan (`scan-type: fs`) that runs on
`pull_request`, fails the job on `HIGH,CRITICAL` (`exit-code: 1`,
`ignore-unfixed: true`). The existing post-publish image scan + SARIF upload are
unchanged. PRs now gate on a vuln scan; pushes still get the image-level scan.

## gosec triage

Enabling gosec surfaced **15 findings**. Resolution — gate ends at `0 issues`:

**Fixed in production code (genuine):**
- `cmd/lore/main.go:155` G114 — pprof loopback server had no timeouts. Replaced
  the bare `http.ListenAndServe` with an `http.Server{ReadHeaderTimeout: …}`.
- `cmd/lore/main.go:326` G112 — main HTTP server missing `ReadHeaderTimeout`
  (Slowloris). Added `ReadHeaderTimeout`.
- `internal/network/awsvpc/awsvpc.go:83` G115 — `int(maxEvents)` → `int32`
  conversion. Added a non-negative clamp before the conversion so the cast is
  provably safe (and bounded by CloudWatch's own page cap).
- `internal/outcome/ledger.go:94` G302 — ledger file created `0644`. Tightened
  to `0600` (audit log already used `0600`; matches it).
- `internal/eval/record.go:37,45` G301/G306 — eval report dir `0755` / file
  `0644`. Tightened to `0750` / `0600`.

**Justified with targeted `#nosec` (genuine-but-intended behavior):**
- `cmd/lore/main.go:18` G108 — `net/http/pprof` import. Loopback-only, opt-in via
  `RUNLORE_PPROF`; already documented. `//nosec G108`.
- `cmd/lore/main.go:600` G204 — `exec.CommandContext("sh","-c",step)` in the eval
  scenario runner. `step` is operator-authored scenario YAML, not untrusted
  input; this is the whole point of the runner. `#nosec G204`.
- `internal/audit/audit.go:71,131` and `internal/config/load.go`,
  `internal/catalog/load.go`, `internal/eval/case.go` G304 — file opens from a
  config-supplied path (audit log, config file, catalog dir). Operator-controlled
  paths, not attacker input. `#nosec G304`.

**Excluded by config (test-only noise):**
- G301/G304/G306 in `*_test.go` (test fixtures intentionally use `0755`/`0644`,
  read temp paths). Excluded via a scoped `gosec` rule in `.golangci.yml` that
  applies the linter only to non-test files — security analysis of test fixtures
  adds noise, not signal.

Net: gosec runs on production code, fixes/justifications keep the local
`golangci-lint run ./...` gate at `0 issues`. No deferral needed.

## Testing
- `go build ./... && go vet ./... && go test ./...` — green.
- `gofmt -l .` — empty.
- `golangci-lint run ./...` (gosec now enabled) — `0 issues`.
- Workflow YAML: SHAs cross-checked against the GitHub API; `# vX.Y.Z` comments
  match the resolved digests.
