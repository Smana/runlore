# RunLore — Review & Roadmap (2026-06-25)

> Max-effort external review of RunLore, commissioned by the author. This document
> **sequences a 5-step review** and tracks findings. Step 1 (positioning) is complete and
> summarized here; Steps 2–5 are scoped with method + seeded findings, ready to execute.
>
> **Grounding:** current `main` @ `a8a9bf4`, cross-checked against code at `file:line`. The
> internal review note dated 2026-06-24 is **partly stale** (audit log + webhook auth have
> since landed) and is *not* relied upon — every claim below is re-verified against current code.
>
> **Priority key:** **P0** blocks credibility/safety · **P1** important · **P2** nice-to-have.

> **Progress log — 2026-06-26.** The review (Steps 1–5) is complete; this records what has shipped to
> `main`. **Merged:** M0 (AUTH-1 · C1 · C4 · C2) · M1 churn-hardening (GO-P1A · GO-P2A/B/C · **GO-P1B
> graceful drain**) · M2 egress security (**F1** ingress **+ egress** redaction · **F3** reject-gating ·
> **CLONE-1** per-call clone cache · **P3** nits · **SSRF redirect guard** across all outbound clients) ·
> M3 **FEAT-1** (`auto` frozen) · **FEAT-2** (ArgoCD `GitOpsInspector` parity) + the `gitops_*` tool
> rename · **M4 hybrid recall** (pt1 embed foundation + pt2 catalog/recall integration — opt-in,
> BM25-default; cosine-threshold eval-tuning pending) · **POS-4** (HolmesGPT what-changed proposal +
> `lore mcp` MVP) · docs (architecture **mermaid** diagram · prior-art · learning-loop de-dup · roadmap).
> **Remaining:** **M4 pt2 eval-tuning** (operator — needs a live embeddings endpoint + the instant-recall
> eval) · **POS-4 upstream** (HolmesGPT live demo / native-toolset PR — relationship) · **PERSIST-1**
> (decision: doc-only). **Attempted twice, deferred:** **F2** action-target validation — *attempt 1
> (narrow: alert seed + what_changed)* downgraded a legit auto-suspend → reverted; *attempt 2 (proper:
> capture across what_changed + gitops_resource_status + gitops_tree, name-tolerant matching)* validates
> the core (e2e `auto-executed` passes) but **regresses the rung-2 two-approve e2e** (`main` 45/45 → F2
> 44/45 — one approvable action vs two) via an unconfirmed re-fire-action interaction; deferred
> (low-urgency while `auto` is frozen), code parked on branch `fix/f2-action-target-validation`. Next
> attempt: instrument the rung-2 re-fire (`--keep`) before enforcing. **Deferred with reason:** dead-key
> removal (FEAT-3/4 — reserved keys under a strict decoder) · `CheckRedirect`/`strict:true` (broad,
> low-value).

---

## Step 1 — Alternatives & positioning  ✅ **DONE**

Full analysis delivered separately. Conclusions that become work items:

### Verdict
The engineering is **real** (verified at `file:line`): the go-git what-changed spine, the closed
learning loop, server-side safety derivation, hash-chained audit, adversarial verify. But the
**positioning oversells defensibility**:

- **Differentiator #1 — "what-changed Git diff" is *not* unique.** Komodor ships manifest-diff-for-RCA
  with Argo/Flux today (Gartner-recognized, $90M raised); Anyshift narrates "what changed" over a
  git-versioned infra graph; Datadog Change Tracking does commit-level correlation. RunLore's genuinely
  *owned* slice is narrow: **rendered-manifest diff between the two commits Flux/Argo actually reconciled,
  as the primary RCA spine.** Real, sharp, but copyable in ~a quarter. It is a *feature*, not a moat.
- **Differentiator #2 — open, reviewable, outcome-weighted knowledge — is the stronger, genuine
  whitespace.** Verified: HolmesGPT still does **not** learn (through v0.33.0, Jun 2026); kagent's memory
  is **opaque** (view-only pgvector, no export, no per-entry edit, per-agent-isolated); commercial
  "memory" is closed. *But:* the moat is **values-segment + human-bottlenecked + earned over time**, not
  architectural — and the **OKF bet is fragile** (OKF is a 2-week-old Google **v0.1 draft**, ~0 production
  adoption, not donated to CNCF).
