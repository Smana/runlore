# Security model

What RunLore is allowed to do, how that's enforced, and the honest limitations. This is the **runtime**
security model — how the agent behaves in your cluster. For reporting a vulnerability, see
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
- `auto` mode starts **paused** (kill-switch engaged, fail-closed) and is gated behind
  confidence/rate/blast limits. It exists but is **not recommended on real clusters**.

The action config is **fail-closed**: `approve`/`auto` won't start without an approval token, and
`auto` additionally requires an audit-log path, an authenticated webhook, a positive confidence
threshold and rate cap, and a non-empty namespace allowlist (see
[Configuration → actions](configuration.md#actions--the-autonomy-ladder-off-by-default)).

## Secret redaction at the LLM and egress boundaries

Tool output and incident text flow to a model provider and, for findings, into your KB PR and chat.
`internal/redact` masks secret-shaped values at **three** boundaries:

1. **Ingress** — incident text before it enters the prompt.
2. **Tool output** — every tool result (pod/controller logs, git diffs, status/event messages) before
   it reaches the model provider.
3. **Delivery** — the finished investigation (root-cause summaries, evidence, suggested actions)
   before it's copied into the KB PR body and chat.

Coverage (high-precision, masks the value while keeping structure): PEM private keys, JWTs,
GitHub / Slack / AWS / Google / Stripe keys, `user:pass@host` URLs, `Authorization` headers, and
generic `*(password|secret|api_key|token|…): <value>` pairs.

> [!warning] Residual gap — base64 Secret data
> Redaction does **not** yet base64-decode `kind: Secret` `data:` blocks, so a Secret manifest
> surfaced verbatim in a git diff can still reach the model unmasked (roadmap). Redaction is a
> mitigation, not a guarantee. If you run a **public KB repo** or **untrusted-tenant namespaces**,
> treat this as a gating concern — and prefer **self-hosting the model** (in-cluster vLLM/Ollama),
> which keeps data in-boundary regardless.

## Least-privilege RBAC

The chart's RBAC is scoped tightly (`deploy/helm/runlore/templates/rbac.yaml`):

- **ClusterRole (read-only, cluster-wide):** `get/list/watch` on Flux/ArgoCD resources and `events`,
  `get/list` on `pods` (status only — **not** `pods/log`). No write verb. `patch` is *intentionally*
  never granted cluster-wide.
- **Namespaced Role for controller logs:** `pods/log` (raw log bodies, which can carry secrets/PII) is
  granted only over `rbac.controllerLogNamespaces` (default `flux-system`) — never cluster-wide.
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
- **Webhook auth.** The incident webhook accepts a bearer token (`server.webhook_token_env`); it is
  **mandatory** under `actions.mode=auto` and recommended for any internet-adjacent ingress. Pair it
  with a restrictive NetworkPolicy.

## Tamper-evident audit log

Every action attempt — inputs, gate result, op, target, actor, outcome — is appended to a
**hash-chained** JSON log (`internal/audit`): each record carries the previous record's hash, the file
is `0600` and **fsync'd after every write**, and a `Verify` pass detects the first broken link. Edits
or deletions are therefore detectable. Outcomes recorded: `executed` / `dry-run` / `skipped` /
`denied` / `failed`.

## Honest limitations

- **The model sees cluster data.** Even with redaction, tool output reaches your model provider. The
  strongest mitigation is self-hosting the model in-cluster. The base64-Secret gap above is real.
- **RCA can be wrong.** Frontier RCA is sub-50% on real incidents; `unresolved` is a first-class
  output and an adversarial verify pass can only *lower* confidence. Treat findings as hypotheses, and
  the human PR review as the load-bearing quality gate.
- **Prompt injection is bounded, not impossible.** A poisoned alert or KB entry can bias an RCA, but it
  **cannot** trigger a write — the action gate ignores model-authored authorization fields, and recall
  is disabled under auto-execution so a poisoned catalog entry can't short-circuit into an action.
