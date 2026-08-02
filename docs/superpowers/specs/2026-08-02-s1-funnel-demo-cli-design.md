# Design — S1: Funnel, keyless demo + CLI front door

- **Date:** 2026-08-02
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Source:** improvement report §2.1, §2.2 — "the single change most likely to move your star count"
- **Index:** [decomposition](2026-08-02-improvement-report-decomposition.md)

## Problem

A stranger cannot make RunLore produce a root cause without provisioning things first.

- `hack/demo.sh` is advertised as "try it in one minute". It runs `lore serve` against a keyless
  config, POSTs `examples/alertmanager-webhook.json`, and greps `msg=incident` out of the log. What
  it shows is **trigger-policy filtering decisions** — the least differentiated part of the product.
- `lore demo investigate` *does* render the real verdict card (`notify.Format`), against real fake
  providers, through the real loop. But `demo.go:96` refuses to start without `ANTHROPIC_API_KEY`
  (or a `--config` naming a configured model). So the good demo is gated behind a credential.
- `lore investigate` — the laptop-to-value path, the equivalent of `holmes ask` — is mentioned only
  in `CONTRIBUTING.md` and has no docs page. Worse, it is **unusable without a config file**:
  `RunInvestigate` calls `config.Load("runlore.yaml")` unconditionally and `config.Load` fails on a
  missing file (`load.go:17`).
- There is no install path. `.goreleaser.yaml` ships `archives:` only — no tap, no script, no
  documented one-liner.

The keyless plumbing already exists: `internal/app/eval_compare_test.go:28` is a keyless,
offline, OpenAI-compatible mock that drives the whole loop. It lives in a `_test.go` file, so no
user can reach it.

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Keyless demo mechanism | **Recorded transcript replay** | A scripted mock emits canned strings ("mock: chart bump broke harbor-db") — it reads as a toy. A recorded transcript replays *genuine model output* over real fixture evidence, so the first thing a stranger sees is a real RCA |
| Transcript regeneration | `--record` flag on the same command | Keeps the fixture reproducible by the maintainer; no hidden generation script |
| Replay fidelity | Replay the **model turns only**; tools execute for real against the existing fake providers | The trace a user watches is real tool I/O, not a recorded log |
| `hack/demo.sh` | Becomes the verdict-card demo | Report §2.1. The trigger-policy demo moves to `hack/demo-trigger-policy.sh` (still referenced from `CONTRIBUTING.md`) |
| `lore investigate` config | Zero-config path when `runlore.yaml` is absent | An explicit `--config` that is missing still hard-errors — silence there would hide a typo |
| Install channel | `install.sh` (curl \| sh), owner's choice | Plus a one-line `go install` alternative in the docs for readers who won't pipe a script to a shell |
| Checksum verification | On by default in `install.sh` | The release already publishes `checksums.txt` + a cosign bundle; a security-conscious audience will read this script |

## Scope

### In scope

1. `internal/model/replay` — a `providers.ModelProvider` that replays a recorded transcript.
2. Transcript fixture for one curated scenario, committed under `examples/demo/`.
3. `lore demo investigate --offline` (replay) and `--record <path>` (capture).
4. Zero-config `lore investigate`: env-derived model, ambient kubeconfig, graceful degradation.
5. `--metrics-url` / `--logs-url` / `--model` / `--base-url` flags on `investigate`.
6. `install.sh` + hosting it at `runlore.io/install.sh`.
7. `hack/demo.sh` rewrite; `hack/demo-trigger-policy.sh` extraction.
8. `website/content/docs/reference/cli.md` — the CLI reference page (S2 restructures Getting
   Started to lead with it; S1 only ships the page).
9. Drift guards (below).

### Non-goals (YAGNI)

- Homebrew tap / winget / nfpms packages (owner deselected).
- Recording transcripts for every scenario — one good one beats five thin ones.
- A TUI, colored output beyond what `notify.Format` already does, or an HTML card export.
- Making `lore serve` keyless — the in-cluster path legitimately needs a model.

## Design

### The replay provider

`providers.ModelProvider` is a single non-streaming method
(`providers.go:495`), which makes replay trivial:

```go
// internal/model/replay
type Provider struct {
    turns []providers.CompletionResponse
    i     int
    mu    sync.Mutex
}

func (p *Provider) Complete(ctx context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error)
```

Turns are returned in recorded order and the request is ignored. Exhaustion is a **loud** error:
`transcript exhausted after N turns (the loop asked for one more — re-record with
'lore demo investigate --record')`. Silently returning an empty completion would produce a demo
that ends with no findings and no explanation.

### Transcript format

**One ordered channel.** `demo.go:107` already sets `verifyModel = nil` whenever a model is
injected, so the verify pass draws from the same model as the loop — the existing
`demo_test.go:39` script proves it (turn 3 is `submit_verdicts`). Replay inherits that shape rather
than inventing a second channel:

