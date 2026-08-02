---
title: GitLab
weight: 20
integration: {kind: forge, id: gitlab}
---

**What it gives you** — the Learn loop on a self-hosted or gitlab.com project: verified root causes
drafted as merge requests against your knowledge-catalog repo, via a scoped project or group access
token. This is what lets a self-hosted GitLab team — the archetype of RunLore's lock-in-averse,
sovereignty-conscious audience — use the curation loop at all.

## Minimal config

```yaml
forge:
  provider: gitlab          # github (default) | gitlab
  kb_repo: your-group/runlore-kb
  base_branch: main
  gitlab:
    base_url: https://gitlab.example.com   # omit for gitlab.com
    token_env: GITLAB_TOKEN
```

### Create the token

GitLab has no GitHub-App equivalent bot identity — OAuth apps are the wrong shape for a headless
curator, so auth is a **project or group access token**, sent as the `PRIVATE-TOKEN` header on every
request.

1. **Project → Settings → Access Tokens** (or **Group → Settings → Access Tokens** to scope one token
   across every project group-wide).
2. **Role: Developer** at minimum — creating a branch, committing the drafted entry, and opening a
   merge request all need it. Give **Maintainer** only if `base_branch` is a protected branch your
   project requires a higher role to push feature branches against.
3. **Scope: `api`** — the full API scope; GitLab has no finer-grained scope that still covers
   creating commits, merge requests, and labels.
4. Set an **expiration date** and rotate before it lapses — an expired token fails closed (every
   forge call 401s), it does not silently disable curation.
5. Copy the token once (GitLab shows it only at creation time) and store it as a Secret:

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=GITLAB_TOKEN=glpat-xxxxxxxxxxxxxxxxxxxx
```

## Verify it locally

```bash
kubectl -n runlore logs deploy/runlore | grep 'curator enabled'
```

Fire a test incident that produces a verified root cause and confirm a merge request lands on the KB
project:

```bash
kubectl -n runlore logs deploy/runlore | grep 'msg=curated url='
```

Check the token's effective scope directly against the API (a 404 here — rather than a permissions
error — is almost always the URL-encoding gotcha below, not a scope problem):

```bash
curl --header "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  "https://gitlab.example.com/api/v4/projects/your-group%2Frunlore-kb"
```

## Notes

- **`kb_repo` is a project PATH, not a numeric ID** (`group/project`, or a nested
  `group/subgroup/project`) — RunLore URL-encodes it into the API path (`group%2Fproject`) on every
  call, per the GitLab v4 API's own requirement. Getting this encoding wrong is the single most
  common GitLab-client bug, and it 404s in a way that reads exactly like a permissions problem, so
  check the encoding first if merge requests never appear.
- **`base_url` is the instance root**, e.g. `https://gitlab.example.com` — RunLore appends `/api/v4`
  itself. Omit it entirely for gitlab.com.
- Auth is a **static credential** (unlike GitHub's short-lived, minted App installation tokens) —
  referenced by env-var indirection only (`token_env` names the variable; the config file never holds
  the token itself). **Never commit the token or put it in `values.yaml`.** Store it in a `Secret`,
  ideally synced from a vault via External Secrets, and rotate it periodically.
- `provider: gitlab` with no `forge.gitlab.token_env`, or a `kb_repo` that isn't a valid GitLab
  project path, both **fail config load closed** — `serve` refuses to start rather than coming up
  with curation silently disabled.
- TLS verification is always on; there is no `insecure_skip_verify` escape hatch. A self-signed
  instance needs a certificate your cluster's trust store already accepts.
- Tested against **GitLab 16.x and 17.x** (self-managed) and gitlab.com; the API surface RunLore uses
  (Commits, Merge Requests, Issues, Notes) has been stable across the v4 API for years, so earlier
  16.x/15.x installs are likely fine too, just not part of the tested matrix.
- RunLore's writes are confined to the forge — it has **no cluster-mutating permissions**.

## Reference

- [Configuration → `forge`]({{< relref "/docs/configuration/configuration.md#forge--the-git-host-for-curation" >}})
  for the full key reference.
- [Learning loop]({{< relref "/docs/concepts/learning-loop.md" >}}) — the full curate/triage lifecycle.
- [GitHub]({{< relref "/docs/integrations/github.md" >}}) — the other supported forge, for a
  side-by-side comparison of the two auth models.
