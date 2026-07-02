# Security Policy

RunLore is an SRE agent that runs **inside your cluster** with privileged reach: it
holds a GitHub App key, LLM provider credentials, and reads Kubernetes Secrets to
investigate incidents. A vulnerability here can expose those credentials or the
clusters RunLore observes — so please report it **privately**, not in a public issue.

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
