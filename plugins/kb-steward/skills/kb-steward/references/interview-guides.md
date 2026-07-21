# Interview guides

One question at a time. Skip anything AGENTS.md already answers. Prefer
multiple-choice when the options are enumerable; open-ended otherwise.
Every answer is a candidate entry — keep a running list and confirm the
batch with the SRE before drafting.

Field-by-field OKF contract (what each frontmatter field requires): see
`references/okf-format.md`.

## Seed interview (flow 1)

### 1. Platform inventory
- Which clusters exist, and what is each for? (name, environment, criticality)
- GitOps engine — Flux or ArgoCD? Where is the gitops repo, how is it laid out?
- Cloud/on-prem, regions, anything multi-?
- What workload classes run here (stateless services, stateful, batch/ML)?

### 2. Observability & alerting
- Metrics / logs / traces stacks — and which are actually trustworthy?
- Where do alerts route (Alertmanager → Slack/PagerDuty…), who gets paged?
- Which alerts are known-noisy, and why? (each answer → a Concept entry —
  exactly what keeps an agent from chasing ghosts)

### 3. Conventions
- Namespace scheme, naming conventions, labels/annotations that mean something
- Environment promotion flow — how does a change reach prod?
- A tag vocabulary for KB entries: agree on ~10 tags now, record them in the
  platform profile

### 4. Failure modes & tribal knowledge
- What breaks regularly? What's the fix nobody wrote down?
- What would you tell a new on-call in their first week?
- Which dependencies bite (external SaaS, DNS, certificates, quotas, IPs)?
- Any "never do X" rules — and what happened when someone did?

### 5. Existing material
- Runbooks, ADRs, wiki pages worth converting — where?
- For each: still accurate? Who owns it? One entry per symptom/procedure,
  never a bulk dump — confirm with the SRE which are worth converting.

### Mapping answers to types
- Facts about how the platform is built/behaves → `Concept`
- Step-by-step procedures ("how to drain", "how to rotate") → `Playbook`
- Past outages worth remembering → `Incident` (use the post-incident map)

### Platform profile (AGENTS.md at the KB root)

Write or refresh it after the interview, with OKF frontmatter so the loader
indexes it like any other entry (`AGENTS.md` isn't in the skip list) and
future kb-steward sessions can skip answered questions.

Set expectations honestly about *recall*: with no `resource`, this entry is
scopeless, and scopeless entries only match incidents that carry no workload
at all — any alert with a namespace will never recall it. Its real audience is
humans and the agents reading the repo, plus `kb_search` during an
investigation. That is a reason to keep it, not a reason to fake a `resource`:
the durable, recallable knowledge belongs in the small scoped entries.

(It lives at the KB root, not under `incidents/`/`playbooks/`/`concepts/` — a
deliberate exception to that write convention, since it's the platform
profile.)

```markdown
---
type: Concept
title: Platform profile
description: <one sentence: stack, cloud, GitOps engine, environments — use real names>
tags: [platform, profile, <gitops-engine>, <cloud>]
last_validated: "<today>"
---

## Platform
- Clusters: …
- GitOps: …

## Observability
- …

## Conventions
- Namespaces: …
- KB tag vocabulary: …

## Known-noisy alerts
- …

## Interview log
- <date>: seeded sections 1–5 (kb-steward)
```

## Post-incident interview (flow 2)

Confirm first: **is the incident resolved?** If it's live, stop — capture
happens after resolution; diagnosis is RunLore's (or the human's) job.

1. **Trigger** — what fired or how was it noticed? Exact alert name / error
   string (these words become the description). Affected workload as
   `namespace/name` (→ `resource`; required for Incident). If the alert
   fired on a different resource than the fault (symptom pod vs faulty
   config), the alert's resource goes in `alert_resource` instead, and
   `resource` stays the actually-faulty workload.
2. **Impact & timeline** — start, detection, mitigation, resolution times;
   blast radius.
3. **What changed** — deploys, config, infra around onset? Git SHAs or
   releases if known.
4. **Root cause — with pushback.** Don't accept the first answer:
   - "Is that the cause, or the first symptom you noticed?"
   - "If you rolled only that back, would it recur?"
   - "What allowed the system to fail this way?" (keep asking why —
     usually 3–5 levels)
5. **Fix** — what actually resolved it? Temporary mitigation or permanent?
6. **Verification** — how would a teammate confirm the fix worked? (becomes
   `## Resolution` / `## Investigate` content)
7. **Prevention** — guardrails added, tickets opened, alerts tuned?

Then: near-duplicate check (search the catalog for the resource + alert
keywords). Update-and-revalidate an existing entry when one matches;
otherwise draft one Incident entry, plus a Playbook only when the procedure
generalizes beyond this resource.
