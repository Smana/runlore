# Security Policy

RunLore is an SRE agent that runs **inside your cluster** with privileged reach: it
holds a GitHub App key and LLM provider credentials. A vulnerability here can expose
those credentials or the clusters RunLore observes — so please report it **privately**,
not in a public issue.

For the technical architecture behind RunLore's defenses — the prompt-injection design,
secret-redaction boundaries, and network guards — see
[`docs/security-architecture.md`](docs/security-architecture.md) and the runtime
[`docs/security-model.md`](docs/security-model.md).

## Reporting a vulnerability

**Preferred — GitHub private security advisories.** Open the repository's
[**Security** tab → **Report a vulnerability**](https://github.com/Smana/runlore/security/advisories/new).
This keeps the report, the discussion, and any fix coordination private until a patch
is ready, and gives you credit on the published advisory.

**Fallback — email.** If you can't use GitHub advisories, write to
**[smaine.kahlouch@ogenki.io](mailto:smaine.kahlouch@ogenki.io)**. Encrypt the report
if you can; otherwise keep the details minimal in the first message and we'll move to a
private channel.

Please **do not** open a public GitHub issue, discussion, or pull request for a
suspected vulnerability — that discloses it to everyone before there's a fix.

### What to include

A good report lets us reproduce and assess impact quickly:

- a description of the issue and the **impact** you believe it has;
- the affected component (e.g. the GitHub forge, the model client, config loading,
  the webhook server) and the version or commit SHA;
- **reproduction steps** or a proof of concept;
- any relevant logs or config — **redact secrets, tokens, and cluster identifiers**.

## Opt-in features that widen what RunLore receives

Two chat-transport features are off by default and change RunLore's exposure or inbound event
surface once turned on:

- **`notify.slack.thread_capture`** exposes a second HTTP endpoint, `POST /slack/events`, alongside
  the alert webhook and (if used) `/slack/interactions`.
- **`notify.matrix.thread_capture`** exposes nothing new, but widens RunLore's Matrix `/sync` filter
  from `["m.reaction"]` to also include `["m.room.message"]`. Stated plainly: **the process starts
  receiving message events** from the configured room, where by default it receives only reactions.
  RunLore acts only on messages that address it and are rooted in one of its own messages —
  everything else is dropped immediately without its body being logged, but every message in the
  room does transit the process first.

Both are opt-in, both fail closed on missing credentials, and full detail — the addressing/attribution
checks, the invite-only-room recommendation, and why Matrix still needs no exposed endpoint for this —
is in the runtime [security model](https://runlore.io/docs/security/security-model/#matrix-thread-capture-notifymatrixthread_capture--a-widened-sync-filter).

## Supply chain

Every tagged release (`v*`) ships with the following, all produced by
[`.goreleaser.yaml`](.goreleaser.yaml) and the [release workflows](.github/workflows/):

- **SBOMs per archive.** [`syft`](https://github.com/anchore/syft) generates an
  SPDX 2.3 SBOM for each of the six release archives, attached to the GitHub
  release as `<archive>.sbom.json`. The container image carries its own
  BuildKit-generated SBOM + SLSA provenance as an OCI attestation on the pushed
  index (`--attest=type=sbom`, `--provenance=mode=max`).
- **A signed checksums bundle.** `checksums.txt` (SHA-256 of every archive and
  its SBOM) is cosign keyless-signed; the signature ships as
  `checksums.txt.bundle`. Verifying the bundle transitively covers every listed
  archive:

  ```bash
  cosign verify-blob --bundle checksums.txt.bundle \
    --certificate-identity-regexp="https://github.com/Smana/runlore/.github/workflows/release-binaries.yml.*" \
    --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
    checksums.txt
  ```

- **A signed container image.** `ghcr.io/smana/runlore` is cosign keyless-signed
  by digest on every release:

  ```bash
  cosign verify \
    --certificate-identity-regexp="https://github.com/Smana/runlore/.github/workflows/release-binaries.yml.*" \
    --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
    ghcr.io/smana/runlore:<version>
  ```

- **A signed Helm chart.** The chart is published as an OCI artifact to
  `ghcr.io/smana/charts/runlore` and cosign keyless-signed by digest:

  ```bash
  cosign verify \
    --certificate-identity-regexp="https://github.com/Smana/runlore/.github/workflows/release-chart.yml.*" \
    --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
    ghcr.io/smana/charts/runlore:<version>
  ```

All signatures are **keyless (Sigstore)**: the release workflows sign with a
short-lived Fulcio certificate issued from the workflow's own GitHub Actions
OIDC token (`id-token: write`), and the signature is recorded in the public
Rekor transparency log — there are no long-lived signing keys to leak or
rotate. The `--certificate-identity-regexp` above pins verification to the
exact workflow that is allowed to produce that artifact, not just "signed by
someone."

An [OpenSSF Scorecard](https://scorecard.dev/) analysis runs weekly
and on every push to `main`; results are on the repo's
[Security tab](https://github.com/Smana/runlore/security) (Code scanning alerts)
and the badge in [`README.md`](README.md).

## Supported versions

RunLore is **pre-1.0 and under active development**. Security fixes land only on the
**latest `main` and the newest tagged release** — there are no maintained back-release
branches. If you're running an older commit, the fix is to update.

| Version            | Supported |
| ------------------ | --------- |
| latest `main`      | ✅        |
| newest release     | ✅        |
| anything older     | ❌        |

## What to expect

- **Acknowledgement within 72 hours** of your report.
- An initial assessment (severity, whether we can reproduce, likely timeline) shortly
  after.
- **Coordinated disclosure.** We'll work with you on a fix and agree on a disclosure
  date; please give us reasonable time to ship a patch before going public. Once the
  fix is released we'll publish an advisory and credit you (unless you'd rather stay
  anonymous).

Thank you for helping keep RunLore and the people who run it safe.
