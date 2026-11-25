# Clickable Provenance Anchors Implementation Plan

> **Status:** Implemented (PR #632, merged 2026-11-24)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn verified GitOps/source diffs (`ChangeRef`, changed files + hunk lines) and matched KB entries into 100% server-derived clickable HTTPS links across Slack, Matrix, CLI output, and drafted KB PRs, without adding lines to main Slack/Matrix cards or ever trusting model-authored URLs.

**Architecture:** Introduce a pure, zero-dependency `internal/provenance` package that normalizes Git URLs (`https://`, `ssh://`, `git@`), resolves GitHub vs GitLab URL style from a deterministic `HostStyles` map (fail-closed on unknown hosts), and extracts `#L<n>` from unified diff hunks. Record observed changes server-side in `internal/investigate/observedchanges.go` (mirroring `observedresources.go`) and stamp `ChangeURL` + up to 5 `Provenance` file links onto matching `Hypothesis` entries before delivery.

**Tech Stack:** Go 1.26, stdlib `net/url` + `strings` + `html`.

**Spec:** [`docs/superpowers/specs/2026-11-24-clickable-provenance-anchors-design.md`](../specs/2026-11-24-clickable-provenance-anchors-design.md)

## Global Constraints

- **100% server-derived URLs.** Never populate `ChangeURL` or `Provenance` from model output. Unobserved SHAs and unknown Git hosts fail closed (empty URL → plain escaped text).
- **HTTPS only.** Reject `http://`, `file://`, `javascript:`, userinfo, whitespace, `|`, `<`, `>`, or control characters in generated URLs.
- **Zero new config keys.** Derive custom host styles strictly from existing `forge.provider`, `forge.git_host`, and `forge.gitlab.url`.
- **Keep main Slack/Matrix cards compact.** Hyperlink existing `What changed:` and `MatchedKB` lines in place; put changed-file links (cap 5) in detail views (Slack thread reply, Matrix detail, CLI, KB PR).
- **Backward-compatible goldens.** Investigations without `ChangeURL`/`Provenance` render byte-identically to today.
- **Quality gate before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`.
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/providers/providers.go` (modify) | Add `ProvenanceLink` struct; add `ChangeURL` + `Provenance` to `Hypothesis`; add `SectionAnchor` to `MatchedKB` |
| `internal/provenance/provenance.go`, `provenance_test.go` (create) | `HostStyles`, `NormalizeRepoHTTPS`, `CompareURL`, `CommitURL`, `FileURL`, `FirstAddedLine`, `SafeHTTPS` |
| `internal/investigate/observedchanges.go`, `observedchanges_test.go` (create) | Server-side per-investigation recorder of observed GitOps + source-repo diffs and `attachProvenance` matcher |
| `internal/investigate/loop.go`, `tools.go`, `recall.go` (modify) | Record observed changes from `what_changed` and `source_diff`; run `attachProvenance` before delivery; stamp `SectionAnchor: "#resolution"` on `MatchedKB` when the entry has a Resolution section |
| `internal/app/investigate.go` (modify) | Build `provenance.HostStyles` from `cfg.Forge` and wire into `LoopInvestigator` |
| `internal/notify/{format.go,slack.go,matrix.go}` + tests (modify) | Render safe clickable `ChangeURL`, `#resolution` KB anchors, and detail-view `Provenance` file links |
| `internal/curator/curator.go`, `curator_test.go` (modify) | Render markdown links for `ChangeURL` and `### Provenance` file links in drafted KB PRs |

---

## Task 1: Create `internal/provenance` package

**Files:**
- Create: `internal/provenance/provenance.go`
- Create: `internal/provenance/provenance_test.go`

- [x] **Step 1: Write failing table-driven tests in `internal/provenance/provenance_test.go`**

Cover:
- `NormalizeRepoHTTPS`:
  - `https://github.com/org/repo.git` → `https://github.com/org/repo`, `StyleGitHub`, `true`
  - `git@github.com:org/repo.git` → `https://github.com/org/repo`, `StyleGitHub`, `true`
  - `ssh://git@gitlab.com/group/sub/repo.git` → `https://gitlab.com/group/sub/repo`, `StyleGitLab`, `true`
  - Custom self-hosted `git@git.corp.example:platform/infra.git` registered as `StyleGitLab` → `https://git.corp.example/platform/infra`, `StyleGitLab`, `true`
  - Unregistered host `https://git.unknown.example/a/b` → `"", "", false`
  - Cleartext `http://github.com/org/repo`, embedded creds `https://tok@github.com/org/repo`, path traversal `https://github.com/org/../evil` → `false`
- `CompareURL` / `CommitURL` / `FileURL`:
  - GitHub shapes (`/compare/a...b`, `/commit/sha`, `/blob/sha/path#L42`) and GitLab shapes (`/-/compare/a...b`, `/-/commit/sha`, `/-/blob/sha/path#L42`).
  - Path encoding for spaces/special chars (`apps/my app/v(1).yaml`).
  - Invalid/unsafe rev tokens rejected cleanly (`""`).
- `FirstAddedLine`:
  - `@@ -10,4 +12,6 @@` → `12`
  - `@@ -1 +1 @@` → `1`
  - `@@ -10,5 +0,0 @@` (deleted file) → `0`
  - Binary / empty patch → `0`

- [x] **Step 2: Run test to verify failure**

```bash
go test ./internal/provenance/...
```

Expected: FAIL.

- [x] **Step 3: Implement `internal/provenance/provenance.go`**

Implement `ForgeStyle`, `HostStyles`, `DefaultHostStyles(forgeProvider, gitHost, gitlabURL string)`, `NormalizeRepoHTTPS`, `CompareURL`, `CommitURL`, `FileURL`, `FirstAddedLine`, and `SafeHTTPS`.

- [x] **Step 4: Run tests with race detector**

```bash
go test -race -v ./internal/provenance/...
```

Expected: PASS.

---

## Task 2: Track observed changes and attach server-derived provenance in `internal/investigate`

**Files:**
- Modify: `internal/providers/providers.go`
- Create: `internal/investigate/observedchanges.go`
- Create: `internal/investigate/observedchanges_test.go`
- Modify: `internal/investigate/loop.go`, `tools.go`, `recall.go`
- Modify: `internal/app/investigate.go`

- [x] **Step 1: Write failing tests in `internal/investigate/observedchanges_test.go`**

Cover:
- Recorded GitOps change (`github.com/org/infra`, `aaa1111`..`bbb2222`, 6 changed files) matched against `Hypothesis{ChangeRef: "aaa1111..bbb2222", Evidence: ["apps/harbor/values.yaml bumped tag"]}`:
  - `ChangeURL == "https://github.com/org/infra/compare/aaa1111...bbb2222"`.
  - `Provenance` has 5 entries (capped) with `apps/harbor/values.yaml#L<n>` ranked first because it appeared in `Evidence`.
- Single-commit observed change (`FromRev == ""` or `FromRev == ToRev`) emits `/commit/<ToRev>`.
- Unobserved SHA (`ChangeRef: "deadbeef1234"`) produces empty `ChangeURL` and empty `Provenance`.
- Unknown host fails closed (`ChangeURL == ""`, `Provenance == nil`).
- `MatchedKB` with a `## Resolution` section receives `SectionAnchor: "#resolution"`; without one receives `""`.

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/investigate/... -run TestObservedChanges
```

Expected: FAIL.

- [x] **Step 3: Implement `observedchanges.go` and wire into `loop.go`, `tools.go`, `recall.go`, and `app/investigate.go`**

- Add `ProvenanceLink`, `Hypothesis.ChangeURL`, `Hypothesis.Provenance`, and `MatchedKB.SectionAnchor` in `internal/providers/providers.go`.
- Add thread-safe `observedChanges` recorder (`sync.Mutex`, mirroring `observedResources`) passed via context or tool wrapper into `WhatChangedTool` and `SourceDiffTool` so each materialized diff records `{RepoURL, FromRev, ToRev, Files}` with zero extra Git calls.
- Add `HostStyles provenance.HostStyles` onto `LoopInvestigator` (populated in `internal/app/investigate.go` via `provenance.DefaultHostStyles(...)`; defaults to GitHub+GitLab public hosts when nil).
- Call `attachProvenance(&inv, observed, li.HostStyles)` in `loop.go` right after verify and before delivery/curation.
- In `recall.go` / `kb_search` match assembly, check `catalog.Section(entry.Body, "Resolution") != ""` and set `MatchedKB.SectionAnchor = "#resolution"`.

- [x] **Step 4: Run tests**

```bash
go test -race ./internal/investigate/... ./internal/app/...
```

Expected: PASS.

---

## Task 3: Render safe clickable links in Slack, Matrix, CLI, and KB PRs

**Files:**
- Modify: `internal/notify/format.go`, `format_test.go`
- Modify: `internal/notify/slack.go`, `slack_test.go`, `card_golden_test.go`
- Modify: `internal/notify/matrix.go`, `matrix_test.go`
- Modify: `internal/curator/curator.go`, `curator_test.go`

- [x] **Step 1: Write failing tests across `notify` and `curator`**

- `slack_test.go`:
  - Main card hyperlinks `What changed:` as `<https://github.com/org/infra/compare/a...b|a..b>` with zero added blocks/lines; label containing `|>` is escaped via `escapeMrkdwn`; non-HTTPS or malformed `ChangeURL` degrades to plain escaped code span.
  - Thread detail renders up to 5 `<url|label>` provenance links.
  - `MatchedKB` link appends `#resolution` when `SectionAnchor` is set.
- `matrix_test.go`: HTML `<a href="...">` properly escaped for `ChangeURL`, `MatchedKB` `#resolution`, and detail `Provenance`.
- `format_test.go`: CLI prints `What changed: <ref> (<url>)` and indented `↳ <label>: <url>` lines.
- `curator_test.go`: KB PR markdown renders `[<ref>](<url>)` and `### Provenance` file link list.

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/notify/... ./internal/curator/... -run 'Test.*Provenance|Test.*ChangeURL'
```

Expected: FAIL.

- [x] **Step 3: Implement safe per-surface link rendering**

- Use `provenance.SafeHTTPS` guard before emitting any clickable link in `slack.go`, `matrix.go`, `format.go`, and `curator.go`.
- Preserve byte-identical rendering whenever `ChangeURL == ""` and `len(Provenance) == 0` so all existing golden snapshots stay green.

- [x] **Step 4: Run full quality gate**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected: all tests PASS, `gofmt -l .` prints nothing, `hack/lint.sh` reports `0 issues`.

- [x] **Step 5: Commit**

Commit using Conventional Commits (English, no co-author line), e.g.:
`feat(provenance): attach server-derived Git and KB permalinks to cards and KB PRs`
