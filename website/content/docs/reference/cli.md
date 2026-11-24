---
title: CLI
weight: 5
---

`lore` is a single static binary: a demo you can run with no key and no cluster, a
one-off investigator for your own stack, and the same engine that runs continuously
as [`lore serve`]({{< relref "/docs/getting-started.md" >}}). This page covers the CLI
front door — install, the two zero-ceremony commands, and how each one degrades
when something isn't configured.

## Subcommands

Every subcommand `lore` wires; `lore help` prints each one's flags.

| Subcommand | Does |
|---|---|
| `lore status` | zero-LLM cluster triage snapshot (unhealthy pods, failing GitOps objects, recent Warning events) |
| `lore investigate` | one on-demand investigation of a target or alert, findings printed (text or `--json`) |
| `lore demo investigate` | a full investigation on fake providers, recorded (`--offline`) or live |
| `lore serve` | the in-cluster agent, reacting to incidents |
| `lore catalog sync` | clone or pull the knowledge catalog and index it |
| `lore kb` | `search`, `show`, `serve`, `import` and `resolve` knowledge-base entries |
| `lore eval` | replay recorded cases and score root-cause identification; `--live`, `--compare`, `scorecard` |
| `lore curate` | groom the knowledge-base backlog; `queue-issue`, `refresh-entries` |
| `lore mcp` | serve what-changed and knowledge-base search over MCP (stdio) |
| `lore audit verify` | re-walk the action audit log, report the first broken link |
| `lore validate-kb` | validate every OKF entry in a catalog directory (the knowledge-base merge gate) |
| `lore version` | print the version |

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

## `lore status`

`lore status [--namespace <ns>] [--kube-context <name>] [--json] [--exit-code]`
runs a fast, **zero-LLM**, read-only triage snapshot against your active kubeconfig
context — no API key and no `runlore.yaml` required.

It reports:

- **GitOps failures** — auto-detects Flux (`Kustomization` / `HelmRelease` with `Ready=False`) and Argo CD (`Application` with `Health=Degraded`, `Sync=OutOfSync`, or failed sync operation).
- **Unhealthy pods** — `Pending`, `Failed`, `NotReady`, waiting container reasons (`CrashLoopBackOff`, `ImagePullBackOff`, `CreateContainerConfigError`, …), or restarts ≥ 3.
- **Recent Warning events** — last 15 minutes, newest first (capped at 20).

When issues are found, `lore status` prints concrete `Next step:` commands you can
copy-paste straight into `lore investigate <kind>/<ns>/<name>`. Pass `--json` for
structured output, and `--exit-code` in scripts or CI to exit `1` whenever failing
GitOps objects or unhealthy pods are present (warning events alone still exit `0`).

## `lore investigate`

```bash
lore investigate [<ns>/<name> | <kind>/<ns>/<name>] \
  [--alert <name>] [--namespace <ns>] [--message <text>] \
  [--config <path>] [--provider openai|anthropic|gemini] [--model <name>] \
  [--base-url <url>] [--effort <level>] \
  [--metrics-url <url>] [--logs-url <url>] \
  [--kube-context <name>] [--json] [--quiet]
```

`lore investigate` runs one on-demand investigation against **your** stack and prints
the findings — no `runlore.yaml` required. You can pass a positional workload target
(`apps/checkout-api` or `Deployment/apps/checkout-api`) with flags before or after it.
With no `--config`, it reads its model from the environment, in this order:

| Source | Effect |
|---|---|
| `OPENAI_BASE_URL` (+ optional `OPENAI_API_KEY`, `OPENAI_MODEL`) | any OpenAI-compatible endpoint — keyless if the endpoint itself needs no credential (e.g. a local vLLM/Ollama) |
| `ANTHROPIC_API_KEY` (+ optional `ANTHROPIC_MODEL`) | native Anthropic (default `claude-sonnet-5`) |
| `GEMINI_API_KEY` (+ optional `GEMINI_MODEL`) | native Gemini (default `gemini-4-neon`) |
| `OPENAI_API_KEY` (+ optional `OPENAI_MODEL`) | direct OpenAI (`https://api.openai.com/v1`, default `gpt-4o-mini`) |
| none | `investigate` exits with an error naming all options |

A `./runlore.yaml`, if present, is read automatically; `--config <path>` points at
one explicitly (and a missing explicit path is a hard error — the flag is a promise,
not a hint).

Flags override whatever the config resolved to, so you can point the CLI at your
stack without writing a file at all:

| Flag | Overrides |
|---|---|
| `--provider` | model provider (`openai`, `anthropic`, `gemini`) — bypasses env auto-detection precedence |
| `--model` | the model name |
| `--base-url` | the OpenAI-compatible endpoint |
| `--effort` | model reasoning effort level |
| `--metrics-url` | PromQL endpoint — enables the `query_metrics` tool |
| `--logs-url` | logs endpoint — enables the `query_logs` tool |
| `--kube-context` | kubeconfig context to query |
| `--json` | emit the final findings as indented JSON on stdout (suppresses stderr progress) |
| `--quiet` | suppress stderr provider notice and live tool progress (`→ tool(...)`) |

During live runs (unless `--quiet` or `--json` is set), `lore investigate` prints the
chosen provider/model and one concise, secret-redacted `→ <tool>(...)` line per tool
call to stderr while keeping stdout strictly for the final verdict.

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
