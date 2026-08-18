---
title: Concepts
weight: 20
---

How RunLore is designed, and why a deployment gets better at your platform over time instead of
re-deriving the same diagnosis every week. These pages explain the model; they are not setup
guides — [Integrations]({{< relref "/docs/integrations/_index.md" >}}) and
[Configuration]({{< relref "/docs/configuration/configuration.md" >}}) are.

- **[Design]({{< relref "design.md" >}})** — what RunLore is for, its goals and non-goals, the
  three pillars (React, Investigate, Learn), and the autonomy ladder. Start here.
- **[Architecture]({{< relref "/docs/concepts/architecture/index.md" >}})** — the same flow as a
  diagram, with the read-only boundary drawn on it.
- **[Learning Loop]({{< relref "learning-loop.md" >}})** — the heart of it: how a verified finding
  becomes recallable knowledge, how recall is kept trustworthy, and what makes a wrong entry decay
  instead of persisting.
- **[Data Sources]({{< relref "data-sources.md" >}})** — the signal model: which tool each backend
  unlocks, behind which interface. Every source is pluggable and none is assumed.
- **[Reviewing Knowledge]({{< relref "reviewing-knowledge.md" >}})** — RunLore never writes to the
  catalog directly; it opens pull requests. This is what you are agreeing to when you merge one.
- **[Knowledge Commons]({{< relref "knowledge-commons.md" >}})** — why a fresh deployment starts
  useful instead of empty, and what the shared catalog does and does not contain.
- **[Prior Art]({{< relref "prior-art.md" >}})** — where RunLore sits among the other AI-SRE
  tools, stated honestly.
