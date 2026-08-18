---
title: GitHub
weight: 501
integration: {kind: forge, id: github}
---

**What it gives you** — the Learn loop: verified root causes drafted as PRs (or issues, below the
confidence bar) against your knowledge-catalog repo, via a scoped GitHub App.

## Minimal config

```yaml
forge:
  kb_repo: your-org/runlore-kb            # the repo the App is installed on
  base_branch: main
  skip_verdicts: [no_action]              # keep benign/self-healed findings out of the PR queue
  github_app:
    app_id: 123456
    installation_id: 7654321
    private_key_env: GITHUB_APP_PRIVATE_KEY
```

### Create the App

1. **Settings → Developer settings → GitHub Apps → New GitHub App.** Homepage URL: anything (e.g.
   your repo). Disable Webhooks (RunLore doesn't receive GitHub webhooks).
2. **Repository permissions** (least privilege — grant only these):

   | Permission | Access | Why |
   |---|---|---|
   | Contents | **Read & write** | push the drafted OKF entry to a branch on the KB repo |
   | Pull requests | **Read & write** | open the curation PR |
   | Issues | **Read & write** | open knowledge-gap issues for recurring unresolved patterns |

   If your Flux/Argo source repos are **private** and you want real Git diffs from them
   (`source_repos.allow`), also grant **Contents: Read-only** and install the App on those repos too.
   Public source repos need nothing.
3. **Create**, then **Generate a private key** — download the `.pem` (you only see it once).
4. Note the **App ID** (on the App's page).
5. **Install App** → install it on **only the specific repos** it needs (the KB repo, plus any private
   source repos) — *not* "All repositories". Open the installation and note the **Installation ID**
   (the number in the install settings URL: `.../installations/<id>`).

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-file=GITHUB_APP_PRIVATE_KEY=/path/to/app-private-key.pem
```

## Verify it locally

```bash
kubectl -n runlore logs deploy/runlore | grep 'curator enabled'
```

Fire a test incident that produces a verified root cause and confirm a PR/issue lands on the KB repo:

```bash
kubectl -n runlore logs deploy/runlore | grep 'msg=curated url='
```

Check the App's installation scope directly:

```bash
gh api /installation/repositories
```

## Notes

- Auth is a **GitHub App** — fine-grained permissions, per-repo installation, and short-lived (1-hour)
  installation tokens minted on demand from the App's private key. No long-lived credential ever sits
  in the cluster.
- **Never commit the private key** or put it in `values.yaml`. Store it in a `Secret`, ideally synced
  from a vault via External Secrets.
- **Scope the installation** to specific repos, and grant only the three write permissions above.
  Restrict who can administer the App in your org. Rotate the private key periodically anyway.
- A **private** KB repo (via `catalog.git`) authenticates with the **same curation GitHub App** by
  default — one credential for both reads and writes; set `git.token_env` only to use a different
  token.
- `source_diff` clones source repos over HTTPS with the **forge GitHub App installation token** — for
  each private GitHub source repo you list under `source_repos.allow`, the App must be installed on
  that repo with `contents: read`, or the clone 404s even though the URL looks right. Public GitHub
  repos need no grant.
- `github_api_url` is only needed for **GitHub Enterprise Server** (e.g.
  `https://ghe.example.com/api/v3`); omit it for github.com.
- **The installation token is only ever sent to your forge's git host.** `what_changed` clones the
  repo a GitOps `spec.source.repoURL` names, and that field is cluster state — anyone who can create
  an Argo CD `Application` or a Flux `GitRepository` writes it. A repoURL on any other host therefore
  clones **anonymously** (public repos still work; a private one produces no diff). Nothing is lost by
  this: an installation token authenticates only on the instance the App is installed on.
- **GitHub Enterprise with [subdomain isolation](https://docs.github.com/en/enterprise-server/admin/configuring-settings/hardening-security-for-your-instance/enabling-subdomain-isolation)
  needs `forge.git_host`.** With isolation on, the API is served from `api.HOSTNAME` while git and web
  stay on `HOSTNAME`, so `github_api_url` alone cannot say which host to authenticate a clone to.
  RunLore refuses to start on that config rather than guess — guessing wrong would withhold the token
  from *every* GitOps repo and show up only as an empty `what_changed`:

  ```yaml
  forge:
    github_api_url: https://api.ghe.example.com   # API (subdomain isolation)
    git_host: ghe.example.com                     # git remotes / clone auth
  ```

  Without isolation (`https://ghe.example.com/api/v3`), the host is unambiguous and `git_host` is not
  needed.
- RunLore's writes are confined to the forge — it has **no cluster-mutating permissions**.

## Reference

- [Configuration → `forge`]({{< relref "/docs/configuration/configuration.md#forge--the-git-host-for-curation" >}})
  for the full key reference.
- [Data sources → Source repos]({{< relref "/docs/concepts/data-sources.md" >}}) — the App's role in
  `source_diff` auth.
- [Learning loop]({{< relref "/docs/concepts/learning-loop.md" >}}) — the full curate/triage lifecycle.
