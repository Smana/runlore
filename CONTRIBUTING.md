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
  [`docs/plans/`](docs/plans) (a bite-sized, test-first task list).
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
API server** with mock external backends. It asserts 20 checks and tears down on exit.

```bash
hack/e2e-k3d.sh           # full run, deletes the cluster afterwards
hack/e2e-k3d.sh --keep    # leave the cluster + mock up for inspection
```

What it covers: deployment + RBAC + config load · catalog (`kb_search`) from a mounted ConfigMap ·
incident webhook → trigger policy · the ReAct loop (`what_changed → kb_search → submit_findings`) ·
Slack + Matrix delivery · GitOps-failure informer on real `Kustomization` CRDs · the curator
(GitHub App token → PR) · leader election (single active leader + failover).

### The mock backends

`hack/e2e/mock/main.go` (behind the `e2e` build tag, so it's excluded from normal builds) stands in for
the OpenAI chat endpoint (it scripts the tool-call sequence), Slack, Matrix, and the GitHub API. It runs
on the host; the in-cluster agent reaches it via `host.k3d.internal`. Build it standalone with:

```bash
go run -tags e2e ./hack/e2e/mock :9999
```

This is how features that talk to paid/external services (the LLM, GitHub) get exercised end-to-end with
zero credentials.

## Quick local demo (no cluster)

```bash
hack/demo.sh    # fires mocked Alertmanager alerts through the trigger policy
```

## Submitting a change

1. Branch from `main`.
2. Make the change test-first; keep the gate green (`-race` where relevant).
3. If it touches the deployment or a feature path, run `hack/e2e-k3d.sh`.
4. Open a PR describing **what** changed and **how it was verified** (cite the gate / e2e results).
