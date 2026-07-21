# OKF entry format — what RunLore actually parses

RunLore's catalog loader reads every `*.md` file in the KB repo (recursively),
splits YAML frontmatter from the markdown body, and indexes the result for
recall during investigations. This file is the contract: what the loader
parses, what the merge gate enforces, and what recall rewards.

## Parsed frontmatter fields

The loader parses exactly these fields (anything else is tolerated and
ignored — OKF consumers must accept unknown keys):

<!-- parsed-fields:start -->
`type` · `title` · `description` · `resource` · `alert_resource` · `tags` ·
`timestamp` · `fingerprint` · `status` · `last_validated`
<!-- parsed-fields:end -->

| Field | Required | Why it matters |
|---|---|---|
| `type` | yes (gate) | `Incident`, `Playbook`, or `Concept` (capitalized). Decides the write directory and body requirements. |
| `title` | yes (gate) | Single line, ≤120 chars. Not indexed as its own search field — its text is the first element of the single corpus (title + description + resource + tags + body) that backs both BM25 and the embedding, so it carries full recall weight but no separate field or boost. Make it the scoped symptom ("KubeContainerOOMKilled for oom-app"), never a vague theme ("OOM issues"). |
| `description` | yes (gate) | One or two sentences carrying the words an alert or a query would contain — prime recall signal. |
| `resource` | Incident: yes (gate) | `namespace/name` of the affected workload, no whitespace. Drives recall's structural workload filter. Omit only deliberately, for platform-wide knowledge (the "scopeless" tier). |
| `alert_resource` | no | Set when the alert fired on a different resource than the fault (symptom pod vs faulty config). An additional way for recall to match, never a replacement. |
| `tags` | no (warned if empty) | Include the workload kind and namespace at minimum; reuse the platform's tag vocabulary. Lexical + vector signal, not a hard filter. |
| `timestamp` | no | RFC3339 or `YYYY-MM-DD`. Fallback freshness date when `last_validated` is absent. |
| `fingerprint` | no | Opaque dedup identity on agent-drafted entries. Omit on hand-written entries. |
| `status` | no | `active` (or absent) / `retired` / `draft`. `retired` and `draft` cannot fire recall — they are excluded from both instant recall and the near-miss lead. They remain *searchable*: an agent's in-investigation `kb_search` still returns them, with the status shown, so it can see the state and judge. Anything other than those two counts as active. Retire — don't delete — entries that no longer apply. |
| `last_validated` | no | Date a human last confirmed the entry works. Older than the deployment's `catalog.instant_recall.stale_after` ⇒ confidence down-weighted (×0.75), never rejected. Set it to today whenever you author or revalidate. |

Agent-drafted entries may also carry extension fields such as `confidence` or
`provenance`. They are legal OKF (unknown keys are ignored) but the loader
does not parse them — never rely on them for recall.

## Files & directories

- Write entries under the type directory: `incidents/`, `playbooks/`,
  `concepts/` (a write-side convention matching RunLore's own PRs — the
  loader itself reads every directory).
- Filename: `<kebab-title-slug>-<short-suffix>.md`. RunLore uses the first 8
  fingerprint chars as suffix; for hand-written entries any short stable
  suffix works (e.g. the date `20260721`). The suffix keeps two entries
  sharing a title from colliding.
- Reserved names the loader SKIPS: `index.md`, `log.md`, any `readme.md`,
  dotfiles, and hidden directories. Never put knowledge in those files.

## Body requirements (merge gate)

- Body must be non-empty for every type.
- **Incident** bodies must contain `## Symptom`, `## Cause`, and
  `## Resolution` sections; `## Investigate` (how to confirm this entry
  applies) is strongly recommended.
- Recall indexes ONE text corpus per entry: title + description + resource +
  tags + body. Everything you want the entry found by must appear in one of
  those.

## Example

```markdown
---
type: Incident
title: KubeContainerOOMKilled for oom-app
description: Container 'hog' is OOMKilled because its memory limit (100Mi) is below actual usage.
resource: shop-prod/oom-app
tags: [deployment, shop-prod, oomkilled, memory]
timestamp: "2026-07-03T09:14:00Z"
status: active
last_validated: "2026-07-21"
---

## Symptom
`KubeContainerOOMKilled` fires for pod oom-app-*; container restarts with reason OOMKilled.

## Cause
Memory limit 100Mi is below the working set (~180Mi) after the v2 image bump.

## Investigate
`kubectl -n shop-prod describe pod -l app=oom-app` → last state OOMKilled;
compare `container_memory_working_set_bytes` against the limit.

## Resolution
Raise the limit to 256Mi in the HelmRelease values (gitops repo:
`apps/shop/oom-app/values.yaml`), reconcile, confirm restarts stop.
```
