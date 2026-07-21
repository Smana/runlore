# kb-steward: a Claude Code skill for RunLore users

**Date:** 2026-07-21
**Status:** Approved design, pre-implementation

## Problem

RunLore's recall is only as good as the catalog behind it, and the catalog has a
cold-start problem: a fresh install ships with example runbooks, not the
adopter's platform knowledge. design.md already anticipates seeding from
"existing runbooks, ADRs, wiki, and AGENTS.md" but nothing guides that work.
Meanwhile SREs (including the maintainer) already use Claude Code locally to
manage KB entries and write post-mortems — with no encoded knowledge of what a
*good* OKF entry looks like (frontmatter is the recall signal; a badly-tagged
entry is invisible).

## Decision

Ship **one Claude Code skill, `kb-steward`**, as a plugin served from the
runlore repo itself. The skill is a **KB steward only**: it interviews the SRE
and writes knowledge. It never diagnoses incidents — diagnosis is RunLore's
job; the boundary is retrospective capture vs live investigation.

## Distribution

The runlore repo becomes a Claude Code plugin marketplace:

```
.claude-plugin/marketplace.json          # lists the kb-steward plugin
plugins/kb-steward/
  .claude-plugin/plugin.json
  skills/kb-steward/SKILL.md             # router over the four flows
  skills/kb-steward/references/
    okf-format.md
    interview-guides.md
    entry-quality-checklist.md
```

Install (documented on a new docs page, linked from reviewing-knowledge.md):

```
/plugin marketplace add Smana/runlore
/plugin install kb-steward@runlore
```

Updates via `/plugin update`. No `lore` binary dependency, no fork-sync issue.

## Flows

`SKILL.md` is a short router ("which situation are you in?") over four flows —
two deep, two thin:

### 1. Seed the KB (deep — the onboarding lever)

Structured interview converting platform/company context into recall-able
entries: platform inventory (clusters, environments, GitOps engine, cloud),
observability stack, naming/namespace/tag conventions, known failure modes and
tribal knowledge, existing artifacts to convert (runbooks, ADRs, wiki pages the
SRE points at). Outputs:

- Small, **scoped** OKF Concept/Playbook entries — many focused entries, not
  one platform bible, because recall matches per-entry. Frontmatter is treated
  as the product.
- An `AGENTS.md` in the KB repo holding the platform profile, so later skill
  runs don't re-interview (and it's the artifact design.md §seeding already
  anticipates ingesting).

### 2. Post-incident capture (deep)

For a **resolved** incident RunLore missed, found inconclusive, or doesn't
watch. Structured RCA interview: what fired, timeline, what changed, root
cause — with pushback ("symptom or cause?", five-whys), fix, verification
steps, prevention. Checks the catalog for near-duplicates first and prefers
updating an existing entry. Output: one Incident entry, plus optionally a
Playbook when the troubleshooting steps generalize.

### 3. Triage RunLore's KB PRs (thin)

Frontmatter-quality checklist, near-dup check against the catalog,
merge/refine/close recommendation. When the queue is systematically noisy,
point at the config levers (`forge.skip_verdicts`, `min_confidence`,
`dup_score`) rather than eating the noise.

### 4. Catalog maintenance (thin)

Scan for stale/decayed entries via `status`/`last_validated` (N4/N6 lifecycle
fields), propose retirement or revalidation, tag hygiene.

## Shared references

- **okf-format.md** — the entry anatomy the Go loader actually parses: type
  directories, frontmatter fields (`type`, `title`, `description`, `resource`,
  `tags`, `fingerprint`, `timestamp`, `confidence`, `provenance`, `status`,
  `last_validated`) and why each matters for recall. Self-contained — the
  plugin runs far from this repo.
- **interview-guides.md** — question maps for flows 1–2, including the
  challenge prompts.
