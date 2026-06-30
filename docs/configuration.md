# Configuration reference

A navigable map of RunLore's configuration, organized by subsystem.

> [!note] `values.yaml` is the authoritative reference
> The single source of truth for every key is the chart's
> [`deploy/helm/runlore/values.yaml`](../deploy/helm/runlore/values.yaml) — every key carries an
> inline rationale comment, and the whole `config:` block is rendered **verbatim** into the agent's
> ConfigMap (`toYaml .Values.config`). This page is the overview and the *why*; `values.yaml` is the
> exhaustive, annotated detail. The `model` / `catalog` / `outcome` / `notify` / `forge` / `network`
> blocks ship commented-out there as copy-paste templates.

## The config model — three things to know

1. **Helm → ConfigMap → agent.** Everything under `values.config.*` becomes the agent's config file.
   Outside Kubernetes, `lore serve --config runlore.yaml` reads the same shape directly.
2. **Strict decoding — typos fail loudly.** The loader uses `KnownFields(true)`: an unknown key aborts
   startup rather than being silently ignored, so a misspelled safety-critical field can't slip
   through. **One exception:** the `sources:` map (it's a free-form map); a mistyped *source* key is
   instead caught at startup by the source builder, which errors with
   `unknown source(s) … under \`sources:\``.
3. **Secrets by indirection.** Config never holds a secret value — it names the **environment
   variable** (or Secret key) that holds it: `api_key_env`, `webhook_url_env`, `private_key_ref`, etc.
   Wire those env vars from a Kubernetes `Secret` (`envFrom`/`env` in the chart). A stray dump of the
   config therefore leaks nothing.

---

## Subsystems

Defaults below are the behaviorally-significant ones (applied in the config loader). For the full key
list and comments, follow each link to `values.yaml`.

### `sources` — what wakes RunLore
Per-source enablement map; presence enables a source. `alertmanager: {}` mounts the incident webhook;
`gitops: { enabled: true }` watches GitOps `Ready=False`. Known keys: `alertmanager`, `gitops`.

### `triggers` — which incidents to investigate
- `incidents.match` / `incidents.ignore` — ANDed matchers (`severity`, `environment`, `namespaces`
  globs, `alertnames` globs, `labels`); empty fields match anything. `ignore` excludes even if `match`
  passes.
- `incidents.dedup.window` — don't re-open a still-firing alert within this window.
- `gitops_failures.debounce` — require a failure to persist this long before investigating. **Default
  60s**; explicit `0` fires immediately on every `Ready=False`.

### `investigation` — loop bounds & noise control
- `coalesce` — fold an alert storm into one investigation. Defaults when enabled: `debounce` **30s**,
  `max_wait` **2m**, `max_batch` **50**, `cooldown` **10m**; `correlation_labels` group related alerts.
- `rate_limit` — `max_per_window` (**0 = unlimited**), `window` (default **1h** when a budget is set),
  `max_requeues`.
- `timeout` — per-investigation deadline, **default 10m** (bounds a hung tool/clone so it can't starve
  the single-worker queue).
- `tool_timeout` — per-**tool**-call timeout, **default 60s** (0 = use the default). Bounds a single
  hung/slow provider (a stuck git clone, an unresponsive metrics/logs endpoint) so it can't eat the
  whole per-investigation budget; on expiry the tool result is a non-fatal "timed out" note and the
  investigation continues.
- `max_steps` (**default 20**), `max_tool_output_bytes` (0 = unlimited), `max_tokens_per_investigation`
  (0 = unlimited, else a hard token ceiling).
- `pod_log_namespaces` — **app-layer allowlist** of namespaces the `pod_logs` tool
  may read RAW pod logs from, *beyond* the incident's own namespace (which is always allowed; RBAC
  still gates the actual read). Pod logs carry secrets/PII and are streamed to the external LLM, so the
  model is constrained here — not by Kubernetes RBAC alone. **Empty ⇒ incident namespace only** (secure
  by default). This **must be a superset of where `pods/log` RBAC is granted** (`rbac.controllerLogNamespaces`)
  or `pod_logs` is blocked at the app layer for those namespaces. **The chart auto-defaults this to
  `rbac.controllerLogNamespaces`** when you leave `config.investigation.pod_log_namespaces` unset, so the
  app-layer allowlist and the RBAC scope stay in sync automatically — set it explicitly only to widen or
  narrow beyond the RBAC scope. See [Security model → least-privilege RBAC](security-model.md).

### `catalog` — the knowledge base & instant recall
- `dir` — local OKF bundle / git-sync mirror path; `git` — `url`, `branch` (default `main`), `interval`
  (default 5m), `token_env`.
- `instant_recall` — `enabled`, and when enabled the trust gates `min_score` **1.0**, `margin_gap`
  **1.0**, `solo_floor` **4.0**, `outcome_prior` **2.0**, `outcome_floor` **0.5**;
  `require_workload_match`; experimental `hybrid*` vector knobs.
- The search index is in-memory bleve (BM25), rebuilt from `dir` at startup — not persisted.