```json
{
  "version": 1,
  "scenario": "harbor-chart-bump",
  "recorded_at": "2026-08-02T09:14:00Z",
  "recorded_with": {"provider": "anthropic", "model": "claude-sonnet-5"},
  "turns": [
    {"text": "", "tool_calls": [{"id": "1", "name": "what_changed", "args": "{\"namespace\":\"harbor\"}"}],
     "usage": {"input_tokens": 4211, "output_tokens": 96}}
  ]
}
```

`usage` is preserved so the demo's cost footer shows the real token counts from the recorded run —
the footer is part of what the report says sells the product.

### Recording

`--record <path>` wraps the model in a decorator that appends each `CompletionResponse` to `turns`
and writes the file on completion. Recording forces the verify pass onto the same model (exactly as
replay does), so the recorded call sequence and the replayed one are identical by construction. The
decorator lives beside the replay provider (`internal/model/replay/record.go`) and is the only place
that writes transcripts.

### Wiring

`runDemoInvestigateWithModel` already has a model seam used by tests. `--offline` resolves the model
to a `replay.Provider` and sets `verifyModel` from the transcript's verify channel, then everything
downstream — fake providers, `tracingTool`, the loop, `notify.Format` — is untouched production
code.

### Zero-config `lore investigate`

```
--config given explicitly + missing  → hard error (unchanged)
--config absent + ./runlore.yaml present → load it (unchanged)
--config absent + ./runlore.yaml absent  → synthesize from env
```

Synthesis order:

| Env | Result |
|---|---|
| `OPENAI_BASE_URL` (+ optional `OPENAI_API_KEY`, `OPENAI_MODEL`) | OpenAI-compatible provider, keyless when no key is set (local vLLM/Ollama) |
| `ANTHROPIC_API_KEY` | native Anthropic |
| neither | error naming both paths and the `--model`/`--base-url` flags |

Flags override the synthesized config, never a loaded file (a file is an explicit statement of
intent). Kube access comes from the ambient kubeconfig; **every unset source disables its tool** —
no KB, no forge, no notifier, no Flux required. The command warns once on stderr listing which
tools are disabled, so a thin result is explainable rather than mysterious.

### `install.sh`

Detects `uname -s`/`uname -m` → the goreleaser archive name, resolves the latest tag from the
GitHub releases API (`LORE_VERSION` env pins it), downloads the archive **and** `checksums.txt`,
verifies the SHA-256, extracts to `$LORE_INSTALL_DIR` (default `/usr/local/bin`, falling back to
`~/.local/bin` when not writable). It prints the cosign verification command rather than requiring
cosign. Served from the website as `static/install.sh` so `curl -fsSL https://runlore.io/install.sh`
works, with the GitHub raw URL documented as the auditable source.

## Testing

| Test | Guards |
|---|---|
| `TestDemoOfflineRendersVerdictCard` | `--offline` produces a card containing the recorded root cause, with no network and no env keys set |
| `TestReplayExhaustionIsLoud` | asking for turn N+1 returns the re-record error, not an empty completion |
| `TestTranscriptToolNamesExist` | **drift guard**: every tool name in the committed transcript is a tool the demo actually registers — a renamed tool fails CI instead of producing a broken demo |
| `TestRecordRoundTrip` | record → replay reproduces the same turns byte-for-byte |
| `TestInvestigateZeroConfig` | env-only synthesis produces a valid config for both provider paths; explicit missing `--config` still errors |
| `TestInvestigateDegradesGracefully` | with no kube/KB/forge/notifier, the command still runs and reports which tools were disabled |
| `shellcheck hack/*.sh` + `install.sh` | already in CI for `hack/`; extend to `install.sh` |

The drift guard is mutation-tested during implementation: rename a tool in the transcript, confirm
CI goes red, revert.

## Risks

| Risk | Mitigation |
|---|---|
| The transcript goes stale as the prompt/tool set evolves | The tool-name drift guard fails CI on rename; `--record` makes regeneration a one-liner. Regeneration is a documented step in `CONTRIBUTING.md` |
| A replayed demo misrepresents current model quality | The card footer names the recorded model and date, from the transcript's `recorded_with` |
| `install.sh` becomes an attack surface | Checksum verification on by default, no `sudo` inside the script, the source URL documented, and it goes through the S1 security pass |
| Zero-config `investigate` silently produces a weak answer | The one-time stderr warning lists disabled tools |

## Acceptance criteria

1. On a machine with only Go and no API keys, `hack/demo.sh` prints a real verdict card in under
   60 seconds, with no outbound LLM call.
2. `curl -fsSL https://runlore.io/install.sh | sh` installs a working `lore` on Linux and macOS,
   amd64 and arm64, verifying the checksum.
3. `lore investigate --alert KubePodCrashLooping --namespace apps` works against a live cluster with
   only `OPENAI_BASE_URL`/`ANTHROPIC_API_KEY` set and **no** `runlore.yaml`, no Flux, no KB repo, no
   GitHub App, no notifier.
4. `website/content/docs/reference/cli.md` documents install, both commands, and the degradation
   rules; the docs link-check passes.
5. `go test ./...`, `golangci-lint`, and the security pass are clean.
</content>
