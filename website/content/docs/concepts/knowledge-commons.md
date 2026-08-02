---
title: Knowledge Commons
weight: 55
---

A fresh RunLore deployment has an empty catalog. `kb_search` returns nothing, and it stays
that way until you have investigated enough incidents to curate entries of your own. The
**knowledge commons** fills that gap: a shared, read-only corpus of generic playbooks —
`CrashLoopBackOff`, `ImagePullBackOff`, unbound PVCs, stuck cert-manager challenges,
unschedulable pods — that covers failure *classes* every Kubernetes platform hits.

It is a **second catalog root**, indexed alongside your own. Enable it and the investigation
loop has something to ground on from day one.

```yaml
catalog:
  dir: /var/lib/runlore/catalog
  commons:
    url: https://github.com/Smana/runlore-kb-commons
    branch: main
    interval: 24h
    dir: /var/lib/runlore/commons    # MUST differ from catalog.dir
```

## Two roots, four rules

The commons is deliberately not just "more entries in your catalog". Four properties hold,
and each one exists to stop a specific failure:

**Both roots are indexed together.** `kb_search` searches one index covering both. There is
no separate tool and no flag to remember mid-incident.

**Commons entries are marked.** Every entry carries its provenance, so a citation tells you
whether the knowledge came from your cluster's history or from the shared corpus. Those
deserve different amounts of trust and you should be able to tell them apart at a glance.

**Your entry wins ties.** When a commons entry and one of your own score equally, yours
ranks first. Knowledge written from your actual incidents beats a generic description of the
same failure class, so the commons can only ever act as a floor — never as competition.

**The curator never writes to it.** Drafted entries always land in your own catalog repo.
The commons directory is a mirror of an upstream repository and stays pristine; there is no
configuration that makes it writable.

## Why commons entries never fire instant recall

This is the property most worth understanding before you enable it.

[Instant recall]({{< relref "learning-loop.md" >}}) short-circuits the LLM loop entirely when the catalog
already holds a trustworthy answer. Before any scoring happens, it applies a **structural
filter**: an entry's `resource` must agree with the alert's workload. An entry with no
`resource` can only agree with a request that itself carries no workload — and real
Kubernetes alerts carry a namespace and a workload.

Commons entries are resource-less by construction. They describe a failure class, not one
cluster's workload, so there is no honest `resource` to give them. The consequence follows
directly: **a commons entry can never fire instant recall.**

That is the correct behaviour, not a limitation to work around. Instant recall answers an
incident without looking at it. Doing that from a generic playbook would mean asserting that
*your* `CrashLoopBackOff` has the same cause as the textbook one — which is exactly the
confidently-wrong answer that makes cached knowledge worse than none.

What commons entries do instead is ground `kb_search`, which applies no structural filter.
The model finds and cites them mid-loop, where they belong: telling the investigation which
command to run next and which readings distinguish one cause from another. The evidence
still gets gathered; the playbook just makes gathering it faster.

To get instant recall you need entries scoped to your workloads (`resource:
<namespace>/<name>`). Those are the ones RunLore drafts for you from your own incidents.

## Scope discipline

Every commons entry ends with a `# Not covered` section naming the adjacent failure classes
it does **not** explain — the OOM playbook says it does not cover node memory pressure, the
scheduling playbook says it does not cover volume node affinity conflicts.

This is load-bearing rather than decorative. A generic playbook is most dangerous at its
edges, where it looks applicable and is not, so each entry states its own boundary instead
of leaving the model to infer one.

## Operational notes

The commons syncs on its own schedule, separate from your catalog's: its own directory, a
much longer interval (a shared corpus changes slowly), and a sync failure that logs rather
than propagates. An upstream repository being briefly unreachable must never degrade the
catalog an incident actually depends on.

Its checkout is an `emptyDir` — re-cloned on startup, never persisted. Nothing is authored
there, so there is nothing to lose on restart, and keeping it off the persisted data path
means an upstream sync cannot dirty the volume holding your outcome ledger and audit log.

`commons.dir` must differ from `catalog.dir`. Both the agent's config validation and the
Helm chart refuse to start with a shared root, because one directory serving both corpora
would let an upstream sync overwrite the entries you curate.
