---
title: Design
weight: 10
---

> A self-improving SRE agent that **reacts** to *any* incident — whatever the cause — **investigates**
> the likely cause by forming and testing hypotheses across your signals (sharpest at *what changed*,
> and deepest on GitOps platforms), and **learns** — writing every resolved incident into an open,
> git-versioned knowledge catalog that makes the next investigation faster.

| | |
|---|---|
| **Status** | `v1` — MVP + Phase-2 learning loop shipped (React + Investigate + Learn closed end-to-end; autonomy ladder off/suggest/approve shipped, `auto` gated) |
| **Date** | 2026-06-20 |
| **CLI** | `lore` |
| **Module** | `github.com/Smana/runlore` *(adjustable — could move to a dedicated org)* |
| **License** | Apache-2.0 |
| **Language** | Go (single static binary) |

> **See also:** [`learning-loop.md`]({{< relref "learning-loop.md" >}}) — a focused deep-dive on *how the
> learning loop works* (retrieve → capture → curate → compound), the outcome-driven
> decay edge, and the design rationale behind each gate, with diagrams.

---

## 1. Why this exists

On a modern cloud-native platform (Kubernetes + a GitOps engine + a metrics/logs stack), a *lot* of
incident investigation is **already solved interactively**: a human in an AI session, wired to the
right MCP servers and debugging skills (a GitOps MCP, metrics/logs MCPs, and the like), can trace a
failing rollout to its root cause today. Rebuilding that interactive experience would be reinvention.

Four things are **not** solved — and they are what RunLore is:

1. **Unattended operation.** Every existing piece needs a human in the loop, in a session. Nothing
   wakes at 03:00 on an alert, investigates, and hands you a root-cause hypothesis.
2. **Cross-signal correlation across causes — with "what changed" as the sharpest lens.** Incidents
   come from many causes: a deploy, but also a node/disk/cloud failure, a network/DNS issue,
   saturation, a dependency outage, a cert expiry, organic load. Today the GitOps engine, metrics,
   logs, and network are separate tools; nobody ties them into one investigation — and nobody
   **diffs Git between the two deployed revisions** to show the *actual* delta when a change *is* the
   cause. RunLore does both: correlates the signals, and makes "what changed" a first-class lens.
3. **Learning.** Existing OSS agents rely on *your hand-curated* runbooks; they don't accumulate
   knowledge. Commercial tools that "learn" are all closed.
4. **Shareability.** None of the above is an installable product a *different* team can adopt.

RunLore fills exactly these gaps, and only these.

## 2. Positioning (honest prior art)

See [`prior-art.md`]({{< relref "prior-art.md" >}}) for the full landscape. The short version:

| Capability | OSS today | Commercial today | RunLore |
|---|---|---|---|
| Autonomous react → investigate → report | **HolmesGPT** (CNCF); k8sgpt-operator/kagent partial | the whole market's commodity | **table stakes** (we do it too) |
| "What-changed" Git-diff RCA, GitOps-/metrics-**agnostic** | effectively nobody (OSS is Prom/Loki/vanilla-k8s) | **Komodor**, **Anyshift** do change-diff RCA | **sharp, but copyable** — *provenance* for the catalog, not the moat |
| **Open, reviewable, outcome-weighted** knowledge catalog | nobody (HolmesGPT doesn't learn; kagent's memory is opaque) | Cleric/Resolve/PagerDuty/Google — all **closed** | **the durable wedge** |
| Honest about the sub-50% reality (read-only-first, `unresolved`, eval-proven) | partial | mostly autonomy-theatre | **the under-sold asset** |

The autonomous runtime is a commodity, and the Git diff — though sharp — is now matched by Komodor and
Anyshift (see [`prior-art.md`]({{< relref "prior-art.md" >}})). The defensible reason to build RunLore is the
**combination the open tools don't have**: an **open, portable, PR-reviewed knowledge catalog** that
**learns from outcomes** (HolmesGPT doesn't learn; kagent's memory is opaque), grounded in the **exact
GitOps change**, from an agent that is **honest about what it can't determine** — all **self-hosted, on
your models**. Target user: lock-in-averse, sovereignty-conscious, GitOps-native teams.

