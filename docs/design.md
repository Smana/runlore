# RunLore — Design

> The self-improving, GitOps-native SRE agent. RunLore **reacts** to incidents, **investigates**
> by correlating *what changed* across your GitOps engine and observability stack, and **learns** —
> writing every resolved incident into an open, git-versioned knowledge catalog that makes the next
> investigation faster.

| | |
|---|---|
| **Status** | Draft `v0.1` (design) |
| **Date** | 2026-06-20 |
| **CLI** | `lore` |
| **Module** | `github.com/Smana/runlore` *(adjustable — could move to a dedicated org)* |
| **License** | Apache-2.0 |
| **Language** | Go (single static binary) |

---

## 1. Why this exists

On a modern cloud-native platform (Kubernetes + a GitOps engine + a metrics/logs stack), a *lot* of
incident investigation is **already solved interactively**: a human in an AI session, wired to the
right MCP servers and debugging skills (a GitOps MCP, metrics/logs MCPs, and the like), can trace a
failing rollout to its root cause today. Rebuilding that interactive experience would be reinvention.

Four things are **not** solved — and they are what RunLore is:

1. **Unattended operation.** Every existing piece needs a human in the loop, in a session. Nothing
   wakes at 03:00 on an alert, investigates, and hands you a root-cause hypothesis.
2. **Cross-signal correlation anchored on "what changed."** Today the GitOps engine, metrics, logs,
   and network are separate tools. Nobody **diffs Git between the two deployed revisions** to show
   the *actual* manifest/values delta and ties it to the metric/log/network impact.
3. **Learning.** Existing OSS agents rely on *your hand-curated* runbooks; they don't accumulate
   knowledge. Commercial tools that "learn" are all closed.
4. **Shareability.** None of the above is an installable product a *different* team can adopt.

RunLore fills exactly these gaps, and only these.

## 2. Positioning (honest prior art)

See [`prior-art.md`](prior-art.md) for the full landscape. The short version:

| Capability | OSS today | Commercial today | RunLore |
|---|---|---|---|
| Autonomous react → investigate → report | **HolmesGPT**; k8sgpt-operator/kagent partial | the whole market's commodity | **table stakes** (we do it too) |
| GitOps-/metrics-**agnostic** + "what-changed" Git-diff spine | nobody (all Prom/Loki/vanilla-k8s) | nobody | **differentiating wedge** |
| Self-filling, *learning*, **open** knowledge catalog | effectively nobody | Cleric/Resolve/PagerDuty/Google — all **closed** | **the moat** |

The autonomous runtime is a commodity — if that were all RunLore is, it would be "HolmesGPT, but Go
+ GitOps." The **open, compounding knowledge catalog** is the part that is closed-source-only
everywhere and absent from OSS. That, plus being **GitOps-engine- and metrics-backend-agnostic**, is
the reason to build it.

## 3. Goals / Non-goals

**Goals**
- Autonomous, **read-only-first** incident investigation that a team can `helm install`.
- **GitOps-engine-agnostic** (Flux + ArgoCD) and **metrics-backend-agnostic** (VictoriaMetrics +
  Prometheus) from day one; logs/network/cloud pluggable behind the same pattern.
- A **"what changed" spine**: revision history + real Git diffs as the investigation anchor.
- An **open OKF knowledge catalog** the agent reads (fast, cached) and writes (PR-gated) — knowledge
  is portable markdown in git, never vendor lock-in.
