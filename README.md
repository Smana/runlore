<div align="center">

<img src="assets/logo.png" alt="RunLore" width="160" />

# RunLore

**An open-source SRE agent that investigates any incident — and learns _your_ platform as it goes.**

[![CI](https://github.com/Smana/runlore/actions/workflows/ci.yaml/badge.svg)](https://github.com/Smana/runlore/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/Smana/runlore)](https://goreportcard.com/report/github.com/Smana/runlore)
[![Go Version](https://img.shields.io/github/go-mod/go-version/Smana/runlore)](go.mod)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-early%20development-orange)](docs/design.md)

</div>

---

RunLore is an **on-call teammate that never sleeps**. It wakes on any incident — *whatever the cause* —
and works it like a good SRE:

- **What changed?** *(the sharpest first question)* — deploys, config, images, infra, certs, scaling;
  on a GitOps platform, the **exact Git diff**.
- **What's wrong?** *(when nothing changed)* — saturation, network drops, node health, dependency
  outages, load.

…then it hands you a confidence-scored root cause with evidence and **learns** — each resolved incident
becomes a reviewed entry in an open, git-versioned knowledge base, so it gets sharper at **your**
platform over time.

**Read-only first · single Go binary · in your terminal or your cluster · on your models.**

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

**…and here's what lands in chat** — a real RunLore investigation delivered to Slack: ranked root cause with confidence, the evidence trail, read-only suggested next steps, open questions for a human, and a link to the knowledge-base entry it learned.

<div align="center">
<img src="assets/slack-notification.png" alt="RunLore Slack notification — HarborRegistryDown: 95% confidence root cause (Crossplane AccessKey hit the AWS IAM AccessKeysPerUser quota → Secret missing the username key → CreateContainerConfigError), with the evidence trail, suggested next steps, and open questions" width="760" />
</div>

## Three pillars

| | |
|---|---|
| **React** | incident/alert webhook gated by a **trigger policy** (only prod, only critical, by namespace/team/label) · GitOps failure events · proactive watch · on-demand CLI / chat |
| **Investigate** | forms & tests hypotheses across **what changed** (deploys/infra/certs/scaling — on GitOps, the exact Git diff) **and no-change causes** (saturation/network/nodes/deps) · runbook-grounded · confidence + explicit `unresolved` |
| **Learn** | reads a cached [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog) catalog (instant recall) and writes new incidents back via reviewed PRs — knowledge compounds in *your* git |

## Design principles

- **Cause-agnostic** — reacts to any incident and investigates any cause; "what changed" is the sharpest lens (deepest on GitOps), not the only one.
- **Read-only by default, full autonomy ladder when you want it** — `off` → `suggest` → `approve` (human-gated) → `auto` (unattended). Every rung above read-only is reversible-only, envelope-bounded, audited, and kill-switchable (see [`docs/design.md`](docs/design.md)).
- **GitOps- and metrics-agnostic** — Flux + ArgoCD, VictoriaMetrics + Prometheus; logs/network pluggable.
- **Cloud-aware (read-only)** — correlates the cloud control plane (AWS CloudTrail "what changed" + EC2/ASG/EKS health) using in-cluster identity (EKS Pod Identity / IRSA), so infra changes outside GitOps are in scope too.
- **Single static Go binary** — terminal (`lore investigate`) or in-cluster (`lore serve`).
- **Model-agnostic** — Anthropic, Google Gemini, or any OpenAI-compatible endpoint (in-cluster vLLM, Ollama…); your telemetry needn't leave the boundary.
- **Built-in core providers, MCP as the extension layer** — self-contained, but composable.
- **Pluggable notifications** — Slack + Matrix first; PagerDuty and incident.io next.

## Quickstart

**Deploy to a cluster** → **[Getting Started](docs/getting-started.md)**: create a knowledge-base repo,
a scoped GitHub App, the secrets, then `helm install`. **Hack on it** → **[CONTRIBUTING](CONTRIBUTING.md)**.

```bash
# try it locally, no cluster: fire mocked Alertmanager alerts through the trigger policy
hack/demo.sh

# verify every feature end-to-end on a throwaway k3d cluster
hack/e2e-k3d.sh

# run the agent against incident webhooks
lore serve --config runlore.yaml

# or investigate one incident on-demand from your terminal (prints the findings)
lore investigate --alert HarborProbeFailure --namespace apps --config runlore.yaml
```

## Status & docs

- 📐 [Design](docs/design.md) · 🚀 [Getting started](docs/getting-started.md) · 🛠 [Contributing](CONTRIBUTING.md) · [Prior art](docs/prior-art.md) · [Plans](docs/plans/)
- ✅ **End-to-end working** (verified on k3d and a live EKS cluster):
  - **React** — incident webhook (trigger policy + dedup) + GitOps failure watch (**Flux & Argo CD**)
  - **Investigate** — ReAct loop with 10 tools across every signal source + **instant recall** (skip the loop on a high-confidence catalog hit) + an **adversarial verify pass** (a skeptic model rejects correlation-only findings and re-calibrates confidence); model-agnostic (**Anthropic**, **Gemini**, or any OpenAI-compatible endpoint):
    - *GitOps* — `what_changed` (exact Git diff; resolves `GitRepository`/`OCIRepository`/`ExternalArtifact` sources), `flux_resource_status`, `flux_tree`, `controller_logs`
    - *Signals* — `query_metrics` (PromQL), `query_logs` (LogsQL), `network_drops` (Hubble)
    - *Cloud (AWS)* — `cloud_what_changed` (CloudTrail), `cloud_resource_health` (EC2/ASG/EKS) via EKS Pod Identity / IRSA, **read-only**
    - *Knowledge* — `kb_search`
  - **Deliver** — Slack (with interactive **Approve/Reject buttons**) + Matrix
  - **Learn** — OKF catalog (read + **git-sync**) + curator PRs/issues with a **lifecycle** (`triggered → investigating → solved`) → knowledge compounds; **re-run on demand** by adding the `reinvestigate` label to a curated issue
  - **Act** — full **autonomy ladder**: `off` → `suggest` → `approve` (curl or Slack buttons, token-gated) → `auto` (unattended, reversible-only, confidence-gated, rate-limited, **kill-switchable**, audited)
  - **Run** — `lore serve` (in-cluster, **HA via leader election**) or `lore investigate` (on-demand terminal); `lore eval` RCA benchmark; `lore catalog sync`. Packaged Helm chart + CI image build.
- 🚧 Next: more notifiers (PagerDuty, incident.io), MCP extension layer, proactive (non-incident) watch.

## License

[Apache-2.0](LICENSE).