## 3. Goals / Non-goals

**Goals**
- Autonomous, **read-only-first** incident investigation that a team can `helm install`.
- **GitOps-engine-agnostic** (Flux + ArgoCD) and **metrics-backend-agnostic** (VictoriaMetrics +
  Prometheus) from day one; logs/network/cloud pluggable behind the same pattern.
- A **"what changed" spine** (revision history + real Git diffs) as the *sharpest* investigation
  lens — co-equal with the no-change hypotheses (saturation, network, node health, dependency, load),
  not the only question asked.
- An **open OKF knowledge catalog** the agent reads (fast, cached) and writes (PR-gated) — knowledge
  is portable markdown in git, never vendor lock-in.
- **Single static Go binary**; runs in your terminal (`lore investigate`) or in-cluster (`lore serve`).
- **Three model providers: Anthropic, Google Gemini, and OpenAI-compatible.** The last covers an
  in-cluster vLLM, Ollama, OpenRouter, or any OpenAI-compatible endpoint — so data needn't leave the boundary,
  with no LiteLLM-style multi-broker dependency.

**Non-goals (for now)**
- **Unattended** remediation of production. The autonomy ladder's lower rungs are **shipped**
  (`off`/`suggest`/`approve`): in `approve` the agent executes only *reversible* Flux ops
  (suspend/resume/reconcile) **after explicit human approval**, validated server-side. The top rung
  (`auto`, execute without approval) **exists but is gated and not recommended on real clusters**
  (§6, §9). Default remains read-only; "writes" without an approved action mean *markdown to git via
  reviewed PR*.
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

> 📐 The detailed component diagram is on the [Architecture page]({{< relref "/docs/concepts/architecture" >}}). The ASCII sketch below is a quick text rendering.

![RunLore architecture — React → Investigate → Learn](/docs/concepts/architecture/runlore-architecture.svg)

The main components map onto `internal/` packages — see the illustrative layout in §13.

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
- **On-demand**: `lore investigate --alert "<symptom>"` or a chat mention in Slack/Matrix (same engine, human-initiated).

Example trigger policy (`internal/config`, `config.TriggerPolicy`). Source *enablement*
lives under `sources.<name>`; the `triggers` block is purely the match/ignore/dedup
criteria applied to admitted alerts:

```yaml
sources:                                # enablement is per-source
  alertmanager: {}                      # mount the incident webhook ingress
  gitops: { enabled: true }             # watch GitOps Ready=False (secondary trigger)
triggers:
  incidents:                            # match/ignore/dedup for admitted alerts
    match:                              # ANDed; empty fields match anything
      severity:    [critical]           # only paging-grade
      environment: [prod]               # ignore staging/dev
      namespaces:  ["apps*", "payments"]
      labels:      { team: platform }   # arbitrary label matchers
    ignore:
      alertnames:  [Watchdog, InfoInhibitor]
    dedup: { window: 30m }              # don't re-open a still-firing alert
    debounce: 60s                       # hold a NON-CRITICAL firing alert, skip it if a
                                        # matching resolved webhook lands within the window
                                        # (default 60s; 0s = investigate on every fire).
                                        # A `critical` alert is NEVER held — a debounce
                                        # must never delay the first look at a page.
    cancel_queued_on_resolve: true      # default. Drop a QUEUED (not yet started)
                                        # investigation when the alert resolves first —
                                        # this, not the hold, is what filters a
                                        # self-resolving CRITICAL, at zero added latency
  gitops_failures:                      # secondary trigger (enabled via sources.gitops)
    debounce: 60s                       # require the failure to persist this long
                                        # (re-check still Ready=False) before
                                        # investigating — filters reconcile-churn
                                        # transients; 0 = fire immediately
```

### Investigate — form & test hypotheses across causes

