---
title: Reference
weight: 60
---

Lookup material: the command-line surface, how to choose a model, the knowledge-stewardship skill,
and a full worked investigation.

- **[CLI]({{< relref "cli.md" >}})** — `lore` is one static binary: a demo that runs with no key
  and no cluster, the server, and the knowledge-catalog subcommands.
- **[Benchmarking]({{< relref "benchmarking.md" >}})** — RunLore is model-agnostic, so the model
  you pick is a real decision. This is how to measure it on your own incidents rather than trusting
  a leaderboard.
- **[kb-steward]({{< relref "kb-steward.md" >}})** — the skill for seeding, capturing and curating
  catalog entries. Diagnosis stays RunLore's job; the skill only writes knowledge down well.
- **[Examples]({{< relref "/docs/reference/examples/harbor-registry-down.md" >}})** — a real
  investigation end to end, from the firing alert to the merged catalog entry.

Looking for the **tools the investigation loop calls** — `query_metrics`, `what_changed`,
`pod_logs` and the rest? Those are described with the backends that unlock them, in
[Data Sources]({{< relref "/docs/concepts/data-sources.md" >}}).
