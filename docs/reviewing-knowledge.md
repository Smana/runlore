# Reviewing & approving RunLore's knowledge

> **Your part of the learning loop.** RunLore investigates incidents and proposes
> what it learned as a **pull request to your knowledge-base repo**. You are the
> editor-in-chief: you review the finding, refine it, and decide whether it
> becomes permanent, recall-able knowledge. This page is everything you do.

RunLore never writes to the catalog directly — it only opens PRs. Nothing enters
your knowledge base until **a human merges it**. That's the whole point: the
knowledge is yours, reviewed, and in your Git.

---

## 1. What you see when RunLore finds something

Two things happen when an investigation produces a confident, verified finding:

1. **A chat message** (Slack/Matrix) — the ranked root cause with confidence, the
   evidence trail, suggested next steps, and a **link to the KB pull request**.
   It's a notification, not where you act.
2. **A pull request** on your KB repo (`forge.kb_repo`), labelled
   `runlore` + `triggered` ("raw finding, not yet worked"). **This PR is where
   you review and approve.**

> If a finding isn't confident/verified (e.g. the investigation was inconclusive),
> RunLore opens **no PR** — by design, it won't fabricate knowledge.

---

## Expected triage volume

Be honest with yourself about the queue this creates. In a 6-day pilot RunLore
drafted **29 PRs (~3.5/day)**, and **~72% were closed without merging** — not
because they were wrong, but because they weren't worth permanent knowledge:
benign infrastructure churn, synthetic test canaries, and near-duplicates of
entries the catalog already held. That's a real reviewer-fatigue risk: a queue
that's mostly noise trains people to stop looking.

Three `forge` knobs cut that volume *before* a PR is ever opened, so the queue you
see is closer to the ~28% worth keeping:

- **`skip_verdicts: ["no_action"]`** — the biggest lever. A `no_action` verdict is
  RunLore's own "benign / self-healed / synthetic; nothing to do" classification.
  Skipping it keeps those findings out of the review queue entirely while still
  notifying chat, so nobody's blind to them — they just don't become PRs.
- **`min_confidence`** (default `0.75`) — the quality bar. Findings the model
  isn't confident about stay chat-only, never a PR.
- **`dup_score`** (default `5.0`) — the dedup threshold. A finding that closely
  matches an existing catalog entry is coalesced (a comment on the open PR) or
  dropped, instead of drafting a near-duplicate.