- **The most under-used asset is the honesty/eval/read-only posture** — uniquely well-aligned to the
  category's credibility crisis (Gartner "Peak of Inflated Expectations"; ITBench ~13.8% resolution).

### Repositioning actions
- **P1 · POS-1** — Lead messaging with **open/reviewable/portable + outcome-weighted knowledge** and the
  **honesty/eval posture**; demote the Git-diff to "the *provenance source* that makes the knowledge
  high-quality," not a standalone wedge.
- **P1 · POS-2** — **Stop claiming the Git diff "exists nowhere else"** (`docs/prior-art.md`, README "Why
  RunLore"). Name Komodor/Anyshift/Datadog honestly and draw the *narrow* true line.
- **P1 · POS-3** — **De-risk the OKF dependency.** Lean on the *concept* (git-versioned, PR-reviewed,
  outcome-decayed knowledge — which is genuinely unique), not on OKF being a "standard." Treat OKF
  compliance as a nice-to-have interop format, reversible if it stalls.
- **P2 · POS-4** — Evaluate **alignment over competition**: contribute a "what-changed" toolset to
  HolmesGPT, run *on* kagent (A2A), or be a reference OKF writer-back — turning "compete with CNCF
  incumbents" into "be the GitOps + open-knowledge layer they don't build."
- **P2 · POS-5** — Name the **ICP** explicitly: lock-in-averse, sovereignty-conscious, self-hosting,
  GitOps-native platform teams (Flux/Argo + VM/Prom). Underserved and growing (EU-sovereign tailwind),
  but narrow — own it deliberately.

### Ranked threats (track, don't fix)
1. **HolmesGPT adds change-diff + OKF write-back** — CNCF reach + MCP + existing Git toolsets ⇒ could
   erase *both* differentiators. Highest impact.
2. **Komodor** owns change-aware RCA commercially.
3. **Anyshift** owns the "what-changed" narrative (versioned infra graph).
4. **Google** could commoditize OKF into Gemini Cloud Assist.
5. **Single-maintainer sustainability** vs CNCF-backed incumbents (structural, not technical).

---

## Step 2 — Feature ↔ intent audit  ✅ **DONE**

**Goal.** For each shipped feature, decide: *serves the core intent* (React → Investigate → Learn,
what-changed-first, open knowledge, read-only-first, honest about sub-50%) · *table-stakes parity*
(keep but freeze) · or *scope-creep* (defer to protect focus). The breadth (2 GitOps engines, 2 metrics
backends, 3 model providers, Slack+Matrix, cloud, 3 network providers, autonomy ladder, eval, HA) is
impressive for a 5-day-old solo project but **dilutes the wedge and multiplies maintenance** — this step
decides what to double down on vs freeze.

**Method.** Walk `internal/` package-by-package; map each to a stated goal in `design.md §3`; tag
`core | parity-freeze | defer`; flag any feature with no intent anchor.

**Inventory to score:** trigger policy · GitOps-failure watch · what-changed spine · no-change
hypotheses · instant recall · adversarial verify · output contract · curator PR/Issue · outcome
ledger + decay · curate grooming (queue/recurrence) · autonomy ladder (off/suggest/approve/auto) ·
providers ×8 · eval harness · observability · HA leader election.

### Findings
**Verdict: high intent-discipline — near-zero capability scope-creep.** Every package is wired and maps
to a goal; all 8 backend impls are real (0 stubs); models at genuine parity. The mismatches:

- **P1 · FEAT-1 — the `auto` rung is the one genuine intent mismatch.** It contradicts the stated
  non-goal ("unattended prod remediation"), carries ~13 config fields + the audit subsystem for a
  paused/off capability, and is the *sole* reason the prompt-injection→cluster-mutation threat chain
  exists. It is, however, the most defensively-built code in the repo (fails closed, reversible-only,
  ns-denied, server-derived envelope, authenticated, audited). **Recommendation:** freeze `auto` as
  explicitly experimental (no further investment) and weigh removing the executable path entirely —
  `approve` delivers ~95% of the value at a fraction of the attack surface. **Decision for the author.**
- **P1 · FEAT-2 — "backend-agnostic" is overstated.** ArgoCD has no `GitOpsInspector` (deep tools no-op
  on Argo); Matrix has no action-approval (rung-2 = Slack only). Either invest to parity or, preferred
  for single-maintainer focus, **document honestly** ("Flux-first; Argo basic; approvals via Slack") → Step 4.
- **P2 · FEAT-3 — doc/impl drift.** No "hypothesis ranker" module exists (prompt-only). 3 dead config
  keys (`ActionPolicy.RequireApproval`, `Telemetry.OTLPEndpoint`, `GitHubApp.PrivateKeyRef`) → Step 4 docs
  + Step 5 removal. (`GitHubApp.PrivateKeyRef` dead == the differ-auth P0 plumbing is missing.)
- **P2 · FEAT-4 — maintenance weight is concentrated** (~110–140 config keys). Trimming dead keys + (if
  frozen) the `auto` surface is a real simplification → Step 5.
- **Keep & market harder:** the full Learn loop (the moat), model-agnosticism, and the **eval harness**
  (under-sold honesty asset). Observability is correctly self-health-scoped (respects the non-goal).

Status: **complete.**

---

## Step 3 — Issues: security / Go / performance / cost  ✅ **DONE**

18 findings across 3 audits + 2 earlier-confirmed gaps, de-duped. **Fundamentals are sound** (auth,
action gate, leader-election lifecycle, audit log) — see "Refuted/solid". Issues cluster in three
places: the headline feature is half-wired (P0), the data-egress path lacks redaction (P1), and the
**interruption/churn paths** (leader flap · SIGTERM · interrupted clone · crash) need hardening — the
highest-leverage work, since those are the events an HA tool exists for.

#### P0 — fix first
- **AUTH-1 (Sec/Correctness)** — GitHub App token not plumbed into the production differ
  (`cmd/lore/main.go:1423` builds `&whatchanged.Differ{}` empty; token source `forge/github/auth.go:20`;
  `GitHubApp.PrivateKeyRef` is a dead key). **Private-repo what-changed is silently unauthenticated** —
  the headline feature broken for the common case. *Fix:* wire the App token into `buildGitOps`'s differ.

#### P1
- **SEC-F1 (Sec)** — No content-redaction layer. Raw logs/diffs → LLM (`investigate/loop.go:332`);
  model-quoted `Evidence` → KB PR body (`curator/draft.go:30`, `forge/github/github.go:109`, public-capable)
  + chat; secret names/keys leak via pod-status (`cluster.go:111`). **Top priority for the data-residency
  ICP**; already roadmapped as R19 — promote. *Fix:* scrub tool output pre-prompt + Evidence/Body pre-forge/notify.
- **COST-C1 (Cost)** — No prompt caching (`anthropic.go:104`, `loop.go:230`): system+schemas+history
  re-sent uncached every step (≤20). *Fix:* `cache_control: ephemeral` on the static prefix → 50–90%
  input-cost cut. Biggest lever, smallest change.
- **COST-C4 (Cost)** — $-ceilings ship OFF: `max_tokens_per_investigation`, `max_tool_output_bytes`
  (`config.go:138`), `rate_limit.max_per_window`, storm-`coalesce` (`values.yaml:50,57`) all default
  0/disabled. *Fix:* sane non-zero defaults + `coalesce.enabled: true`.
- **GO-P1A (Go)** — Leader-flap stampede: GitOps-failure informer initial-LIST re-fires every Ready=False
  workload as a *new* failure each re-acquire; `trigger.Deduper` is per-term (`flux/dynamic.go:211`,
  `main.go:1386`). *Fix:* hoist Deduper to process scope, or seed-skip the initial sync.
- **GO-P1B (Go)** — No graceful drain: `ShutDown()` not `ShutDownWithDrain()`, no worker join
  (`investigate.go:193`, `main.go:287`). SIGTERM kills in-flight work → lost ledger `open`; in `auto`,
  possible silent un-notified mutation. *Fix:* drain + WaitGroup join < `terminationGracePeriodSeconds`.
- **PERSIST-1 (Sec/Go)** — kill-switch resume + rate-limit in-memory (`action/auto.go`,
  `ratelimit/window.go`); fails closed (good) but resume lost + counter resets; `design.md §9` /
  `learning-loop.md §4` imply persistence (drift). *Fix:* persist to PVC (ledger/audit already do), or fix docs.

#### P2
- **SEC-F2 (Sec)** — Model controls `Target.Name`, gate never validates it (`investigate/tools.go:178`,
  `action/policy.go:88`) → injection steers *which* named resource in an allowed ns is suspended (bounded:
  reversible, allowed-ns). *Fix:* require target to match a read-tool-surfaced resource.
- **SEC-F3 (Sec)** — `runlore_reject` not approver-gated (`server.go:283`) → any valid Slack user can
  cancel a pending remediation. *Fix:* same approver allowlist as approve.
- **COST-C2 (Cost)** — Verify always-on, hardcoded, same expensive model (`loop.go:319`, `verify.go:67`).
  *Fix:* configurable + cheaper judge model (`buildJudgeModel` plumbing exists).
- **CLONE-1 (Cost/Perf/Sec; =C3+F5)** — Full clone per change, no cache (`whatchanged/differ.go:138`):
  K clones/monorepo (latency) + disk-DoS. *Fix:* `Depth:1`/`SingleBranch` + per-investigation clone cache + cleanup.
- **GO-P2A (Go)** — Interrupted catalog clone wedges syncer *permanently* (`catalog/sync.go:64`): partial
  `.git` → Pull branch errors forever. *Fix:* temp-dir+rename, or `RemoveAll` before re-clone.
- **GO-P2B (Go)** — Outcome ledger never fsyncs (`outcome/ledger.go:126`; audit does) → crash loses tail
  → decay signal under-counts. *Fix:* `f.Sync()` in `appendLocked`.
- **GO-P2C (Go)** — `Reload` failure advances `lastRev` (`catalog/sync.go:98`) → transient re-index
  failure sticks catalog on old index. *Fix:* advance `lastRev` only after successful re-index.

#### P3 — polish
Guessable approval IDs (`approvals.go:62` → `crypto/rand`) · no `CheckRedirect` + `networkPolicy.strict:false`
default (block link-local on redirect; strict+CIDRs in prod) · Slack decodes possibly-empty 2xx body →
false failure (`slack.go:98`) · discarded `io.ReadAll` error in anthropic/gemini (`anthropic.go:128`) →
wrap like openai · `auto.reserve()` leaks a rate slot if a pause lands in the reserve→exec window.

#### Refuted / solid — do NOT spend effort here
- **Refuted:** "Approve() TOCTOU" (claim-then-execute under one lock — safe); "workqueue dead after
  re-acquire" (restarts correctly; bug is the *opposite* — P1-A over-fires); 156 MB binary (gitignored,
  not in history).
