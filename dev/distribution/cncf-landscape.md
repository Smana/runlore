# cncf/landscape

**Status: blocked. Do not submit yet.**

## The blocker

The landscape has one stated maturity gate, and RunLore does not meet it.

From [README.md § New entries](https://github.com/cncf/landscape/blob/master/README.md#new-entries):

> Cloud native projects with **at least 300 GitHub stars** that clearly fit in an
> existing category are generally included.

The PR template makes you assert it:

```markdown
* [ ] Is your project closed source or, if it is open source, does your project have at least 300 GitHub stars?
```

`Smana/runlore` currently has **13 stars** (repo created 2026-06-20). A submission today
would be declined on the box the author has to tick. Stars are the *only* documented
gate — there is no rule about project age or adoption count.

**Re-check before submitting:**

```bash
gh repo view Smana/runlore --json stargazerCount -q .stargazerCount
```

Everything below is ready for the day that number clears 300.

## Where it goes

Category **`Observability and Analysis`** → subcategory **`Observability`**.

That is where every comparable tool already sits, verified by direct lookup in
`landscape.yml`: HolmesGPT (line 11432), K8sGPT (11546), Keep (11562), Botkube (11016),
Gonzo (11305).

The README discourages multi-listing ("we generally will only list a company's product in
one box"), so do **not** also request `AI Agent / Agent Framework` via `second_path`.

Note the default branch is **`master`**, not `main`.

## The entry

Insert into `landscape.yml` under that subcategory, **in alphabetical order** — this is
actively enforced; the CNCF CTO has asked for it on review as recently as
[PR #4998](https://github.com/cncf/landscape/pull/4998).

Indentation is exact: `          - item:` is 10 spaces, fields are at 12.

```yaml
          - item:
            name: RunLore
            description: RunLore is a self-hosted SRE agent that investigates Kubernetes and GitOps incidents against the exact rendered-manifest diff Flux or Argo CD reconciled, and curates each verified root cause into a PR-reviewed Git knowledge base the operator owns.
            homepage_url: https://runlore.io
            repo_url: https://github.com/Smana/runlore
            logo: runlore.svg
            license: Apache-2.0
```

Only `name`, `homepage_url` and `logo` are required by the enforced
[JSON Schema](https://github.com/cncf/landscape2/blob/main/docs/config/schema/data.schema.json);
the rest are optional and worth supplying. `license` must be a valid SPDX identifier.

### Deliberately omitted

- **`crunchbase`** — optional under `landscape2`, despite older guidance saying otherwise.
  207 of 2,410 items omit it, including
  [kubara](https://github.com/cncf/landscape/pull/4992), a non-CNCF, non-member OSS
  project merged 2026-07-28. Do **not** point it at the CNCF's own Crunchbase org — that
  pattern is for CNCF-affiliated (sandbox/incubating/graduated) projects, which RunLore
  is not.
- **`project:`** — this field marks CNCF project maturity (`sandbox`, `incubating`,
  `graduated`). RunLore is not a CNCF project. Omit it.
- **`extra:`** — every documented `extra.*` key (`lfx_slug`, `clomonitor_name`,
  `dev_stats_url`, `artwork_url`, `accepted`) presupposes CNCF affiliation.
- **`twitter`** — only if an account actually exists.

> `extra.ai: true` appears on 7 items (HolmesGPT, K8sGPT, Kubeflow, and others) and would
> plausibly apply here. It is **not in the schema and not in the docs** — it passes
> validation only because `additionalProperties` is unset. Its semantics are unknown, so
> it is left out rather than cargo-culted.

## The logo

Ships as [`runlore.svg`](runlore.svg). Copy to `hosted_logos/runlore.svg` in the
landscape repo — the `logo:` field takes a **bare filename, never a URL**.

Documented requirements, all satisfied:

| Requirement | Status |
|---|---|
| SVG format | ✅ |
| Includes the project name in English | ✅ "runlore" |
| Not reversed (no non-white, non-transparent background) | ✅ transparent |
| Stacked, not horizontal, when variants exist | ✅ mark above wordmark |
| Lives in `hosted_logos/` | ✅ on copy |

Text is converted to outlines. This rule is documented only in the **legacy**
[`cncf/landscapeapp`](https://raw.githubusercontent.com/cncf/landscapeapp/master/README.md)
README — the generator `cncf/landscape` replaced in 2024 — and is no longer mechanically
enforced (`landscape2`'s logo handling only strips `<title>` and recomputes `viewBox`; the
validate action does not inspect logos at all). It is still the right thing to do: a
text-based SVG yields a wrong computed bounding box and renders in whatever font the
viewer happens to have.

In RunLore's case that is not theoretical — see [README.md](README.md) for why the shipped
`assets/logo-wordmark.svg` silently renders in Noto Sans.

> **Unverified:** no maximum file size, pixel dimensions, or aspect ratio is stated
> anywhere in the repo, PR template, schema, or generator source. If you see such a figure
> quoted elsewhere, treat it as folklore.

## Submitting, when eligible

1. Confirm ≥300 stars.
2. Fork `cncf/landscape`, branch from **`master`**.
3. Add `hosted_logos/runlore.svg`.
4. Insert the YAML block above, alphabetically within
   `Observability and Analysis / Observability`.
5. PR title: `Add RunLore to Observability and Analysis / Observability`.
6. Complete the 7-item checklist in the PR template honestly.
7. A bot posts a preview link; check the rendered card and comment `LGTM`.

The landscape regenerates daily, so the entry appears within 24 hours of merge.
