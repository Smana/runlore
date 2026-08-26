---
title: Source repos
weight: 312
integration: {kind: source_repos, id: source_repos}
---

**What it gives you** — the `source_diff` tool: turns a manifest-layer change ("image `v1.2.2 →
v1.2.3`") into the actual code behind it — commit subjects, a per-file diffstat, and the largest
changed hunks — deepening a correlation into a cause.

## Minimal config

```yaml
source_repos:
  allow:
    - github.com/acme/*              # every repo directly under the org
    - gitlab.com/acme/infra-modules  # or exact host/org/repo
```

## Verify it locally

Fire a test incident whose root cause is a version bump and confirm `source_diff` was called:

```bash
kubectl -n runlore logs deploy/runlore | grep 'tool=source_diff'
```

## Notes

- **Unset (default) ⇒ the tool is not registered.** Only patterns you list are reachable — this
  allowlist is the security boundary, enforced **server-side before any network call**: the model can
  only make RunLore clone repos you explicitly listed. Patterns are `host/org/repo` with per-segment
  globs.
- **Auth — the forge GitHub App must be granted access to each private source repo.** `source_diff`
  clones over HTTPS using the **forge GitHub App installation token** (the same App as
  `forge.github_app`, see [GitHub]({{< relref "github.md" >}})), and that token attaches to *every*
  clone on the forge's host. For each **private GitHub** repo you list, the App must be **installed on
  that repo with `contents: read`** — otherwise the installation token is out of scope and the clone
  fails (`404`/`repository not found`), even though the URL looks right. Verify with
  `gh api /installation/repositories`, or grant it in the App's settings (**Repository access → Only
  select repositories → add the repo**). Keep that installation scope as tight as this allowlist.
- **Public GitHub repos need no grant** — an installation token can read any public repo regardless of
  scope.
- **Entries on a non-forge host** (e.g. `gitlab.com/...`) **clone anonymously** — the GitHub token is
  never sent off-host. Private non-GitHub hosts are not supported yet.
- **Read-only, no working-tree checkout.** Bare mirrors when the mirror cache is enabled (the
  default — reuses the `gitops.mirror` directory, under a `source/` subdir, so a warm mirror costs
  nothing after the first clone), or a no-checkout clone per call when the cache is disabled. Either
  way, nothing is ever written back to the source repo.
- A wrong repo guess fails at ref resolution (the tag won't exist), and the error response lists
  nearby tags so the model can self-correct instead of retrying blind.

## Reference

- [Configuration → `source_repos`]({{< relref "/docs/configuration/configuration.md#source_repos--source-repo-allowlist-for-source_diff" >}})
  for the full key reference and token-cost details.
- [GitHub]({{< relref "github.md" >}}) — the forge App whose installation token authenticates these
  clones.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