- **Single static Go binary**; runs in your terminal (`lore investigate`) or in-cluster (`lore serve`).
- Model-agnostic: Claude, your in-cluster vLLM, or Ollama (data needn't leave the boundary).

**Non-goals (for now)**
- Autonomous **remediation** of production — *in the initial versions*. Cluster-mutating actions
  are off; "writes" in early phases mean *markdown to git via reviewed PR*. This is the **first rung
  of an autonomy ladder, not a permanent constraint**: the architecture (§9, "Act") is built so
  *active tools* slot in later behind a policy gate, without re-architecting.
- Being an observability platform. RunLore *reads* your metrics/logs; it doesn't store them.
- Re-implementing interactive Flux/k8s debugging that `gitops-cluster-debug` + MCPs already do.
- Multi-agent / A2A orchestration. A single tight investigation loop first.

## 4. Core concepts

**`Change`** — the engine-agnostic unit of "what changed". Both Flux and ArgoCD reduce to *revision
history + a Git diff between revisions*, so the investigator and the knowledge entries are written
against `Change`, never against Flux or Argo directly. (See `internal/providers/providers.go`.)

**OKF knowledge catalog** — a git repo of markdown-with-frontmatter entries
([Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog)). Bundled default
runbooks ship as the initial catalog; learned incidents accrete on top. Same family as a Karpathy
LLM-wiki / auto-memory.

**Providers** — every backend the agent touches is a pluggable interface (GitOps, Metrics, Logs,
Network, Cloud, Model, Notifier, Issue). Core providers are **built-in** so the binary is
self-contained; MCP is the **extension** layer (consume existing/community MCP servers as extra
tools, but never *require* them).

## 5. Architecture

```
 triggers:  [ incident webhook (Alertmanager/VMAlert) ── trigger policy ── | GitOps failures | timer | Slack | CLI ]
                          │
          ┌──────── RunLore agent  (Go — `lore serve` / `lore investigate`) ────────┐
          │  Investigator — ReAct loop                                               │
          │   ├─ what-changed spine    (revision history + Git diff)                 │
          │   ├─ catalog retrieval      (cached OKF index — instant recall)          │
          │   ├─ runbook grounding      (OKF playbooks)                              │
          │   ├─ tool orchestration     (providers, built-in + MCP)                  │
          │   └─ hypothesis ranker + explicit `unresolved`                           │
          │  Curator — confidence-routed: known→recall · novel→PR · uncertain→Issue  │
          │  Catalog — syncer + local mirror + bleve/chromem-go index (kb_search)    │
          │  Model: Anthropic | OpenAI-compatible (in-cluster vLLM | Ollama)         │
          │  Audit log (append-only) → (P3) cross-incident memory                    │
          └───────────┬──────────────────────────────┬──────────────────────────────┘
        providers     │ (built-in clients)           │ git forge (issues/PRs)
   ┌──────┬───────────┼─────────┬────────┬─────────┐  └─► GitHub (now) / GitLab (later)
   ▼      ▼           ▼         ▼        ▼         ▼
 gitops  metrics    logs     network  cloud     model
 flux|   vm|prom    vl       hubble   aws        …
 argocd  (PromQL)
   │
   └─ what-changed: client-go (revision history) + go-git (diff between revisions)
```

Components map 1:1 to `internal/` packages (§13).

## 6. The three pillars

### React — wake without a human

The **primary** trigger is an **incident** (an alert from Alertmanager/VMAlert), gated by a
configurable **trigger policy** so RunLore investigates only what matters — not every alert. Noise,
relevance, and LLM cost are all controlled here.

- **Incident-triggered** (primary): an HTTP endpoint receives Alertmanager/VMAlert webhooks; a
  **trigger policy** decides which incidents start an investigation, by:
  - **environment** (e.g. `prod` only — matched on alert labels / namespace conventions),
  - **severity** (e.g. `critical` / `page` only),
  - **namespace / team / owner**, alert-name globs, and arbitrary label matchers,
  - **dedup / rate limits** (don't re-investigate a still-firing alert).
- **GitOps-failure-triggered** (secondary): `GitOpsProvider.WatchFailures()` surfaces `Ready=False`
  (Flux) / `Degraded`/`OutOfSync` (ArgoCD) → catch a bad rollout before a metrics alert fires.
- **Proactive watch** (Phase 3): periodic scan for SLO burn-rate / drift.
- **On-demand**: `lore investigate "<symptom>"` or a Slack mention (same engine, human-initiated).

Example trigger policy (`internal/config`, `config.TriggerPolicy`):

```yaml
triggers:
  incidents:
    enabled: true
    match:                              # ANDed; empty fields match anything
      severity:    [critical]           # only paging-grade
      environment: [prod]               # ignore staging/dev
      namespaces:  ["apps/*", "payments"]
      labels:      { team: platform }   # arbitrary label matchers
    ignore:
      alertnames:  [Watchdog, InfoInhibitor]
    dedup: { window: 30m }              # don't re-open a still-firing alert
  gitops_failures: { enabled: true }    # secondary trigger
```

### Investigate — correlate, grounded on "what changed"
1. **Instant recall**: `kb_search(symptom)` against the cached catalog. High-confidence known-pattern
   hit → short-circuit to the known resolution (no full loop). *(HolmesGPT data: ~40 % of incidents
   self-resolve on a runbook/pattern match; tool-calls drop 16→2.)*
2. **What changed**: build the ranked `Change` timeline around the incident window
   (`what_changed_near`), then `diff_revisions` for the actual landed delta.
3. **Ground**: retrieve relevant OKF runbooks/incidents and seed the loop.
4. **ReAct**: pull metrics (PromQL), logs, network, k8s state *just-in-time* via providers; form and
   test hypotheses.
5. **Output contract** (structured): ranked root cause(s) + **confidence** + `change_ref` +
   **evidence trail** + **suggested reversible action** + explicit **`unresolved`** (honest about what
   it couldn't determine — designed for the ITBench <50 % reality, §10).

### Learn — compound an open catalog
`retrieve → capture → curate → compound`, routed by confidence:

```
investigation result
  ├─ KB hit (known)          → post known resolution. No issue, no PR. (instant recall)
  ├─ novel + confident       → draft OKF entry as a PR; humans refine via review → merge → reindex
  └─ novel + uncertain       → open a GitHub ISSUE (findings + open questions);
                               humans answer in-thread; on resolve/`/kb` →
                               crystallize thread → OKF PR → merge → reindex
```
The catalog only grows from **genuinely novel, human-sharpened** incidents. Every learned entry cites
the **issue** (reasoning), the **causing** change, and the **fixing** change — provenance no closed
"memory" gives you.

### Act — evolve toward (gated) action *(future)*

Read-only is the **starting posture, not the ceiling.** RunLore is designed to climb an **autonomy
ladder** as eval earns trust — without re-architecting, because an action is just *a tool with extra
metadata behind a policy gate*:

```
read-only  →  suggest (PR/command)  →  approve-to-execute (human click)  →  bounded auto
rung 0         rung 1                   rung 2                              (reversible + low-blast
(v1)                                                                        + non-critical only)
```

Every action tool declares `mutating` / `reversible` / `blastRadius` / `target`; an **action policy**
(mirroring the trigger policy) sets the **mode** (`off | suggest | approve | auto`), scoped by
environment, reversibility, and blast radius. v1 ships `mode: off` and registers no action tools —
adding remediation later is *enabling a gated capability + config*, not new architecture. The
metadata already exists (`providers.Action`, `config.ActionPolicy`) so nothing has to be retrofitted.

## 7. Provider abstraction

Interfaces live in `internal/providers/providers.go`. "For the moment" impls:

| Provider | Interface | Impls now | Later |
|---|---|---|---|
| GitOps | `GitOpsProvider` | **Flux**, **ArgoCD** | — |
| Metrics | `MetricsProvider` | **VictoriaMetrics**, **Prometheus** (one PromQL impl, 2 endpoints) | — |
| Logs | `LogsProvider` | **VictoriaLogs** | Loki, … |
| Network | `NetworkProvider` | **Hubble** | — |
| Cloud | `CloudProvider` | **AWS** (Steampipe) | — |
| Model | `ModelProvider` | **Anthropic**, **OpenAI-compatible** (vLLM/Ollama) | — |
| Notifier | `Notifier` | **Slack** | — |
| Issue | `IssueProvider` | **GitHub** | GitLab |

**Why the GitOps abstraction is real, not hand-wavy** — both engines reduce to *revision history +
git diff*:

| | Flux | ArgoCD |
|---|---|---|
| revision history | `HelmRelease`/`Kustomization` `.status.history` + `lastAppliedRevision` | `Application.status.history[]` |
| deployed now | source `GitRepository` revision | `status.sync.revision` |
| failure (React) | `Ready=False`, source `FetchFailed` | `health=Degraded`, `sync=OutOfSync` |
| the diff | go-git between revisions over `spec.path` | go-git over `source.path` (Argo also has native `app diff`) |

**Auto-discovery**: detect `argoproj.io/Application` → ArgoCD; `helm.toolkit.fluxcd.io` → Flux; probe
the metrics endpoint → VM vs Prometheus. Config overrides. Flux + VictoriaMetrics is the primary
reference combo; Argo + Prometheus exercises the abstraction.

## 8. The knowledge catalog & its cache

**Source of truth = a git repo** (OKF bundle: markdown + YAML frontmatter; `index.md` + `log.md`
reserved; entries link to assert relationships). Reviewed, versioned, portable.

**The agent never queries git at investigation time.** A `Catalog` subsystem keeps it fast:

```
KB git repo  ──syncer──►  local mirror  ──build──►  index:  bleve (BM25)  [+ chromem-go (vectors)]
  (truth)    (poll HEAD ±webhook/Receiver)              ▲
                                          kb_search(query, k)  — in-process, sub-ms, no git/network
```
- **Syncer** polls remote `HEAD` (cheap), pulls on change, **incrementally re-indexes** changed
  entries. Optional push-webhook / Flux `Receiver` for instant refresh.
- **Index** is embedded + persisted: **`bleve`** (BM25) in v1 — no embedding dependency, genuinely
  "easy"; **`chromem-go`** (pure-Go vectors) added later, embeddings served by the in-cluster vLLM.
  Both pure-Go → the single binary holds the whole retrieval stack.
- **Sync mechanism** (default A; B is the in-cluster upgrade):
  - **A — built-in self-syncer** (poll + optional webhook). Fewest moving parts. *Default.*
  - **B — Flux `GitRepository` + `Receiver`** — the KB syncs like every other source; Flux notifies a reload.
  - **C — `git-sync` sidecar + shared volume** — classic decoupling.

## 9. Safety & trust model

- **Read-only-first, by construction.** v1 ships **no cluster-mutating tools**. The providers and
  any wired MCP servers are read-only. Read-only is structural, not a prompt instruction.
- **"Writes" mean markdown-to-git via reviewed PR** + opening issues — never touching prod. The
  Curator is cluster-read-only.
- **Scoped identity.** In-cluster, the agent runs under a least-privilege, read-mostly identity
  (a scoped ServiceAccount; or EKS Pod Identity / Workload Identity on managed clusters).
- **Append-only audit log** of every tool call + decision (feeds eval + trust).
- **Honest uncertainty.** `unresolved` is a first-class output field; the agent says what it doesn't
  know rather than hallucinating.
**Designed to evolve — the autonomy ladder.** Read-only-first is rung 0, not the ceiling. When action
tools are introduced (Phase 4+), they are gated by an **action capability model** + **action policy**
(`providers.Action` + `config.ActionPolicy`):
- every action declares `mutating` / `reversible` / `blastRadius` / `target`;
- the policy sets the **mode** (`off | suggest | approve | auto`), scoped by environment,
  reversibility, and blast radius — **irreversibility is the trip-wire for mandatory human approval**;
- **dry-run / preview by default**, **append-only audit**, and **rollback** for anything applied;
- scoped agent identity (RBAC) is the hard backstop regardless of policy.

These types exist from day one (carrying no enabled actions in v1) precisely so the read-only→active
evolution needs no retrofit.

## 10. Evaluation

The independent benchmark (**ITBench**, IBM/ICML 2025) found frontier models identify the root cause
**< 50 %** of the time and fully resolve **~11–14 %** of real K8s incidents. RunLore treats sub-50 %
as the baseline and makes failure handling a first-class primitive.

- **Deterministic core is unit-tested**: the `Change` timeline + `diff_revisions` are mechanical →
  Go table tests over recorded cluster+Git fixtures. No flaky LLM scoring for the spine.
- **Replay harness** (`lore eval`): snapshot real past incidents (your metrics/logs history),
  replay them offline, score **end-state root-cause identification** via LLM-as-judge. Optionally
  driven by `promptfoo`.
- Ship the eval harness so users can **trust** the agent against *their* incidents and contributors
  can't regress it.

## 11. Tech stack

- **Go 1.26**, single static binary (`goreleaser` / `ko` for images).
- `client-go` (Flux/Argo CRDs, k8s), **`go-git`** (revision diffs), `cobra` (CLI), `bleve` (BM25),
  `chromem-go` (vectors, later), official **Go MCP SDK** (extension tools),
  `anthropic-sdk-go` + `openai-go` (models).
- Distribution: single binary + container image + **Helm chart** (`deploy/helm/runlore`).

## 12. Phased roadmap (read-only throughout P1–P3)

| Pillar | Phase 1 (MVP) | Phase 2 | Phase 3 | Phase 4 |
|---|---|---|---|---|
| **React** | Incident-triggered (Alertmanager/VMAlert) + **trigger policy** (env/severity/namespace/label filters + dedup) | + GitOps-failure events, Slack mention, `lore investigate` | + proactive SLO-burn watch | — |
| **Investigate** | what-changed spine + VM/VL/Hubble correlation + OKF-runbook grounding + confidence/`unresolved` | + ArgoCD + Prometheus providers proven | + cross-incident pattern recognition | — |
| **Learn** | catalog **read** (cached index, instant recall) | catalog **write** (confidence-routed Issue/PR curation) — *loop closes* | hybrid vector retrieval, auto-curated playbooks, postmortems | — |
| **Act** | rung 0: read-only (no action tools) | — | — | climb the ladder: suggest → approve-to-execute → bounded reversible auto (eval-earned, policy-gated) |

Phase 1 headline: *an alert fires → RunLore investigates by correlating what-changed with
metrics/logs → posts a confidence-scored RCA + suggested rollback to Slack, grounded in the catalog.*

## 13. Repo layout

```
cmd/lore/                      CLI / agent entrypoint
internal/
  config/                      config + auto-discovery of providers
  investigate/                 the ReAct investigation loop + output contract
  whatchanged/                 revision history + go-git diffs → []Change (the spine)
  catalog/                     syncer + local mirror + bleve/chromem-go index + kb_search
  curator/                     confidence-routed Issue/PR crystallization → OKF entries
  audit/                       append-only decision/tool-call log
  model/                       ModelProvider impls (anthropic, openai-compatible)
  notify/                      Notifier impls (slack)
  providers/
    providers.go               the interfaces + the Change model (the contract)
    gitops/{flux,argocd}/      GitOpsProvider impls
    metrics/                   MetricsProvider (PromQL: vm | prometheus)
    logs/victorialogs/         LogsProvider
    network/hubble/            NetworkProvider
    cloud/aws/                 CloudProvider (Steampipe)
deploy/helm/runlore/           in-cluster chart (Phase 2)
examples/runbooks/             seed OKF catalog (ships as default knowledge)
docs/                          design.md, prior-art.md
```

## 14. Open questions & risks

1. **Git auth for diffs** *(the main risk)* — default: **reuse the credentials the GitOps engine
   already uses** (read the `GitRepository.spec.secretRef` / Argo repo secret). Fallback: a GitHub
   App token. Needs care.
2. **Embedding source** for vector search — defer vectors to a later phase (BM25-first keeps v1
   dependency-free); when added, serve embeddings from the in-cluster vLLM.
3. **Multi-replica index** — v1 single-replica with a local index; later, shared PVC or per-replica
   rebuild from the mirror.
4. **Noise control** — only novel/unresolved investigations open issues; instant-recall short-circuits
   known patterns so the KB (and your issue tracker) don't get spammed.
5. **HolmesGPT overlap** — accepted. Differentiation is GitOps-/metrics-agnostic + what-changed spine
   + the open learning catalog; not the runtime.
