---
title: Remediation Factory Protocol
weight: 15
---

RunLore integrates with any external remediation broker or agent factory over a small,
vendor-neutral HTTP protocol: RunLore files each finding with `POST /v1/tasks`, listens for
HMAC-signed lifecycle webhooks on `POST /factory/webhook`, and optionally posts a post-soak
verification verdict to `POST /v1/tasks/{task}/verification`.

RunLore is an ordinary API client of the broker. The broker decides every gate — whether a finding
becomes a task, what changes it may propose, and whether a fix is published.

## Authentication modes

Every outbound request (`POST /v1/tasks` and `POST /v1/tasks/{task}/verification`) carries
`Authorization: Bearer <token>`. Credentials are always configured as environment variable **names**,
never literal values. Both `factory.url` and `factory.token_url` require `https://` on public hosts
(plain `http://` is permitted only on loopback or in-cluster hosts such as `*.svc.cluster.local`).

### Mode A — Static Bearer token

Use `token_env` for internal brokers or API gateways that authenticate with a static bearer token.
`token_env` is mutually exclusive with all OAuth2 fields (`token_url`, `client_id_env`,
`client_secret_env`, `project_id_env`, `oauth_scope`, `oauth_audience`). On `401`, RunLore returns
the status immediately without retrying a token refresh.

```yaml
factory:
  url: https://broker.example.com
  token_env: FACTORY_API_TOKEN
  repository: my-org/infra-gitops
  max_cost_usd: 2
  webhook:
    secret_env: FACTORY_WEBHOOK_KEY
```

### Mode B — Generic OAuth2 client credentials

Use `token_url`, `client_id_env`, and `client_secret_env` for Keycloak, Auth0, Okta, Dex, Entra ID,
or any RFC 6749 client-credentials token endpoint. `oauth_scope` and `oauth_audience` are optional
and omitted from the form body when empty. RunLore caches the returned `access_token`, refreshes it
60 seconds before `expires_in`, and performs a single refresh-and-retry on `401`.

```yaml
factory:
  url: https://broker.example.com
  token_url: https://idp.example.com/oauth/token
  client_id_env: FACTORY_CLIENT_ID
  client_secret_env: FACTORY_CLIENT_SECRET
  oauth_scope: tasks:write
  oauth_audience: https://broker.example.com
  repository: my-org/infra-gitops
  max_cost_usd: 2
  webhook:
    secret_env: FACTORY_WEBHOOK_KEY
```

### Mode C — ZITADEL shorthand

Setting `project_id_env` (mutually exclusive with `oauth_scope`) sends the ZITADEL project audience
scope `openid urn:zitadel:iam:org:project:id:<project_id>:aud` on every token exchange:

```yaml
factory:
  url: http://agent-factory.agent-system.svc.cluster.local:8082
  token_url: https://auth.example.com/oauth/v2/token
  client_id_env: FACTORY_CLIENT_ID
  client_secret_env: FACTORY_CLIENT_SECRET
  project_id_env: FACTORY_PROJECT_ID
  repository: my-org/infra-gitops
  max_cost_usd: 2
  webhook:
    secret_env: FACTORY_WEBHOOK_KEY
```

## Filing a task — `POST /v1/tasks`

After an investigation finishes, RunLore sends a redacted finding to `POST {factory.url}/v1/tasks`
with headers:

- `Authorization: Bearer <token>`
- `Content-Type: application/json`
- `Idempotency-Key: <key>` — the incident's `TriggerKey`, or its 64-character lowercase SHA-256 hex
  digest when the `TriggerKey` exceeds 255 bytes.

### Request body

```json
{
  "repository": "my-org/infra-gitops",
  "text": "<secret-redacted investigation summary>",
  "dataClass": "internal",
  "maxCostUSD": 2,
  "incident": {
    "alert": "KubePodCrashLooping",
    "resource": "apps/web",
    "severity": "critical",
    "cluster": "prod-eu",
    "verdict": "action_required",
    "confidence": 0.9
  }
}
```

### Response status codes

