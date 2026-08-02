---
title: RunLore vs k8sgpt
description: "k8sgpt is CNCF-native and needs no LLM to run; RunLore is the multi-signal investigation loop that starts where a scan leaves off. An honest, sourced comparison."
last_verified: 2026-08-02
---

*Claims about k8sgpt below were checked against the project's own repository and
documentation on 2026-08-02. If k8sgpt has shipped something new since, please
[open an issue](https://github.com/Smana/runlore/issues).*

k8sgpt is the CNCF's cluster-scanning tool: deterministic analyzers that turn "kubectl
describe and squint" into a one-line diagnosis, with an LLM as an optional add-on rather
than a requirement. RunLore doesn't compete with it as a scanner — the difference is
what happens when a scan alone doesn't explain **why** something broke.

## What k8sgpt does better

- **A CNCF Sandbox project with a governance home and a far bigger community.** k8sgpt
  was [accepted into the CNCF Sandbox on 2023-12-19](https://www.cncf.io/projects/k8sgpt/)
  ([onboarding issue](https://github.com/cncf/sandbox/issues/38)), and has
  [8,039 GitHub stars](https://github.com/k8sgpt-ai/k8sgpt) and roughly
  [120 contributors](https://github.com/k8sgpt-ai/k8sgpt/graphs/contributors) (counted via
  the GitHub API) as of this check. RunLore, today, is maintained by one person.
- **Deterministic analyzers that need no LLM at all.** `k8sgpt analyze` runs its default
  checks without ever constructing an AI client — the code
  [returns early unless `--explain` is passed](https://github.com/k8sgpt-ai/k8sgpt/blob/main/pkg/analysis/analysis.go),
  so no API key, no cost, and no model choice are required for a scan. RunLore's
  investigation loop is LLM-first; there is no LLM-free mode.
- **Trivial to adopt.** `brew install k8sgpt` then `k8sgpt analyze` is a working cluster
  scan; `k8sgpt analyze --explain` layers an AI-generated explanation on top
  ([Quick Start](https://github.com/k8sgpt-ai/k8sgpt#quick-start)). RunLore's tools stay
  disabled until you wire up the backend behind each one ([data
  sources]({{< relref "/docs/concepts/data-sources.md" >}})) — a genuinely useful answer
  takes real configuration, not a single install command.
- **`Result`-as-CRD fits a Kubernetes-native workflow.** The
  [k8sgpt-operator](https://github.com/k8sgpt-ai/k8sgpt-operator) writes findings as
  `Result` custom resources — `kubectl get results -n k8sgpt-operator-system` — so scan
  output composes with the rest of the cluster's declarative tooling (RBAC-scoped,
  watchable, GitOps-friendly) instead of living in a separate UI or log stream.

## What RunLore does that k8sgpt does not

- **k8sgpt is a detector with optional per-finding LLM explanation; RunLore is a
  multi-step investigation loop.** Even with `--explain`, k8sgpt sends each analyzer
  failure to the AI **in isolation** — one prompt per finding, built only from that
  finding's own error text
  ([`GetAIResults`](https://github.com/k8sgpt-ai/k8sgpt/blob/main/pkg/analysis/analysis.go)).
  It does not correlate metrics, logs, network flows, and GitOps history into a single
  ranked, evidence-backed root cause the way RunLore's investigation loop does.
- **No memory across incidents.** k8sgpt caches raw AI responses keyed to identical
  error text — a cost-saving de-dup, not a knowledge store (same file, `getAIResultForSanitizedFailures`).
  A new phrasing of the same problem, or the same problem on a different resource, is a
  cache miss and starts from zero. RunLore curates each **verified** finding into a pull
  request against a Git repo you own, and a note's trust decays if it stops matching
  real-world outcomes. See the [learning loop]({{< relref "/docs/concepts/learning-loop.md" >}}).
- **No knowledge curation.** k8sgpt supports
  [custom analyzers](https://github.com/k8sgpt-ai/k8sgpt#custom-analyzers) you write
  yourself — a static, human-authored extension point. Nothing in the documented
  architecture generates or refines an analyzer, or any other persisted knowledge, from
  what a scan observed. RunLore's catalog is built from its own investigations,
  human-reviewed via pull request.

## At a glance

| | RunLore | k8sgpt |
|---|---|---|
| What it is | Multi-signal investigation agent | Deterministic Kubernetes cluster analyzer, optional LLM explain |
| LLM required | Yes, for the investigation loop | No — [only for `--explain`](https://github.com/k8sgpt-ai/k8sgpt/blob/main/pkg/analysis/analysis.go) |
| Investigation breadth | Metrics, logs, network flows, cloud control plane, Kubernetes, GitOps, source repos ([data sources]({{< relref "/docs/concepts/data-sources.md" >}})) | Kubernetes API objects only — [14 default + 17 optional built-in analyzers](https://github.com/k8sgpt-ai/k8sgpt#built-in-analyzers) |
| Learning loop | Yes — [outcome-weighted recall]({{< relref "/docs/concepts/learning-loop.md" >}}), decays on real resolve-rate | No — [per-finding response cache](https://github.com/k8sgpt-ai/k8sgpt/blob/main/pkg/analysis/analysis.go), not a knowledge store |
| Knowledge portability | Plain markdown in your Git repo, OKF-compatible | Not applicable — no persisted knowledge to port |
| GitOps "what changed" | Revision-exact diff, Flux + Argo CD ([data sources]({{< relref "/docs/concepts/data-sources.md" >}})) | Not documented — no ArgoCD/Flux integration in the repository |
| Findings as Kubernetes objects | Not applicable — findings live in the knowledge catalog | Yes — [`Result` CRD](https://github.com/k8sgpt-ai/k8sgpt-operator) via the operator |
| Project maturity | Solo-maintained, pre-1.0 | [CNCF Sandbox](https://www.cncf.io/projects/k8sgpt/) (Dec 2023), Apache-2.0, [~120 contributors](https://github.com/k8sgpt-ai/k8sgpt/graphs/contributors) |
| MCP | Client and server ([MCP]({{< relref "/docs/configuration/mcp.md" >}})) | Server only — [12 tools](https://github.com/k8sgpt-ai/k8sgpt/blob/main/MCP.md), not documented as an MCP client |

## Complementary, not competing

These aren't really rival tools. k8sgpt is a fast, cheap first pass that runs before an
LLM is even in the picture, and its analyzers produce exactly the kind of deterministic,
per-resource finding that could feed a deeper multi-signal investigation. Nothing in
RunLore today reads a k8sgpt `Result` as an input — but the shapes fit, and a cluster
already running both loses nothing.

## Pick k8sgpt instead if…

- You want fast, deterministic cluster analysis and don't want an LLM in the path at all.
- You want the lightest possible thing to run — a single binary, no GitOps engine, no
  metrics/logs backend to wire up first.
- You want a CNCF-governed project with a large community and a Kubernetes-native
  `Result` CRD your existing tooling can already watch.

## Sources

- [k8sgpt GitHub repository](https://github.com/k8sgpt-ai/k8sgpt) — license, stars, description, checked 2026-08-02.
- [k8sgpt contributors graph](https://github.com/k8sgpt-ai/k8sgpt/graphs/contributors) — contributor count (verified via the GitHub API), checked 2026-08-02.
- [CNCF — K8sGPT project page](https://www.cncf.io/projects/k8sgpt/) — CNCF Sandbox acceptance date, checked 2026-08-02.
- [CNCF Sandbox onboarding issue — K8sGPT](https://github.com/cncf/sandbox/issues/38) — Sandbox application record, checked 2026-08-02.
- [k8sgpt README](https://github.com/k8sgpt-ai/k8sgpt) — Quick Start, built-in analyzers list, MCP section, checked 2026-08-02.
- [k8sgpt `pkg/analysis/analysis.go`](https://github.com/k8sgpt-ai/k8sgpt/blob/main/pkg/analysis/analysis.go) — `explain` flag gating AI client construction, per-finding `GetAIResults`, response cache, checked 2026-08-02.
- [k8sgpt `MCP.md`](https://github.com/k8sgpt-ai/k8sgpt/blob/main/MCP.md) — MCP server tool count, checked 2026-08-02.
- [k8sgpt-operator README](https://github.com/k8sgpt-ai/k8sgpt-operator) — `Result` CRD, `kubectl get results`, checked 2026-08-02.