- **Solid:** server-authoritative action gate (discards model-supplied safety, re-validated at exec,
  fail-closed kill-switch); auth (constant-time, replay window, approver + `response_url` SSRF allowlists);
  least-privilege RBAC; untrusted text wrapped in an explicit "UNTRUSTED DATA" delimiter; atomic catalog
  index swap; audit hash-chain.
- **Leave as-is (intentional):** single serialized worker (caps concurrent LLM loops at 1× — core
  cost-safety property); coalesced-but-uncapped queue (bounded by distinct alert identities); catalog-in-RAM.

Status: **complete.**

---

## Step 4 — Docs: comprehensive, pretty, not verbose  ✅ **DONE (core); diagram + deep de-dup deferred**

**Goal.** Comprehensive **and tight** — one source of truth per topic; claims aligned with Step 1.

**Findings already evident:**
- Docs are **extensive and high-quality but verbose and overlapping** — README, `design.md`, and
  `learning-loop.md` re-explain the learning loop three times. Establish a single canonical home per
  concept and cross-link.
- **`prior-art.md` is partly stale** — predates Komodor/Anyshift/Azure-SRE-GA framing and the OKF-v0.1
  reality. Reconcile with Step 1 (this is also POS-2).
- **Overclaiming language** ("exists nowhere else", "the moat") must soften per Step 1.
- **No architecture diagram.** Add a `draw.io` diagram under `docs/architecture/` (per house style) and
  cite it from README/design as the source of truth.