The cause is unknown at wake time, so RunLore explores a **hypothesis space**, not a fixed pipeline:
**what changed** (the sharpest first question) *and* the causes nothing changed for (saturation,
network drops, node/disk health, dependency outages, organic load). "What changed" spans more than
GitOps — deploys, config, images, infra/cloud, autoscaling, cert rotation, manual changes — and on a
GitOps platform it resolves to the *exact* Git diff (the spine below).

1. **Instant recall**: `kb_search(symptom)` against the cached catalog. High-confidence known-pattern
   hit → short-circuit to the known resolution (no full loop). *(HolmesGPT data: ~40 % of incidents
   self-resolve on a runbook/pattern match; tool-calls drop 16→2.)*
2. **What changed**: build the ranked `Change` timeline around the incident window — the Git diff on a
   GitOps platform (`what_changed`, resolving GitRepository/OCIRepository/ExternalArtifact sources) **and**
   the AWS control plane (`cloud_what_changed` = CloudTrail), unified in one `Change` model so an infra
   change outside GitOps lands on the same timeline.
3. **Ground**: retrieve relevant OKF runbooks/incidents and seed the loop.
4. **ReAct**: pull metrics (PromQL), logs, network, Flux status/tree, controller logs, and cloud
   resource health *just-in-time* via providers; form and test hypotheses across both the change-caused
   and no-change branches.
5. **Adversarial verify pass**: before delivery a *skeptic* model call re-examines each root cause and
   **rejects correlation-only findings** (moving them to `unresolved`) and **re-calibrates confidence**.
   It can only ever lower/withdraw a claim, never invent one — and if it rejects every root cause, the
   proposed actions are dropped too. Recalled (catalog) findings go through it as well, since catalog
   content is untrusted.
