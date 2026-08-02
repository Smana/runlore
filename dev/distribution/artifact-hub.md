# Artifact Hub

**Status: ready. No maturity gate, no human review.** This is the only one of the three
that is actionable today — and it is still a public action under the maintainer's account,
so it is staged, not done.

RunLore's chart is published as an OCI artifact by
[`release-chart.yml`](../../.github/workflows/release-chart.yml):

```
oci://ghcr.io/smana/charts/runlore
```

## The one thing that trips people up

For a **classic HTTP Helm repo**, `artifacthub-repo.yml` is a file served next to
`index.yaml`.

For an **OCI repo there is no file path at all.** The metadata is pushed into the registry
as a separate OCI artifact under a reserved tag, `artifacthub.io`, alongside the chart
version tags. Artifact Hub pulls that tag and looks for a specific layer media type.

Confirmed three ways: the
[docs](https://artifacthub.io/docs/topics/repositories/helm-charts/), the source
(`internal/repo/manager.go` defines `MetadataLayerMediaType` and `artifacthubTag`), and by
inspecting a real verified-publisher chart in GHCR
(`ghcr.io/spegel-org/helm-charts/spegel:artifacthub.io`), whose manifest carries exactly
those media types with an empty config blob.

## Order of operations

`repositoryID` does not exist until after the repository is added, so the sequence is
fixed:

### 1. Add the repository

Sign in → **Control Panel** → **Repositories** → **Add**.

| Field | Value |
|---|---|
| Kind | `Helm` |
| Name | `runlore` — must match `^[a-z][a-z0-9-]*$`; becomes the URL slug |
| Display name | `RunLore` |
| Url | `oci://ghcr.io/smana/charts/runlore` |

The URL **must** follow `oci://registry/namespace/chart-name` and points at the *chart*,
not an org or a directory — Artifact Hub repositories are chart-scoped for OCI. Chart
version tags must be valid semver.

### 2. Copy the repository ID

Shown on the repository's card in the Repositories tab.

### 3. Write the metadata file

```yaml
# Artifact Hub repository metadata file
repositoryID: <UUID from step 2>
owners:
  - name: Smaine Kahlouch
    email: <the email registered with the Artifact Hub account>
```

The email **must** match the Artifact Hub sign-in email, or ownership verification fails.
`repositoryID` is optional in general but is what enables Verified Publisher.

### 4. Push it to the reserved tag

```bash
oras push \
  ghcr.io/smana/charts/runlore:artifacthub.io \
  --config /dev/null:application/vnd.cncf.artifacthub.config.v1+yaml \
  artifacthub-repo.yml:application/vnd.cncf.artifacthub.repository-metadata.layer.v1.yaml
```

Both media types are exact and load-bearing — Artifact Hub matches on the **layer media
type**, not on the filename annotation (real publishers use different local paths there).

### 5. Wait for reprocessing

The flag is set on the next processing run, and a repository is not reprocessed unless it
has changed.

> **Unverified:** the docs describe change-detection for HTTP-Helm (`index.yaml` changed)
> and git (last commit hash changed) only. **The OCI case is undocumented.** In practice,
> pushing a new chart version tag triggers reprocessing — so the pragmatic move is to do
> this before a release, or cut a patch release afterwards.

## Verified Publisher

Purely mechanical: no application, no review, no approval. Publishing an
`artifacthub-repo.yml` containing the correct `repositoryID` proves write access to the
artifact source, and the badge is granted on the next processing run.

Do not confuse it with **Official** status, which *is* a manual request (requires Verified
Publisher first, plus a `README.md` in every package, applied for by filing an issue).

## Chart.yaml annotations

Optional, and they materially improve the listing. Add under `annotations:` in
[`deploy/helm/runlore/Chart.yaml`](../../deploy/helm/runlore/Chart.yaml):

```yaml
annotations:
  artifacthub.io/category: monitoring-logging
  artifacthub.io/license: Apache-2.0
  artifacthub.io/links: |
    - name: Documentation
      url: https://runlore.io/docs/
    - name: support
      url: https://github.com/Smana/runlore/issues
```

`artifacthub.io/maintainers` is **deliberately absent**. It overrides display names by
matching on email — and `Chart.yaml`'s `maintainers:` entry currently carries a `name`
with no `email`, so the annotation would have nothing to match and would be ignored.
Adding an email to `Chart.yaml` first would be the prerequisite, and that is a separate
decision about publishing an address.

Notes on the exact values:

- **`category`** accepts only: `ai-machine-learning`, `database`, `integration-delivery`,
  `monitoring-logging`, `networking`, `security`, `storage`, `streaming-messaging`.
  `monitoring-logging` is the closest fit for an incident-investigation agent;
  `ai-machine-learning` is defensible but describes how it works rather than what it is
  for. Setting it explicitly suppresses Artifact Hub's ML category guesser, which is worth
  doing either way.
- A link literally named **`support`** renders as a highlighted "report an issue" button.
- `license` must be a valid SPDX identifier.
- Boolean-valued annotations (`prerelease`, `containsSecurityUpdates`, `operator`) are
  **quoted strings**, not YAML booleans. None apply here.
- `artifacthub.io/changes` powers a ChangeLog tab; it would need maintaining per release,
  so it is deliberately not proposed as part of the initial setup.

> **Known OCI limitation:** "force an existing version to be reindexed by changing its
> digest" is not available for OCI-hosted Helm repositories.

## Do not automate this

Steps 1 and 2 are interactive and account-bound. Step 4 pushes to the project's public
registry namespace under the maintainer's credentials. There is an
[API](https://github.com/artifacthub/hub/blob/master/docs/api/openapi.yaml)
(`POST /api/v1/repositories/user`, `kind: 0` for Helm) if this is ever worth scripting,
but a one-time listing is not.
