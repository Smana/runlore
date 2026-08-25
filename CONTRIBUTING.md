# Contributing

Thanks for hacking on RunLore. This covers local development and testing. For *deploying* RunLore to a
real cluster, see [docs/getting-started.md](https://runlore.io/docs/getting-started/).

## Prerequisites

- **Go 1.26+**
- **golangci-lint v2.12.2** (the CI version)
- For the end-to-end suite: **docker**, **[k3d](https://k3d.io/) v5+**, **kubectl**, **helm v3.12+**,
  and `openssl` (generates a throwaway GitHub App key).

## The quality gate

Every change must keep this green (it's what CI runs, see `.github/workflows/ci.yaml`):

```bash
go build ./...
go vet ./...
go test ./...
gofmt -l .                 # must print nothing
hack/lint.sh               # must report 0 issues
```

`hack/lint.sh` is `golangci-lint run ./...` with `GOTOOLCHAIN` pinned to the `toolchain`
line in `go.mod`, which is also where CI resolves its Go version. Run bare and with a newer
Go on your PATH, golangci-lint's bundled staticcheck panics while building its IR and takes
the entire run down with it — a failure that says nothing about your change. It forwards
arguments, so `hack/lint.sh ./internal/notify/...` narrows it.

Run race detection on anything touching goroutines (the queue, informer, leader election):

```bash
go test -race ./...
```

## Project layout

```
cmd/lore/            CLI + the `serve` entrypoint (wiring)
internal/
  config/            config schema + trigger policy
  trigger/           incident parsing, policy decision, dedup
  server/            HTTP: /webhook/alertmanager, /healthz, /readyz
  investigate/       the workqueue + the ReAct loop + tools (what_changed, kb_search)
  catalog/           OKF load + bleve index + Search (the Learn read half)
  curator/           confidence-routed curation (the Learn write half)
  forge/github/      GitHub IssueProvider (issues/PRs) + App token source
  model/openai/      OpenAI-compatible ModelProvider
  notify/            Slack + Matrix notifiers + fan-out
  providers/         the backend interface contracts (the architecture seam)
  whatchanged/       Git revision diffing
  providers/gitops/flux/   Flux GitOpsProvider (informer-backed)
deploy/helm/runlore/ the Helm chart
hack/                demo + the k3d e2e harness
docs/                design, getting-started, plans
```

`internal/providers/providers.go` is the contract: everything the agent touches is an interface, so the
loop is written against engine-agnostic types, never against Flux/OpenAI/GitHub directly.

## How we work

- **TDD.** Write the failing test first, then the implementation. Non-trivial work starts from a plan in
  [`dev/plans/`](dev/plans) (a bite-sized, test-first task list).
- **Small, focused commits**, conventional-commit style (`feat(scope): …`, `fix(scope): …`,
  `test(...)`, `docs(...)`, `ci: …`). One concern per commit.
- **Branch + PR**; keep the gate green on the branch.

## Unit testing

External backends are tested without network using `httptest` (the OpenAI client, Slack/Matrix, the
GitHub forge) and fakes (the GitOps `Reader`, the catalog `Searcher`, a scripted `ModelProvider`). The
Flux adapter is tested against a dynamic fake client. So `go test ./...` covers the logic of every
feature with no cluster.

## End-to-end on k3d (or kind)

`hack/e2e-local.sh` is the real-cluster proof: it spins up a throwaway local cluster, installs minimal
Flux CRDs, builds + imports the image, `helm install`s the chart, and verifies **each feature against a
real API server** with mock external backends. It asserts ~20 checks and tears down on exit.

```bash
hack/e2e-local.sh                     # full run on k3d (default), deletes the cluster afterwards
hack/e2e-local.sh --keep              # leave the cluster + mock up for inspection
E2E_PROVIDER=kind hack/e2e-local.sh   # same suite on a kind cluster instead
```

For local runs the suite supports **k3d (default) and kind** — pick with `E2E_PROVIDER=k3d|kind`; the
provider only changes cluster create/delete, image loading, the kubeconfig context, and how pods reach
the host-side mocks (`host.k3d.internal` vs the kind docker network's gateway IP) — every assertion is
identical. `hack/e2e-k3d.sh` remains as a thin k3d-pinned wrapper, and CI stays on k3d.

What it covers: deployment + RBAC + config load · catalog (`kb_search`) from a mounted ConfigMap ·
incident webhook → trigger policy · the ReAct loop (`what_changed → kb_search → submit_findings`) ·
Slack + Matrix delivery · GitOps-failure informer on real `Kustomization` CRDs · the curator
(GitHub App token → PR) · leader election (single active leader + failover).

In CI (`.github/workflows/e2e-k3d.yml`) the suite runs nightly (05:00 UTC), on manual dispatch —
and **on demand on a pull request**: apply the **`run-e2e`** label to the PR. Unlabeled PRs never
start the job (it is `if:`-gated on the label, so no runner time is spent), keeping the
~15-minute suite opt-in rather than a per-PR tax. Apply the label when a change touches the
deployment path, the chart, or a behaviour the e2e asserts.

### The mock backends

`hack/e2e/mock/main.go` (behind the `e2e` build tag, so it's excluded from normal builds) stands in for
the OpenAI chat endpoint (it scripts the tool-call sequence), Slack, Matrix, and the GitHub API. It runs
on the host; the in-cluster agent reaches it via `host.k3d.internal` (on kind: the docker network's
gateway IP). Build it standalone with:

```bash
go run -tags e2e ./hack/e2e/mock :9999
```

This is how features that talk to paid/external services (the LLM, GitHub) get exercised end-to-end with
zero credentials.

## Eval harness (RCA benchmark)

`lore eval` replays recorded incident cases through the investigation loop and reports the
root-cause-identification rate — use it to measure (and guard against regressions in) RCA quality as
the loop/prompt/tools evolve. A case (`examples/eval/*.yaml`) records the evidence each tool returns and
the keywords the **claim** must contain; the loop runs against the **configured model** with that
evidence, and each case is scored pass/fail. Scoring reads the claim only — the title plus each
hypothesis's summary and suggested action — never the replayed evidence, which the harness itself fed
the model (see `Score` in `internal/eval/score.go`).

```bash
lore eval --config runlore.yaml --cases examples/eval
```

It needs a configured model (`config.model`). The harness logic (`internal/eval`) is unit-tested with a
fake model, so `go test ./internal/eval/` runs without an API key.

### Comparing models (benchmark)

`lore eval --compare <spec.yaml>` benchmarks **several** models against the same replay suite in one
command and writes an aggregated report (markdown + JSON) to `eval/reports/`: per-model rubric medians,
pass rate, coverage, confident-wrong count, total tokens, and optional estimated cost. Grading is by one
fixed, blind judge so scores are comparable. See **[docs/benchmarking.md](https://runlore.io/docs/reference/benchmarking/)** for the
spec shape, the report columns, and how to publish results honestly. The pipeline has a keyless offline
test (`go test ./internal/app/ -run TestRunEvalCompareOffline`).

### Nightly eval (CI)

`.github/workflows/eval.yaml` runs the replay eval every night (06:00 UTC) and on
manual dispatch. It repeats each case 5× and fails the run when the campaign
pass-rate drops below 70% (`-n 5 -fail-under 0.7`), then uploads the JSON report as
a build artifact.

The run then **publishes a public scorecard** to the
[`eval-scorecard`](https://github.com/Smana/runlore/tree/eval-scorecard) branch:
`scorecard.md` (per-scenario pass/fail, recall outcomes, confidence calibration,
model, date, estimated cost), `badge.json` (the README's shields.io endpoint
badge), and `history.jsonl` (one line per run, capped at a year). Publishing is
deliberately unconditional on the gate: a night below 70% is published exactly
like a green one. Render the same artifacts locally from any report:

    lore eval scorecard -report eval/reports/<stamp>-replay.json -dir /tmp/scorecard

The per-run cost figure comes from the optional `pricing:` rates in
`eval/ci.runlore.yaml`; token totals are always reported, the dollar estimate
only when rates are set.

To enable it, add one repository secret — **`RUNLORE_EVAL_API_KEY`** — holding the
API key for the provider in `eval/ci.runlore.yaml`. Without the secret the job is
**skipped, not failed** — a fork or a repo without the secret configured stays
green — but the skip is loud: a `::warning::` annotation plus a `$GITHUB_STEP_SUMMARY`
line call it out in the Actions UI, so a run with no key never reads as a real,
quiet pass. The eval never runs on pull requests, so it imposes no per-PR cost and
never blocks merges; the deterministic scoring logic is already covered by
`go test ./...` on every PR.

Run it locally the same way CI does:

    lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7

One of the replay cases, `examples/eval/poisoned-recall-verify.yaml`, is
self-seeding (its own fixture catalog under `examples/eval/fixtures/poisoned-recall`)
and needs no extra setup; its sibling **live-fire** scenario,
`eval/scenarios/poisoned-recall-rejected.yaml` (run via `lore eval --live`, not by
any CI workflow), instead requires a poisoned catalog entry to be manually seeded
into a real cluster's catalog and `RUNLORE_POISON_READY` set, or it SKIPs.

## Quick local demo (no cluster)

```bash
hack/demo.sh                 # a real investigation on recorded evidence (no cluster, no key)
hack/demo-trigger-policy.sh  # fires mocked Alertmanager alerts through the trigger policy
```

## Docs site (Hugo)

```bash
cd website && hugo server        # http://localhost:1313
hugo --minify --gc               # what CI builds
hack/check-anchors.sh            # in-page anchors must resolve (run after a build)
```

**Hugo ≥ 0.146.0 is required**, and CI pins **0.156.0**
([`docs.yml`](.github/workflows/docs.yml)). The pinned Hextra theme uses the `try` template
function, so an older binary fails with:

```
function "try" not defined
```

That error names the template, not the version, so it reads like a theme bug. It is not — check
`hugo version` first. Every website change in this repo has hit it at least once.

Two things the build will not catch on its own, hence the extra step above:
`refLinksErrorLevel: ERROR` validates `relref` links **between** pages but ignores `#fragment`
links **within** one, and a heading containing an em dash renders a **double** hyphen in its id
(`## Step 2 — GitHub App` → `#step-2--github-app`). `hack/check-anchors.sh` checks both against
the rendered HTML.

### Every YAML block on the site is loaded as a real config

`internal/docsguard` runs **every** ` ```yaml ` fence under `website/content/` through
`config.Load` — the same strict loader `lore serve` uses, with unknown keys rejected and
`Config.Validate` run. A block a reader could paste into `runlore.yaml` only to have the agent
refuse to start is a **test failure**, not something to catch in review.

Validation is **opt-out**, because the mistake worth catching is a new snippet going silently
unchecked. If a fence is genuinely *not* a `runlore.yaml` — Helm chart values, a Kubernetes
manifest, an Alertmanager receiver — put an HTML comment on the line above it:

````markdown
<!-- docsguard:ignore Helm chart values, not a runlore.yaml -->
```yaml
replicaCount: 1
```
````

Hugo renders the site with `unsafe: true`, so the comment reaches the HTML as a comment and a
reader never sees it. Keep it at the fence's own indentation when the fence sits inside a list
item. The reason is **mandatory** — a reviewer reads it to tell an honest exemption from a
silenced failure.

Two things the guard will not let you get away with:

- **A marked fence that loads cleanly fails the test.** The marker only covers blocks that are
  genuinely not a config, so it cannot rubber-stamp a real failure — and it cannot be left behind
  after the block is fixed.
- **A marker that is not immediately above a fence fails the test**, instead of quietly exempting
  nothing.

**Prefer fixing a block to marking it.** A snippet that looks like a complete config is one a
reader will paste, so if it is missing a required companion key — `outcome.ledger_path` beside
`notify.*.thread_capture`, a delivery target beside `silence_button` — add the key rather than
excusing the block.

## Submitting a change

1. Branch from `main`.
2. Make the change test-first; keep the gate green (`-race` where relevant).
3. If it touches the deployment or a feature path, run `hack/e2e-local.sh`.
4. Open a PR describing **what** changed and **how it was verified** (cite the gate / e2e results).

## Releasing

Releases are fully automated from the [Conventional Commits](https://www.conventionalcommits.org/) you
already write — there is nothing to tag by hand.

> ### Changing existing behaviour? Say so, or the migration note goes nowhere.
>
> `CHANGELOG.md` renders **only the commit subject line** for every type. However good the explanation
> in your commit body, an operator upgrading never sees it — the one exception is a
> `BREAKING CHANGE:` footer, whose prose release-please lifts verbatim into a `⚠ BREAKING CHANGES`
> section at the top of the release. So a change that alters what an existing config key *means* needs:
>
> 1. **`!` after the type/scope** — `fix(investigate)!: …` — and, because PRs are **squash-merged**,
>    the PR **title** is what release-please parses, not the commits on your branch. Put the marker
>    there.
> 2. **A `BREAKING CHANGE:` footer in the squash-commit body** (editable in the GitHub merge dialog)
>    carrying the migration paragraph: what changed, what an operator will observe, and what to set.
>
> Nothing in CI lints this — a missing marker fails silently by simply not appearing in the changelog,
> which is why it is written down here. Pre-1.0 a breaking change bumps the **minor** version
> (`bump-minor-pre-major` in `release-please-config.json`); it does not cut a 1.0.0.

1. **You merge `feat:` / `fix:` / etc. PRs to `main`** as usual.
2. **[release-please](https://github.com/googleapis/release-please) opens (and keeps updating) a release
   PR** — `.github/workflows/release-please.yml`. It computes the next [SemVer](https://semver.org/) from
   the commit types since the last release, bumps the version, and regenerates `CHANGELOG.md`. The first
   release PR will propose **v0.1.0** (the `feat:` history so far is a 0.x minor bump).
3. **You merge the release PR.** That tags `vX.Y.Z` and creates the GitHub release with the changelog.
4. **The `vX.Y.Z` tag then fires one release build — `release-binaries.yml` runs
   [GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`), which:**
   - builds and pushes the **multi-arch container image** (the `vX.Y.Z` / `{major}.{minor}` / `latest`
     tags on `ghcr.io/smana/runlore`) and **cosign keyless-signs it by digest**; buildx attaches
     **SLSA provenance and SBOM attestations** to the pushed image index (attestations are produced
     from the next tagged release onward).
   - **attaches the cross-platform `lore` binaries** (linux/darwin/windows × amd64/arm64) as
     `tar.gz`/`zip` archives, plus `checksums.txt`, a syft **SBOM per archive**, and a **keyless
     cosign signature** of the checksums file — to the release release-please just created.

   (`build-image.yml` only validates PR/main image builds; it does not run on tags.)

The image and the binaries share the same `-X main.version` ldflags, so `lore --version` matches the
image tag.

### One-time setup (required): the `RELEASE_PLEASE_TOKEN` PAT

> **This must exist before the pipeline works.** Without it the automation silently half-runs: the
> release PR opens but **CI never runs on it**, and merging it tags the release but **the GoReleaser
> release build (`release-binaries.yml`) never fires**. This is a GitHub safeguard — events created
> using the default `GITHUB_TOKEN` do **not** trigger further workflow runs.

Create a **fine-grained Personal Access Token** scoped to `Smana/runlore` with these **repository**
permissions:

| Permission     | Access       |
| -------------- | ------------ |
| Contents       | Read & write |
| Pull requests  | Read & write |
| Workflows      | Read & write |

Then add it as an **Actions repository secret** named **`RELEASE_PLEASE_TOKEN`**
(`Settings → Secrets and variables → Actions → New repository secret`). The `goreleaser` job uses the
default `GITHUB_TOKEN` (it only uploads assets to the already-created release), so no extra secret is
needed there.
