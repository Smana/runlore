# Serving RunLore over MCP

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
