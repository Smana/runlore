# Live GCP CloudProvider Verification & Embedded KB HTML Explorer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** (Track A) Capture and verify real live GKE Standard payloads for RunLore's GCP CloudProvider (`logging`, `container`, `compute`) into redacted regression fixtures and lift the "lenses unobserved" caveat; (Track B) ship an opt-in, zero-dependency, read-only embedded Go HTML explorer for the OKF catalog and outcome ledger at `/kb` (`lore serve`) and `lore kb serve`.

**Architecture:** Track A adds an env-gated live capture test (`TestGCPLiveCapture`) plus hermetic tests over captured redacted fixtures in `internal/providers/cloud/gcp/testdata/`. Track B adds a self-contained `internal/kbui` package using Go stdlib `html/template`, `embed.FS`, and an escape-first markdown subset renderer (zero JS, strict CSP), wired behind `server.kb_ui.enabled` (off by default) and `lore kb serve` (loopback default).

**Tech Stack:** Go 1.26, stdlib `html/template` + `embed` + `html` + `crypto/subtle`, `internal/catalog`, `internal/outcome`, GCP REST APIs.

**Spec:** [`docs/superpowers/specs/2026-11-24-gcp-live-verify-and-kb-ui-design.md`](../specs/2026-11-24-gcp-live-verify-and-kb-ui-design.md)

## Global Constraints

- **Track A and Track B are independently committable** — Track B must not block on access to a live GKE cluster.
- **Zero third-party dependencies for Track B.** Stdlib `html/template`, `embed`, and pure-Go escape-first markdown only.
- **Zero JS, zero external assets, strict CSP** (`default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`) on every HTML response.
- **Off by default on `lore serve`** (`server.kb_ui.enabled: false`); **loopback-only default on `lore kb serve`** (`127.0.0.1:8081`).
- **Keep GCP tier-3 `node-provider-id` intact** (Autopilot remains a separate follow-up).
- **Redact every captured GCP payload** before committing under `testdata/` (no real project IDs, numbers, IPs, emails, or tokens).
- **Quality gate before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`.
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/providers/cloud/gcp/live_capture_test.go` (create) | Env-gated (`RUNLORE_GCP_LIVE_CAPTURE=1`) live capture & payload redactor + hermetic captured-fixture parser tests |
| `internal/providers/cloud/gcp/testdata/captured_*.json` (create during live run) | Redacted live GKE Standard Cloud Logging, Container, and Compute payloads |
| `internal/providers/cloud/gcp/{auditlog.go,resourcehealth.go}` (modify if needed) | Fix any real-API schema or filter discrepancies discovered during capture |
| `website/content/docs/integrations/data-sources/gcp-cloud.md`, `design.md`, `integrations/_index.md`, `README.md` (modify) | Update GCP live-validation status once captured payloads pass |
| `internal/kbui/markdown.go`, `markdown_test.go` (create) | Escape-first safe markdown subset renderer (`https://`-only links, heading slugs, code blocks, lists) |
| `internal/kbui/handler.go`, `handler_test.go`, `templates/*.html` (create) | Embedded `html/template` read-only KB & outcome explorer HTTP handler with CSP, auth, search, filters, and entry detail |
| `internal/config/config.go` (or `config_server.go`), `config_test.go` (modify) | Add `server.kb_ui: {enabled, token_env}` config + validation |
| `internal/server/server.go`, `server_test.go` (modify) | Mount optional non-forwarded `/kb` handler when configured |
| `internal/app/kb_ui.go`, `kb_cmd.go`, `serve.go` + tests (modify/create) | Wire `BuildKBUIHandler` into `serve` and add `lore kb serve` subcommand |
| `cmd/lore/main.go`, `website/content/docs/{configuration/configuration.md,security/security-model.md,reference/cli.md}` (modify) | Document `server.kb_ui`, security posture, and `lore kb serve` |

---

## Track A — Live GCP CloudProvider Verification

### Task A1: Build env-gated capture test & redactor

