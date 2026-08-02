# last9/awesome-sre-agents

**Status: already listed. No submission needed.**

RunLore is at `README.md` line 44, merged via
[PR #21](https://github.com/last9/awesome-sre-agents/pull/21), authored by `Smana`.

Current entry, verbatim from
[the raw README](https://raw.githubusercontent.com/last9/awesome-sre-agents/main/README.md):

```markdown
- [RunLore](https://github.com/Smana/runlore) (Open Source) - Agent that investigates Kubernetes incidents using GitOps-exact change diffs and records what it learns as PR-reviewed markdown in a Git knowledge base you own.
```

It sits under `## SRE Agents`, alongside HolmesGPT, k8s-GPT and NudgeBee — the correct
section for a self-hosted, open-source, GitOps-native Kubernetes investigation agent.

## If the entry ever needs updating

The list's own format, from
[CONTRIBUTING.md](https://raw.githubusercontent.com/last9/awesome-sre-agents/main/CONTRIBUTING.md):

```
- [Name](URL) - One sentence describing what it does.
- [Name](URL) (Open Source) - One sentence describing what it does.
```

Rules that actually bind:

- `(Open Source)` goes **before** the dash, for open-source projects.
- **Entries are sorted alphabetically within each section.**
- One factual sentence describing what the tool does — the guide explicitly says "not
  marketing copy".
- The project must be **actively maintained**.
- No license, language, or star fields. No per-entry badges.

Sections that exist: `## Agent Benchmarks`, `## Incident Response Agents`,
`## Platforms of Agents`, `## SRE Agents` (with a nested `### Archived`).

PR title format is not documented; merged PRs use `Add <Name> to <Section>` or plain
`Add <Name>`.

## A note on the current description

At ~33 words it is the longest entry in the list — the median is around 12. It is
accurate and it does describe what the tool does rather than selling it, so it is within
the stated rules. If it is ever revised, shortening it would fit the list's house style
better. Something like:

```markdown
- [RunLore](https://github.com/Smana/runlore) (Open Source) - Agent that investigates Kubernetes incidents against GitOps-exact change diffs and curates each verified cause into a knowledge base you own.
```

This is a suggestion, not a pending change. **Do not open a PR to reword an entry that is
already merged and correct** — it costs a maintainer a review for no user-visible gain.
