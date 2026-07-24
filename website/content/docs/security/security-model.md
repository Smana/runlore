---
title: Security Model
weight: 10
---

What RunLore is allowed to do, how that's enforced, and the honest limitations. This is the **runtime**
security model — how the agent behaves in your cluster. For the LLM-specific trust story — prompt
injection, redaction boundaries, untrusted-output handling, network guards — see the
[LLM security architecture](security-architecture.md). For reporting a vulnerability, see
[`SECURITY.md`](../SECURITY.md). For the deeper design rationale, see [Design §9](design.md).

The guiding principle: **safety is enforced in code, not promised in prose.** The agent's own claims
(and the LLM's output) are never trusted for an authorization decision.

## Read-only by default

RunLore reads your cluster, metrics, logs, and network — its only writes are **markdown to Git via
reviewed PRs**. Two independent layers keep it that way:

1. **RBAC grants no write verbs by default.** The ServiceAccount gets `get/list/watch` cluster-wide
   and nothing else (see RBAC below).
2. **The action policy defaults to `off`** (`actions.mode`). No cluster-mutating tool is wired to the
   LLM; the model can only *propose* an action, never dispatch one.

The Curator is cluster-read-only — its "writes" are PRs and issues against your knowledge-base repo,
never the cluster.

## The action gate (climbing the autonomy ladder)

When you enable `actions` (the `suggest → approve → auto` ladder), every executable action passes a
**server-authoritative gate** (`internal/action`) that **re-derives** its safety from a canonical op
registry and **discards the model's own metadata**:

- Reversibility and blast radius come from the registry (only `suspend` / `resume` / `reconcile` are
  executable, all reversible, blast-radius 1) — an unknown op is treated as irreversible and refused.
- The policy envelope enforces `reversible_only`, `max_blast_radius`, an allowed-`kinds` list, and a
  namespace allowlist. **`flux-system` and `kube-system` are always denied** as targets, regardless of
  config; an empty `allow.namespaces` permits nothing.
- The gate is re-validated at **every** execution boundary (approval handling *and* auto), so a stale
  or tampered decision can't slip through (defense in depth).
- The target **name** is corroborated against the resources the investigation actually observed
  server-side (the triggering workload + everything the GitOps read tools returned). An unobserved
  target **never auto-executes** (the action is downgraded to a suggestion); under `approve` it stays
  in the queue but carries an explicit possible-injection warning for the human approver.
- `auto` mode starts **paused** (kill-switch engaged, fail-closed) and is gated behind
  confidence/rate/blast limits. It exists but is **not recommended on real clusters**.

The action config is **fail-closed**: `approve`/`auto` won't start without an approval token **and an
audit-log path** (both modes execute cluster mutations, so both must be audited), and `auto`
additionally requires an authenticated webhook, a positive confidence
threshold and rate cap, and a non-empty namespace allowlist (see
[Configuration → actions](configuration.md#actions--the-autonomy-ladder-off-by-default)).

## External MCP tools

Remote MCP tools run outside RunLore's action gate: the gate stops RunLore from *executing* cluster
operations, but a remote tool that mutates state server-side would do so the moment it is called.
Treat every configured MCP server as part of your TCB. Two controls bound this: per-server
`mcp.servers[].tools` allowlists (a tool not listed is never registered, so the model can never call
it), and `mcp.require_allowlist: true` to refuse startup unless every server is allowlisted. Tool
*output* remains untrusted data (redaction + no-instruction-following), and per-server discovery
failures are isolated.

## Secret redaction at the LLM and egress boundaries

Tool output and incident text flow to a model provider and, for findings, into your KB PR and chat.
`internal/redact` masks secret-shaped values at **three** boundaries:

1. **Ingress** — incident text before it enters the prompt.
2. **Tool output** — every tool result (pod/controller logs, git diffs, status/event messages) before
   it reaches the model provider.
3. **Delivery** — the finished investigation (root-cause summaries, evidence, suggested actions)
   before it's copied into the KB PR body and chat.

Coverage (high-precision, masks the value while keeping structure): PEM private keys, JWTs,
GitHub / Slack / AWS / Google / Stripe keys, `user:pass@host` URLs, `Authorization` headers,
generic `*(password|secret|api_key|token|…): <value>` pairs, and the values under a `kind: Secret`
manifest's `data:`/`stringData:` block — including one surfaced inside a git diff. A masked
Secret value is also **learned**: its base64 blob is decoded and both forms are scrubbed from the
**whole payload**, so the same secret quoted decoded in a log line or encoded in an event does not
outlive the manifest that names it.

> [!WARNING]
> **Redaction is a mitigation, not a guarantee**
>
> The ruleset is deliberately high-precision, and the cost of precision is recall: unlabeled
> high-entropy strings, bare AWS secret keys with no context cue, and base64 blobs whose `kind:
> Secret` manifest is **not in the same payload** (decoding happens only with the manifest as
> ground truth — a lone blob is indistinguishable from a SHA or log blob) are **not** caught — see
> [LLM security architecture §2](security-architecture.md#2-secret-redaction-three-boundaries-one-chokepoint)
> for the full list. If you run a **public KB repo** or **untrusted-tenant namespaces**, treat this
> as a gating concern — and prefer **self-hosting the model** (in-cluster vLLM/Ollama), which keeps
> data in-boundary regardless.

## Least-privilege RBAC

The chart's RBAC is scoped tightly (`deploy/helm/runlore/templates/rbac.yaml`):

- **ClusterRole (read-only, cluster-wide):** `get/list/watch` on Flux/ArgoCD resources and `events`,
  `get/list` on `pods` (status only — **not** `pods/log`). No write verb. `patch` is *intentionally*
  never granted cluster-wide.
- **Namespaced Role for pod/controller logs:** `pods/log` (raw log bodies, which can carry secrets/PII) is
  granted only over `rbac.controllerLogNamespaces` (default `flux-system`) — never cluster-wide.
- **Defense-in-depth app-layer guard:** because pod logs are streamed to the external LLM, the `pod_logs`
  tool is *also* constrained in the agent config to **{the incident's own namespace} ∪
  `config.investigation.pod_log_namespaces`** — a request for any other namespace is rejected before the
  cluster is queried, not just denied by RBAC. The chart **auto-defaults `pod_log_namespaces` to
  `rbac.controllerLogNamespaces`**, so the app-layer allowlist tracks the RBAC scope by default (no
  silent drift); leaving both at the defaults limits raw-log reads to the incident namespace plus
  `flux-system`. The app guard must stay a superset of the RBAC namespaces, or `pod_logs` is blocked at
  the app layer for namespaces RBAC would otherwise permit.
- **Namespaced Role for actions:** only when `rbac.allowActions` is set, `get/patch` on
  `kustomizations`/`helmreleases` over `rbac.actionNamespaces` — a bounded, opt-in blast radius that
  must mirror `config.actions.allow.namespaces`.

## Credentials & the GitHub App

- **Short-lived tokens, no PAT.** The GitHub App mints an RS256 **JWT (~9 min)**, exchanges it for a
  **~1-hour installation token**, and refreshes ~1 minute before expiry. There is no long-lived
  personal access token; revocation is central (uninstall the App).
- **Scope the App to the KB repo** (Contents/PRs/Issues read-write), plus optional read-only on your
  GitOps source repos for the what-changed diff. Disable the App's webhook. See
  [Getting started → GitHub App](getting-started.md).
- **Secrets by indirection.** Every credential is referenced by the *name* of an env var / Secret key,
  never inlined in config — so config can't leak a secret (see [Configuration](configuration.md)).
- **Webhook auth.** The incident webhook accepts a bearer token (`server.webhook_token_env`). It is
  **mandatory once any model is configured** (the `serve` path fails closed — an unauthenticated
  webhook must not reach the LLM and bill the model) and also enforced by `config.Validate` under
  `actions.mode=auto`. It is warning-only for the model-less log-only investigator. Pair it with a
  restrictive NetworkPolicy.
- **Failed-auth backoff.** Failed authentications on the control endpoints and the alert webhook are
  rate-limited per remote host: after 10 consecutive failures the host is blocked for 1s, doubling up
  to a 60s cap, and the block is checked before the token compare. A correct token always clears the
  counter. Behind a shared NAT this can delay a legitimate caller for at most one block window during
  a live attack. Tokens should be ≥128-bit random values (e.g. `openssl rand -hex 16`); the backoff
  is a brake on weak tokens, not a substitute for a strong one.

## Tamper-evident audit log

Every action attempt — inputs, gate result, op, target, actor, outcome — is appended to a
**hash-chained** JSON log (`internal/audit`): each record carries the previous record's hash, the file
is `0600` and **fsync'd after every write**, and a `Verify` pass detects the first broken link. Outcomes
recorded: `executed` / `dry-run` / `skipped` / `denied` / `failed`.

The chain is **load-bearing**, not just an artifact tests check:

- **Verified on startup, fail-closed under `approve`/`auto`.** Both executing modes are **required** to
  set `actions.audit_log_path` (enforced by config validation), so the guarantee always has a chain to
  verify — neither can silently downgrade to an unaudited run. When the agent opens the log it re-walks
  the existing chain in a single read pass and reuses that same handle for appends (no verify→append
  re-read window). If a link is broken and `actions.mode` is `approve` or `auto`, **startup fails** —
  RunLore refuses to execute and audit cluster mutations against a history it can no longer vouch for.
  Under `off`/`suggest` (nothing executes) it logs a loud **warning** and keeps appending, so a
  read-only deployment isn't blocked by a damaged file. An empty or absent log is a valid (zero-record)
  chain.
- **Verifiable on demand.** `lore audit verify --path <audit.jsonl>` (or `--config <runlore.yaml>` to
  read `actions.audit_log_path`) re-walks the chain out-of-band: it prints `OK: chain intact (<N>
  records)` and exits `0`, or prints the first broken link and exits non-zero. Run it from CI, a cron, or
  an incident review.

Verification catches **insertion**, **edit** (any byte of a recorded field), and **mid-chain deletion**
— each breaks a `prev_hash`/`hash` link.

**Honest residual limit — tail-truncation.** Dropping the *most-recent* records leaves a shorter but
internally consistent prefix, which still verifies. Chain verification alone therefore cannot detect
that the tail was lopped off. Fully closing this needs an **external anchor** (e.g. periodically
publishing the head hash + record count to an append-only store the writer can't rewrite), which is out
of scope: a sidecar high-water mark doesn't help — a privileged writer that can truncate the log can
truncate the sidecar too, and making it crash-consistent is fiddly. Until an external anchor exists,
mitigate operationally: keep the log on **durable storage with restricted write access** (ideally a
medium where the agent's own identity cannot rewrite history), and back it up.

## The feedback channels (👍/👎) — exposure & trust model

Human feedback ratings weigh recalled-knowledge trust and re-arm the recurrence cooldown, so the
channels that carry them are part of the security surface. The two channels have opposite exposure
profiles and one shared trust model.

**Shared trust model — votes are workspace/room-scoped opinions.** Feedback is deliberately
*unprivileged*: any authenticated member of your Slack workspace / Matrix room can rate (it is an
opinion feeding the learning loop, not a cluster mutation — approve/reject keep their allowlist).
The blast radius of a hostile voter is bounded by construction: **one live vote per (incident
trigger, user), latest wins** (no stacking), a vote is one Bernoulli observation in the same Beta
posterior as resolve signals (several independent voters are needed to move an established entry),
recalled answers still pass the adversarial verify pass, recall confidence is hard-capped at 0.90,
and the worst a 👎 campaign achieves is *extra fresh investigations* (cost, not wrong answers —
decay fails toward re-investigation, never toward trusting). Every vote is an append-only ledger
line carrying the voter's stable id, so a campaign is auditable after the fact.

**Slack (`notify.slack.feedback_buttons`) — an exposed endpoint, hardened.** Clicks arrive on
`POST /slack/interactions`, which must be reachable from Slack's servers. Every request is verified
against the app **signing secret** (HMAC-SHA256 over the raw body, ±5-minute timestamp window
against replay, constant-time compare) *before* any parsing-derived action; unsigned or stale
requests are rejected and the body read is capped at 1 MiB. Replay within the window is idempotent
by the vote dedup. The message-update callback (`response_url`) is restricted to
`https://*.slack.com` with a bounded client (no SSRF). **Expose only the path, not the pod**: route
*only* `/slack/interactions` through your ingress/gateway — the same listener also serves the
alert webhook (open when `server.webhook_token_env` is unset!), `/metrics`, and the token-gated
control endpoints, none of which belong on the internet. If any part of the server is reachable
from outside, set `server.webhook_token_env` regardless of action mode.

**Matrix (`notify.matrix.feedback_reactions`) — nothing exposed, one explicit check.** Reactions
arrive over the client-server `/sync` **long-poll — an outbound HTTPS request** authenticated by
the notifier's existing access token. No inbound endpoint, no signing secret, no NetworkPolicy
change; responses are size-capped before decoding. The one attack Matrix enables that Slack cannot
is **attribution forgery**: any room member could post their own message carrying the
`io.runlore.trigger_key` content field and vote on it, misdirecting ratings to an arbitrary
incident. The listener closes this by resolving its own identity (`/whoami`) at startup and
counting a vote **only when the reacted-to event was sent by the bot itself** — and it refuses to
listen at all until that identity is known. Operational requirement: because vote identity is room
membership, use an **invite-only room** (and prefer disabling federation for it); in a federated
room, remote homeservers assert their own users' identities.

## Honest limitations

- **The model sees cluster data.** Even with redaction, tool output reaches your model provider. The
  strongest mitigation is self-hosting the model in-cluster. The redaction recall gaps above are real.
- **RCA can be wrong.** Frontier RCA is sub-50% on real incidents; `unresolved` is a first-class
  output and an adversarial verify pass can only *lower* confidence. Treat findings as hypotheses, and
  the human PR review as the load-bearing quality gate.
- **Prompt injection is bounded, not impossible.** A poisoned alert or KB entry can bias an RCA, but it
  **cannot** trigger a write — the action gate ignores model-authored authorization fields, and recall
  is disabled under auto-execution so a poisoned catalog entry can't short-circuit into an action.
