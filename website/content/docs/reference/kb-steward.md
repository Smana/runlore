---
title: kb-steward
weight: 10
---

> RunLore investigates and proposes knowledge automatically. **kb-steward** is
> the human half: a [Claude Code](https://code.claude.com/docs) skill that
> interviews you and turns what you know into recall-grade
> [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog) entries.

Diagnosis stays RunLore's job — the skill only captures and curates knowledge.

## Install

```
/plugin marketplace add Smana/runlore
/plugin install kb-steward@runlore
```

Update later with `/plugin update kb-steward`. No binary, no server change —
the plugin is served from this repo.

## Using it with another agent

The plugin above is packaging, not a dependency. The skill is plain markdown
with no harness-specific instruction in it — a Go test
(`TestSkillContentIsHarnessNeutral`) fails the build if that ever stops being
true — so any coding agent that reads files can run it:

1. Copy `plugins/kb-steward/skills/kb-steward/` (SKILL.md plus `references/`)
   into your KB repo, e.g. as `.kb-steward/`:

   ```bash
   git clone --depth 1 https://github.com/Smana/runlore /tmp/runlore
   cp -r /tmp/runlore/plugins/kb-steward/skills/kb-steward .kb-steward
   ```

2. Point your agent at it — either directly ("follow `.kb-steward/SKILL.md`")
   or, better, from your KB's `AGENTS.md`, which most agents read
   automatically:

   ```markdown
   ## Knowledge-base conventions
   When adding or curating entries in this repo, follow `.kb-steward/SKILL.md`.
   ```

SKILL.md's YAML frontmatter is metadata for Claude Code's skill loader; other
agents ignore it harmlessly. Nothing else in the file assumes a runtime.

Reading the catalog is already agent-agnostic by a different route: `lore mcp
<kb-dir>` serves `kb_search` and `kb_get` to any MCP client, with no cluster,
model, or config — see [MCP](mcp.md). This skill is the writing half.

## What it does

| You say | It does |
|---|---|
| "Seed my RunLore knowledge base" | Structured interview about your platform (clusters, GitOps, alerting, conventions, tribal knowledge) → small scoped Concept/Playbook entries (Incident too, for past outages worth remembering), plus an `AGENTS.md` platform profile so it never re-asks |
| "Write up the incident we just resolved" | RCA interview with pushback (symptom vs cause, five whys) → one gate-passing Incident entry — updating a near-duplicate instead when one exists |
| "Review RunLore's KB PRs" | Quality + duplicate check per PR, merge/refine/close recommendation — and points at `forge.skip_verdicts` & friends when the queue is systematically noisy |
| "Clean up the catalog" | Finds stale or weak entries, proposes revalidation or `status: retired` |

## Example session

What the post-incident flow looks like in practice:

> **You:** Write up the incident we just resolved
>
> **Steward:** First — is it fully resolved? If it's still live, diagnosis is
> RunLore's job; I only capture what's settled.
>
> **You:** Resolved an hour ago.
>
> **Steward:** What fired, exactly — alert name and affected workload?
>
> **You:** `KubeContainerOOMKilled`, pod `oom-app` in `shop-prod`.
>
> **Steward:** You said the memory limit was the cause. Is that the cause, or
> the first symptom you noticed — if you only raised the limit, would it
> recur?
>
> **You:** The v2 image bump grew the working set to ~180Mi and the 100Mi
> limit was never adjusted. Raised it to 256Mi in the HelmRelease values;
> restarts stopped.
>
> **Steward:** Searched the catalog for `shop-prod/oom-app` and OOMKilled —
> no existing entry. Drafting one Incident entry.

The draft runs through the entry-quality checklist, a secret scan, and `lore
validate-kb`, then lands on a branch (`kb-steward/oom-app-memory-limit`) as a
gate-passing entry:

```markdown
---
type: Incident
title: KubeContainerOOMKilled for oom-app
description: Container 'hog' is OOMKilled because its memory limit (100Mi) is below actual usage.
resource: shop-prod/oom-app
tags: [deployment, shop-prod, oomkilled, memory]
…
```

(the full entry is the example in the skill's `references/okf-format.md`)

The skill opens a PR against your KB repo; you merge. Next time that alert
fires, RunLore's instant recall serves the entry. Seed, PR-triage, and
maintenance sessions follow the same shape: interview or scan →
checklist-validated drafts → a PR you merge.

## Ground rules the skill enforces on itself

- **PR by default, never merges unless explicitly told to** — nothing enters
  the KB without a human merge, the same gate RunLore's own findings go
  through. A solo maintainer can ask for a direct commit or merge, and the
  skill complies and says so.
- **No fabrication** — unknowns are recorded as unknowns.
- **Secret scan** before any draft is written (SREs paste logs; logs leak).

## Staying honest

The skill documents the exact frontmatter contract the catalog loader parses.
A test in this repo (`internal/catalog/skillcontract_test.go`) fails if the
two drift apart, and `claude plugin validate .` checks the plugin layout.

See also: [Reviewing & approving RunLore's knowledge](reviewing-knowledge.md)
— the merge side of the same loop.
