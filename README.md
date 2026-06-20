<div align="center">

<img src="assets/logo.png" alt="RunLore" width="160" />

# RunLore

**A self-improving SRE agent for _any_ incident.**
Reacts to any alert, investigates the likely cause across your signals — sharpest at _what changed_, deepest on GitOps — and learns from every resolution.

[![CI](https://github.com/Smana/runlore/actions/workflows/ci.yaml/badge.svg)](https://github.com/Smana/runlore/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/Smana/runlore)](https://goreportcard.com/report/github.com/Smana/runlore)
[![Go Version](https://img.shields.io/github/go-mod/go-version/Smana/runlore)](go.mod)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-early%20development-orange)](docs/design.md)

</div>

---

RunLore wakes on **any incident** — a metrics or log alert, an SLO burn, a network or infra anomaly —
*whatever the cause*. It **investigates** by forming and testing hypotheses across your signals: the
sharpest first question is **what changed** (deploys, config, images, infra, certs, scaling — and on a
GitOps platform, the *exact* Git diff), alongside the causes nothing changed for — saturation, network
drops, node health, dependency outages, load. It **delivers** a root-cause hypothesis with evidence,
then **learns**, writing each resolved incident into an open, git-versioned knowledge base that makes
the next investigation faster. Read-only first; a single Go binary; runs in your terminal or in your
cluster, on your models.

## Why another one?

The autonomous *alert → RCA → Slack* loop is already a **commodity**. RunLore's bet is the part that
isn't: a **GitOps-native "what changed" spine** and an **open knowledge base that compounds**.

| | What it is | What RunLore adds |
|---|---|---|
| [**k8sgpt**](https://github.com/k8sgpt-ai/k8sgpt) | A *detector* — analyzers + LLM explanation | An investigation loop, cross-signal correlation, real Git diffs, and learning |
| [**HolmesGPT**](https://github.com/HolmesGPT/holmesgpt) | The strongest OSS investigation agent | Prometheus/Loki-centric and relies on *your* hand-curated runbooks (it doesn't learn); RunLore is metrics-agnostic, what-changed-first, and self-improving |
| [**kagent**](https://github.com/kagent-dev/kagent) | A generic in-cluster agent *framework* | A focused, opinionated SRE agent (and RunLore can run *on* kagent later) |

RunLore is **GitOps-engine-agnostic** (Flux + Argo), **metrics-backend-agnostic** (VictoriaMetrics +
Prometheus), and the only one that **learns into an open** [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog)
knowledge catalog you own — portable markdown in git, never vendor lock-in.

## How it works

```mermaid
flowchart LR
    A["Incident<br/>any alert · event"] -->|"trigger policy<br/>(prod · critical · ns…)"| B
    subgraph B["🔎 Investigate · form & test hypotheses"]
      direction TB
      W["what changed?<br/>deploys · infra · certs · scaling<br/>(GitOps → exact Git diff)"]
      C["what's wrong?<br/>saturation · network · nodes · deps"]
    end
    B --> R["🎯 Root cause<br/>+ evidence + suggested fix"]
    R -->|"read-only"| D["💬 Deliver<br/>Slack · Matrix"]
    R -. learn .-> K[("📚 OKF knowledge<br/>catalog · git")]
    K -. instant recall .-> B
```

## Three pillars

| | |
|---|---|
| **React** | incident/alert webhook gated by a **trigger policy** (only prod, only critical, by namespace/team/label) · GitOps failure events · proactive watch · on-demand CLI / chat |
| **Investigate** | forms & tests hypotheses across **what changed** (deploys/infra/certs/scaling — on GitOps, the exact Git diff) **and no-change causes** (saturation/network/nodes/deps) · runbook-grounded · confidence + explicit `unresolved` |
| **Learn** | reads a cached [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog) catalog (instant recall) and writes new incidents back via reviewed PRs — knowledge compounds in *your* git |

## Design principles

- **Cause-agnostic** — reacts to any incident and investigates any cause; "what changed" is the sharpest lens (deepest on GitOps), not the only one.
- **Read-only first** — v1 ships no cluster-mutating tools (rung 0 of an autonomy ladder; see [`docs/design.md`](docs/design.md)).
- **GitOps- and metrics-agnostic** — Flux + ArgoCD, VictoriaMetrics + Prometheus; logs/network pluggable.
- **Single static Go binary** — terminal (`lore investigate`) or in-cluster (`lore serve`).
- **Model-agnostic** — Anthropic or any OpenAI-compatible endpoint (in-cluster vLLM, Ollama…); your telemetry needn't leave the boundary.
- **Built-in core providers, MCP as the extension layer** — self-contained, but composable.
- **Pluggable notifications** — Slack + Matrix first; PagerDuty and incident.io next.

## Quickstart

> Early development — the React foundation (`lore serve`) works today; the investigation loop is landing.

```bash
go build ./...

# end-to-end demo: fire mocked Alertmanager alerts through the trigger policy
hack/demo.sh

# run the agent: react to incident webhooks per your trigger policy
lore serve --config runlore.yaml
```

## Status & docs

- 📐 [Design](docs/design.md) · [Prior art & positioning](docs/prior-art.md) · [Plans](docs/plans/)
- ✅ Phase 1 — React foundation (trigger policy + `lore serve`)
- 🚧 What-changed spine (Git revision diffing) → GitOps providers (Flux/Argo) → correlation → catalog → investigation loop

## License

[Apache-2.0](LICENSE).
