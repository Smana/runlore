# Prior art & positioning

Where RunLore sits in the AI-SRE landscape (mid-2026). The honest version: the autonomous
"alert → RCA → Slack" runtime is a **commodity**, and change-diff RCA is **no longer unique**. RunLore's
defensible reason to exist is the **combination** the open tools don't have — an **open, reviewable,
outcome-weighted** knowledge catalog, grounded in the **exact GitOps change**, from an agent that is
**honest about the sub-50% reality** — all self-hostable.

## Open source

| Project | What it is | Learns? | vs RunLore |
|---|---|---|---|
| [**HolmesGPT**](https://github.com/HolmesGPT/holmesgpt) (CNCF Sandbox, Robusta) | Strongest OSS ReAct agent: 60+ toolsets, runbooks, MCP client; read-only. | **No** — stateless, human-authored runbooks. | The one to beat. But Prom/Loki-centric, no Flux/Argo revision-diff, no learning. We borrow its toolset + runbook discipline. |
| [**OpenSRE**](https://github.com/swapnildahiphale/OpenSRE) (Apache-2.0, self-hosted) | LangGraph multi-agent (planner → subagents → synthesizer); provider-agnostic via LiteLLM; ~46 skills (Datadog, Grafana, PagerDuty, Elasticsearch, K8s, AWS…). | **Yes** — episodic memory + a Neo4j knowledge graph of service topology; recalls what worked on similar past incidents. | The closest rival on the learning loop — it genuinely learns, unlike Holmes/k8sgpt. But its memory is an **internal store** (episodic memory + Neo4j); RunLore's is human-gated markdown in *your* git — every entry a reviewable PR with provenance, confidence decaying by real-world outcome. Our CI eval harness even ships a poisoned-entry scenario proving bad knowledge is rejected at recall time. |
| [**k8sgpt**](https://github.com/k8sgpt-ai/k8sgpt) (CNCF) | Deterministic analyzers + optional LLM-explain. | No | A *detector*, not an investigation loop. We borrow analyzer-first + `Result`-as-CRD. |
| [**kagent**](https://github.com/kagent-dev/kagent) (CNCF Sandbox, Solo.io) | Declarative in-cluster agent framework; ships pgvector **agent memory** + `requireApproval` HITL. | Memory, but **opaque** — view-only vectors, no export/edit, per-agent. | Closest on "memory" — but closed-shape. RunLore's knowledge is open, reviewable, portable. Possible deploy target via A2A. |
| **Aurora** (Arvo AI) | Hybrid RAG over docs; auto-generates postmortems. | Postmortems, not runbook self-update. | The RAG angle. We borrow hybrid retrieval. |

**Takeaway:** no OSS agent auto-fills an **open, git-versioned, PR-reviewed, outcome-weighted** catalog
from its own investigations. *Reviewability + portability* — not learning-vs-not — are the distinction.

## Change-aware RCA — no longer ours alone

"What changed" is the #1 RCA lever, and others now ship it:

- **Komodor** — workload-scoped **manifest-diff RCA** with Argo CD/Flux integration; Gartner 2026
  "Representative Vendor". The closest commercial analogue to our diff spine.
- **Anyshift** (ex-driftctl) — GraphRAG RCA over a **git-versioned infrastructure graph**; owns the
  "what changed" narrative.
- **Datadog Change Tracking** — commit-level deploy correlation (no file-level source diff).

RunLore's genuinely-owned slice is narrow: the **rendered diff between the two commits Flux/Argo
actually reconciled**, scoped to the failing workload, as the *primary* RCA spine. It is a sharp
feature, not a moat — treat it as **provenance for the knowledge**, not the headline.

## Commercial

Converged on one shape — *live model of prod → agentic RCA → evidence in Slack* — competing on the
**autonomy ceiling**. The shipped default everywhere is read-only RCA + suggest-with-approval; only
**Microsoft Azure SRE Agent** (GA, Mar 2026) takes approved writes natively.

- **Specialists:** Cleric, Resolve.ai, Traversal, Parity, NeuBird, Causely, Flip AI, ewake (EU residency).
- **Incumbents:** Datadog, PagerDuty, Grafana, Dynatrace, New Relic, AWS, Google, ServiceNow.
- **Memory/learning** (Cleric, Resolve, PagerDuty, Google) is **all closed**.

> [!note] The market is frothy and under scrutiny
> Resolve.ai raised a $1B-headline Series A (Dec 2025) on ~$4M ARR; Gartner places agentic AI at the
> "Peak of Inflated Expectations" (>40% of agentic projects predicted cancelled by 2027) and flags
> "agent-washing." Pressure-test every "autonomous"/"%MTTR" claim against the vendor's own docs.

## The eval reality

**ITBench** (IBM/ICML 2025) — frontier models identify the root cause **< 50%** of the time and fully
resolve only **~11–14%** of real K8s incidents. Treat sub-50% as the baseline; design for failure; make
honest uncertainty a feature, not a footnote.

## Where RunLore is different

1. **Open, reviewable, outcome-weighted knowledge** — portable markdown in *your* git, PR-reviewed,
   provenance-tracked, with trust that decays on real-world resolve-rate. The closed tools that learn
   keep the knowledge; the OSS agents don't learn at all.
2. **Honest by construction** — read-only-first, `unresolved` as a first-class output, an adversarial
   verify pass that can only *lower* confidence, and a shipped eval harness. The best-aligned response
   to the sub-50% reality in a market full of autonomy-theatre.
3. **GitOps-revision-exact "what changed"** — the provenance that makes the knowledge trustworthy.
4. **Self-hostable & model-agnostic** — for lock-in-averse, sovereignty-conscious, GitOps-native teams.

> **On OKF.** RunLore's catalog is [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog)-compatible
> markdown. OKF is a young Google spec (v0.1 draft, mid-2026, no CNCF governance yet); we treat it as a
> portable interop format, not a settled standard — the value is the open, reviewable, git-versioned
> shape, which holds regardless of OKF's trajectory.
