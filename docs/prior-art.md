# Prior art & positioning

Where RunLore sits in the AI-SRE landscape (mid-2026). The honest version: the autonomous
"alert → RCA → Slack" runtime is a **commodity**, and change-diff RCA is **no longer unique**. RunLore's
defensible reason to exist is the **combination** the open tools don't have — an **open, reviewable,
outcome-weighted** knowledge catalog, grounded in the **exact GitOps change**, from an agent that is
**honest about the sub-50% reality** — all self-hostable.

## Open source

| Project | What it is | Learns? | vs RunLore |
|---|---|---|---|
| [**HolmesGPT**](https://github.com/HolmesGPT/holmesgpt) (CNCF Sandbox, Robusta) | Strongest OSS ReAct agent: ~60 toolsets spanning every major observability vendor (Datadog, New Relic, Splunk, Sentry, ES/OS, VictoriaMetrics…), runbooks, MCP client — and since v0.34 an **approval-gated remediation** path (signed approval tickets). | **No** — stateless, human-authored runbooks; every incident starts from zero. | The one to beat on investigation breadth — we can't and shouldn't out-toolset it. What it lacks is ours: **no learning** (its commercial parent sells the intelligence layer), an ArgoCD toolset but **no Flux** and **no revision-exact what-changed diff**. We borrow its toolset + runbook discipline. |
| [**OpenSRE**](https://github.com/swapnildahiphale/OpenSRE) (Apache-2.0, self-hosted) | LangGraph multi-agent (planner → subagents → synthesizer); provider-agnostic via LiteLLM; 46 skills (Datadog, Grafana, PagerDuty, Elasticsearch, K8s, AWS…). Single maintainer; the public repo is a periodic sync of a private one. | **Yes** — episodic memory + a Neo4j knowledge graph of service topology; recalls what worked on similar past incidents. | The closest rival on the learning loop — it genuinely learns, unlike Holmes/k8sgpt. But its automatic learning is **ungated**: every investigation is auto-stored as an episode with no review (an in-app admin approval queue exists only for a secondary agent-"teachings" channel whose backing service isn't in the OSS repo). RunLore's is human-gated markdown in *your* git — every entry a reviewable PR with provenance, confidence decaying by real-world outcome. Our CI eval harness even ships a poisoned-entry scenario proving bad knowledge is rejected at recall time. *(Naming hazard: the unrelated [Tracer-Cloud "opensre"](https://github.com/tracer-cloud/opensre) framework (~9k stars) absorbs most of the "OpenSRE" search traffic — don't confuse the two, and expect evaluators to.)* |
| [**k8sgpt**](https://github.com/k8sgpt-ai/k8sgpt) (CNCF) | Deterministic analyzers + optional LLM-explain. | No | A *detector*, not an investigation loop. We borrow analyzer-first + `Result`-as-CRD. |
| [**kagent**](https://github.com/kagent-dev/kagent) (CNCF Sandbox, Solo.io) | Declarative in-cluster agent framework; ships pgvector **agent memory** + `requireApproval` HITL. | Memory, but **opaque** — view-only vectors, no export/edit, per-agent. | Closest on "memory" — but closed-shape. RunLore's knowledge is open, reviewable, portable. Possible deploy target via A2A. |
| [**Aurora**](https://github.com/Arvo-AI/aurora) (Arvo AI, Apache-2.0) | LangGraph agents running kubectl/aws/az/gcloud in sandboxed pods; **deployment diffs + Terraform/IaC analysis** as RCA input; RAG knowledge base "that grows over time" (Postgres + Weaviate + Memgraph); "Actions": auto-postmortems, **fix PRs**, Slack. Aggressive comparison-marketing vs Holmes/k8sgpt. | **Yes** — auto-ingested RAG KB. But locked in its databases: no review gate, no git export, no outcome signal. | **The fastest-moving OSS threat.** It has diffs, a KB, and PR machinery — combining them into knowledge-PRs is one feature away. Our structural answer: user-owned markdown, human review, outcome weighting. |

**Takeaway:** no OSS agent auto-fills an **open, git-versioned, PR-reviewed, outcome-weighted** catalog
from its own investigations. *Reviewability + portability* — not learning-vs-not — are the distinction.

## Change-aware RCA — no longer ours alone

"What changed" is the #1 RCA lever, and others now ship it:

- **Komodor** — workload-scoped **manifest-diff RCA** with Argo CD/Flux integration (the only
  commercial vendor with explicit Flux support); Gartner 2026 "Representative Vendor". As of
  [**Klaudia Memory** (2026-07-21)](https://komodor.com/platform/klaudia-ai-powered-troubleshooting/)
  it also keeps persistent incident memory — the closest commercial analogue to RunLore overall,
  now on both the diff spine *and* the learning loop. Its memory is closed and non-exportable;
  ours is the differentiator that remains.
- **Anyshift** (ex-driftctl) — GraphRAG RCA over a **git-versioned infrastructure graph**; owns the
  "what changed" narrative.
- **Datadog Change Tracking** — commit-level deploy correlation via APM deployment SHAs (`git log`
  between deployments; no GitOps-revision semantics). Bits AI SRE went GA Dec 2025 with closed
  investigation memory; its effective per-investigation price dropped ~75% in 2026 (from $25 to
  ~$6.50 via pooled ["AI Credits"](https://www.nobs.tech/blog/datadog-bits-ai-pricing-ai-credits-governance)) —
  per-investigation economics are racing to the bottom, which favors a BYO-model OSS entrant.

RunLore's genuinely-owned slice is narrow: the **rendered diff between the two commits Flux/Argo
actually reconciled**, scoped to the failing workload, as the *primary* RCA spine. It is a sharp
feature, not a moat — treat it as **provenance for the knowledge**, not the headline.

## Commercial

Converged on one shape — *live model of prod → agentic RCA → evidence in Slack* — competing on the
**autonomy ceiling**. The shipped default everywhere is read-only RCA + suggest-with-approval; only
**Microsoft Azure SRE Agent** (GA, Mar 2026) takes approved writes natively.

- **Specialists:** Cleric, Resolve.ai, Traversal, Parity, NeuBird, Causely, Flip AI, ewake (EU residency).
- **Incumbents:** Datadog, PagerDuty, Grafana, Dynatrace, New Relic, AWS, Google, ServiceNow.
- **Memory/learning** is now the specialists' *whole pitch* (Cleric "first self-learning AI SRE",
  Komodor Klaudia Memory, NeuBird's vector-DB KB, AWS "Learned Skills", New Relic Knowledge) —
  and **all of it is closed and non-exportable, without exception**. Portability + review is the
  unoccupied position, not "learning".

> [!NOTE]
> **The market is frothy and under scrutiny**
>
> Resolve.ai raised a $1B-headline Series A (Dec 2025) on ~$4M ARR; Gartner places agentic AI at the
> "Peak of Inflated Expectations" (>40% of agentic projects predicted cancelled by 2027) and flags
> "agent-washing." Pressure-test every "autonomous"/"%MTTR" claim against the vendor's own docs.

## The eval reality

**ITBench** (IBM/ICML 2025) — frontier models identify the root cause **< 50%** of the time and fully
resolve only **~11–14%** of real K8s incidents. The successor
[**ITBench-AA**](https://artificialanalysis.ai/evaluations/itbench-aa) (Artificial Analysis + IBM,
May 2026) confirms it: the best frontier model scores **47%** on agentic IT tasks, with "confusing
symptoms for root causes" the classic failure — while vendors market 92–94% accuracy. Treat sub-50%
as the baseline; design for failure; make honest uncertainty a feature, not a footnote — and publish
reproducible numbers (see the eval scorecard) where vendors publish claims.

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
