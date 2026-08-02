---
title: MCP
weight: 100
integration: {kind: mcp, id: mcp}
---

**What it gives you** — extends the agent's own toolbox with remote [Model Context
Protocol](https://modelcontextprotocol.io) tools, over streamable-HTTP (JSON-RPC 2.0) — no Go code
required.

## Minimal config

```yaml
mcp:
  servers:
    - name: mydb           # short identifier; namespaces all tools as mydb__<tool>
      url: https://mcp.example.com/mcp
      token_env: MYDB_MCP_TOKEN   # optional — env var holding a bearer token
      headers:                    # optional extra request headers
        X-Tenant: my-org
```

Deny-by-default, allowlisting only specific tools per server:

```yaml
mcp:
  require_allowlist: true    # refuse startup unless EVERY server declares a tools allowlist
  servers:
    - name: mydb
      url: https://mcp.example.com/mcp
      token_env: MYDB_MCP_TOKEN
      tools: [query, describe_schema]   # only these two tools are ever registered
```

## Verify it locally

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'mcp server|mydb__'
```

Fire a test incident and confirm a namespaced MCP tool was called:

```bash
kubectl -n runlore logs deploy/runlore | grep 'tool=mydb__'
```

## Notes

- **Opt-in** — an empty `servers` list (the default) disables MCP entirely.
- **Namespaced names.** Every remote tool registers as `<server>__<tool>` (e.g. `mydb__query`), so MCP
  tools never collide with RunLore's built-in tools. Built-in names always win on a collision.
- **Read-only from RunLore's side** — the MCP adapter only calls `tools/call`; it never mutates
  RunLore's own state. The remote tool itself may still mutate *its* backend — see the security note
  below.
- **Failure-isolated.** A server that fails the `initialize` handshake or `tools/list` is logged at
  Warn and skipped; RunLore starts the investigation loop with whatever tools it does have, rather
  than aborting startup over one bad server.
- **Secrets by indirection** — `token_env` names an environment variable; never put the token value
  directly in config.
- **`headers` are not secret-safe over plain HTTP.** Only `token_env` is checked at config validation
  time; custom `headers` values are not. Do not carry secrets in `headers` when `url` is plain
  `http://` to a public host — use `https://` for any server on a public network, and keep `headers`
  for non-secret metadata (e.g. `X-Tenant`).
- **A remote MCP server is part of your trust boundary.** RunLore's own action gate only stops
  RunLore from executing cluster operations directly — a remote tool that mutates state server-side
  does so the instant it's called, gate or no gate. Scope `tools` per server to exactly what an
  investigation needs, and use `require_allowlist: true` to refuse startup if any server is left
  wide open. See [Security model → External MCP tools]({{< relref
  "/docs/security/security-model.md#external-mcp-tools" >}}).
- `name` must be non-empty, contain no `__` or whitespace, and be unique across servers — enforced at
  config load, not silently at wiring time. Each `tools` entry must likewise be non-empty, whitespace-
  free, and unique within its server.

This is the **client** half of RunLore's MCP support — extending the investigating agent's own
toolbox. RunLore is also an MCP **server** (`lore mcp`, serving `kb_search`/`kb_get`/what-changed to
any MCP client, no cluster or model required) — a separate, unrelated capability covered on its own
page.

## Reference

- [Configuration → `mcp`]({{< relref "/docs/configuration/configuration.md#mcp--external-mcp-tool-servers-opt-in" >}})
  for the full key reference.
- [Security model → External MCP tools]({{< relref "/docs/security/security-model.md#external-mcp-tools" >}})
  for the trust-boundary reasoning above.
- [MCP (concept)]({{< relref "/docs/configuration/mcp.md" >}}) — the fuller write-up, including the
  `lore mcp` server side.
