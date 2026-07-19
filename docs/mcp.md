# MCP — RunLore as server and as client

MCP is RunLore's extension layer, in **both directions**:

- **Server** (`lore mcp`) — expose RunLore's capabilities (what-changed, KB search) as
  tools to any MCP client.
- **Client** (`mcp.servers` in the config) — extend the **investigating agent's own
  toolbox** with remote MCP tools, **zero Go required** (see
  [Extending the agent's tools](#extending-the-agents-tools-mcp-client) below).

## Serving RunLore over MCP

`lore mcp` is a read-only [Model Context Protocol](https://modelcontextprotocol.io)
server on **stdio**: point any MCP client (Claude Code, HolmesGPT, kagent, an
editor) at the binary and it can query RunLore's capabilities as tools. stdout
carries the protocol; logs go to stderr.

It serves whichever capabilities are configured — each one is optional:

| Capability | Tools | Needs |
|---|---|---|
| GitOps what-changed | `gitops_what_changed` | a Kubernetes client + `gitops` in the config |
| Knowledge base | `kb_search`, `kb_get` | an OKF catalog directory |

## Knowledge-base tools

Your merged KB entries are plain OKF markdown in Git — which means they're useful
far beyond the agent's own instant recall: during a postmortem, reviewing an
infra PR ("has this bitten us before?"), or on-call from a laptop. The KB tools
need **no cluster, no model, no config file**:

```bash
git clone https://github.com/you/your-kb && lore mcp ./your-kb
```

- **`kb_search`** — BM25 search over the catalog. Args: `query` (required),
  `k` (default 5, cap 20). Returns scored hits: `path`, `type`, `title`,
  `description`, `resource`, `tags`, `score`.
- **`kb_get`** — one full entry (frontmatter + markdown body) by the
  bundle-relative `path` a search hit returned. Curated entries also carry their
  `timestamp` (last-change stamp) and `fingerprint` (dedup identity) so a client
  can judge freshness; hand-written entries omit both. Lookups go through the
  in-memory index, never the filesystem, so path traversal is structurally
  impossible.

The catalog directory resolves from the positional argument first, then
`catalog.dir` in the config. The catalog is indexed once at startup; re-run
`lore catalog sync` (or `git pull`) and restart to pick up new entries.

## Client configuration

Claude Code:

```bash
claude mcp add runlore-kb -- lore mcp /path/to/kb
```

Generic MCP client config (stdio):

```json
{
  "mcpServers": {
    "runlore-kb": {
      "command": "lore",
      "args": ["mcp", "/path/to/kb"]
    }
  }
}
```

In-cluster (what-changed + KB), keep using the config file:

```bash
lore mcp --config runlore.yaml
```

A missing config file is tolerated when a catalog dir is given (KB-only mode); a
present-but-invalid config still fails loudly.

## Extending the agent's tools (MCP client)

The investigation loop's built-in tools are deliberately few; the way to add more is
**MCP, not Go**. Declare remote servers in the config and their tools join the agent's
toolbox at startup:

```yaml
mcp:
  # require_allowlist: true                # fail closed: every server MUST list tools
  servers:
    - name: runbooks                       # tools appear as runbooks__<tool>
      url: https://mcp.internal.example/mcp # streamable-HTTP transport
      # token_env: RUNBOOKS_MCP_TOKEN       # optional bearer token (env-var name)
      # tools: [search, get]                # exact-name allowlist; omit ⇒ all tools
```

What to know:

- **Transport:** streamable HTTP (the current MCP standard). stdio *client* transport is
  not supported — RunLore runs in-cluster, remote servers are network services.
- **Namespacing:** every remote tool is exposed to the model as `<server>__<tool>`, and
  the system prompt marks them as **external, untrusted-output, read-only** tools — a
  remote tool can inform an investigation, never perform an action (the action gate only
  knows the built-in operations). Note the gate constrains RunLore's *own* write path — a
  remote tool with server-side side effects is only prevented from being *called* by the
  allowlist below.
- **Allowlist:** `tools:` is an exact-name allowlist enforced at discovery — an un-listed
  tool is never registered, so the model cannot call it. `mcp.require_allowlist: true`
  refuses startup unless every server declares one (deny-by-default). Discovery follows
  `tools/list` pagination (bounded at 16 pages), so the allowlist is applied to the
  server's complete tool list.
- **Failure isolation:** discovery happens at startup per server; an unreachable server
  is logged and skipped — it never blocks the agent or the other servers. Remote calls
  are not retried (they are not assumed idempotent).
- **Security:** the same cleartext-credential guard as the model endpoints applies — a
  bearer token over public `http://` is rejected at startup. Tool *output* is treated as
  untrusted data like every other tool result (redaction, no instruction-following).
