# LLM security architecture

RunLore feeds **attacker-influenceable text** — alert annotations, pod logs, Kubernetes events, git
diffs, MCP tool output — to an LLM, and then delivers the model's reply to chat, a git repo, and (on
the upper autonomy rungs) a cluster executor. That is the textbook prompt-injection setup: anything
in that text can *tell the model what to do*, and no model reliably refuses.

This page explains how RunLore is built so that a successful injection **degrades an answer, not a
cluster**. It is the companion to the [Security model](security-model.md) (permissions, RBAC, audit)
and [Design §9](design.md#9-safety--trust-model) (rationale): this page is the LLM-specific trust
story, sourced from the code paths it cites.

The design stance, in one line: **the model is an untrusted text generator inside a trusted Go
program.** Every security-relevant decision — what executes, what leaves the cluster, what renders in
chat — is made by server code that treats model output the same way it treats alert text: as data.

---

## 1. The core invariant: the model proposes, the server decides

The investigation loop gives the model read-only tools and exactly one structured exit:
`submit_findings`. A finding may include *proposed* actions, but a proposal is inert text until the
server-side action gate (`internal/action`) lets it through.

### A closed registry of executable operations

The only operations an executor can ever run are the ones in the canonical registry
(`internal/providers/providers.go`):

```go
var Ops = map[string]OpSafety{
    "suspend":   {Reversible: true, Blast: 1},
    "resume":    {Reversible: true, Blast: 1},
    "reconcile": {Reversible: true, Blast: 1},
}
```

Three Flux operations, all reversible, all blast-radius 1. There is no `delete`, no `apply`, no
`exec`, and no cluster-mutating MCP tool. A model that "invents" an op proposes something the
registry doesn't contain — the gate refuses it as `unknown op`.

### Server-derived safety metadata

The model's `submit_findings` schema *does* let it claim `reversible: true` or a small
`blast_radius` — and the gate throws those claims away. `deriveSafety`
(`internal/action/safety.go`) overwrites reversibility and blast radius from the registry for every
executable action; an unknown op is marked not-reversible. Model-authored safety fields never reach
an authorization decision.

### The autonomy ladder fails closed

`actions.mode` is `off | suggest | approve | auto` (default `off`). The fail-closed properties are
structural, not configuration hygiene:

- **`off` (default):** proposals aren't even surfaced.
- **`suggest`:** proposals are shown, never executed.
- **`approve`:** execution requires an authenticated human approval (see [§5](#5-server-authentication)).
- **`auto`:** unattended execution of reversible, in-envelope actions — with the sharp edges filed
  down in code (`internal/action/auto.go`):
  - the **kill-switch starts engaged**: `NewAuto` constructs with `paused: true`, so a process or
    leader restart can never resume unattended execution on its own — an operator must explicitly
    `Resume()` through an authenticated endpoint;
  - an **empty namespace allowlist permits nothing** (`internal/action/policy.go`:
    `namespaceViolation`) — there is no "allow all" default;
  - **`flux-system` and `kube-system` are hard-denied** as targets regardless of config
    (`builtinProtectedNamespaces` in `internal/action/safety.go`); operators can extend the deny
    list, never shrink it;
  - config validation (`internal/config/config.go` `Validate`) refuses to start `auto` without an
    approval token, an audit-log path, an authenticated webhook, a positive confidence threshold,
    and a positive rate cap.

### Unobserved action targets never auto-execute

The gate validates *op*, *kind*, and *namespace* — but the target **name** is model-authored, so a
prompt-injected finding could steer an otherwise in-envelope action onto an arbitrary named
resource. Each investigation therefore tracks the resources it confirmed **server-side**
(`internal/investigate/observedresources.go`): the request's own workload (the alert/failure
subject always counts), plus everything `what_changed`, `gitops_resource_status`, and `gitops_tree`
actually read. An executable action whose target was never observed is handled per rung
(`guardUnobservedTargets`):

- **`auto`:** the executable op is **stripped** — the proposal is still delivered, but only as a
  suggestion. A hallucinated or injected target cannot reach an unattended cluster write.
- **`approve`/`suggest`:** the action stays executable but its description carries an explicit
  *"target was never observed server-side"* warning into the approval queue and chat message. The
  human approval is the gate; the observed set is a heuristic with false negatives (a legitimate
  target the investigation never re-read), so under a human gate its verdict is advisory — it arms
  the approver rather than silently starving the queue.

### Recall is disabled under auto

The knowledge catalog is community-shaped markdown — **untrusted input**. Under `off`/`suggest`/
`approve`, a catalog hit can short-circuit an investigation (instant recall), and even then the
recalled finding goes through the adversarial verify pass ("catalog content is untrusted"). Under
`auto`, instant recall is disabled outright (`internal/investigate/loop.go`):

> *Instant recall is disabled under auto-execution: a poisoned catalog entry must not short-circuit
> a real investigation straight into an auto-executed action.*

A crafted KB entry can therefore bias a *suggestion*; it cannot become an unattended cluster write.

### Re-validation at the execution boundary (TOCTOU)

The envelope is checked when actions are surfaced (`Policy.Review`) **and re-checked at every
execution boundary**, so a decision made earlier can't go stale between check and use:

- `Approvals.Approve` (`internal/action/approvals.go`) **claims** the pending entry under the same
  lock that reads it — two concurrent approvals of one id cannot both execute — then re-runs
  `deriveSafety` + `policy.violation` before touching the executor.
- `Auto.runOne` (`internal/action/auto.go`) re-checks the kill-switch immediately before executing
  (a `Pause()` landing mid-gate wins), re-derives safety, re-validates the full envelope, and only
  then reserves a rate-limit slot — so a denial can't burn budget for an action that never ran.

---

## 2. Secret redaction: three boundaries, one chokepoint

Tool output flows to a model provider and, quoted as evidence, on into a (possibly public) KB pull
request and chat. `internal/redact` masks secret-shaped values at three boundaries, all in
`internal/investigate/loop.go`:

1. **Ingress** — the incident text (alert annotations/messages) is redacted before it enters the
   seed prompt, so a secret pasted into an alert never reaches the model provider.
2. **The tool-output chokepoint** — every tool result passes through
   `truncateOutput(redact.Secrets(...))` before joining the message history. Because *all* tools —
   built-in and **MCP** — share this one call path, an operator-added MCP tool gets the same
   protection with zero extra wiring. Redaction runs **before** truncation, so a secret that
   straddles the size cap is masked, not sliced into an unrecognizable (and unredacted) fragment.
   And because the model only ever *sees* redacted text, the evidence it later quotes into findings
   inherits the protection.
3. **The egress catch-all** — `redactInvestigation` re-scrubs the finished investigation (title,
   root-cause summaries, evidence, unresolved notes, action names and descriptions) in `deliver`,
   just before chat/KB delivery. This is the only boundary that also covers **non-model text**
   appended after the loop — e.g. the recall-confirmation step's pod-status lines, or the raw
   incident title.

What the ruleset catches (`internal/redact/redact.go`): PEM private-key blocks, JWTs, GitHub /
Slack / Stripe / Google / OpenAI-style / AWS key formats, `user:password@host` URLs, `Bearer`/
`Basic` authorization values, generic sensitive `key: value` pairs (`password`, `secret`,
`api_key`, `token`, `dsn`, `connection_string`, …), and — line-oriented, diff-marker-aware — the
**values under `data:`/`stringData:` of a `kind: Secret` manifest**, including one surfaced inside
a `what_changed` git diff.

> [!WARNING]
> **Redaction is a mitigation, not a guarantee**
>
> The ruleset is deliberately **high-precision**: it masks values while preserving structure (the
> key name, the diff line) so the investigation can still reason — "the password field changed" —
> without the secret. The cost of precision is recall. Things the regexes deliberately do **not**
> catch:
>
> - a **bare 40-character AWS secret key** with no adjacent context cue (`aws_secret_access_key`,
>   a `key:`/`key=` form) — a bare `[A-Za-z0-9/+]{40}` rule would false-positive on every git SHA
>   and base64 log blob;
> - **unlabeled high-entropy strings** — a password sitting in free prose with no recognizable
>   prefix and no key label;
> - **base64 blobs outside a `kind: Secret` `data:` block** — the redactor masks Secret-manifest
>   values wholesale but never *decodes* base64 to inspect contents, so a secret copied base64-encoded
>   into a log line or ConfigMap is not detected;
> - secrets under a key name outside the sensitive-keyword vocabulary.
>
> If you run a public KB repo or untrusted-tenant namespaces, the strongest mitigation is
> **self-hosting the model** (in-cluster vLLM/Ollama), which keeps data in-boundary regardless of
> redaction recall.

**The `Action.Name` lesson.** The egress pass originally scrubbed `Action.Description` but not
`Action.Name` — which `buildInvestigation` fills with the same model-authored description, and which
`GET /actions` serializes verbatim (fixed in
[#197](https://github.com/Smana/runlore/pull/197)). The rule that came out of it: when a struct
crosses the egress boundary, **enumerate the serialized shape** — redact every field that appears
on the wire, not the fields you remember being free text.

---

## 3. Untrusted output stays inert in every renderer

Model output quotes cluster logs and alert text, so everything RunLore renders is transitively
attacker-influenceable. Each output surface neutralizes it for *its own* interpreter:

- **Slack mrkdwn** (`internal/notify/slack.go`, [#196](https://github.com/Smana/runlore/pull/196)):
  `escapeMrkdwn` replaces Slack's three mrkdwn control characters (`&`, `<`, `>`) with HTML
  entities before untrusted text is interpolated — so a hostile log line like
  `<https://evil.example|click here>` renders as literal text instead of a phishing link. The
  plain-text fallback (parsed as mrkdwn by notifications and block-less clients) is escaped too;
  the header block is `plain_text`, which Slack never parses.
- **Matrix HTML** (`internal/notify/matrix.go`): escape-first — `html.EscapeString` runs *before*
  the markup transforms, which only ever emit a fixed tag set (`<strong>`, `<code>`, `<a>`), so
  user content can never inject live markup.
- **Model-client error surfacing** (`internal/model/{anthropic,openai,gemini}`,
  [#193](https://github.com/Smana/runlore/pull/193) /
  [#195](https://github.com/Smana/runlore/pull/195)): a non-2xx body from a model endpoint —
  which, for OpenAI-compatible backends, is an *operator-configurable* upstream — is read through
  a **4 KiB `LimitReader`**, and only the **structured** `error.type` / `error.message` fields are
  surfaced (never the raw bytes), with **control characters collapsed to spaces** and the message
  truncated at 300 chars. A hostile or broken upstream can't inject log lines, terminal escapes,
  or megabytes of noise through an error path.
- **Log-injection defense for headers** (`internal/httpx/redact.go`): upstream request-id headers
  are logged for correlation, but only after `SanitizeHeader` drops everything below `0x20` plus
  DEL (CR/LF/NUL/ANSI ESC) and caps the value at 200 chars — an upstream can't forge log structure
  through a header.

---

## 4. Network boundaries

- **`httpx.SecureClient` redirect guard** (`internal/httpx/client.go`): every outbound client to an
  operator- or externally-configurable endpoint (model, forge, notifiers, MCP, metrics/logs,
  embeddings — including the SSE variant `SecureStreamingClient`) installs `DenyInternalRedirect`,
  which:
  - refuses a redirect from a **public-origin** chain to a loopback / private / link-local /
    unspecified address — closing the redirect → `169.254.169.254` cloud-metadata SSRF path — and
    **fails closed** when the target doesn't resolve;
  - **strips credential headers** (`Authorization`, `X-Api-Key`, `X-Goog-Api-Key`) whenever a
    redirect changes host, so a provider key is never replayed to a different host (Go strips
    `Authorization` itself, but not custom headers);
  - caps the chain at 10 redirects. Chains that *originate* at a private address (an in-cluster
    backend behind a proxy) are allowed, so the guard never breaks in-cluster traffic.
- **Slack `response_url` host pinning** (`internal/server/server.go` `updateSlack`): the
  interaction payload's `response_url` is attacker-influenceable, so it is only followed when it is
  `https` on `slack.com`/`*.slack.com`, posted with a bounded `SecureClient` — no SSRF into
  internal services via a forged interaction.
- **Cleartext-key startup rejection** (`internal/config/config.go` `checkSecureKeyEndpoint`):
  config validation refuses to start when an API key would be sent over plain `http` to a
  **public** host — covering the model endpoint, a verify override (including one that *inherits*
  an insecure parent `base_url`), embeddings, and every MCP server. Loopback and in-cluster hosts
  are exempt; the check is pure (no DNS) so validation stays deterministic.

> [!NOTE]
> **Explicitly out of scope: dial-time DNS rebinding**
>
> The redirect guard resolves the redirect target when deciding; it does not re-check at dial time,
> and no client pins resolved IPs. Endpoints you put in the config are part of the **operator trust
> boundary** — RunLore defends against a *legitimate* endpoint redirecting somewhere hostile, not
> against an operator-configured hostname whose DNS is adversarial.

---

## 5. Server authentication

Every endpoint that can influence execution authenticates, and every check compares in constant
time (`internal/server/server.go`):

- **Control endpoints** (`GET /actions`, approve/reject, pause/resume): require `X-Approval-Token`,
  compared with `subtle.ConstantTimeCompare`. The check **fails closed** — an empty configured
  token denies everything, and config validation refuses to start `approve`/`auto` without one, so
  a running executing-rung server always has a token.
- **The alert webhook**: optional bearer token (constant-time), **mandatory once any model is
  configured** (the `serve` path fails closed — an unauthenticated webhook must not reach the LLM
  and bill the model) and enforced by `config.Validate` under `actions.mode=auto`; warning-only for
  the model-less log-only investigator.
- **Slack interactions**: the request signature is verified (HMAC-SHA256 over `v0:{ts}:{body}`)
  with a ±5-minute timestamp window against replay — and then the **user** is authorized
  separately: only Slack IDs in the approver allowlist may approve *or* reject (a signature-valid
  but unlisted user must not cancel a pending remediation either). An empty allowlist permits no
  one. Approval ids are `crypto/rand` (unguessable), as defense-in-depth behind the auth gates.

Every action attempt — approved, auto, denied, dry-run — lands in the hash-chained, fail-closed
audit log; see [Security model → Tamper-evident audit log](security-model.md#tamper-evident-audit-log).

---

## 6. Threat model at a glance

| Attacker-controlled input | Reaches the model? | What stops it from doing damage |
| --- | --- | --- |
| **Alert text** (webhook payload) | Yes, redacted | Bearer-token webhook auth (mandatory once a model is configured; also required under `actions.mode=auto`); ingress redaction; injected "instructions" can only shape a *proposal*, which the action gate re-derives and re-validates |
| **Cluster logs / events / status** (pod logs, controller logs, kube events) | Yes, redacted | RBAC + app-layer namespace allowlist bound what's readable ([Security model → RBAC](security-model.md#least-privilege-rbac)); redact-before-truncate at the tool-output chokepoint; mrkdwn/HTML escaping keeps quoted lines inert in chat |
| **Git diffs** (`what_changed`) | Yes, redacted | Same chokepoint; `kind: Secret` `data:`/`stringData:` values masked even inside diff markers |
| **MCP tool output** (operator-added servers) | Yes, redacted | Same single chokepoint (no per-tool wiring to forget); no cluster-mutating MCP tools exist; MCP endpoints get the SecureClient redirect guard and the cleartext-key startup check |
| **KB catalog entries** (poisoned recall) | Yes | Recall disabled under `auto`; recalled findings go through the adversarial verify pass; unconfirmable recalls get their confidence capped; entries enter the catalog via human-reviewed PRs |
| **The model's own output** | — | The core invariant (§1): closed op registry, server-derived safety metadata, namespace allow/deny, approval or fail-closed auto gates, exec-boundary re-validation; egress redaction; escaped rendering |
| **Slack interaction payloads** | No | HMAC signature + replay window; per-user approver allowlist for approve *and* reject; `response_url` pinned to `https` `*.slack.com`; unguessable approval ids |
| **Upstream provider responses** (error bodies, headers) | No | Bounded 4 KiB read, structured-fields-only surfacing, control-character collapse, truncation; sanitized request-id headers; redirect guard + credential-header strip |

---

## 7. What this architecture does not claim

Honesty is part of the design (see [Security model → Honest limitations](security-model.md#honest-limitations)):

- **A prompt injection can still bias an answer.** The controls above bound *consequences* — no
  write without the gate, no secret past the redactor's coverage, no live markup in chat — but a
  poisoned log line can absolutely steer the model toward a wrong root cause. The human review of
  findings and KB PRs is the load-bearing quality gate.
- **Redaction is best-effort** (§2). The model provider sees redacted cluster data; if that is
  unacceptable, self-host the model.
- **Configured endpoints are trusted** (§4). The network guards defend against redirects and
  response content, not against a hostile operator-supplied hostname.
- **The audit chain can't detect tail-truncation** without an external anchor — see
  [Security model → Tamper-evident audit log](security-model.md#tamper-evident-audit-log).
