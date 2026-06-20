# Prior art & positioning

Where RunLore sits relative to the open-source and commercial AI-SRE landscape (mid-2026). This is
the honest version: the autonomous "alert → RCA → Slack" runtime is a **commodity**; RunLore's reason
to exist is the combination of **GitOps-/metrics-agnostic** + **what-changed Git-diff spine** +
**open, learning knowledge catalog**.

## Open source

| Project | What it is | Overlap with RunLore | What we borrow |
|---|---|---|---|
| **k8sgpt** (CNCF) | Deterministic *analyzers* + optional LLM explanation; operator does continuous scan → `Result` CRD + Slack sinks. Strictly read-only. | Shallow on investigation (single-shot `analyze`, no loop, no cross-signal correlation, no Git diff). | The **analyzer-first** idea: cheap deterministic detectors before the LLM; `Result`-as-CRD; pluggable backends; anonymization. |
| **HolmesGPT** (CNCF, Robusta) | The strongest OSS reference: a ReAct loop over 50+ **toolsets**, **runbooks**, and an **MCP client**; autonomous alert→RCA→Slack via the Robusta platform. | **Heavy** — this is the closest thing to RunLore's runtime. But it is Prom/Loki/Datadog-centric, has no Flux-revision-aware Git diffing, and relies on *your* hand-curated runbooks (it does not learn). | The **toolset abstraction**, **runbook format**, ReAct + payload discipline, read-only-by-default posture. |
| **kagent** (CNCF, Solo.io) | In-cluster *declarative* agent framework (agents as CRDs, MCP + A2A, Google ADK engine). | The in-cluster reactive-agent runtime; generic, Prom/Grafana-centric, pre-1.0. | The declarative/GitOps-native packaging idea; could be a deployment target via A2A later. |
| **Aurora** | OSS AI-SRE with hybrid RAG over runbooks/postmortems. | The RAG/learning angle. | Hybrid retrieval (BM25 + vector) as table stakes for grounding. |

**Takeaway:** the loop and the runtime are solved in OSS. **Nobody in OSS auto-fills a human-readable,
git-versioned knowledge catalog from its own investigations.**

## Commercial

The market has converged on one shape — *live model of prod → agentic RCA → evidence in Slack* — and
competes on **autonomy ceiling**, **RCA depth**, and **provable eval/trust**, not features. The
shipped default everywhere is **read-only RCA + suggest-with-approval**; only Microsoft's Azure SRE
Agent natively takes approved write actions at GA.

- **Specialized**: Cleric, Resolve.ai, Traversal, Parity, NeuBird, Causely, ewake (EU/data-residency),
  incident.io, Flip AI — all closed.
- **Incumbents**: Datadog (Bits AI SRE), PagerDuty (SRE Agent), Grafana (Sift/Assistant), Dynatrace
  (Davis), New Relic, Splunk/Cisco, AWS (DevOps Agent), Google (Gemini Cloud Assist), Elastic,
  ServiceNow.
- **Memory / learning** is claimed by Cleric (Decision Model), Resolve (context layer), PagerDuty
  ("context flywheel"), and Google (AI Insights) — **all closed**.

**The reality check (ITBench, IBM/ICML 2025):** independently, frontier models identify the root
cause **< 50 %** of the time and *fully resolve* only **~11–14 %** of real K8s incidents. Treat
sub-50 % as the baseline; design for failure; be skeptical of vendor accuracy claims.

## Where RunLore is different

1. **GitOps-engine- and metrics-backend-agnostic** (Flux + Argo, VM + Prom) — the stack the OSS
   incumbents treat as second-class.
2. **A "what changed" spine** — Flux/Argo revision history + a real **Git diff between deployed
   revisions**, scoped to the affected workload. Exists nowhere else.
3. **An open, compounding knowledge catalog** ([OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog))
   the agent reads (cached) and writes (PR-gated). The closed tools that learn keep the knowledge;
   RunLore's is portable markdown in *your* git, with provenance linking the issue, the causing
   change, and the fix.