**Files:**
- Create: `internal/providers/cloud/gcp/live_capture_test.go`

- [ ] **Step 1: Write `redactGCPFixture` helper and hermetic redaction unit test**

Ensure `redactGCPFixture(raw []byte, projectID, projectNum, cluster string) []byte` deterministically scrubs project IDs, project numbers, email addresses, IPv4 addresses, and bearer/token fields while keeping JSON structure valid.

- [ ] **Step 2: Write env-gated `TestGCPLiveCapture` (skipped unless `RUNLORE_GCP_LIVE_CAPTURE=1`)**

Calls real GCP endpoints (ADC):
1. `logging.googleapis.com/v2/entries:list` (normal + `protoPayload.status.code!=0` filter)
2. `container.googleapis.com/v1` cluster & node pools
3. `compute.googleapis.com/compute/v1` MIG `listErrors` & instances
4. Verifies negative-path `Preflight` 403 stderr `gcloud` command formatting.
Writes redacted JSON into `internal/providers/cloud/gcp/testdata/captured_{auditlog,auditlog_failed,container,compute}.json`.

- [ ] **Step 3: Run hermetic unit tests to verify skip-by-default and redactor**

```bash
go test -race -v ./internal/providers/cloud/gcp/... -run 'TestRedactGCPFixture|TestGCPLiveCapture'
```

Expected: `TestRedactGCPFixture` PASS; `TestGCPLiveCapture` SKIP when env unset.

### Task A2: Execute live capture on GKE Standard, add captured-fixture tests, fix any discrepancies & update docs

- [ ] **Step 1: Run live capture against a GKE Standard cluster**

```bash
RUNLORE_GCP_LIVE_CAPTURE=1 RUNLORE_GCP_PROJECT=<project> RUNLORE_GCP_LOCATION=<loc> RUNLORE_GCP_CLUSTER=<cluster> \
  go test -v ./internal/providers/cloud/gcp/... -run TestGCPLiveCapture
```

Inspect captured `testdata/captured_*.json` to confirm zero secrets or real account identifiers remain.

- [ ] **Step 2: Add hermetic tests parsing `testdata/captured_*.json` and fix any parser/filter bugs found**

Verify `auditlog.go` (`CloudChanges`, including failed-only filter behavior) and `resourcehealth.go` (`ResourceHealth`, `StatusCondition`, MIG `listErrors`) against the real captured payloads.

- [ ] **Step 3: Update GCP docs (`gcp-cloud.md`, `design.md`, `integrations/_index.md`, `README.md`)**

Mark questions 2, 3, 5, and negative-path 6 as settled with GKE Standard version + capture date; keep Autopilot (tier-3 `node-provider-id`) explicitly marked as the one remaining open mode.

- [ ] **Step 4: Run quality gate & commit Track A**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Commit: `test(gcp): verify CloudProvider against captured live GKE Standard payloads`

---

## Track B — Embedded Read-Only KB HTML Explorer (`internal/kbui`)

### Task B1: Escape-first safe markdown renderer (`internal/kbui/markdown.go`)

**Files:**
- Create: `internal/kbui/markdown.go`
- Create: `internal/kbui/markdown_test.go`

- [ ] **Step 1: Write failing XSS & formatting tests in `internal/kbui/markdown_test.go`**

Test:
- Raw `<script>alert(1)</script>`, `<img src=x onerror=alert(1)>`, `"><svg onload=alert(1)>` are HTML-escaped and never emit live tags.
- `[evil](javascript:alert(1))` and `[evil](data:text/html,...)` render as plain escaped text (no `<a>` tag).
- `[safe](https://github.com/org/repo/commit/abc)` renders `<a href="https://github.com/org/repo/commit/abc" rel="noopener noreferrer">safe</a>`.
- `## Resolution` renders `<h2 id="resolution">Resolution</h2>`.
- Fenced code blocks, bullet lists, numbered lists, bold, and inline code render cleanly.

- [ ] **Step 2: Run test to verify failure**

