# Server Hardening (R9) — Design Spec

- **Date:** 2026-06-24
- **Status:** Draft (awaiting review)
- **Type:** Hardening (HTTP server + webhook auth + ingress scope)
- **Author:** RunLore maintainers

## 1. Problem

Item R9 collects four bounded ingress-hardening gaps on the HTTP serve path. Each
is independently shippable; together they close the "anonymous/unbounded inbound
request" surface in front of the LLM investigator.

### (a) `http.Server` has no timeouts or header/body caps — `cmd/lore/main.go:326`

```go
httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
```

With no `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout`/
`MaxHeaderBytes`, the server is open to Slowloris (slow-header), slow-body, and
unbounded-idle-connection exhaustion. Go's defaults for all of these are **zero =
no limit**.

### (b) Alertmanager body is decoded with no size cap — `internal/trigger/incident.go:33` (called from `internal/server/server.go:367`)

`ParseAlertmanager(r.Body)` streams `json.NewDecoder(r).Decode(&p)` directly off
the unbounded request body. The Slack handler already caps at 1 MiB
(`server.go:244`, `io.LimitReader(r.Body, 1<<20)`); the alert webhook does not, so
a single large POST forces unbounded allocation. (`MaxHeaderBytes` from (a) bounds
headers, not the body — these are distinct.)

### (c) Anonymous alert webhook when the LLM is wired but `actions.mode≠auto` — `internal/config/config.go:441`

`Validate()` only *requires* `server.webhook_token_env` when `actions.mode=auto`
(`config.go:441-443`). In `off`/`suggest`/`approve`, the webhook accepts anonymous
POSTs whose labels/annotations flow verbatim into the investigator's LLM prompt
(`server.go:360-361` documents exactly this data-flow). So a server with a
configured model but no executing actions still lets an anonymous caller drive
(and bill) the LLM.

### (d) NetworkPolicy ingress has no `from:` selector — `deploy/helm/runlore/templates/networkpolicy.yaml:16`

```yaml
ingress:
  - ports:
      - protocol: TCP
        port: {{ .Values.service.port }}
```

A `policyTypes: [Ingress]` rule with ports but no `from:` allows **all pods/IPs**
to reach the webhook port. There is no knob to scope ingress to Alertmanager.

## 2. Goals / Non-goals

**Goals**
- Bound every inbound HTTP dimension: header read, body read, response write,
  idle, header size, body size.
- Authenticate the alert webhook whenever the LLM investigator is wired on the
  **serve** path — without breaking `lore investigate` or any non-serve subcommand.
- Provide an **optional** ingress `from:` scope; **default permissive** so existing
  installs do not break.

**Non-goals**
- Changing the Slack/control-endpoint auth (already token/HMAC gated).
- Touching `networkpolicy.yaml` **egress** — R3 owns that block (coordinate note).
- A breaking, mandatory ingress lockdown (out-of-box ingress must keep working).
- mTLS / per-caller identity (out of scope; bearer token is the contract).

## 3. CHALLENGE — each finding vs current code

### (a) Server timeouts — **VALID.** `cmd/lore/main.go:326`
The struct is built with only `Addr`+`Handler`; the four timeouts and
`MaxHeaderBytes` default to zero (unlimited) in `net/http`. Slowloris and
idle-conn exhaustion are real against a long-lived in-cluster server. **Verdict:
fix.** Values: `ReadHeaderTimeout 5s`, `ReadTimeout 30s`, `WriteTimeout 30s`,
`IdleTimeout 60s`, `MaxHeaderBytes 1<<20`. Rationale: webhook payloads are small
and synchronous; 30s read/write is generous for Alertmanager/Slack while still
killing slow attackers. *Not overstated.*

### (b) Body cap — **VALID, with a placement refinement.** `server.go:367`
The brief says wrap `http.MaxBytesReader(w, r.Body, 1<<20)` "at the call site
(`server.go:367`) before decode." `ParseAlertmanager` takes an `io.Reader`
(`incident.go:31`), so wrapping at the call site is the right seam — the trigger
package stays HTTP-agnostic (it does not import `net/http`). **Verdict: fix at
`server.go`, not in `incident.go`.**

*Status-code nuance (decided):* `MaxBytesReader` makes `Decode` fail once the cap
is exceeded; the current handler maps any decode error to **400** (`server.go:369`).
The brief's test bar is "413/400". To make the cap explicit and observable we set
`r.Body = http.MaxBytesReader(w, r.Body, 1<<20)` and, on decode error, detect
`*http.MaxBytesError` and return **413** (else keep **400** for malformed JSON).
This is strictly more informative than a bare 400 and satisfies the bar.

### (c) Require token when LLM wired — **VALID, but MUST NOT go in `Validate()`.** `config.go:441`
**Challenge (both sides):**
- *For the brief's framing:* an anonymous webhook in `suggest`/`approve` with a
  model set is a real exposure (LLM prompt injection + cost). True.
- *Against putting it in `Validate()`:* `config.Load()` → `Validate()` is shared by
  **every** subcommand (`config.Load` call sites: `main.go:143` serve, `:1108`
  investigate, `:521`, `:822`, `:926`, `validate.go:34`). `lore investigate`
  legitimately requires a model (`main.go:1112 modelConfigured`) and has **no**
  webhook — so a `Model.BaseURL ⇒ require webhook_token_env` rule in `Validate()`
  would break `investigate` and the other non-serve paths. **This is the trap the
  brief flags.**

