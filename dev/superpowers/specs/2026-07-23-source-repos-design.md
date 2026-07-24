# Source-repo whitelist: `source_diff` tool — design

**Date:** 2026-07-23
**Status:** approved (brainstorming session with Smaine)
**Scope:** v1 — bounded investigation-time tool. Phase 2 (OCI-label auto-discovery) noted as future work, not designed here.

## Problem

RunLore's core differentiator is *"what changed?" → exact Git diff*, but today that stops at
the **manifest layer**. `what_changed` can report `image: v1.2.2 → v1.2.3` or a Terraform
module ref bump `v1.4.0 → v1.5.0` — it cannot see *inside* the bump. The root cause of a
deploy-correlated incident usually lives in the application or module source diff between
those two versions ("commit `a1b2c3` raised the DB pool size, which matches the connection
exhaustion"). Investigations currently end one level short of that.

## Non-goals (explicitly rejected during design)

- **Free repo exploration** (list/grep/read over whole repos): token cost, evidence-loop
  dilution. Rejected.
- **Pre-fetch enrichment** (inject recent commits into initial context): pays the token cost
  on every investigation, relevant or not. Rejected.
- **New trigger source** (watch repos, investigate on push): noise machine — most pushes are
  healthy. Rejected. The feature is **investigation-time only**.
- **Runbook/docs repos**: out of scope; the KB is the knowledge surface.
- **OCI-label auto-discovery** (`org.opencontainers.image.source` via registry lookup):
  deferred to phase 2, and only if pilot evidence shows the LLM mis-matching repos in
  practice. It would add a registry client, image-pull-secret handling, and a network
  dependency for what is a convenience.

## Design summary

An optional allowlist of source repos unlocks one new bounded LLM tool, `source_diff`, which
diffs a whitelisted repo between two refs the agent already found in evidence (image tags,
module refs). All git plumbing is reused from `whatchanged` (mirror cache, GitHub App auth,
go-git diff). No new required config; feature absent → tool absent (existing data-source
pattern).

## Config surface

```yaml
source_repos:          # absent → feature off, no tool registered
  allow:
    - github.com/acme/*              # glob on host/org/repo
    - gitlab.com/acme/infra-modules  # or exact
```

- No other required keys. Output caps (max commits listed, max hunk bytes) default **in
  code**; not exposed in v1 (simplicity constraint — add knobs only on demonstrated need).
- **Auth:** reuses the GitHub App installation `TokenSource` that `what_changed` already
  uses. Public repos need nothing; private GitHub repos need the app installed on them;
  private non-GitHub repos are a **documented v1 limitation**.
- **Mirrors:** reuse `gitops.mirror` (same dir, eviction, and clone-per-call fallback).

## Tool contract: `source_diff`

Registered only when `source_repos.allow` is non-empty. The tool description enumerates the
allowed patterns so the model knows what it may request. The loop system prompt gains one
nudge: *"if what_changed shows an image or module version bump and source_diff is available,
consider diffing the source between the two versions."*

**Args:**

| arg | type | notes |
|---|---|---|
| `repo` | string, required | must match the allowlist — validated server-side, never trusted |
| `from`, `to` | string, required | git refs (tags or SHAs) |
| `paths` | []string, optional | zoom: return full hunks for these files only |

**Repo matching is done by the LLM.** For infra modules the repo URL is literally present in
the GitOps diff, so it is exact. For app images the model name-matches image → repo from the
enumerated allowlist; a wrong guess dies at ref resolution (the tag will not exist), which is
the honesty backstop.

**Ref resolution:** try the ref as given, then with a `v` prefix (image tag `1.2.3` vs git
tag `v1.2.3` is the common mismatch). The response states the refs actually resolved.

**Default (summary-first) response — token-optimized:**

1. Resolved repo + refs.
2. Commit log: short SHA, subject, date — capped count (suggested default: 50, oldest
   dropped with a `… and N more` marker).
3. Diffstat for **all** changed files (nothing hidden).
4. Hunks for only the top most-changed **non-generated** files, within a byte cap
   (suggested default: 8 KiB).

**Zoom response** (`paths` set): full hunks for the requested files, byte-capped (suggested
default: 16 KiB). Suggested defaults are implementation-time tunable — sized against the
existing summary tools' output budget — but live in code, not config.

**Noise filtering:** generated/vendored content (`go.sum`, `package-lock.json`, `yarn.lock`,
`vendor/`, `*.pb.go`, `dist/`, other lockfiles) is excluded from *hunks* by default but stays
in the *diffstat*, annotated (e.g. `go.sum +180 -40 (hunks skipped: generated)`). This is
routinely 50–90% of a release diff's bytes.

**Truncation:** existing rune-safe truncate, with an explicit
`[truncated — use paths to zoom]` marker so the model knows zooming is available rather than
assuming it saw everything.

**Errors are recovery-oriented, never fatal to the loop:**

- Repo not in allowlist → error message states the allowed patterns.
- Ref not found → error lists a few tags sharing the same prefix so the agent can
  self-correct.
- Clone/auth/infra failure → tool error string; the investigation degrades to today's
  behavior. Mirror misbehavior already falls back to clone-per-call.

## Internals

- New package `internal/sourcerepo`: allowlist matcher + ref-fallback logic. Thin.
- New `SourceDiffTool` in `internal/investigate`, following the existing `Tool` interface
  pattern (`cloud_tools.go`, `logs_summary_tool.go` are the templates).
- Git heavy lifting (mirror acquire, incremental fetch, revision walk, go-git diff) is
  called through the existing `whatchanged` differ/mirror — **no new git code paths**.

## Security

- **The allowlist match is the security boundary.** Normalize the model-supplied URL (strip
  scheme and `.git`, lowercase host, reject path traversal), match against globs, reject
  *before any network call*. The model can only make RunLore touch repos the operator
  explicitly listed — this kills the SSRF/arbitrary-clone vector.
- Diff and commit text is **untrusted third-party content** (a commit author can write
  anything). It enters the model the same way GitOps diffs and pod logs already do, and
  flows through the existing egress-redaction path before any notification leaves. No new
  trust class.
- Read-only throughout: bare mirror, `NoCheckout`, no worktree ever materialized.

## Token budget integration

Output caps sized comparably to existing summary tools (`logs_error_summary`) so a
`source_diff` call does not distort the step/token budget machinery. The summary-first +
zoom contract means the common case is one cheap call; the expensive second call often never
happens.

## Testing

- Table-driven allowlist matcher tests: globs, normalization, bypass attempts
  (`github.com/acme/../evil`, scheme tricks, case games).
- Differ-level tests on a fixture repo: ref `v`-prefix fallback, caps, most-changed-first
  ordering, noise filtering, `paths` zoom.
- Tool-registration gating: no config → no tool.
- One eval scenario exercising image-bump → `source_diff` → root-cause-commit, wired into
  the hybrid eval gates.

## Docs

- `configuration.md`: new `source_repos` section (allowlist semantics, auth scope, v1
  limitations).
- `data-sources.md`: paragraph on source repos as a data source.
- README: one line in the "what changed?" pitch — this is a marketable depth increase
  ("…down to the offending commit").
- Explicit v1 limitations: GitHub-App auth only for private repos; LLM does the repo
  matching; phase-2 OCI-label discovery as future work.

## Value rationale (for the record)

The pitch is **targeted deepening of the existing differentiator**, not "more data = better
context" — unbounded context dilutes the evidence loop. This closes the single biggest depth
gap in the "what changed?" story and compounds the learning loop: KB entries that cite the
offending commit are far more actionable on recall.