| Status | Meaning | Expected JSON fields | RunLore behavior |
|---|---|---|---|
| `201 Created` | New task accepted | `task`, `room`, `issue` | Recorded in `factory.jsonl` for the `TriggerKey` |
| `200 OK` | Duplicate of an existing task | `task`, `room`, `issue`, `duplicate: true` | Recorded/kept in `factory.jsonl`; no second task created |
| `202 Accepted` | Broker declined (not an error) | `reason` (e.g. `below_threshold`) | Logged at `info`; not retried |
| `429 Too Many Requests` | Rate or quota limit | `error` or `reason` | Logged at `info`; not retried |
| `503 Service Unavailable` | Transient broker outage | `error` | Retried twice more (after `1s`, then `3s`) with the same `Idempotency-Key` |
| `401 Unauthorized` | Rejected token | `error` | OAuth2 modes drop cached token and retry once; static bearer returns immediately |

## Inbound webhooks — `POST /factory/webhook`

RunLore mounts `POST /factory/webhook` on its serve port when `factory.url` is set (follower
replicas relay requests unchanged to the leader).

### Signature verification

Requests must follow the [Standard Webhooks](https://www.standardwebhooks.com/) HMAC-SHA256
specification:

- Required headers: `webhook-id` (max 256 bytes), `webhook-timestamp` (Unix seconds, within ±5
  minutes of RunLore's clock), `webhook-signature` (space-delimited `v1,<base64>` entries, up to 8
  checked).
- Signed payload: `<webhook-id>.<webhook-timestamp>.<raw-request-body>`.
- `factory.webhook.secret_env` holds one or two base64-encoded keys (at least 32 decoded bytes each,
  one per line) and is re-read per request for zero-downtime key rotation.
- Invalid or missing signatures fail closed with `401` and no detail; verified events return `204`
  (whether or not the room is known), and duplicate `webhook-id` values within 10 minutes are
  dropped idempotently.

### Event payload

Events carry IDs and short identifier codes only (never free-form room text):

```json
{
  "type": "room.merged",
  "room": "fix-room-id",
  "origin": "task-room-id",
  "repo": "my-org/infra-gitops",
  "pr": 42,
  "state": "completed",
  "reason": "approval_required",
  "verdict": "healthy"
}
```

Recognized event types:

- `room.task_received` — task or published fix room started (`origin` binds a fix room to its
  originating task room).
- `room.pr_opened` — pull request opened (`pr`).
- `room.waiting_for_human` — fix paused for human action (`reason`).
- `room.merged` — fix PR merged (`pr`); starts the post-soak verification timer when enabled.
- `room.reverted` — fix PR reverted (`pr`).
- `room.monitor_verdict` — broker's own post-merge monitor verdict (`verdict`: `healthy`,
  `regression`, or `inconclusive`).
- `room.ended` — room finished (`state`).

## Post-soak verification — `POST /v1/tasks/{task}/verification`

When `factory.verify.alertmanager_url` is configured, RunLore waits `factory.soak` (default `30m`,
minimum `5m`) after `room.merged`, checks Alertmanager for the firing alert, runs a short
re-investigation (`factory.verify.max_steps`, default `6`) for redacted evidence, and posts:

```json
{
  "verdict": "resolved",
  "alert": {
    "name": "KubePodCrashLooping",
    "firingAtMerge": true,
    "firingNow": false
  },
  "checkedAt": "2026-11-24T10:00:00Z",
  "evidence": "pods stable for 30m"
}
```

- `verdict` is `"resolved"` when Alertmanager reports the alert no longer firing, or `"persisting"`
  otherwise.
- `201` / `200` record the verification; `503` retries twice (`1s`, `3s`); terminal refusals (`400`,
  `403`, `404`, `409 window_closed`) are recorded and not retried.

## Deferred KB curation

With `factory.curation.defer: true`, RunLore holds an accepted incident's knowledge-base entry in
`factory.jsonl` until either:

1. Both RunLore's post-soak verdict (`resolved`) and the broker's `room.monitor_verdict` (`healthy`)
   agree (outcome `held`), or a revert / `persisting` / `regression` occurs (outcome `failed`), or
2. `factory.curation.window` (default `7d`, minimum `24h`) expires, at which point the entry is
   drafted as-is.

See [`factory` in Configuration]({{< relref "/docs/configuration/configuration.md#factory--the-agent-factory-opt-in" >}})
for every config key and default.
