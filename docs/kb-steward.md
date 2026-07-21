# kb-steward — a Claude Code skill for your knowledge base

> RunLore investigates and proposes knowledge automatically. **kb-steward** is
> the human half: a [Claude Code](https://code.claude.com/docs) skill that
> interviews you and turns what you know into recall-grade OKF entries.

Diagnosis stays RunLore's job — the skill only captures and curates knowledge.

## Install

```
/plugin marketplace add Smana/runlore
/plugin install kb-steward@runlore
```

Update later with `/plugin update kb-steward`. No binary, no server change —
the plugin is served from this repo.

## What it does

| You say | It does |
|---|---|
| "Seed my RunLore knowledge base" | Structured interview about your platform (clusters, GitOps, alerting, conventions, tribal knowledge) → small scoped Concept/Playbook entries, plus an `AGENTS.md` platform profile so it never re-asks |
| "Write up the incident we just resolved" | RCA interview with pushback (symptom vs cause, five whys) → one gate-passing Incident entry — updating a near-duplicate instead when one exists |
| "Review RunLore's KB PRs" | Quality + duplicate check per PR, merge/refine/close recommendation — and points at `forge.skip_verdicts` & friends when the queue is systematically noisy |
| "Clean up the catalog" | Finds stale or weak entries, proposes revalidation or `status: retired` |

## Ground rules the skill enforces on itself

- **PR by default, never merges** — nothing enters the KB without a human
  merge, the same gate RunLore's own findings go through.
- **No fabrication** — unknowns are recorded as unknowns.
- **Secret scan** before any draft is written (SREs paste logs; logs leak).

## Staying honest

The skill documents the exact frontmatter contract the catalog loader parses.
A test in this repo (`internal/catalog/skillcontract_test.go`) fails if the
two drift apart, and `claude plugin validate .` checks the plugin layout.

See also: [Reviewing & approving RunLore's knowledge](reviewing-knowledge.md)
— the merge side of the same loop.
