---
title: CLI
weight: 5
---

`lore` is a single static binary: a demo you can run with no key and no cluster, a
one-off investigator for your own stack, and the same engine that runs continuously
as [`lore serve`]({{< relref "/docs/getting-started.md" >}}). This page covers the CLI
front door — install, the two zero-ceremony commands, and how each one degrades
when something isn't configured.

## Install

```bash
curl -fsSL https://runlore.io/install.sh | sh
```

The script downloads the release archive for your OS/arch from the GitHub release,
**verifies its SHA-256 against the published `checksums.txt` before extracting
anything**, and installs `lore` to `/usr/local/bin` — falling back to `~/.local/bin`
(no `sudo`, ever) if that isn't writable. It's a small, readable POSIX shell script;
audit it yourself at
[`website/static/install.sh`](https://github.com/Smana/runlore/blob/main/website/static/install.sh).

Environment variables:

| Variable | Effect |
|---|---|
| `LORE_VERSION` | pin a release tag, e.g. `v0.11.0` (default: the latest release) |
| `LORE_INSTALL_DIR` | install target (default: `/usr/local/bin`, falls back to `~/.local/bin`) |

**Prefer not to pipe a script into a shell?** Build from source instead — no
third-party trust beyond the Go toolchain and the module graph:

```bash
go install github.com/Smana/runlore/cmd/lore@latest
```

**Verify the release signature (optional).** Every release is keyless-signed with
[cosign](https://github.com/sigstore/cosign) in CI (`id-token: write`, no long-lived
key) — the installer prints this command, or run it yourself:

```bash
cosign verify-blob --bundle checksums.txt.bundle \
  --certificate-identity-regexp 'https://github.com/Smana/runlore/.github/workflows/release-binaries.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
```

Verifying `checksums.txt` transitively covers every archive it lists.

## `lore demo investigate --offline default`

The fastest way to see RunLore reach a real root cause — **no API key, no network,
no cluster**:

```bash
lore demo investigate --offline default
```

This replays a **recorded transcript** of a genuine investigation
(`examples/demo/harbor-chart-bump.transcript.json`) through the real production
loop — the same ReAct tool calls, verify pass, and verdict-card renderer that run
in `lore serve` — over fake (but realistic) evidence for a Harbor chart-bump
incident. Nothing about the reasoning is scripted for the demo: the model turns
were captured once against a live model and are replayed byte-for-byte.

The card discloses its own provenance so you know exactly what you're looking at:

```text
== RunLore demo: investigating "harbor-chart-bump" (recorded model turns, fake providers, no cluster) ==
   model turns recorded 2026-08-02T07:48:52Z with openai/glm-4.5-air
```

That line is not decoration — it's the honesty mechanism. A recording ages: model
behavior on the same evidence can drift release to release, so the date and model
tell you exactly how current the transcript is, rather than implying it's live.
Re-record it yourself against your own model with `--record`:

```bash
lore demo investigate --record my-transcript.json   # needs a model key; runs live
lore demo investigate --offline my-transcript.json  # replays it back, keyless
```

Without `--offline`, `lore demo investigate [--scenario <name>]` runs the same
loop **live** against fake providers — it still needs no cluster, but it does need
a model key (`ANTHROPIC_API_KEY` by default, or `--config` pointing at a
`runlore.yaml`).

## `lore investigate`

`lore investigate --alert <name> [--message <text>] [--namespace <ns>]` runs one
on-demand investigation against **your** stack and prints the findings — no
`runlore.yaml` required. With no `--config`, it reads its model from the
environment, in this order:

| Source | Effect |
|---|---|
| `OPENAI_BASE_URL` (+ optional `OPENAI_API_KEY`, `OPENAI_MODEL`) | any OpenAI-compatible endpoint — keyless if the endpoint itself needs no credential (e.g. a local vLLM/Ollama) |
| `ANTHROPIC_API_KEY` | native Anthropic, checked if no `OPENAI_BASE_URL` is set |
| neither | `investigate` exits with an error naming both options |

A `./runlore.yaml`, if present, is read automatically; `--config <path>` points at
one explicitly (and a missing explicit path is a hard error — the flag is a promise,
not a hint).

Flags override whatever the config resolved to, so you can point the CLI at your
stack without writing a file at all:

| Flag | Overrides |
|---|---|
| `--model` | the model name |
| `--base-url` | the OpenAI-compatible endpoint |
| `--metrics-url` | PromQL endpoint — enables the `query_metrics` tool |
| `--logs-url` | logs endpoint — enables the `query_logs` tool |

### What degrades, and what that notice means

`investigate` never refuses to run just because a signal is missing — it runs on
whatever's configured and says so on stderr:

```text
note: running without metrics (query_metrics), logs (query_logs), knowledge catalog (kb_search, instant recall) — pass --metrics-url/--logs-url or a --config to enable them
```

| Unset | Disables |
|---|---|
| `--metrics-url` / `config.metrics.url` | `query_metrics` (PromQL evidence) |
| `--logs-url` / `config.logs.url` | `query_logs` (log evidence) |
| a configured catalog (`config.catalog`) | `kb_search` and instant recall (knowledge-base grounding) |

**This notice means "under-configured," not "broken."** The investigation still
runs to completion and still reaches a verdict — just over whatever evidence is
available. Treat the notice as a checklist for widening the picture, not as an
error to work around.

## When to move to `lore serve`

The CLI is for a one-off look — a specific alert, right now, from your terminal.
Once you want RunLore reacting to incidents continuously, delivering to chat, and
writing what it learns back to a knowledge-base repo as pull requests, the same
engine runs in-cluster as `lore serve`, deployed via Helm. See
[Getting Started]({{< relref "/docs/getting-started.md" >}}) for the full production
install — GitOps engine wiring, the GitHub App for curation, credentials, and a
complete `values.yaml` reference.