None of these fabricate or hide knowledge — every skipped finding is still
delivered to chat. They only decide what's worth a *permanent, reviewed* entry.
Recommended starting point for a production install: `skip_verdicts: ["no_action"]`,
then tune `min_confidence` up if benign churn still leaks through. See
[configuration.md](configuration.md#forge--the-git-host-for-curation).

---

## 2. Anatomy of a proposed entry

Every PR adds one Markdown file (an [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog)
entry) under a type directory — `incidents/kubecontaineroomkilled-for-oom-app-pod-a3938440.md`
(title slug + a short fingerprint, so two incidents sharing a title never collide).
Here is a real one RunLore opened for an OOMKilled pod:

```markdown
---
type: Incident
title: KubeContainerOOMKilled for oom-app pod
description: The container 'hog' is OOMKilled because its memory limit (100Mi) is too low.
resource: runlore-test/oom-app
tags: [runlore, incident, pod, runlore-test]  # + workload kind & namespace — recall signal
timestamp: "2026-07-03T09:14:00Z"             # OKF-recommended last-change stamp
fingerprint: a393844afec8e3d1…                # deterministic identity (resource + cause)
confidence: 0.9                               # queryable extension fields (OKF: frontmatter
provenance: [flux/oom-app-values]             #   is for what you filter/index on)
---

## Decision
- **why keep:** … the memory limit is too low …
- **confidence:** 90%

## Symptom
KubeContainerOOMKilled for oom-app pod

Affected resource: Pod runlore-test/oom-app

## Investigate                              # the evidence trail (what it observed)
- pod_status: hog OOMKilled (exit 137); memory limit 100Mi …

## Cause                                    # ranked root causes, each with a %
1. **The memory limit of 100Mi is too low for the workload.** (90%)

## Resolution                               # suggested, reversible-first
- Increase the memory limit for the 'hog' container. (reversible=false)

## Unresolved                               # what it honestly couldn't determine
- (none)

## Citations                                # OKF §8: the causing-change provenance
[1] flux/oom-app-values
```

The **Decision card** makes the merge call quick; the **sections** make the entry
reusable knowledge for the next person (and for RunLore's instant recall).

The same PR also keeps the OKF bundle self-describing: the entry's link line is
added to your `index.md` (when the bundle has one) and a `**Creation**` record is
appended to `log.md` — all in the one diff you review.

---

### Related knowledge in the PR

Each drafted PR ends with a **Related knowledge** section: the closest existing
entries at draft time (linked, with their BM25 score and affected resource) and
— when the trigger has fired before — a `Trigger seen ×N` line pointing at the
previous entry. Use it to answer the two review questions cheaply: *is this a
duplicate of something merged?* and *does this incident keep coming back?*
Scores are corpus-relative hints, not a ranking guarantee.

---

## 3. Reviewing it — three checks

Formatting is already enforced for you (a CI check validates the structure — see
[KB-entry validation](../dev/superpowers/specs/2026-06-24-kb-entry-validation-design.md)).
Your job is the **judgment** the machine can't make:

1. **Is the cause real?** Does the `Investigate` evidence actually support the
   `Cause`? (The evidence is quoted verbatim from live cluster/cloud/Git state.)
2. **Does the cause explain *this* symptom?** A confident, well-formed entry can
   still describe a *related* problem rather than the one that fired. Check the
   top cause answers the `Symptom`, not just "something nearby is wrong".
3. **Is it durable, generalizable knowledge — or transient noise?** A
   self-resolving blip (a bootstrap race, a one-off capacity spike) is *not*
   worth a permanent entry, even at high confidence. Keep entries that will help
   the *next* incident.

---

## 4. Add context / refine it

The PR is just Markdown — **edit it like any other PR**:

- Sharpen the `Cause`, add a `Resolution` step, link a runbook or dashboard,
  correct the `title`, add `tags`.
- Add operational context RunLore didn't have ("this recurs after every node
  rotation", "owned by the data team").
- Your edits **are** the knowledge — they're what future recalls return.

(Prefer a quick comment over an edit? Comment for discussion, but only the merged
file content is indexed.)

---

## 5. Approve or reject

- **Approve = merge the PR.** The entry is committed to your catalog, re-indexed
  within minutes, and becomes **instant-recall-able** for every future matching
  incident (RunLore answers in milliseconds instead of re-investigating). Add the
  `accepted` label if you want an explicit audit trail.
- **Reject = close the PR.** Use this for transient noise, a wrong/duplicate
  finding, or knowledge you don't want to keep. Nothing is lost — RunLore will
  re-propose if the incident genuinely recurs.

That single merge/close decision **is** "validating a KB entry". There is no
separate approval UI — your Git review *is* the gate.

---

## 6. What happens after you merge

- **Instant recall.** On the next matching incident, RunLore recalls the entry
  (after confirming it against current state and re-verifying it) instead of
  running a full investigation — saving minutes and tokens.
- **Outcomes feed back.** RunLore records whether incidents that recalled this
  entry actually resolved. An entry with a poor real-world resolve-rate is
  **automatically decayed out of recall** — so bad knowledge stops being used
  without anyone policing it. (See [the learning loop](learning-loop.md).)

---

## 7. Label reference

| Label | Meaning | Set by |
|---|---|---|
| `runlore` | Opened by RunLore | RunLore |
| `triggered` | Raw finding, not yet reviewed | RunLore |
| `investigating` | A human is working it | you |
| `accepted` | Reviewed & merged knowledge | you (on merge) |
| `solved` / `ready-to-merge` | The incident resolved; promote on review | you / the groomer |
| `knowledge-gap` | A pattern recurs but RunLore couldn't solve it — needs a human-authored entry | the groomer |

## 8. Optional: the backlog groomer

`lore curate` (run as a scheduled Job) keeps the PR backlog tidy without you:
it **dedups** near-identical PRs, **closes stale** unreviewed ones after a
configurable age, **promotes** `solved`→`ready-to-merge` when the incident
resolved, and opens a **`knowledge-gap`** issue when an unsolved pattern recurs.
It only ever comments/labels/closes — it never merges. See
[getting-started](getting-started.md#step-7--the-learn-loop-kb-lifecycle--re-runs).

---

**In one sentence:** RunLore drafts; CI checks the format; **you** check that the
cause is real, explains the symptom, and is worth keeping — then **merge to teach
it, or close to skip it.**
