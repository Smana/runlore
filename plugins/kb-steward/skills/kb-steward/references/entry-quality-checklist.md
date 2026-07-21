# Entry quality checklist

Run every draft through this list before writing it to the repo. The first
block is what `lore validate-kb` enforces; the second is what separates a
recall-able entry from noise.

**How binding the first block is depends on the KB repo.** RunLore ships the
validator as a command, not as a merge hook — it blocks a PR only where the KB
repo runs `lore validate-kb` in CI. If nobody wired that up, a structurally
invalid entry merges fine and is merely warned about when the catalog loads,
then served anyway. So treat the block as a real gate, and if the KB repo has
no CI check, say so — wiring one is a bigger win than any single entry.

Field-by-field contract (source of truth for what each field means and
requires): `references/okf-format.md`. The gate items below restate only
what's worth a quick self-review pass.

## Run the real validator when you can

**Match the version CI runs, not whatever is at hand.** The KB repo's CI
workflow pins the validator (look for `go install …/cmd/lore@vX.Y.Z` under
`.github/workflows/`); a PATH binary or a source build of a different version
can green-light entries the pinned gate rejects, or fail entries the gate
accepts — validation rules have changed across releases (e.g. `resource` was
required on every type before it became Incident-only). Install exactly that
version:

```
GOBIN=/tmp go install github.com/Smana/runlore/cmd/lore@<pinned-version>
/tmp/lore validate-kb <catalog-dir>
```

It walks the whole catalog and exits non-zero when any structural error is
found. Two things to expect: it validates *every* entry, so pre-existing
failures in entries you didn't touch may surface — report those, don't
silently fix them; and `--semantic` (an LLM advisory) needs a configured
model, so plain `validate-kb` is what you want.

If the pinned version rejects an entry that current OKF semantics accept, the
pin is stale — propose a pin-bump PR on the KB repo rather than contorting
the entry to satisfy outdated rules.

No CI pin? Use a `lore` on PATH, or build from the RunLore source repo if it
is at hand (`go build -o /tmp/lore ./cmd/lore` from the repo root). No binary
and no source? Then the gate block below is the fallback — check it by hand.
Expect that often: the skill runs wherever the SRE is, which is not
necessarily next to a RunLore install.

## Gate (what `lore validate-kb` rejects)

- [ ] `type` is `Incident`, `Playbook`, or `Concept`
- [ ] `title` is a single line, ≤120 bytes (bytes, not characters — accents count double)
- [ ] `description` present
- [ ] `resource` present for Incident
- [ ] if `resource` is set at all (any type), it has no whitespace
- [ ] body non-empty; Incident body has `## Symptom`, `## Cause`, `## Resolution`,
      each with actual content — present but empty also fails the gate

## Recall strength (the actual point)

- [ ] title scoped to one symptom/procedure, not a vague theme (editorial —
      not gate-enforced, but the gate only checks length, not content)
- [ ] description contains the words an alert or on-call query would use
      (alert name, error string, resource kind)
- [ ] `resource` is shaped `namespace/name` — the gate only checks that it is
      present and whitespace-free, but recall's workload filter needs the shape
- [ ] tags non-empty: workload kind + namespace + platform vocabulary
- [ ] one concern per entry — split anything covering two symptoms/procedures
- [ ] `## Investigate` section tells a stranger how to confirm the entry applies
- [ ] `last_validated` set to today
- [ ] near-duplicate check done: searched the catalog for the resource and
      the alert/title keywords; preferred updating an existing entry over
      adding a twin
- [ ] scopeless (no `resource`) only for genuinely platform-wide knowledge

## Triaging agent-drafted entries (PR triage)

The two blocks above are written for authoring. When reviewing a RunLore-drafted
PR, the allowances and generator artifacts below apply.

Allowances — expected on RunLore drafts, not refine-blockers:

- `last_validated` absent: RunLore drafts don't set it — the human merge is
  the validation. Suggest adding it only when the PR needs refining anyway.
- `fingerprint` (parsed — RunLore's dedup identity; see okf-format.md),
  `confidence` / `provenance` (unparsed extension fields): all expected on
  drafts — leave them alone.
- The near-duplicate check is already satisfied by the triage flow's
  group-level dedupe — don't re-search the catalog per keeper.

Generator artifacts — include fixes for these in any refine recommendation:

- Title ending in `…` (the generator caps titles at the 120-byte gate).
  Rewrite to a scoped title that fits without the ellipsis — and single-quote
  the YAML value when the new title contains `: ` (an unquoted colon+space
  breaks frontmatter parsing, which fails the whole entry, not just the
  title).
- The description pasted verbatim into `## Decision` and `## Cause`. The
  repetition adds no recall signal (recall indexes one corpus per entry —
  okf-format.md); trim the body copies to what each section uniquely says.
- Required sections present but empty (a gate item above) — `## Resolution`
  especially, on drafts from RunLore builds older than the no-action fix
  (Smana/runlore#350; fixed drafts emit an explicit "No action suggested"
  line instead). A keeper needs real content, usually recoverable from the
  entry's own `## Unresolved` notes or the alert's runbook.

## Secret scan (always, before writing the file)

You are the only scanner in this path — RunLore redacts what *it* collects
during an investigation, but nothing filters what a human pastes into an
interview. Search the draft for and redact:

- private key blocks (`-----BEGIN … PRIVATE KEY-----`)
- JWTs (`eyJ….eyJ….…`) — very common in pasted request logs
- cloud credentials: `AKIA…`/`ASIA…`, `aws_secret_access_key`, Google `AIza…`
  and `ya29.…`, service-account JSON
- provider keys: `sk-…`, `sk_live_…`/`rk_live_…`, `xox[baprs]-…`,
  `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`, `github_pat_…`, `glpat-…`
- auth headers (`Authorization:`, `Bearer …`, `Basic …`)
- `password=`, `token=`, `secret=`, `api_key=`, `client_secret=`,
  `connection_string=` values in URLs, env dumps, or configs
- credentials embedded in URLs (`scheme://user:PASSWORD@host`)
- kubeconfig blobs and base64 strings longer than ~40 chars of unclear origin

(This mirrors the shapes RunLore's own redaction handles — `internal/redact`
in the RunLore repo is the fuller list if you need to check one.)

Replace with `<redacted>`. If a secret already reached git or chat via the
user's paste, tell them so they can rotate it.

## Unknowns

Write `unknown` rather than a plausible guess. A wrong "cause" recalled with
confidence during a future incident is worse than a gap.
