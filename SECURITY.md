# Security Policy

RunLore is an SRE agent that runs **inside your cluster** with privileged reach: it
holds a GitHub App key and LLM provider credentials. A vulnerability here can expose
those credentials or the clusters RunLore observes — so please report it **privately**,
not in a public issue.

For the technical architecture behind RunLore's defenses — the prompt-injection design,
secret-redaction boundaries, and network guards — see
[LLM security architecture](https://runlore.io/docs/security/security-architecture/) and the runtime
[security model](https://runlore.io/docs/security/security-model/).

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

### `model.chat` — a paid model call any channel member can trigger

A third opt-in feature, off by default, does not widen what RunLore receives but **does let people
outside your team spend your money**. Setting a `model.chat` block turns on conversational replies:
RunLore answers an addressed thread message that carries no recognised command prefix with a model
call, instead of a fixed "I can only record notes" reply.

Stated plainly, without softening:

- **Every addressed message that is not a `note:` costs one model call.** Exactly one, structurally —
  the model is offered a single forced tool and no search tool, so there is no agent loop. `note:`
  itself remains deterministic and costs nothing; so does a bare mention with nothing after it.
  `note:` is recognised as a whole word *anywhere* in the message — `note: …`, `please note: …` and
  `hey @runlore note: …` are all the free path, so the promise above does not depend on where the
  operator happened to put the mention. A word merely ending in it (`footnote:`) is not the command.
- **On Matrix, "addressed" does not require a mention entity.** MSC3952 `m.mentions` counts, but so
  does the bot's full MXID *or its bare localpart* appearing in the message body as a whole word. Any
  member of the room can therefore trigger a paid call by typing the bot's name in a thread reply.
  Keep the room invite-only. On Slack the trigger is a real `app_mention`, but every member of the
  channel can send one.
- **The reply is model-authored text that can become a knowledge-base PR.** The model may propose
  note *content*; it never chooses where the note is filed — routing is derived from the thread's
  investigation context alone, and the note goes through the same per-thread cap and the same global
  forge-write window an explicit `note:` uses. Untrusted message text is fenced in the prompt with a
  per-turn marker, and the model is instructed to treat everything inside it as data, never
  instructions.

**Capped, and the key that bounds each:** model calls per hour
(`notify.thread.chat_calls_per_hour`, default 30) · provider-reported tokens per hour
(`notify.thread.chat_tokens_per_hour`, default 109940) · output tokens per call
(`model.chat.max_tokens`, default 1024, deliberately *not* inherited from `model.max_tokens`) · the
human's message reaching the model (`notify.thread.max_note_bytes`, default 8192 bytes) · the whole
assembled prompt (~15 KB, ≈3.8k input tokens; `max_note_bytes` moves it, and so does any edit to the system prompt or the tool spec) ·
knowledge-base PR writes per hour (`notify.thread.forge_writes_per_hour`, default 20) · notes per
thread (`notify.thread.max_notes_per_thread`, default 20). Both hourly windows are global across
every thread and both transports. The token default is not a round number because it is not chosen:
it is *derived* from the most one call can cost times the hourly call budget, taken at two thirds, so
it moves whenever any term of that derivation moves — `max_note_bytes`, but also the system
prompt's own wording and the tool spec, since both are counted by length. A release whose notes
mention only a prompt change can therefore raise this ceiling. Pin it explicitly if you need a
fixed one.

**Not capped — read this before enabling it:**

- **There is no ceiling in currency.** `model.pricing` remains a *reporting* table that turns token
  counts into a dollar estimate for display. **Nothing compares a cost to a threshold and stops.**
  The only spend ceilings are the call and token counts above; convert them to money yourself at
  your provider's rate.
- **The token ceiling counts *reported* usage.** When a provider returns no usage at all, RunLore
  charges a deliberately conservative estimate rather than zero, and logs a warning saying it did.
  The estimate can only over-charge — the safe direction — but it is an estimate, not a measurement.
- **It is an hourly sliding window, not a monthly or absolute budget.** `chat_tokens_per_hour` at its
  default permits 109940 tokens every hour, indefinitely. There is no cumulative cap over a day, a
  month, or the life of the process.

`model.chat.model` must be named explicitly or startup fails: it is the one field that does not
inherit, so a member-triggerable path can never silently run on the frontier investigation model.
Full reference, including the failure modes that degrade back to deterministic capture, is in
[Conversational replies and what they cost](https://runlore.io/docs/configuration/configuration/#conversational-replies-and-what-they-cost).

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