**Targets:** trim README to a scannable landing page; de-dupe design↔learning-loop; fix doc/impl drift
(persistence); refresh prior-art; add the diagram. **Principle:** every paragraph earns its place.

**Done (2026-06-25):** rewrote `prior-art.md` (current + honest — Komodor/Anyshift/Azure-GA, HolmesGPT-CNCF,
kagent-opaque-memory, OKF-v0.1, market froth; reframed "where different" per Step 1); softened `design.md §2`
positioning (dropped "nobody"/"the moat"; led with the open catalog + honesty); added honest **ArgoCD-depth +
Matrix-approval** notes (`design.md §7`); promoted **redaction (R19)** from a footnote to a stated **Known
limitation** callout covering the full egress path (`design.md §9`); added **kill-switch/rate-limit in-memory**
precision (`design.md §9`). README already repositioned (Step 1). *Left `learning-loop.md:207` intact — its
persistence claim is about the ledger/audit (PVC-backable), which is accurate.*

**Deferred (offered):** (1) a `docs/architecture/*.drawio` diagram (house style) — bigger artifact, confirm
scope; (2) the deep README↔`design.md`↔`learning-loop.md` de-duplication — these are good, author-voiced docs,
so a structural trim is a separate opt-in pass, not a unilateral gut. Status: **core complete.**

