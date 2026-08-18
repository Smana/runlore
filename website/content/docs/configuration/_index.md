---
title: Configuration
weight: 30
---

The exhaustive key reference, and the extension layer that reaches tools RunLore does not ship.

**Presence is enablement.** A key under `sources.<name>` — or the equivalent notifier or
data-source block — is what turns that subsystem on. There is no separate `enabled: true` flag,
and nothing is required: leave a source out and RunLore runs without the tool it would have
unlocked.

- **[Configuration Reference]({{< relref "configuration.md" >}})** — every key, organised by
  subsystem: what wakes RunLore (`sources`), which incidents it takes (`triggers`), the loop's
  bounds and spend ceilings (`investigation`), the catalog and instant recall (`catalog`), the
  learning ledger (`outcome`), the autonomy ladder (`actions`, off by default), the model, the
  forge, and where findings land (`notify`).
- **[MCP]({{< relref "mcp.md" >}})** — the extension layer in both directions: giving the
  investigation loop tools from external MCP servers, and exposing RunLore's own knowledge to
  other MCP clients.

Looking to wire one specific backend rather than read the whole reference? Each
[Integration]({{< relref "/docs/integrations/_index.md" >}}) page carries the minimal config for
that backend and deep-links back here.