### `outcome` — the learning ledger
`ledger_path` — append-only JSONL of investigation outcomes (empty disables). Drives outcome-weighted
recall decay; **must be on the PVC** to compound (see [Upgrade & Uninstall](upgrade-uninstall.md)).

### `actions` — the autonomy ladder (off by default)
- `mode` — `off` (default) · `suggest` · `approve` · `auto` (experimental, frozen/not recommended).
- `allow` — the envelope: `reversible_only`, `namespaces` (allowlist — **empty permits nothing**),
  `protected_namespaces` (added to built-in `flux-system`/`kube-system` denies), `max_blast_radius`,
  `kinds`.
- `require_approval` + `approval_token_env`, `audit_log_path`, and `auto.*` (`dry_run`,
  `min_confidence`, `max_per_window`, `window`).
- **Fail-closed validation:** `approve`/`auto` require `approval_token_env`; `auto` *additionally*
  requires `audit_log_path`, `server.webhook_token_env`, `auto.min_confidence > 0`,
  `auto.max_per_window > 0`, and a non-empty `allow.namespaces`. See [Security model](security-model.md).

### `model` — the LLM provider
`provider` — `openai` (default; any OpenAI-compatible endpoint incl. vLLM/Ollama/OpenRouter) ·
`anthropic` · `gemini`. `base_url`, `model`, `api_key_env`. Optional `verify` (a separate model for the
adversarial verify pass) and `embeddings` (for hybrid recall).

### `forge` — the Git host for curation
`kb_repo` (`owner/name`), `base_branch` (default `main`), `github_api_url` (default
`https://api.github.com`), `dup_score` (default **5.0**), `min_confidence` (default **0.75**, the
quality bar below which a finding is chat-only). `github_app` — `app_id`, `installation_id`, and
`private_key_ref` **or** `private_key_env`.

### `notify` — where findings go
`slack` (`webhook_url_env` or `bot_token_env`, `channel`, `signing_secret_env`, `approver_ids`),
`matrix` (`homeserver`, `room_id`, `access_token_env`), plus inline blocks for any registered notifier
(e.g. `webhook` with `url_env`).

### `server` — the HTTP listener
Only `webhook_token_env` (the bearer token for the incident webhook; **required under
`actions.mode=auto`**). The listen address is the `--addr` CLI flag (`:8080` in the chart), **not** a
config key. TLS is terminated externally (ClusterIP + NetworkPolicy).

### `rbac` — chart-only (not in the agent config)
Set under `values.rbac.*`, not `values.config`: `controllerLogNamespaces` (default `[flux-system]` —
where `pods/log` is granted, namespaced; the app-layer `config.investigation.pod_log_namespaces`
allowlist **auto-tracks this value** unless overridden, so RBAC scope and app guard never drift),
`allowActions` (gate for the patch Role), `actionNamespaces` (the patch allowlist — **must mirror
`config.actions.allow.namespaces`**). See [Security model](security-model.md).

### `mcp` — external MCP tool servers (opt-in)

RunLore can call tools advertised by external [Model Context Protocol](https://modelcontextprotocol.io)
servers over streamable-HTTP (JSON-RPC 2.0). MCP is **opt-in** — the default empty `servers` list
disables it entirely.

```yaml
mcp:
  servers:
    - name: mydb           # short identifier; namespaces all tools as mydb__<tool>
      url: https://mcp.example.com/mcp
      token_env: MYDB_MCP_TOKEN   # optional — env var holding a bearer token
      headers:                    # optional extra request headers
        X-Tenant: my-org
```

**Key behaviours:**

- **Namespaced names.** Every remote tool is registered as `<server>__<tool>` (e.g. `mydb__query`),
  so MCP tools never collide with RunLore's built-in tools. Built-in names always win on collision.
- **Read-only.** The MCP adapter only calls `tools/call`; it never mutates RunLore state.
- **Failure-isolated.** A server that fails the `initialize` handshake or `tools/list` is logged at
  Warn and skipped — RunLore starts the investigation loop with the remaining tools rather than
  aborting. Fix the server and restart to pick it up.
- **Secrets by indirection.** `token_env` names the environment variable — never embed the token
  value directly in config. Wire it from a Kubernetes `Secret` (`env`/`envFrom`).
- **`headers` are not secret-safe over plain HTTP.** Only `token_env` is checked at config
  validation time; custom `headers` values are not. Do not carry secrets in `headers` when `url`
  is plain `http://` to a public host — use `headers` for non-secret metadata only (e.g. `X-Tenant`).
  Use `https://` whenever the server is on a public network.

### Other top-level keys
`gitops.engine` (`flux` default · `argocd`), `cloud` (`provider: aws`, `region`, `cluster_name`),
`network` (pluggable: `hubble` · `aws-vpc-flow-logs` · `gcp-firewall-logs`), `metrics`/`logs`
(`Endpoint{url}` for the PromQL/logs query tools), `telemetry` (`metrics_enabled`, `otlp_endpoint`),
`logging` (`format: text|json`, `level`), `leader_election` (`enabled`, `name`).