---

## Step 5 — Improvements: sequenced plan  ✅ **DONE (plan)**

Everything from Steps 1–4, grouped into 5 milestones, sized (**XS** <½d · **S** ~1d · **M** ~2–3d ·
**L** ~1wk) and ordered. Two items are **decisions** (🧭), not code. IDs trace to earlier steps.

### M0 — Make the headline true & cheap out-of-the-box · *do first · ~2–3d*
A fresh `helm install` today has the core feature half-broken and no cost ceiling. Smallest diffs, biggest payoff.
| Item | What | Size |
|---|---|---|
| **AUTH-1** (P0) ✅ | **Done** (`fee6968`, branch `fix/differ-github-app-auth`): `Differ` holds a per-clone `TokenSource`, wired from the shared App token in `buildGitOps`; fail-loud on mint error; tests + lint clean. Unblocked private-repo what-changed — the wedge. | **S** |
| **C1** (P1) ✅ | **Done** (`d822498`): `cache_control: ephemeral` on the system block + last tool (GA, no beta header — verified vs API docs); savings surface as reduced `input_tokens`. | **S** |
| **C4** (P1) ✅ | **Done** (`a5e6e5e`): chart ships bounded defaults (coalesce on · rate 20/window · tokens 100k · output 32KB); Go `0=unlimited` preserved so operators can opt out. helm lint clean. | **S** |
| **C2** (P2) ✅ | **Done** (`8c6d463`): optional `model.verify` override (inherits parent fields) routes the verify pass to a cheaper model; verify stays mandatory. | **S** |

