# Supply-chain hardening (R13) Implementation Plan

> **For agentic workers:** spec at `docs/superpowers/specs/2026-06-24-supply-chain-hardening-design.md`.
> Gate green before EACH commit: `go build/vet/test ./... && gofmt -l . && golangci-lint run ./...` must stay `0 issues`. Commit each concern separately.

**Goal:** Add standard supply-chain controls to CI/CD — dependency automation, SHA-pinned actions, `govulncheck`, `gosec`, and a PR-time image/fs scan — without breaking the lint gate.

**Tech stack:** GitHub Actions, Dependabot, golangci-lint v2 (bundled gosec), `golang.org/x/vuln` (govulncheck via `go run`), Trivy.

## Global Constraints
- No new go.mod dependencies (`govulncheck` via `go run …@latest`).
- Local `golangci-lint run ./...` must end at `0 issues`.
- Keep each workflow's existing action *versions* (don't upgrade as a side effect); only pin them.

---

### Task 1: Spec + plan
- [x] Write spec and this plan; commit first.

### Task 2: Dependabot
- [ ] Add `.github/dependabot.yml` — `gomod` + `github-actions`, weekly, grouped.
- [ ] Gate (no Go impact) → commit.

### Task 3: SHA-pin all actions
- [ ] `ci.yaml`, `build-image.yml`, `eval.yaml`: replace every `@vX` tag with the resolved 40-char SHA + `# vX.Y.Z` comment (table in spec).
- [ ] Gate → commit.

### Task 4: govulncheck in CI
- [ ] Add a `govulncheck ./...` step to `ci.yaml` (via `go run golang.org/x/vuln/cmd/govulncheck@latest`).
- [ ] Gate → commit.

### Task 5: gosec — fixes + config
- [ ] Fix genuine findings: G114/G112 (HTTP timeouts), G115 (int32 clamp), G302/G301/G306 (file/dir perms in ledger + eval/record).
- [ ] Justify intended findings with targeted `#nosec G### -- reason`: G108 (pprof), G204 (eval shell runner), G304 (config/audit/catalog paths).
- [ ] Enable `gosec` in `.golangci.yml`; scope it to exclude `*_test.go` fixture noise.
- [ ] Gate (with gosec on) MUST be `0 issues` → commit.

### Task 6: Trivy on PRs
- [ ] Add a PR-time `trivy-action` fs scan to `build-image.yml` that fails on HIGH/CRITICAL; leave the post-publish image scan intact.
- [ ] Gate → commit.
