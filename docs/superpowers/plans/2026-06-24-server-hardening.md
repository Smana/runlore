# Server Hardening (R9) — Implementation Plan

> **For agentic workers:** execute task-by-task with TDD (test → fail → impl →
> pass → commit). Spec: `docs/superpowers/specs/2026-06-24-server-hardening-design.md`.

**Goal:** Close four bounded inbound-hardening gaps on the serve path — HTTP server
timeouts/caps, an alert-body size cap, a serve-scoped webhook-auth requirement when
the LLM is wired, and an optional (default-permissive) NetworkPolicy ingress scope.

**Architecture:** (a) extract `newHTTPServer(addr, h) *http.Server` in `cmd/lore` so
the timeouts are set in one testable place; (b) wrap `r.Body` with
`http.MaxBytesReader` at the `server.go` call site (keeps `internal/trigger`
HTTP-agnostic) and map `*http.MaxBytesError` → 413; (c) a fail-closed guard in
`runServe` (NOT in `config.Validate`, which is shared by all subcommands); (d) a
`networkPolicy.ingressFrom` values list spliced into `from:`, empty by default.

**Tech Stack:** Go (stdlib `net/http`, `testing` — no testify), Helm/Go templates.

## Global Constraints
- `Validate()` is shared by `serve` + `investigate` + others (`config.Load` sites:
  `main.go:143,521,822,926,1108`, `validate.go:34`) — the webhook-auth rule must NOT
  live there (would break `lore investigate`). Serve-path guard only.
- Use `modelConfigured(cfg)` (covers anthropic/gemini built-in endpoints + any
  `base_url`), not `Model.BaseURL != ""` — consistency with how serve builds the LLM.
- NetworkPolicy **egress is R3's** — touch only the ingress block + add the value.
- Default `ingressFrom: []` ⇒ no `from:` rendered ⇒ no out-of-box break.
- `%w` wrapping, `ctx` first param; full gate green before each commit;
  `golangci-lint run ./...` = 0 issues; chart change ⇒ `helm lint` + `helm template`.

---

### Task 0: Spec + plan (this file)
**Files:** `docs/superpowers/specs/2026-06-24-server-hardening-design.md`, this plan.
- [x] Write spec (CHALLENGE each gap, verdict + file:line, decisions).
- [x] Write plan. Commit `docs(server-hardening): spec + plan (R9)`.

### Task 1: (a) HTTP server timeouts + caps
**Files:** Modify `cmd/lore/main.go`; add `cmd/lore/main_test.go` case.
- [ ] Test: `newHTTPServer(":0", h)` returns a `*http.Server` with all five fields
  (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`,
  `MaxHeaderBytes`) non-zero.
- [ ] Impl: extract `newHTTPServer(addr string, h http.Handler) *http.Server` with
  5s/30s/30s/60s + `1<<20`; `runServe` calls it. `go test ./cmd/...`; commit
  `feat(server): HTTP server timeouts + MaxHeaderBytes (Slowloris/DoS)`.

### Task 2: (b) Alert-body size cap → 413
**Files:** Modify `internal/server/server.go`; add cases to
`internal/server/server_test.go`.
- [ ] Test: POST `/webhook/alertmanager` with a >1 MiB body → 413; reaffirm
  malformed → 400 and valid → 202 (existing tests stay green).
- [ ] Impl: `r.Body = http.MaxBytesReader(w, r.Body, 1<<20)` before
  `trigger.ParseAlertmanager`; on error, `errors.As(err, &mbErr)` → 413, else 400.
  `go test ./internal/server/`; commit
  `feat(server): cap Alertmanager webhook body at 1 MiB (413)`.

### Task 3: (c) Serve-scoped webhook-auth requirement
**Files:** Modify `cmd/lore/main.go` (~207, after `webhookToken`); add
`cmd/lore/main_test.go` case.
- [ ] Test: table over {model configured y/n} × {token "" / set} asserting the
  guard's refuse-to-start boolean (`modelConfigured(cfg) && token==""`). Confirm a
  model-configured config with empty token trips it; `investigate`'s Validate path
  is unaffected (Validate unchanged — assert a model-only config still
  `Load`s/`Validate`s clean in `internal/config`).
- [ ] Impl: after reading `webhookToken`, if `modelConfigured(cfg) &&
  webhookToken == ""` → `return fmt.Errorf("model configured but %s is empty:
  refusing to start with an unauthenticated alert webhook (fail closed)",
  cfg.Server.WebhookTokenEnv)` (mirrors the `main.go:190` approval guard).
  `go test ./cmd/... ./internal/config/...`; commit
  `feat(server): require webhook token when the LLM investigator is wired`.

### Task 4: (d) Optional NetworkPolicy ingress scope
**Files:** Modify `deploy/helm/runlore/templates/networkpolicy.yaml` (ingress block
only), `deploy/helm/runlore/values.yaml`.
- [ ] Test: `helm template deploy/helm/runlore` (default) → ingress has **no**
  `from:`; `helm template ... --set 'networkPolicy.ingressFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring'`
  → `from:` block present. `helm lint deploy/helm/runlore`.
- [ ] Impl: in the ingress rule, `{{- with .Values.networkPolicy.ingressFrom }}from:
  {{- toYaml . | nindent 8 }}{{- end }}`; add `ingressFrom: []` to `values.yaml`
  under `networkPolicy` with a comment + an Alertmanager namespaceSelector example.
  Commit `feat(helm): optional NetworkPolicy ingress scope (default permissive)`.

### Task 5: Final gate
- [ ] `go build ./... && go vet ./... && go test ./... && gofmt -l . &&
  golangci-lint run ./...` (0 issues); `helm lint deploy/helm/runlore`;
  `helm template` default + `--set ingressFrom`. Paste outputs into the report.

## Out of scope / coordinate
- NetworkPolicy **egress** (R3, separate branch) — do not edit.
- Slack/control-endpoint auth — already gated, untouched.
- mTLS / per-caller identity — bearer token is the contract.