6. **Output contract** (structured): ranked root cause(s) + **confidence** + `change_ref` +
   **evidence trail** + **suggested reversible action** + explicit **`unresolved`** (honest about what
   it couldn't determine — designed for the ITBench <50 % reality, §10).

### Learn — compound an open catalog

> The **full operational mechanics** — instant-recall trust gates, the outcome ledger, outcome-driven
> decay, curation, and compounding — are documented once in **[learning-loop.md]({{< relref "learning-loop.md" >}})**
> (the canonical deep-dive). This section is the *architectural* view: how Learn fits the design and why.

RunLore's learning is modeled on the **Open Knowledge Format (OKF)** introduced by
[GoogleCloudPlatform/knowledge-catalog](https://github.com/GoogleCloudPlatform/knowledge-catalog):
a git tree of markdown + YAML-frontmatter entries that agents both **read and write** (its enrichment
agent follows a *read existing → generate entries → human pushes* loop). RunLore applies that pattern
to incidents. The loop is `retrieve → capture → curate → compound`, routed by confidence:

```
investigation result
  ├─ KB hit (known)          → post the known resolution. No issue, no PR. (instant recall)
  ├─ novel + confident       → draft OKF entry as a PR; humans refine via review → merge → reindex
  └─ novel + uncertain       → chat-only: the findings + open questions are delivered to chat, but
                               NO repo artifact is written — a below-bar guess must not enter the catalog
```
The catalog only grows from **genuinely novel, human-sharpened** incidents. (Issues are opened *only*
by the scheduled Phase-2 grooming, and only for a **recurring unresolved pattern** — a "knowledge gap",
never for a one-off uncertain finding.) Every learned entry cites its **evidence trail**, the
**causing** change, and the **fixing** change — provenance no closed "memory" gives you.

**Lifecycle labels gate the catalog's quality.** Curated artifacts carry a lifecycle label —
`triggered` (raw, just opened) → `investigating` → `solved` (root cause confirmed *and* resolution
captured) — plus `wontfix`. Only a **`solved` entry with a written resolution** should be merged as a
reusable Playbook, so unverified findings can't silently become "knowledge."

**Re-running on demand (`reinvestigate`).** RunLore takes no inbound GitHub webhooks, so re-triggering is
an **outbound poll**: a human adds the `reinvestigate` label to a curated issue; the leader re-runs the
investigation (with the prior finding as context), comments the fresh result, and advances the label to
`investigating`. Only RunLore-originated issues (carrying the `runlore` provenance label) are eligible —
a drive-by issue can't spend an investigation.

**Two kinds of knowledge — seeded vs learned (a deliberate distinction).** "Learns your context" is
only half emergent:
- **Seeded** (authored / ingested, *not* conjured from RCA): your **constraints**, **architecture &
  tooling**, and **workflows & procedures** — e.g. "never restart the payments DB in market hours,"
  "egress is a single NAT, so DNS flakes look like app errors," "cap-touching changes escalate to
  platform." These come from your existing runbooks, ADRs, wiki, and `AGENTS.md`, ingested as OKF
  entries — making the agent context-aware on **day one**, not after a slow accumulation.
- **Learned** (emergent from investigations): **incident patterns** ("this symptom → this cause →
  this fix"), captured via the loop above.

To keep the catalog an asset and not a liability (frontier RCA is <50 % accurate, §10), every entry
carries lifecycle frontmatter that recall now **honours**: **`status`** — a `retired`/`draft` entry is
indexed but never fires or leads (the seam the retirement pass writes to, learning-loop.md §5) — and
**`last_validated`** — recall down-weights an entry no human has confirmed within a configurable
horizon (`catalog.instant_recall.stale_after`; one confidence step, never a rejection). Both are
fail-safe: an absent or unknown value reproduces the pre-field behaviour exactly. A third key,
**`confidence`**, is *written* by the curator but deliberately **not read back** — recall derives trust
from the live outcome track record (resolve rate + 👍/👎), and a static authored confidence would fight
that dynamic signal. On top of these, an outcome-driven decay down-weights knowledge that stops
resolving incidents ([mechanics in learning-loop.md §6]({{< relref "learning-loop.md" >}})).
Curation — the PR/issue review — is the **load-bearing** quality gate that separates this from opaque
vendor "memory."

### Act — the (gated) autonomy ladder

Read-only is the **default posture, not the ceiling.** RunLore climbs an **autonomy ladder** as eval
earns trust — an action is just *a tool with extra metadata behind a policy gate*:

```
read-only  →  suggest (PR/command)  →  approve-to-execute (human click)  →  bounded auto
rung 0         rung 1                   rung 2                              (reversible + low-blast
(default)      (v1, shipped)           (v1, shipped)                       + non-critical only;
                                                                            gated, not recommended)
```

Every action tool declares `mutating` / `reversible` / `blastRadius` / `target`; an **action policy**
(mirroring the trigger policy) sets the **mode** (`off | suggest | approve | auto`), scoped by
environment, reversibility, and blast radius. **Rungs `off`/`suggest`/`approve` are wired in v1**: in
`approve`, an approved decision executes a **reversible** Flux op (suspend/resume/reconcile), with
reversibility/blast-radius/target validated **server-side** (`internal/action`) — never trusted from
model output. Rung **`auto`** (execute without approval) **exists but is gated** (reversible-only,
confidence-thresholded, rate-limited, kill-switchable, off by default) and **is not recommended on
real clusters**. The metadata (`providers.Action`, `config.ActionPolicy`) was in place from day one, so
none of this was a retrofit.

> **Decision (2026-06-24) — no in-cluster mutating `rollback`.** A reversible "re-pin to the
> prior revision" op was scoped and rejected. The executor today runs only namespace-scoped,
> single-resource Flux ops (suspend/resume/reconcile). A real GitOps rollback can't fit that
> shape cleanly:
> - **It fights the protected-namespace model.** A Flux Kustomization has no per-resource
>   revision pin, so re-pinning it must patch its owning **GitRepository** — which in the common
>   monorepo layout lives in **`flux-system`**, a built-in *protected* namespace the action gate
>   forbids as a target. The only ways past that all weaken a safety invariant.
> - **It is anti-GitOps.** An in-cluster re-pin makes the cluster's desired state diverge from
>   Git: Flux then reports drift, the next legitimate Git change collides with the manual pin, and
>   the divergence must be remembered and undone.
> - **Little is lost.** The agent already *suggests* a rollback in `suggest` mode ("roll back
>   `apps` to `abc123`"); only the act of executing it is withheld. For a GitOps shop, "the agent
>   advises, a human merges the revert" is the correct division of labor — and the upside is
>   modest against the outage risk of a wrong rollback on a sub-50%-RCA baseline.
>
> If the loop is ever closed, the GitOps-correct form is a **Git-revert PR** (reuse the
> curator/forge path) — in sync with Git, inherently reversible, reviewed — **not** a cluster
> patch. Until then, remediation stays read-only / propose-and-approve, and the autonomy ladder's
> executable vocabulary remains the three reversible Flux ops above.

## 7. Provider abstraction

Interfaces live in `internal/providers/providers.go`. "For the moment" impls:

| Provider | Interface | Impls now | Later |
|---|---|---|---|
| GitOps | `GitOpsProvider` | **Flux**, **ArgoCD** | — |
| Metrics | `MetricsProvider` | **VictoriaMetrics**, **Prometheus** (one PromQL impl, 2 endpoints) | — |
| Logs | `LogsProvider` | **VictoriaLogs** | Loki, … |
| Network | `NetworkProvider` | **Cilium Hubble**, **AWS VPC Flow Logs**, **GCP Firewall Logs** (pluggable, CNI-agnostic; none default-on) | Azure NSG Flow Logs; CNI-agnostic eBPF (Retina) |
| Cloud | `CloudProvider` | **AWS** (CloudTrail what-changed + EC2/ASG/EKS health) | GCP, Azure via native SDKs; Steampipe/cloud-MCP optional |
| Model | `ModelProvider` | **Anthropic**, **Gemini**, **OpenAI-compatible** (in-cluster vLLM, Ollama, OpenRouter, …) | — |
| Notifier | `Notifier` | **Slack**, **Matrix** | PagerDuty, incident.io |
| Issue | `IssueProvider` | **GitHub** (App auth) | GitLab (access token) |

> **Cloud via native SDKs.** The AWS impl (`internal/providers/cloud/aws`, `aws-sdk-go-v2`) is **read-only**:
> `CloudChanges` = CloudTrail `LookupEvents` (mutating events → the engine-agnostic `Change` model, so
> cloud joins the same "what changed" timeline as Git diffs); `ResourceHealth` = EC2/ASG/EKS `Describe`.
> Auth is **in-cluster identity** (EKS Pod Identity / IRSA via the default credential chain) — *not*
> static keys, *not* Steampipe, *not* shelling out to cloud CLIs (both add heavy deps and break the
> single-binary property). GCP/Azure follow the same shape. Steampipe and cloud MCP servers remain
> available as optional MCP extensions.
>
> On Cilium clusters the Pod Identity credential endpoint is a host-network target (`169.254.170.23:80`),
> which a Kubernetes NetworkPolicy can't match — the chart's `networkPolicy.awsPodIdentity` renders a
> `CiliumNetworkPolicy` (`toEntities: [host]`) to allow it.

**Why the GitOps abstraction is real, not hand-wavy** — both engines reduce to *revision history +
git diff*:

| | Flux | ArgoCD |
|---|---|---|
| revision history | `HelmRelease`/`Kustomization` `.status.history` + `lastAppliedRevision` | `Application.status.history[]` |
| deployed now | source `GitRepository` revision | `status.sync.revision` |
| failure (React) | `Ready=False`, source `FetchFailed` | `health=Degraded`, `sync=OutOfSync` |
| the diff | go-git between revisions over `spec.path` | go-git over `source.path` (Argo also has native `app diff`) |

> **Engine depth — deep lens now at parity.** Flux is the reference implementation, but the deep
> introspection tools (`gitops_resource_status`, `gitops_tree`) and the failure-persistence re-check run on the
> `GitOpsInspector` interface, which **both Flux and Argo CD now implement** (FEAT-2): on Argo via
> native `status.health`/`status.sync`, error conditions, and the `status.resources` tree (app-of-apps
> aware). Remaining honest asymmetry: **action-approval (rung-2) is Slack-only** today; Matrix is delivery-only.

**Auto-discovery**: detect `argoproj.io/Application` → ArgoCD; `helm.toolkit.fluxcd.io` → Flux; probe
the metrics endpoint → VM vs Prometheus. Config overrides. Flux + VictoriaMetrics is the primary
reference combo; Argo + Prometheus exercises the abstraction.

## 8. The knowledge catalog & its cache

**Source of truth = a git repo** — an
[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog) bundle (markdown + YAML frontmatter;
`index.md` + `log.md` reserved; entries link to assert relationships). Reviewed, versioned, portable.

**The agent never queries git at investigation time.** A `Catalog` subsystem keeps it fast:

```
KB git repo  ──syncer──►  local mirror  ──build──►  index:  bleve (BM25)  [+ opt-in vectors: cosine + RRF]
  (truth)    (poll HEAD ±webhook/Receiver)              ▲
                                          kb_search(query, k)  — in-process, sub-ms, no git/network
```
- **Syncer** polls remote `HEAD` (cheap), pulls on change, **incrementally re-indexes** changed
  entries. Optional push-webhook / Flux `Receiver` for instant refresh.
- **Index** is embedded + persisted: **`bleve`** (BM25) — no embedding dependency, genuinely
  "easy". Hybrid vector recall (opt-in, `catalog.instant_recall.hybrid`) is **already implemented** as an
  in-process **cosine + RRF fusion** over a persisted vector cache (`internal/embed`,
  `internal/catalog/hybrid.go`) — no vector-DB dependency; embeddings are served by the in-cluster vLLM.
  Both pure-Go → the single binary holds the whole retrieval stack.
- **Sync mechanism** (default A; B is the in-cluster upgrade):
  - **A — built-in self-syncer** (poll + optional webhook). Fewest moving parts. *Default.*
  - **B — Flux `GitRepository` + `Receiver`** — the KB syncs like every other source; Flux notifies a reload.
  - **C — `git-sync` sidecar + shared volume** — classic decoupling.

## 9. Safety & trust model

- **Read-only by default; execution is gated and server-authoritative.** Read-only is the default
  posture. When the autonomy ladder is enabled (`config.actions`), the executor runs only a fixed set
  of **reversible** Flux ops (suspend/resume/reconcile); reversibility, blast radius, and the target
  namespace are validated **server-side** (`internal/action`) — derived from the op, never trusted
  from model output — and an unknown op or out-of-allowlist target is refused. No cluster-mutating MCP
  tools are wired. The alert webhook is authenticated (required under `auto`), and the kill-switch
  **fails closed on cold start** (auto starts paused until an authenticated resume). *Note:* the
  kill-switch resume and the rate-limit window are **in-memory** — a restart/failover re-pauses (safe)
  but loses an operator's resume and resets the rate-limit counter; back them with a PVC if resume/budget
  must survive a restart.
- **The Curator is cluster-read-only** — its "writes" are markdown-to-git via reviewed PR + issues,
  never the cluster. Cluster mutations come only from the gated action executor above.
- **Scoped identity.** In-cluster, the agent runs under a least-privilege, read-mostly identity
  (a scoped ServiceAccount; or EKS Pod Identity / Workload Identity on managed clusters). Execution
  rights (`patch`) are granted as **namespace-scoped** Roles over an allowlist, never cluster-wide.
- **Raw pod logs are namespace-scoped, not cluster-wide.** `controller_logs` reads raw pod log bodies
  (which can carry secrets/PII) only from the Flux controllers, so `pods/log` is granted as a
  **namespaced** Role over `rbac.controllerLogNamespaces` (default `flux-system`) — never cluster-wide.
  Cluster-wide `pods`/`events` *get/list* (pod status + event messages, not log bodies) stays in the
  ClusterRole because `pod_status`/`kube_events` triage arbitrary incident namespaces.

- **Secret redaction at the model and egress boundaries (`internal/redact`).** Secret-shaped values
  are masked at three trust boundaries before they can leave the cluster: the **incident text**
  entering the prompt, **every tool output** (logs, git diffs, status/event messages) before it reaches
  the **model provider**, and the **finished investigation** (root-cause summaries, evidence, suggested
  actions) before it is copied into the **KB pull-request body** and **chat**. The ruleset is
  high-precision — PEM private keys, JWTs, GitHub / Slack / AWS / Google / Stripe keys,
  `user:pass@host` URLs, `Authorization` headers, and generic `*secret*/*token*/*password*: <value>`
  pairs, plus the values under a `kind: Secret` manifest's `data:`/`stringData:` block (including
  inside a git diff; the values are also base64-decoded and scrubbed from the whole payload) —
  masking the *value* while keeping surrounding structure so the agent can still
  reason ("the password field changed"). Redaction is a **mitigation, not a guarantee**: unlabeled
  high-entropy strings and base64 blobs with no `Secret` manifest in the payload are not caught (see the
  [LLM security architecture]({{< relref "security-architecture.md" >}})). Defense-in-depth still applies — the RBAC
  scoping above limits what tool output can contain, and **self-hosting the model** (in-cluster
  vLLM/Ollama) keeps data in-boundary regardless. If you run a public KB repo or untrusted-tenant
  namespaces, treat the residual recall gaps as a gating concern.
- **Append-only, tamper-evident audit log** (`internal/audit`): every action attempt — inputs, gate
  results, op, target, actor, outcome — is a hash-chained JSON line, so edits/deletions are detectable.
- **Honest uncertainty.** `unresolved` is a first-class output field; the agent says what it doesn't
  know rather than hallucinating.
**The autonomy ladder.** Read-only is the default rung, not the ceiling. Action tools are gated by an
**action capability model** + **action policy** (`providers.Action` + `config.ActionPolicy`):
- every action declares `mutating` / `reversible` / `blastRadius` / `target`;
- the policy sets the **mode** (`off | suggest | approve | auto`), scoped by environment,
  reversibility, and blast radius — **irreversibility is the trip-wire for mandatory human approval**;
- **dry-run / preview by default**, **append-only audit**, and reversibility for anything applied;
- scoped agent identity (RBAC) is the hard backstop regardless of policy.

`off`/`suggest`/`approve` are **shipped in v1** (the executor runs only reversible Flux ops, behind
human approval); `auto` exists but is **gated and not recommended on real clusters**. The capability
metadata was present from day one, so reaching the higher rungs needed no retrofit.

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
- `client-go` (Flux/Argo CRDs, k8s), **`go-git`** (revision diffs), `bleve` (BM25 index).
- **Everything else is hand-rolled — deliberately, to keep one static binary with no SDK sprawl:**
  the CLI is a stdlib `switch os.Args[1]` (no cobra); the MCP server *and* client are dependency-free
  (`internal/mcp` speaks streamable-HTTP directly); the model clients
  (`internal/model/{anthropic,openai,gemini}`) are `net/http` against the raw provider APIs; and vector
  recall is an in-process cosine + RRF fusion (`internal/embed`), not a vector-DB dependency.
- Distribution: single binary + container image + **Helm chart** (`deploy/helm/runlore`).

## 12. Phased roadmap (read-only by default; autonomy ladder's lower rungs shipped)

| Pillar | Phase 1 (MVP) | Phase 2 | Phase 3 | Phase 4 |
|---|---|---|---|---|
| **React** | Incident-triggered (Alertmanager/VMAlert) + **trigger policy** (env/severity/namespace/label filters + dedup) | + GitOps-failure events, chat mention (Slack/Matrix), `lore investigate` | + proactive SLO-burn watch | — |
| **Investigate** | what-changed spine + VM/VL/Hubble correlation + OKF-runbook grounding + confidence/`unresolved` | + ArgoCD + Prometheus providers proven | + cloud context (native SDKs: AWS/GCP/Azure) + cross-incident pattern recognition | — |
| **Learn** | catalog **read** (cached index, instant recall) | catalog **write** (confidence-routed Issue/PR curation) — *loop closes* | hybrid vector retrieval, auto-curated playbooks, postmortems | — |
| **Act** | rung 0: read-only (default) | **shipped:** suggest + approve-to-execute (reversible Flux ops, human-approved, server-authoritative); `auto` exists but gated/not recommended | — | further rungs as eval earns trust (policy-gated) |

Phase 1 headline: *an alert fires → RunLore investigates by correlating what-changed with
metrics/logs → posts a confidence-scored RCA + suggested rollback to chat (Slack/Matrix), grounded in the catalog.*

## 13. Repo layout

```
cmd/lore/                      CLI / agent entrypoint
internal/
  config/                      config + auto-discovery of providers
  investigate/                 the ReAct investigation loop + output contract
  whatchanged/                 revision history + go-git diffs → []Change (the spine)
  catalog/                     syncer + local mirror + bleve BM25 (+ opt-in vector) index + kb_search
  curator/                     confidence-routed Issue/PR crystallization → OKF entries
  curate/                      Phase-2 backlog groomer (dedup, lifecycle, retirement sweeps)
  outcome/                     outcome ledger — Beta-posterior decay over resolve/vote counts
  audit/                       append-only decision/tool-call log
  model/                       ModelProvider impls (anthropic, gemini, openai-compatible)
  notify/                      Notifier impls (slack, matrix; pagerduty/incident.io later)
  providers/
    providers.go               the interfaces + the Change model (the contract)
    gitops/{flux,argocd}/      GitOpsProvider impls
    metrics/                   MetricsProvider (PromQL: vm | prometheus)
    logs/victorialogs/         LogsProvider
    network/hubble/            NetworkProvider
    cloud/{aws,gcp,azure}/     CloudProvider — native SDKs, Phase 2+ (Steampipe/MCP optional)
deploy/helm/runlore/           in-cluster chart (Phase 2)
examples/runbooks/             seed OKF catalog (ships as default knowledge)
docs/                          design.md, learning-loop.md, getting-started.md, … (see the Docs index)
```

## 14. Open questions & risks

1. **Git & forge auth — a GitHub App** (best practice; one fine-grained, short-lived identity for
   both needs):
   - `contents: read` → clone/diff the GitOps source repo(s) for the what-changed spine;
   - `issues: write` + `pull_requests: write` + `contents: write` (KB repo) → the Curator.

   The App mints **1-hour installation tokens** (no long-lived PAT; bot identity; central revoke).
   The installation token also works as the git HTTP password (`x-access-token:<tok>@github.com/…`),
   so the *same* App serves clone/diff and API writes. The private key lives in a k8s Secret (ideally
   via External Secrets), referenced — never inlined:

   ```yaml
   forge:
     github_app:
       app_id: 123456
       installation_id: 7891011
       private_key_ref: runlore-github-app   # Secret name/key
   ```
   Non-GitHub hosts (GitLab, self-hosted) fall back to a scoped access token / deploy key — auth is
   per-host. Local `lore investigate` can use ambient git credentials instead of the App.
2. **Embedding model** for vector search — hybrid vector recall is now **implemented** (opt-in,
   in-process cosine + RRF, embeddings from the in-cluster vLLM); the remaining question is which
   embedding model to standardize on.
3. **Multi-replica index** — v1 single-replica with a local index; later, shared PVC or per-replica
   rebuild from the mirror.
4. **Noise control** — only novel/unresolved investigations open issues; instant-recall short-circuits
   known patterns so the KB (and your issue tracker) don't get spammed.
5. **HolmesGPT overlap** — accepted. Differentiation is GitOps-/metrics-agnostic + what-changed spine
   + the open learning catalog; not the runtime.
