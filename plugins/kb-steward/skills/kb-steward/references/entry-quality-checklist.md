# Entry quality checklist

Run every draft through this list before writing it to the repo. The first
block mirrors RunLore's merge gate (failures block the PR); the second is
what separates a recall-able entry from noise.

Field-by-field contract (source of truth for what each field means and
requires): `references/okf-format.md`. The gate items below restate only
what's worth a quick self-review pass.

## Gate (must pass — RunLore validates these on merge)

- [ ] `type` is `Incident`, `Playbook`, or `Concept`
- [ ] `title` is a single line, ≤120 chars
- [ ] `description` present
- [ ] `resource` present for Incident (`namespace/name`)
- [ ] if `resource` is set at all (any type), it has no whitespace
- [ ] body non-empty; Incident body has `## Symptom`, `## Cause`, `## Resolution`,
      each with actual content — present but empty also fails the gate

## Recall strength (the actual point)

- [ ] title scoped to one symptom/procedure, not a vague theme (editorial —
      not gate-enforced, but the gate only checks length, not content)
- [ ] description contains the words an alert or on-call query would use
      (alert name, error string, resource kind)
- [ ] tags non-empty: workload kind + namespace + platform vocabulary
- [ ] one concern per entry — split anything covering two symptoms/procedures
- [ ] `## Investigate` section tells a stranger how to confirm the entry applies
- [ ] `last_validated` set to today
- [ ] near-duplicate check done: searched the catalog for the resource and
      the alert/title keywords; preferred updating an existing entry over
      adding a twin
- [ ] scopeless (no `resource`) only for genuinely platform-wide knowledge

## Secret scan (always, before writing the file)

Search the draft for and redact:

- private key blocks (`-----BEGIN … PRIVATE KEY-----`)
- cloud credentials (`AKIA…`, `aws_secret_access_key`, service-account JSON)
- tokens/headers (`Authorization:`, `Bearer `, `xox[a-z]-`, `ghp_`, `glpat-`)
- `password=`, `token=`, `secret=` values in URLs, env dumps, or configs
- kubeconfig blobs and base64 strings longer than ~40 chars of unclear origin

Replace with `<redacted>`. If a secret already reached git or chat via the
user's paste, tell them so they can rotate it.

## Unknowns

Write `unknown` rather than a plausible guess. A wrong "cause" recalled with
confidence during a future incident is worse than a gap.
