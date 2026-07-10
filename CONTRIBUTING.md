# Contributing

Thanks for hacking on RunLore. This covers local development and testing. For *deploying* RunLore to a
real cluster, see [docs/getting-started.md](docs/getting-started.md).

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
golangci-lint run ./...    # must report 0 issues
```

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

## End-to-end on k3d

`hack/e2e-k3d.sh` is the real-cluster proof: it spins up a throwaway k3d cluster, installs minimal Flux
CRDs, builds + imports the image, `helm install`s the chart, and verifies **each feature against a real
API server** with mock external backends. It asserts ~20 checks and tears down on exit.

```bash
hack/e2e-k3d.sh           # full run, deletes the cluster afterwards
hack/e2e-k3d.sh --keep    # leave the cluster + mock up for inspection
```

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
on the host; the in-cluster agent reaches it via `host.k3d.internal`. Build it standalone with:

```bash
go run -tags e2e ./hack/e2e/mock :9999
```

This is how features that talk to paid/external services (the LLM, GitHub) get exercised end-to-end with
zero credentials.

## Eval harness (RCA benchmark)

`lore eval` replays recorded incident cases through the investigation loop and reports the
root-cause-identification rate — use it to measure (and guard against regressions in) RCA quality as
the loop/prompt/tools evolve. A case (`examples/eval/*.yaml`) records the evidence each tool returns and
the keywords the findings must contain; the loop runs against the **configured model** with that
evidence, and each case is scored pass/fail.

```bash
lore eval --config runlore.yaml --cases examples/eval
```

It needs a configured model (`config.model`). The harness logic (`internal/eval`) is unit-tested with a
fake model, so `go test ./internal/eval/` runs without an API key.

### Comparing models (benchmark)

`lore eval --compare <spec.yaml>` benchmarks **several** models against the same replay suite in one
command and writes an aggregated report (markdown + JSON) to `eval/reports/`: per-model rubric medians,
pass rate, coverage, confident-wrong count, total tokens, and optional estimated cost. Grading is by one
fixed, blind judge so scores are comparable. See **[docs/benchmarking.md](docs/benchmarking.md)** for the
spec shape, the report columns, and how to publish results honestly. The pipeline has a keyless offline
test (`go test ./internal/app/ -run TestRunEvalCompareOffline`).

### Nightly eval (CI)

`.github/workflows/eval.yaml` runs the replay eval every night (06:00 UTC) and on
manual dispatch. It repeats each case 5× and fails the run when the campaign
pass-rate drops below 70% (`-n 5 -fail-under 0.7`), then uploads the JSON report as
a build artifact.

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
hack/demo.sh    # fires mocked Alertmanager alerts through the trigger policy
```

## Submitting a change

1. Branch from `main`.
2. Make the change test-first; keep the gate green (`-race` where relevant).
3. If it touches the deployment or a feature path, run `hack/e2e-k3d.sh`.
4. Open a PR describing **what** changed and **how it was verified** (cite the gate / e2e results).

## Releasing

Releases are fully automated from the [Conventional Commits](https://www.conventionalcommits.org/) you
already write — there is nothing to tag by hand.

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