### M1 — Harden the churn paths · *~3–4d · highest-leverage correctness*
For an HA tool, leader-flap / SIGTERM / interrupted-clone / crash are the events it exists for — today lossy & noisy.
| Item | What | Size |
|---|---|---|
| **GO-P1A** (P1) ✅ | **Done** (`e3419c5`): deduper hoisted to process scope, survives leader flaps; cross-term test. | **M** |
| **GO-P1B** (P1) ✅ | **Done** (PR #122): decoupled `workCtx` from the SIGTERM signal — on shutdown the leader drains the in-flight investigation to completion (lease held → no split-brain), then releases; lost leadership still aborts promptly. `Queue.Drain` (ShutDownWithDrain) + chart `terminationGracePeriodSeconds: 40`. Unit + e2e incl. failover. | **M** |
| **GO-P2A** (P2) ✅ | **Done** (`99af960`): re-clone a corrupt mirror; drop a partial clone on failure. | **S** |
| **GO-P2B** (P2) ✅ | **Done** (`99af960`): ledger fsyncs each append. | **XS** |
| **GO-P2C** (P2) ✅ | **Done** (`99af960`): roll back `lastRev` on re-index failure → retried next tick. | **XS** |
| **PERSIST-1** 🧭 | Persist kill-switch/rate-limit to PVC **or** stay doc-only (Step 4 clarified the docs → default doc-only unless resume must survive restart). | **S / 0** |

### M2 — Close the data-egress gap · *~4–6d · top security work, ICP-critical*
| Item | What | Size |
|---|---|---|
| **F1 / R19** (P1) | Content-redaction scrub: tool output → prompt, and evidence → KB PR + chat. Biggest security win; start early (long pole). | **L** |
| **F2** (P2) 🔶 | **Attempted twice, deferred.** *Attempt 1 (narrow — alert seed + what_changed only):* downgraded a legitimate alert-driven auto-suspend (target not surfaced server-side) → reverted. *Attempt 2 (proper):* a context-scoped observed-resource set captured across **every** server-confirming read tool (what_changed + gitops_resource_status + gitops_tree), with **name-tolerant** matching (exact ns/name → name-only fallback, since a GitOps object's namespace varies by view and the gate already gates the namespace). The core works — the e2e `auto-executed` (observed target) passes — but it **regresses the rung-2 two-approve e2e**: `main` is 45/45, the F2 branch 44/45, leaving only one approvable action where `main` reliably has two (the second comes from an informer re-fire). Mechanism **unconfirmed** — likely a false-downgrade of the re-fire investigation's action (the attempt-1 class of bug, in a new place). **Deferred** (low-urgency while `auto` is frozen); code parked on `fix/f2-action-target-validation`. Next attempt must instrument the rung-2 re-fire path (`--keep`) before enforcing. | **M** |
| **F3** (P2) | Approver-gate the `runlore_reject` path. | **XS** |
| **CLONE-1** (P2) | Shallow clone (`Depth:1`/`SingleBranch`) + per-investigation clone cache + cleanup. Also a perf/disk-DoS win. | **M** |
| **P3 bundle** | crypto-rand approval IDs · `CheckRedirect`+`strict:true` egress · Slack empty-2xx · wrap ReadAll errors · reserve-after-pause. | **S** |

### M3 — Focus & positioning decisions · *mostly decisions + small trims*
| Item | What | Size |
|---|---|---|
| **FEAT-1** 🧭 | **Freeze or remove the `auto` rung.** Recommend freezing as experimental (or dropping the executable path): it contradicts the no-unattended-remediation non-goal and is the sole reason the injection→mutation surface exists; `approve` keeps ~95% of value. If removed → shed ~13 config keys + the biggest attack surface. | 🧭 / **M** |
| **FEAT-3/4** | Remove 3 dead config keys (`ActionPolicy.RequireApproval`, `Telemetry.OTLPEndpoint`, `GitHubApp.PrivateKeyRef`); trim config if `auto` is frozen. | **S** |
| **POS-4** 🧭 | Strategic-alignment spike: contribute a what-changed toolset to HolmesGPT / run-on-kagent (A2A) / OKF reference writer-back — turn the biggest threat into a channel. | **spike** |
| **FEAT-2** 🧭 | ArgoCD Inspector parity — *only if* Argo demand appears; else the Step-4 honest-docs path stands. | **M / 0** |
| Docs deferred | `docs/architecture/*.drawio` diagram; deep README↔design↔learning-loop de-dup. | **M** |

### M4 — The one real capability gain · *~M · improves the moat*
| Item | What | Size |
|---|---|---|
| **Hybrid BM25 + embeddings recall** | **Implemented** (PR #120 foundation + #127 integration): in-house `internal/embed` (embeddings client + cosine + RRF — no `chromem-go`/Python, single-binary), wired into the catalog index + recall as an **opt-in, BM25-default** path gated on cosine (RRF candidate fusion → cosine confidence). **Eval-tuning pending:** the cosine thresholds (0.80 / 0.05) are conservative placeholders — set them against the live instant-recall eval (needs a real embeddings endpoint); recall records scores-on-rejection so they're measurable. | **M** |

### Execution order
**Now:** M0 (AUTH-1 → C1 → C4 → C2) + kick off **F1** (the long pole). → **Next:** M1 churn hardening. →
**Then:** finish M2 (F2 / F3 / CLONE-1 / P3). → **Decisions in parallel:** FEAT-1 (`auto`), POS-4, PERSIST-1. →
**Later:** M4 pt2 eval-tuning (operator — needs an embeddings endpoint), M3 deep doc de-dup, FEAT-2 (only on demand).

> Living doc — promote items as they land. **The review (Steps 1–5) is complete; this is the standing backlog.**
