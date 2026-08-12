---
title: Knowledge Commons
weight: 55
---

A RunLore deployment starts with an empty catalog of its own. `kb_search` returns nothing
from it, and it stays that way until you have investigated enough incidents to curate entries
worth keeping. The **knowledge commons** fills that gap: a shared, read-only corpus of generic
playbooks — `CrashLoopBackOff`, `ImagePullBackOff`, unbound PVCs, stuck cert-manager
challenges, unschedulable pods — that covers failure *classes* every Kubernetes platform hits.

It is a **second catalog root**, indexed alongside your own, so the investigation loop has
something to ground on from day one.

> [!NOTE]
> **It is on by default in the `standard` Helm profile.** If you installed with
> `values-standard.yaml` you already have it; `minimal` and the chart defaults leave it off,
> and `full` enables it too. Remove the `catalog.commons` block to opt out — nothing else
> depends on it.

What it does *not* do is make instant recall work on day one — see
[below](#why-commons-entries-never-fire-instant-recall). Day-one value is a better-grounded
*investigation*, not a cheap instant answer.

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
same failure class. That decides equal scores only — which corpus leads on a given alert is a
relevance question, answered [below](#which-corpus-leads-and-when).

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

## Which corpus leads, and when

`kb_search` is where commons entries do their work, and how much of it they keep depends on
what is failing. It is worth stating precisely, because nothing warns you when a playbook that
used to surface stops surfacing — or when one you assumed you had replaced surfaces anyway.

**Every `kb_search` query is enriched with the incident's workload.** Whatever the model types,
RunLore appends the failing workload's namespace and normalized name — plus the alert name —
server-side, before the query reaches the index. The reason is retrieval quality: a
label-derived alert's text is one or two generic tokens, and the object that identifies the
incident lives only in the labels. That is the same vocabulary mismatch
[instant recall]({{< relref "learning-loop.md" >}}) already solves, fixed here the same way.

Enrichment is not a provenance rule and knows nothing about which root an entry came from. It
lifts whichever entries contain those tokens — and *demotes* the ones that do not, because
BM25 scores a partly-matched query below a fully-matched one, so adding terms an entry cannot
match costs it rank. **The entry that names the failing object leads.** Which entries can do
that depends on the object:

**Application workloads — your entries, and only yours.** `payments` and `checkout-api` appear
in no generic playbook: a commons entry names a failure class and carries no `resource`. So
once you have curated an incident for `payments/checkout-api`, an alert on that workload puts
*your* entry first — even when the two describe the same failure in nearly the same words, and
even when the commons entry scores higher on the symptom text alone (BM25 normalizes for
document length, and your entry carries a `resource` line the commons entry may not). On an
empty catalog the commons `CrashLoopBackOff` playbook leads that same alert; measured against
the shipped corpus, one curated incident of your own moves it behind yours by roughly an order
of magnitude. For this class the commons really is a floor that fades.

**Platform components — the commons can lead, and usually should.** The playbooks name
cert-manager, ArgoCD, CoreDNS and `kube-system` explicitly, in their tags and in the `kubectl`
commands they tell you to run. When the failing workload *is* one of those — namespace
`cert-manager`, workload `cert-manager-webhook` — the enrichment tokens match the shared
playbook too, and it can take the top slot from an entry of yours about a certificate on some
other service. It should: it describes the component that is actually broken, while yours
describes a different object.

The rule is per *object*, not per failure class. Curate an incident for the platform component
itself — `resource: cert-manager/cert-manager-webhook` — and your entry carries those same
tokens, competes on the symptom text again, and leads. Until then the shared playbook holds
that slot regardless of how much unrelated knowledge you have accumulated.

**Your entry wins ties**, as above. That settles equal scores only; it does not override a
relevance gap in either direction, and the gaps enrichment opens are far larger than a tie.

There is no knob either way — enrichment is unconditional and the tie-break is not
configurable — so plan for both directions. A commons entry can fall out of the top hits for a
failure class you have learned, which is the intended outcome; if losing it costs you
something, your own entry for that class is too narrow. And a commons entry can hold the top
slot for a platform component you have *not* learned, well past the point where you assume your
own catalog is answering. In both cases the fix is the same: write the entry for the object the
alert actually names.

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

### Tracking a branch, or pinning a revision

`commons.branch` tracks a moving branch: an upstream edit reaches `kb_search` at the next
interval. Shared corrections arrive without you doing anything — which is the point of a
commons, and also means the corpus moves without you looking.

`commons.ref` is the other side of that trade. Pin a tag or a full 40-character commit id
and the corpus is frozen until you move it, so you can read the upstream diff before it
grounds an investigation:

```yaml
catalog:
  commons:
    url: https://github.com/Smana/runlore-kb-commons
    ref: v1.2.0                      # a tag name, or a full commit id
    dir: /var/lib/runlore/commons
```

`branch` and `ref` are mutually exclusive — setting both fails at startup rather than
quietly picking one. A pinned revision cannot move, so the periodic sync becomes a local
no-op rather than a fetch, and the index is rebuilt only on startup or when you move the
pin. Your own `catalog.git` repo is deliberately not pinnable: the curator opens PRs
against it, and freezing it would leave "merged" and "in use" permanently apart.

Be precise about what pinning buys, because an unpinned commons is not a hazard you are
patching. The properties that keep a shared corpus in its place hold either way: its
entries are read-only, the curator never writes to them, yours win ties, and none of them
can fire instant recall. What an upstream edit can still influence is what `kb_search`
puts in front of the model mid-investigation — evidence is still gathered, the verdict is
still reached against your cluster, but the prompt was shaped by someone else's most
recent commit. Pinning narrows that to changes you have read. It cannot judge them for
you: a pin you bump without reading the diff buys exactly nothing.
