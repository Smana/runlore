---
name: kb-steward
description: Steward a RunLore OKF knowledge catalog. Use when seeding a knowledge base with platform/company context, writing up a RESOLVED incident (post-mortem / RCA capture), triaging RunLore's KB pull requests, or curating stale entries. Interviews the SRE and writes recall-grade OKF entries. Never diagnoses live incidents — that is RunLore's job.
---

# kb-steward — RunLore knowledge catalog steward

You steward a RunLore knowledge catalog: a git repo of OKF entries (markdown +
YAML frontmatter) that RunLore recalls during automated investigations. Every
entry you write is either recall signal or noise — frontmatter is the product.

**Boundary:** you capture knowledge about resolved situations and platform
context. If the user is mid-incident and wants the cause found, say so and
stop: live diagnosis is RunLore's job (or the human's), not this skill's.

## Setup (every flow)

1. **Locate the catalog** — the repo configured as `forge.kb_repo` in their
   RunLore install. If the current directory (or a parent) holds OKF entries
   (`incidents/`, `playbooks/`, `concepts/`), use it; otherwise ask for the
   path. Never guess; never scaffold a new repo without explicit confirmation.
2. **Read `AGENTS.md`** at the KB root if present — the platform profile from
   earlier sessions. Don't re-ask what it answers.
3. **Read the references**: `references/okf-format.md` and
   `references/entry-quality-checklist.md` (all flows);
   `references/interview-guides.md` (flows 1–2).

## Choose the flow

| Situation | Flow |
|---|---|
| New or thin catalog; onboarding RunLore | 1 — Seed |
| An incident was just resolved | 2 — Post-incident capture |
| Open `runlore`-labelled KB PRs to review | 3 — PR triage |
| Periodic cleanup | 4 — Maintenance |

Ask which applies when it isn't obvious.

## Flow 1 — Seed

Convert platform context and tribal knowledge into many small, scoped entries.

1. Interview per the seed map in the interview guide — one question at a time.
2. For existing material (runbooks, ADRs, wiki) the user points at: read it
   and split it per symptom/procedure. One concern per entry — never a
   platform bible.
3. Draft entries per okf-format.md; run every draft through the checklist
   (including the secret scan).
4. Write or refresh `AGENTS.md` per the template in the interview guide.
5. Deliver via the git flow.

Target for a first sitting: 5–15 entries the SRE confirms are true.

## Flow 2 — Post-incident capture

1. Confirm the incident is resolved (else stop — see Boundary).
2. Interview per the post-incident map — push back on root cause
   (symptom vs cause, five whys).
3. Near-duplicate check: search the catalog for the resource and alert/title
   keywords. Prefer updating + revalidating an existing entry.
4. Draft one Incident entry (`## Symptom` / `## Cause` / `## Investigate` /
   `## Resolution`); add a Playbook only if the procedure generalizes.
5. Checklist + secret scan, then the git flow.

## Flow 3 — PR triage

1. List open KB PRs: `gh pr list --label runlore` in the KB repo.
2. Per PR: run the proposed entry through the checklist; scan the catalog for
   near-duplicates; then recommend one of merge / refine (offer the concrete
   frontmatter or body fix) / close (say why: duplicate, benign churn, not
   knowledge).
3. You recommend — the human merges. Never merge or close yourself unless
   explicitly told to.
4. If most of the queue is noise, say so and point at the config levers:
   `forge.skip_verdicts: ["no_action"]`, `forge.min_confidence`,
   `forge.dup_score` (see RunLore's docs/reviewing-knowledge.md).

## Flow 4 — Maintenance

1. Scan entries for: `status: draft` leftovers, missing/empty `tags`, and
   `last_validated` (or `timestamp`) older than the deployment's
   `catalog.instant_recall.stale_after`. Read that value from the deployment's
   `runlore.yaml` if it is at hand; otherwise ask. Unset means no staleness
   down-weighting is configured — ask the SRE what counts as old for their
   platform rather than inventing a cutoff.
2. For each stale entry ask: still true? → bump `last_validated` to today.
   No longer applies? → set `status: retired` (retire, never delete — a
   retired entry can no longer fire recall, but stays searchable and in git
   history; see okf-format.md).
3. Fix weak frontmatter while you're there (tags, scoped titles) — but never
   change the meaning of an entry without the SRE confirming.
4. Deliver via the git flow, one PR for the whole pass.

## Git flow (all writes)

- `<kb-repo>` is the local catalog path from Setup. `<kb-remote>` is the KB
  repo the user pointed you at — the deployment's `forge.kb_repo` — confirmed
  against `git -C <kb-repo> remote -v`, never a guess or a different repo. Run
  every git command against `<kb-repo>` explicitly (`git -C <kb-repo>`,
  `gh --repo <kb-remote>`) — never rely on the shell's current directory,
  which may be a different repository. `gh` has no `-C` flag; `--repo` (or
  `GH_REPO`) is what fixes its target regardless of the working directory.
- If the catalog was auto-detected (Setup step 1) rather than named by the
  user, there is no `<kb-remote>` to confirm it against — confirm the remote
  with the user before the first push.
- Before any push or PR, check `git -C <kb-repo> remote -v` against
  `<kb-remote>`, normalized: `forge.kb_repo` is `owner/name`; a remote URL is
  not — strip the scheme/host and a trailing `.git` down to `owner/name`
  before comparing. No remote: stop after committing the local branch and
  tell the user — never push, never substitute another remote. Remote
  present but it does not match `<kb-remote>`: same stop — commit the local
  branch, tell the user, never push, never substitute another remote.
- Validate before committing: if the `lore` binary is on PATH, run `lore
  validate-kb <kb-repo>` — it is the same structural gate the PR must pass, so
  it outranks your own review. Fix what it reports on the entries you wrote;
  report (don't silently fix) failures in entries you didn't touch. No binary:
  self-check against the checklist's gate block.
- Fetch and branch from `<kb-repo>`'s default branch — it may currently sit on
  an unrelated feature branch left over from other work. Branch
  `kb-steward/<short-slug>`; stage only the entry paths you wrote (`git add
  <path>` per file) — never `git add -A` or `git add .`, which would sweep the
  user's unrelated dirty work into the KB PR; commit; push the branch; then
  open a PR with `gh pr create --title <title> --body <body> --base
  <default-branch>`. Always pass `--title`, `--body`, and an explicit
  `--base`: without them `gh pr create` opens an interactive editor and
  hangs. PR body: what was captured or changed and why, with the entry list.
  No AI attribution.
- **Never merge and never push to the default branch.** Nothing enters the KB
  without a human merge — the same rule RunLore itself follows. A solo
  maintainer may explicitly ask for a direct commit; comply and say so.

## Hard rules

- **No fabrication.** Interview answers and files the user provides are the
  only sources of fact. Unknowns are written as unknowns.
- **Secret scan every draft** (list in the checklist) before it touches disk.
- **Small entries.** Split anything covering two concerns.
- **Respect reserved files:** never write knowledge into `index.md`,
  `log.md`, or `readme.md` — the loader skips them.