- **entry-quality-checklist.md** — the bar every draft must pass: scoped
  title, resource set, tags include workload kind + namespace, description
  contains the words an alert would contain, no secrets.

**Drift guard:** a Go test in this repo asserts that the frontmatter field
names listed in `okf-format.md` match what the catalog loader parses, so the
skill cannot silently rot when the loader evolves.

## Portability — agent-agnostic by content, Claude-packaged by default

The Claude Code plugin is a *distribution* choice, not a design constraint.
The skill body and its three references are plain markdown containing no
harness-specific instruction — no tool names, no slash commands, no
assumptions about the runtime. The only Claude-specific pieces are the two
`.claude-plugin/*.json` manifests and SKILL.md's YAML frontmatter, which
other harnesses ignore as unknown metadata.

So portability is a matter of stating and defending what is already true,
rather than restructuring:

- **Documented non-Claude paths** in `docs/kb-steward.md`: vendor the
  `skills/kb-steward/` directory into the KB repo and point the agent at
  `SKILL.md`, or reference it from the KB's `AGENTS.md` (the cross-agent
  convention this skill already reads and writes).
- **A neutrality guard** — a Go test fails if harness-coupled vocabulary
  appears in the skill body or references, so the portable core cannot rot
  into a Claude-only artifact through ordinary edits.

Rejected: moving the core to a neutral top-level directory (Claude Code
requires skill files inside the plugin directory, so it buys a cosmetic
signal at the cost of duplication or symlinks) and exposing the flows as MCP
prompts (real protocol-level agnosticism, but Go work for a primitive whose
client support is far patchier than tools).

Cross-agent KB *reading* is already solved and unaffected: `lore mcp` serves
`kb_search`/`kb_get` to any MCP client. This skill is the *writing* half.

## Guardrails

- **PR by default, never merge.** Drafted entries go on a branch + PR —
  "nothing enters the KB without a human merge" holds for human-driven flows
  too. Solo maintainers can explicitly ask for direct commit.
- **Push only to the KB repo.** Every git command names the KB repo
  explicitly and the remote is confirmed to be the catalog before any push
  or PR. A cold-run agent tried `gh pr create` from a checkout with no
  remote; it failed safely, but an ambiguous working directory must never be
  able to aim a KB entry at an unrelated repository.
- **No fabrication.** Interview answers are the only source of facts; unknowns
  are recorded as unknowns. Same principle as the agent's no-PR-when-
  inconclusive rule.
- **Secret hygiene.** Before writing any entry, scan drafts for
  credential/token patterns (SREs paste logs and configs during interviews).
- **Environment check.** Flows assume cwd is (or contains) the KB repo
  checkout; otherwise ask for the path — never guess.

## Testing & acceptance

- **Dogfood:** run the seed interview against the maintainer's Cloud Native
  Ref platform, produce real entries, verify RunLore's recall surfaces them
  (an entry that never recalls is a failed entry).
- **Skill verification** per superpowers:writing-skills — a cold subagent runs
  each flow; outputs must pass entry-quality-checklist.md. (Done for the
  post-incident flow: a fresh agent produced a gate-passing Incident entry on
  a `kb-steward/*` branch and recorded two unknowns as unknowns rather than
  inventing them.)
- **CI:** the drift-guard Go test; JSON validation of the marketplace/plugin
  manifests; the portability neutrality guard.

**v1 acceptance:** a fresh user installs in two commands, seeds ≥5
recall-firing entries in one sitting, and captures one post-incident entry
that passes the loader.

## Out of scope

- Live incident diagnosis from the terminal (RunLore's job; revisit only if
  real demand appears).
- Deepening the triage/maintenance flows beyond checklists (wait for use).
- Per-harness packaging beyond documentation (no Cursor/Codex/Gemini plugin
  manifests) and MCP prompts — see Portability for why both were rejected.
- Any `lore` CLI involvement in skill install (`go:embed` ruled out — most
  Claude Code users won't have the binary locally).
