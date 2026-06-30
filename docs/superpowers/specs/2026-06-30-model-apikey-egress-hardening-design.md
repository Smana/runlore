# Model API-key egress hardening (S1 + S2)

Status: draft for review
Date: 2026-06-30
Scope: one PR, dedicated worktree. Security-only; no behaviour change for correctly-configured deployments.

## Problem

Two related ways a model API key can leak off the intended TLS channel:

- **S1 — cross-host redirect forwards the key.** Go's `net/http` strips `Authorization`,
  `Cookie`, `Www-Authenticate` on a redirect that changes host, but it does **not** strip
  arbitrary custom headers. The Anthropic client sends `x-api-key` (`anthropic.go:191`) and
  the Gemini client sends `x-goog-api-key` (`gemini.go:180`) — both custom headers, both
  replayed across a cross-host redirect. `httpx.DenyInternalRedirect` (`client.go:32-52`)
  only blocks redirects to *internal* targets, so a redirect to a **public** attacker host
  (from a compromised/MITM'd upstream, or a plain-`http` endpoint per S2) makes Go re-send
  the key to that host. The OpenAI `Authorization: Bearer` path is already safe because Go
  strips it.

- **S2 — cleartext key over a public endpoint.** `Config.Validate()` (`config.go:492-500`)
  validates only `max_tokens`. An operator who sets `base_url: http://...` with an API key
  configured sends that key in cleartext (`anthropic.go:185`, `openai.go:203`,
  `gemini.go:169` use `c.baseURL` verbatim). This is also the enabler for S1's redirect path.

Neither is attacker-triggerable via runtime input (`base_url` is operator config), so the
realistic trigger is misconfiguration or a compromised intermediary. Both fixes are cheap and
fail-closed.

## S1 — strip the key header on a host-changing redirect

**Decision (locked):** keep following legitimate redirects, but delete the provider key
headers whenever a redirect changes host. This preserves in-cluster redirects (http→https,
trailing-slash on a private vLLM/Ollama proxy) while guaranteeing the key never reaches a
different host.

**Where:** `internal/httpx/client.go`, inside `DenyInternalRedirect` (the shared
`CheckRedirect` used by both `SecureClient` and `SecureStreamingClient`, hence by all three
model clients and the forge/notifier/metrics/logs clients).

**Behaviour:** at the top of `DenyInternalRedirect`, before the existing internal-target
checks:

```
prev := via[len(via)-1]                      // the hop we are redirecting FROM
if !strings.EqualFold(req.URL.Hostname(), prev.URL.Hostname()) {
    for _, h := range sensitiveAuthHeaders {
        req.Header.Del(h)                    // http.Header canonicalises; Del is case-insensitive
    }
}
// ... existing maxRedirects + internal-origin + internal-target checks unchanged ...
```

- `sensitiveAuthHeaders = []string{"X-Api-Key", "X-Goog-Api-Key", "Authorization"}`
  (canonical form). `Authorization` is included for defence-in-depth even though Go already
  strips it — deleting an absent header is a no-op.
- **Host comparison is hostname-only (port ignored).** A same-host `http→https` upgrade
  (port 80→443, same hostname) keeps the key; only a genuine domain change strips it. This is
  the exfil-relevant boundary.
- `via` is always non-empty when `CheckRedirect` is called (there is always a prior hop), so
  `via[len(via)-1]` is safe; guard with `len(via) > 0` regardless.
- Applies uniformly to every `SecureClient`/`SecureStreamingClient` caller. No downside for
  non-model callers: those custom headers simply aren't present on their requests.

**Accepted edge:** a legitimate *cross-host* API redirect would lose its auth header and most
likely return 401, surfaced as the provider's existing non-200 status error. Real model APIs
do not 3xx a `POST /v1/messages`-style call across hosts, so this is rare and an explicit,
documented trade-off.

## S2 — reject a cleartext key on a public endpoint at config validation

**Decision (locked):** hard-fail at `Config.Validate()`. Fail-closed; loopback/private hosts
exempt (cleartext inside a cluster is a normal, accepted deployment). Brand-new project, so no
installed base to break.

**Where:** `internal/config/config.go`, a new check early in `Validate()`.

**Rule:** for each configured model endpoint that carries an API key, if its `base_url` scheme
is `http` and the host is **not** private/loopback, return an error. Endpoints checked:

- `Model` — when `Model.APIKeyEnv != ""` and `Model.BaseURL != ""`.
- `Model.Verify` — when the override sets its own `BaseURL` *and* `APIKeyEnv` (an override that
  inherits the parent's endpoint/key is covered by the parent check).
- `Model.Embeddings` — when `Embeddings.APIKeyEnv != ""` and `Embeddings.BaseURL != ""`.

An empty `base_url` (Anthropic/Gemini built-in default endpoint) is always fine — the defaults
are `https`. The signal for "a key is intended" is a non-empty `api_key_env` (we do not read
the env var at validation time).

**Private/loopback classification — pure, no DNS** (validation must stay deterministic and
network-free for unit tests). A host is treated as private when any of:

1. it is an IP literal that is loopback / RFC-1918 private / link-local / unspecified
   (`net.ParseIP` + `IsLoopback() || IsPrivate() || IsLinkLocalUnicast() || IsUnspecified()`), or
2. hostname `== "localhost"`, or
3. hostname is a **single DNS label** (contains no `.`) — e.g. `http://vllm:8000`, the common
   in-cluster short service name, or
4. hostname ends with `.local`, `.internal`, `.svc`, or `.cluster.local` (in-cluster / private
   DNS suffixes).

Anything else (a dotted public-looking FQDN, or a public IP literal) is treated as public, so
`http` + key there fails. This heuristic is intentionally conservative toward *allowing*
in-cluster setups; the documented escape hatch for an unusual private FQDN that doesn't match
is "use https".

**Error message** (per offending field):

```
model.base_url is "http://…" with an API key (model.api_key_env set) on a public host —
the key would be sent in cleartext. Use https, or a loopback/in-cluster endpoint.
```

A `base_url` that fails `url.Parse`, or whose scheme is neither `http` nor `https` while a key
is set, also returns a clear validation error (cheap correctness win in the same check).

## Files touched

- `internal/httpx/client.go` — extend `DenyInternalRedirect`; add `sensitiveAuthHeaders`.
- `internal/httpx/client_test.go` — S1 table tests.
- `internal/config/config.go` — add the cleartext-key check + a pure `isPrivateHost(host)` /
  `keyEndpointInsecure(baseURL, apiKeyEnv)` helper.
- `internal/config/config_test.go` (or `network_test.go`) — S2 table tests.
- Doc note in the model config comment block pointing at the https requirement.

No changes to the three model clients themselves — the fix lives entirely in the shared
`httpx` redirect policy and config validation, so all providers are covered at once.

## Testing

**S1 (`DenyInternalRedirect`)** — table-driven, pure `*http.Request` + `via`, DNS stubbed via
the existing `lookupIP` package var:

- cross-host redirect strips `x-api-key`, `x-goog-api-key`, `Authorization`;
- same-host redirect retains all three;
- same-host `http→https` (port change, same hostname) retains the key;
- internal-origin chain still allowed; redirect to an internal target still blocked;
- `maxRedirects` cap unchanged.

**S2 (`Config.Validate`)** — table-driven, no network:

- `http` + public FQDN + key → error; `https` + public + key → ok;
- `http` + private IP literal + key → ok; `http` + `localhost` + key → ok;
- `http` + single-label host + key → ok; `http` + `.svc`/`.cluster.local` + key → ok;
- `http` + public + **no** key → ok (no key to leak);
- empty `base_url` + key → ok (default https endpoint);
- `verify` and `embeddings` endpoints each exercised;
- unparseable `base_url` with a key → error.

## Non-goals (explicitly out of scope for this PR)

- PII redaction (S3), high-entropy-secret redaction (S4) — separate threads.
- Pinning a provider's key to that provider's host, or a min-TLS-version floor.
- Token-usage / caching / MCP work (their own specs).
