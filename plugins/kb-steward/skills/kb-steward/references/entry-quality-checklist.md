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

If the `lore` binary is on PATH, run it after writing entries and before
committing — it is the same structural check the gate block below restates, so
its verdict beats any self-review:

```
lore validate-kb <catalog-dir>
```

It walks the whole catalog and exits non-zero when any structural error is
found. Two things to expect: it validates *every* entry, so pre-existing
failures in entries you didn't touch may surface — report those, don't
silently fix them; and `--semantic` (an LLM advisory) needs a configured
model, so plain `validate-kb` is what you want.

No binary available? Then the gate block below is the fallback — check it by
hand. Expect that often: the skill runs wherever the SRE is, which is not
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