**Verdict: enforce on the serve path only.** In `runServe`, after
`webhookToken := os.Getenv(cfg.Server.WebhookTokenEnv)` (`main.go:207`), if the
LLM investigator is wired (`modelConfigured(cfg)` — the same predicate
`investigate` uses, covering anthropic/gemini built-in endpoints **and** any
provider with a `base_url`, not just `BaseURL!=""`) and `webhookToken == ""`,
**refuse to start** with a fail-closed error — mirroring the existing
approval-token guard at `main.go:190-193`. This scopes the requirement to exactly
the path that serves the webhook, leaving `Validate()` and the CLI untouched.

*Why `modelConfigured` not `Model.BaseURL != ""`:* the brief says "when
`Model.BaseURL` set", but `anthropic`/`gemini` wire a working investigator with an
empty `BaseURL` (built-in endpoint) — see `main.go:410-416`. Using
`modelConfigured` closes that hole too and stays consistent with how `serve`
itself decides to build the LLM investigator. Documented deviation.

### (d) Ingress `from:` scope — **VALID, default-permissive.** `networkpolicy.yaml:16`
A ports-only ingress rule is cluster-wide-open on the webhook port. **Verdict:
add an optional `networkPolicy.ingressFrom` knob** (a list of NetworkPolicyPeers
spliced verbatim into `from:`). **Default `[]` ⇒ render no `from:` ⇒ current
permissive behaviour preserved** (no out-of-box break). When set (e.g. a
namespaceSelector for the Alertmanager namespace), ingress is scoped to those
peers. This mirrors the existing `extraEgress` pattern (`values.yaml:178`) — a
documented values knob, not a hardcoded default — which the project already uses
for the symmetric egress case. **Egress is untouched (R3's block).**

## 4. Changes

| # | File | Change |
|---|------|--------|
| a | `cmd/lore/main.go` (~326) | Set `ReadHeaderTimeout 5s`, `ReadTimeout 30s`, `WriteTimeout 30s`, `IdleTimeout 60s`, `MaxHeaderBytes 1<<20` on `httpSrv`. |
| b | `internal/server/server.go` (~367) | `r.Body = http.MaxBytesReader(w, r.Body, 1<<20)` before `ParseAlertmanager`; on decode error, return **413** for `*http.MaxBytesError`, else **400**. |
| c | `cmd/lore/main.go` (~207) | After reading `webhookToken`: if `modelConfigured(cfg) && webhookToken == ""` → return fail-closed error (do not start). `Validate()` unchanged. |
| d | `deploy/helm/runlore/templates/networkpolicy.yaml` (~16); `values.yaml` (~177) | Optional `networkPolicy.ingressFrom` list → `from:`. Default `[]` ⇒ no `from:` (permissive). Egress untouched. |

## 5. Testing (TDD, stdlib `testing`, table-driven where it fits)

- **(b) oversized body → 413; malformed → 400; normal → 202.**
  `internal/server/server_test.go`: POST `/webhook/alertmanager` with a >1 MiB body
  → 413; existing `TestHandleAlertmanagerBadBody` (400) and `TestHandleAlertmanager`
  (202) keep passing.
- **(c) missing token when LLM wired → start refused.** Unit-test the guard
  predicate (`modelConfigured(cfg) && token==""`) in `cmd/lore/main_test.go` — a
  table over {model set / unset} × {token set / empty} asserting the
  refuse-to-start boolean. (Driving full `runServe` needs a cluster; the guard is
  the load-bearing logic, tested in isolation like the existing approval-token
  guard's intent.)
- **(a) server constructed with non-zero timeouts.** A small constructor seam so
  the `*http.Server` is built by a testable function and a test asserts all five
  fields are non-zero. (Extract `newHTTPServer(addr, h) *http.Server` in
  `cmd/lore`; `runServe` calls it.)
- **(d) `helm template` shows optional ingress scoping.** Assert default render has
  **no** `from:`; render with `--set networkPolicy.ingressFrom[0].namespaceSelector...`
  shows the `from:` block. Covered by `helm template` in the gate + a documented
  command in the plan.

## 6. Gate (full, before each commit)

`go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
(expect `0 issues`); for the chart change additionally `helm lint
deploy/helm/runlore` + `helm template` (default and `--set ingressFrom`).

## 7. Coordinate-with note

R3 already changed `networkpolicy.yaml` **egress** on a separate branch. This work
touches **only the ingress block** (and adds the `ingressFrom` value). No egress
edits here — merge order is independent.

## 8. Success criteria

- **SC-1:** `httpSrv` has all five timeout/size fields set (test asserts non-zero).
- **SC-2:** A >1 MiB alert POST returns 413; malformed returns 400; valid returns 202.
- **SC-3:** `serve` refuses to start when a model is configured and the webhook
  token env is empty; `lore investigate` with a model and no webhook token still
  runs (Validate untouched).
- **SC-4:** Default `helm template` ingress has no `from:`; with `ingressFrom` set,
  the `from:` peers render. `helm lint` clean.
- **SC-5:** Full gate green; no behaviour change to Slack/control auth or egress.