```bash
go test ./internal/kbui/...
```

Expected: FAIL.

- [ ] **Step 3: Implement `internal/kbui/markdown.go` and verify tests pass**

```bash
go test -race -v ./internal/kbui/...
```

Expected: PASS.

### Task B2: Embedded `html/template` HTTP handler (`internal/kbui/handler.go`)

**Files:**
- Create: `internal/kbui/handler.go`
- Create: `internal/kbui/handler_test.go`
- Create: `internal/kbui/templates/index.html`, `internal/kbui/templates/entry.html`

- [ ] **Step 1: Write failing handler tests in `internal/kbui/handler_test.go`**

Test:
- `GET /kb` lists catalog entries with type/status/origin badges and live outcome ledger metrics (resolve rate, 👍/👎, Beta trust factor, staleness indicator).
- `GET /kb?q=oom&type=Incident&origin=local` filters and ranks via `Catalog.SearchScored`.
- `GET /kb/entry/<path>` renders frontmatter card, outcome stats, and safe markdown body.
- Path traversal (`GET /kb/entry/../../etc/passwd`) returns `404`.
- `POST /kb` returns `405 Method Not Allowed`.
- Configured Bearer token: missing/wrong token → `401` (constant-time compare); valid token → `200`.
- Every response includes strict `Content-Security-Policy`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, and zero `<script` tags.

- [ ] **Step 2: Implement `handler.go` and embedded HTML templates (`//go:embed templates/*.html`)**

Keep CSS minimal, inline, dark/light (`prefers-color-scheme`) friendly, and zero-JS.

- [ ] **Step 3: Run `internal/kbui` tests with race detector**

```bash
go test -race -v ./internal/kbui/...
```

Expected: PASS.

### Task B3: Wire `server.kb_ui`, `lore kb serve`, docs & full quality gate

**Files:**
- Modify: `internal/config/config.go` (or `config_server.go`), `config_test.go`
- Modify: `internal/server/server.go`, `server_test.go`
- Create: `internal/app/kb_ui.go`, `kb_ui_test.go`
- Modify: `internal/app/kb_cmd.go`, `serve.go`, `cmd/lore/main.go`
- Modify: `website/content/docs/{configuration/configuration.md,security/security-model.md,reference/cli.md}`

- [ ] **Step 1: Write failing config, server, and CLI wiring tests**

- `server_test.go`: `/kb` returns `404` when `KBUI` handler is nil (default); returns `200` directly from the replica (not leader-forwarded) when wired.
- `kb_ui_test.go`: `BuildKBUIHandler` returns nil when `server.kb_ui.enabled == false`; warns when enabled without `token_env`; enforces Bearer auth when `token_env` is set.
- `kb_cmd_test.go`: `lore kb serve` defaults `--addr` to `127.0.0.1:8081` and serves a temp fixture catalog + optional outcome JSONL ledger.

- [ ] **Step 2: Implement wiring, `lore kb serve`, and docs updates**

- Add `ServerKBUI{Enabled bool, TokenEnv string}` under `config.ServerConfig`.
- Mount `/kb`, `/kb/`, and `/kb/entry/{path...}` in `internal/server/server.go` alongside `/mcp` (non-forwarded).
- Add `lore kb serve [--config <path>] [--dir <catalog>] [--ledger <jsonl>] [--addr 127.0.0.1:8081]` in `internal/app/kb_cmd.go` and update `cmd/lore/main.go` usage.
- Update `configuration.md`, `security-model.md` (document `/kb` exposure & read-only CSP posture alongside `/mcp`), and `reference/cli.md`.

- [ ] **Step 3: Run full quality gate**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected: all tests (including `internal/docsguard`) PASS, `gofmt -l .` prints nothing, `hack/lint.sh` reports `0 issues`.

- [ ] **Step 4: Commit Track B**

Commit using Conventional Commits (English, no co-author line), e.g.:
`feat(kbui): add opt-in zero-dep read-only HTML explorer for KB and outcome ledger`
