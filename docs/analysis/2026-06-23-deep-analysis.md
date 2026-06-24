# RunLore — Deep Analysis & Improvement Report

| | |
|---|---|
| **Date** | 2026-06-23 |
| **Scope** | Whole-codebase analysis, weighted to the **learning loop** (`retrieve → capture → curate → compound`) |
| **Method** | 50-agent orchestrated workflow: 6 subsystem code-maps + 5 best-practice research tracks (via `context7` + web) → adversarial critique across 6 lenses → **every finding independently re-verified against the code** → viability + roadmap synthesis |
| **Commit analyzed** | `fa37bf2` (merge of `feat/outcome-capture` #69) |
| **Build health** | `go build ./...` green; 79 test files; learning-loop slices #68 (recall trust) + A1 (outcome capture) merged |
| **Findings** | 30 raised, **30 confirmed/partially-confirmed, 0 refuted** (verification tightened several severities downward — see notes) |

---

## 0. TL;DR — the one-paragraph verdict

> **⏱ Update (2026-06-23, post-batch).** §0–§8 below are the original analysis snapshot
> at commit `fa37bf2`. **The make-or-break has since shipped:** the outcome-ledger read
> API (`Episodes()`/`OpenCounts()`, PR #71), outcome-driven recall **decay** (PR #72),
> the **BM25** scorer fix (PR #70), and the rest of Waves 0–4 are merged — **17/18
> roadmap items done, only #17 deferred** — and the full k3d e2e passes end-to-end
> (PASS=40 FAIL=0). So where §0 says "accumulates but does not learn", "`Episodes()`
> does not exist", "A2 unbuilt", or "TF-IDF not BM25" — **those are now resolved.** The
> original prose is kept verbatim as a point-in-time record; the **current** status is
> §9.0/§9.3, and the loop is explained in [`../learning-loop.md`](../learning-loop.md).

RunLore is a **genuinely above-OSS-median engine** (adversarial verify pass, honest first-class `unresolved`, derived-not-asserted recall confidence, untrusted-catalog hygiene) wrapped around a **learning claim it has not yet earned**. The loop today **accumulates but does not learn**: the measurement layer (A1 outcome ledger) is fully wired, but **nothing consumes it** — the outcome→recall feedback edge (A2) and the dormant curate passes (A3) are unbuilt, and `Episodes()` (the A1/A2 seam the spec depends on) does not exist in code. Layered on top are several **foundational defects that make every retrieval and eval claim untrustworthy until fixed** — chief among them: **the catalog silently runs legacy TF-IDF, not BM25** (every comment, metric, threshold, and the entire "corpus-portable margin" premise is tuned against a scorer the code does not run). The project is **worth building, but only as a sharply-focused, propose-and-approve play for the GitOps-native, anti-lock-in buyer — and only if the learning loop actually closes.** The differentiator was never "what changed" or "open runbooks" (both copyable in 12–18 months); it is the **outcome-validated, provenance-tracked, communal** catalog. Build *that*, or don't.

---

## 1. What RunLore is, and its current state

RunLore (`lore`) is an open-source, single-binary Go SRE agent (~17k LOC, 162 files, Go 1.26) that wakes on an incident webhook (Alertmanager/VMAlert), runs a **leader-only ReAct investigation** across 10 tools (what-changed Git diff, PromQL, LogsQL, Hubble, Flux/Argo status, controller logs, CloudTrail, kb_search…), delivers a confidence-scored RCA to Slack/Matrix, and **learns** by writing resolved incidents into an open, git-versioned **OKF knowledge catalog** read back via "instant recall." It is GitOps-engine-agnostic (Flux + Argo), metrics-backend-agnostic (VictoriaMetrics + Prometheus), model-agnostic (Anthropic/Gemini/OpenAI-compatible), and read-only-first with a designed-but-dormant autonomy ladder (`off → suggest → approve → auto`).

The **design docs are excellent** — honest about prior art (HolmesGPT, k8sgpt, kagent), the ITBench <50% RCA reality, and the moat thesis. The engineering discipline (thin vertical slices, brainstormed specs, append-only audit, kill-switch-fails-closed) is well above the OSS norm. This report therefore focuses on the **gap between that design and the shipped code**, and on the **learning loop** specifically.

### Learning-loop status map (verified against `cmd/lore/main.go` wiring)

| Stage | State | Evidence |
|---|---|---|
| **Retrieve** — instant recall 3-gate + `kb_search` | ✅ wired | `recall.go`; built `main.go:889`, invoked `loop.go:110-139` |
| **Capture (A1)** — outcome ledger open/resolve | ✅ wired | `ledger.Open` `main.go:1058`; `ledger.Resolve` `server.go:374` |
| **Curate (file-time)** — dedup → quality gate → PR | ✅ wired | `Curator.Curate` `main.go:1096` |
| **Curate (Phase-2)** — backlog groom | ✅ **Dedup + Lifecycle wired** (was: only Dedup) | `runCurate`; opt-in `lore curate` CronJob (PR #86) |
| **Curate (Phase-2)** — `Queue` / `Recurrence` | ✅ **wired** (was: dormant) | ledger-backed via `Episodes()` — Queue: exact-title resolution join; Recurrence: idempotent existence-check (PR #92) |
| **Compound** — git-sync → bleve reindex | ✅ wired (HEAD-gated, was: every-poll) | re-indexes only on real HEAD change; readiness gated on catalog warmth (PR #82) |
| **A2** — outcome → recall ranking / decay | ✅ **BUILT** (was: ❌ UNBUILT) | `Episodes()`/`OpenCounts()` (PR #71) feed `outcomeFactor` → `deriveRecallConfidence` decay (PR #72) |

---

## 2. The headline finding — the catalog runs TF-IDF, not BM25

**Severity: HIGH · Effort: S (one line) · Corroborated independently by 3 critique lenses and verified through the bleve v2.6.0 source.**

`internal/catalog/catalog.go:36` (`NewEmpty`) and `:68` (`buildIndex`) both call `bleve.NewMemOnly(bleve.NewIndexMapping())` and **never set `ScoringModel = "bm25"`**. In bleve, BM25 is opt-in via the index mapping:

- `bleve_index_api@v1.3.11/indexing_options.go:37` → `const DefaultScoringModel = TFIDFScoring`
- `index_impl.go:714-717` → returns `BM25Scoring` only `if isBM25Enabled(i.m)`, else the default
- `isBM25Enabled` (`index_alias_impl.go:626-632`) → true only when `ScoringModel == "bm25"`; the empty default → **false**
- `search/scorer/scorer_term.go` → with `avgDocLength == 0` it runs `score = tf * norm * idf` (classic TF-IDF), not the saturating, avg-doc-length-normalized BM25.

Yet **every comment, the spec, the metric, and the tuning premise call it BM25**: `catalog.go:12,91,107`; `recall.go:18,21,35`; `config.go:120,389` (`MinScore`/`DupScore` "BM25 floor"); `telemetry/metrics.go:28,63` (`recall_score` histogram). No test asserts the scoring model, so the silent fallback was never caught.

**Why it matters:** the recall trustworthiness slice (#68) is built on a *relative margin* precisely because "BM25 scores are corpus-dependent." But the absolute floors (`MinScore`, `SoloFloor`, the curator's `DupScore=5.0`) and the whole `deriveRecallConfidence` curve are tuned against a *different scorer's* distribution. The relative `MarginGap` is partly insulated; the absolute floors are not. **This is the cheapest high-leverage fix in the entire codebase, and it blocks any honest retrieval tuning** (#9 decay, #3 indexing) until it lands.

> **Worth filing to wiki** (`type: Topic` gotcha): bleve v2.6.0 BM25 is opt-in via `mapping.ScoringModel = "bm25"` and **defaults to TF-IDF** — relevance-score thresholds tuned on an unset mapping are tuned against TF-IDF regardless of what your comments say.

---

## 3. The learning loop — does it actually learn?

The honest answer today: **no — it measures.** Five verified structural facts explain why, and exactly what closes the gap.

### 3.1 The make-or-break: no feedback edge exists yet
- `recall_outcome_total` is emitted **only on a matched resolve** with `result="resolved"` (`server.go:379-381`) — the **only** outcome-quality counter. There is **no `unresolved`/`recurred`/`expired` event** anywhere; the negative signal (the one decay needs) is represented purely as *the absence of a resolve line* (spec §3.4 says so explicitly).
- **`Episodes()` — the spec's A2 reader (`2026-06-23-...-design.md:51`) — does not exist** in `ledger.go` (it has exactly four methods: `enabled`, `appendLocked`, `Open`, `Resolve`). Nothing can compute per-entry `recall_count`/`resolved_count`.
- `deriveRecallConfidence` (`recall.go:128-137`) takes only `(score, margin, strength)` — fixed constants, **zero outcome input, no ledger wiring**. Until a recalled entry's resolve-rate changes whether it is recalled again, "self-improving" is a credibility liability.

**This is correctly-scoped, documented A1/A2 deferral — not an accidental bug.** But it is the thesis, and it is unbuilt.

### 3.2 The signal that *is* captured is the wrong one
- **Recall episodes (the only entry-attributed, causally-meaningful outcome) are structurally the rarest.** Recall short-circuit is disabled under `auto` (`loop.go:110`, the safety guard against poisoned-catalog auto-execution), so the mode where the catalog drives behavior produces **zero `kind=recall` opens** — the ledger fills with `kind=fresh` rows carrying **no entry link** (`main.go:1061` `Entry: found.RecalledEntry`, set only on the recall path). Fresh findings can never validate the entry they eventually curate.
- **Coalescing orphans N-1 of N fingerprints.** The flush sink builds the Request from `incs[0]` only (`main.go:238-243`); the other N-1 resolves hit `ledger.go:117-120` and silently drop (`ok=false`). In storm-heavy environments — the exact case coalescing targets — attribution degrades to 1/N.
- **Open/Resolve ordering race.** The `open` is stamped at `OnComplete` with `time.Now()` (`main.go:1058`), but a transient incident can resolve *during* the multi-minute investigation: `Resolve` runs first (signal lost, `ok=false`), then `Open` writes a record that **never resolves**. With no TTL on the open-index (`grep` for `ttl|expir|sweep` over `internal/outcome` → empty), every such case is an **immortal stale open** that JSONL replay re-loads forever.

### 3.3 Recall precision — Gate 2 collapses to namespace equality
The structural-agreement gate is, per decision D2, *"the lever that can separate many-to-one symptom→cause."* In code it **cannot**, on the dominant alert path:
- **Read side:** `FromIncident` builds `Workload{Namespace: inc.Namespace}` with **no `Name`** (`investigate.go:48`); the alert's `pod`/`deployment` labels are available (`incident.go:44`) but never used.
- **Write side:** `parseFindings` (`tools.go:61-92`) has **no field for a discovered affected resource** — the model's identified root-cause workload is structurally unrecordable; `loop.go:201` then overwrites `inv.Resource` with the namespace-only `req.Workload` *after* the model concluded.
- **Round-trip:** `resourceAgrees` (`recall.go:99-113`) therefore only ever compares namespace-to-namespace → **any `apps` alert recalls any `apps` entry**, exactly the CrashLoopBackOff = OOM | bad-image | missing-secret granularity the gate was meant to disambiguate.

Mitigations bound the blast radius (recall off under `auto`; verify pass; confidence capped 0.90; `require_workload_match` opt-in), so this **degrades recall precision** rather than causing wrong auto-remediation — but it defeats the stated purpose of the lever.

### 3.4 Verify is a no-op on the recall path
Gate 3 (verify) is presented as the safety backstop, but `recalledInvestigation` (`recall.go:142-158`) supplies as its **only** evidence the tautological string `"instant recall: matched knowledge-base entry X"`. `verifyFindings` judges "ONLY on the evidence given" (`verify.go:18`) with **no tools and no incident telemetry** (`verify.go:63-87`), and can only lower confidence. A well-written, confidently-worded *wrong* entry sails through. (Bounded: verify is the 3rd gate, not the primary defense, and recall is off under `auto`.)

### 3.5 Curation never compounds at rate
- **File-time open-PR dedup keys on LLM free-text title** (`duplicateOpenPR`, `curator.go:75-87`) — an exact normalized-title equality, **not** the spec's deterministic `alertname+ns+workload+root-cause` fingerprint (`2026-06-21-...:88`). Two investigations of one incident produce different prose titles → **both file** → the 5×-DependencyNotReady flood the spec was written to kill is **not prevented**. (The reliable fields `inv.Resource`/`inv.Fingerprint` exist on the Investigation but neither dedup path consults them.)
- **Catalog dedup can never see an open-but-unmerged PR** — `Novelty.IsDuplicate` searches only the on-disk, main-branch bleve index; drafted PR branches aren't on disk. So the only open-PR guard is the weak title check above.
- **`meetsBar` (`curator.go:92-98`) cannot enforce the spec's quality criteria** — it ignores the verify pass and fixing-change provenance, so an unverified, symptom-only finding can draw a "merge-ready" decision card.
- **Three of four Phase-2 passes are dormant for want of trivial wiring** (`Queue`, `Lifecycle`, `Recurrence`), and the only groom is a **manual one-shot CLI with no scheduler** (no CronJob in the chart). Knowledge compounds only as fast as humans merge (~14% baseline).

---

## 4. Catalog / retrieval architecture

| Issue | Detail | Severity |
|---|---|---|
| **TF-IDF not BM25** | §2 above | High |
| **Single conflated `text` field, default OR matching** | `text = Title+Description+Tags+Body` joined (`catalog.go:74-75`); a title term and a deep body term score equally; `NewMatchQuery` defaults to OR → any one-token overlap is a candidate. Harmless at ~1 seed entry; **precision collapses at 500–5000** with only the relative margin + structural gate between a wrong entry and a short-circuit. | Medium |
| **Structural agreement is a post-rank filter at effective k=1** | `SearchScored(query, 2)` then `resourceAgrees` runs **only on `hits[0]`** (`recall.go:67-68`); `hits[1]` is just the margin denominator. The right entry ranked #3 is never seen — and alert titles rarely share tokens with root-cause runbooks, so the lexically-best 2 are precisely the likely-wrong ones. `Resource` is **deliberately not indexed**. | Medium |
| **`bleve.NewMemOnly` — no persistence, no vectors** | Rebuilt from on-disk markdown every reload; cold-start serves knowledge-free recalls until first sync; `readyz` not gated on `cat.Len()>0`. | Low |
| **Full index rebuild every poll** | `Reload` rebuilds the whole index regardless of HEAD change — contradicts design §8's "incremental re-index"; every standby replica rebuilds an index it never queries (leader-only investigates). | Low |

**The fix path** (validated against `context7` docs for bleve + chromem-go): set BM25; index `cause`/`resolution` as weighted fields and `resource`/`namespace` as **filterable** fields; widen internal `k` to ~10–20 and make structural agreement a **pre-filter** (bleve `ConjunctionQuery`), not a post-gate. Phase-2 vectors should be **chromem-go (pure-Go) + Reciprocal Rank Fusion in Go + vLLM embeddings via `QueryEmbedding`** — this preserves the single-binary property, whereas bleve-native KNN needs on-disk scorch + FAISS cgo (breaks pure-Go). **Do not add a cross-encoder reranker** — the verify pass already plays second-stage judge and the catalog is small.

---

## 5. Evaluation harness — the validity ceiling

The harness is well-architected (two modes, deterministic coverage + LLM-judge rubric, always-teardown, dated reports) but **every quantitative claim it produces is currently unsafe to believe**:

| Issue | Detail | Severity |
|---|---|---|
| **N=3 bare-median gate is a coin flip** | Gate = `median(root_cause)>=2 && coverage==1.0 && !confident_wrong` (`live.go:159`). The committed OOM report scored root_cause `{0,2,1}` (variance 0.67; solution variance 2.0) — flip one of three runs and the verdict flips. N<30 is a flakiness alarm, not a measurement. | High |
| **Single same-family judge, no calibration, no over-claim penalty** | `gemini-2.5-pro` grading `gemini-2.5-flash` (self-preference); `buildJudgeModel` even **defaults to the same model as the agent**. No human-κ golden set, no jury. Rubric accepts `root_cause=2` ("correct but shallow") — the image-tag run **PASSED** while the judge's own rationale said *"it doesn't explicitly state that a change introduced this… the ultimate root cause"* (the ITBench symptom-vs-change failure mode). No precision/FP penalty: "true cause + 2 wrong causes" is unpenalized. | High |
| **Recall closed-loop is dead by construction in eval** | `known-pattern-recall.yaml` claims to validate the short-circuit, but `runEvalLive` discards recall (`main.go:556` `_, _`) and `LiveRunner` has no `Recall` field → the `if li.Recall != nil` block (`loop.go:110`) **can never fire in eval**. The scenario can only "pass" via the agent organically calling `kb_search` — a different mechanism. **"Learning works" is unexercised, not just unrun**, and no committed report ever ran it. | Medium |
| **Errored tools count as covered** | `ScoreCoverage` marks a source `seen` even when the call errored (`coverage.go:109-111`, no `continue`). The one gitops-mandatory scenario could score `Ratio=1.0` from a `what_changed` that returned only an error — falsely passing the deterministic gate on zero diagnostic data. (`CrossSignal` is dead code, read only in tests.) | Medium |
| **Judge failure = silent agent failure** | A judge API error or unparseable JSON leaves a zero `Verdict{}` (`live.go:111-119`) → `root_cause=0` in the median **and** `confident_wrong=false`, silently disengaging the safety net. Indistinguishable from a real RCA failure; fabricates/masks regressions. | Medium |
| **k3d-only evidence; not in CI** | Every committed run is k3d, where only the `kubernetes` source exists — **5/7 signal sources and all GitOps/cloud differentiators are untested**. `grep eval .github/` → empty: eval never gates merges. | High (claims) |

---

## 6. Architecture & production-readiness

| Issue | Detail | Severity |
|---|---|---|
| **Learning signal + audit trail are ephemeral as deployed** | The outcome ledger ("did it work?") and the hash-chained audit log (autonomy accountability) are both written to an `emptyDir` (`deployment.yaml:103`); `values.yaml:70` points `ledger_path` at a "git-sync mirror PV" that **does not exist** (no PV/PVC/StatefulSet anywhere). `updateStrategy: Recreate` kills pods every upgrade. Neither is `fsync`'d. Docs say "survives restart/failover"; as shipped, **destroyed on every restart**. | Medium |
| **The GitOps "what-changed" spine is inert against private repos** | The flagship differentiator: `Differ.Token` is never set, so private GitOps clones are unauthenticated. | High (product) |
| **Autonomy ladder is architecturally complete but functionally hollow** | `auto` can only `suspend`/`resume`/`reconcile` Flux — it can **never remediate a "what-changed" incident** (no reversible `rollback` to the prior revision). The safety machinery gates a vocabulary that can't fix the incidents the engine specializes in. | Medium |
| **ReAct loop grows context O(N²); cost ceiling is advisory** | No tool-result elision after consumption, no loop/repeat detection; the budget is a `len/4` token *guess* and a nudge, not a hard kill → the 768Mi→1.5Gi OOM and runaway-cost risk is structural. | Medium |
| **MCP "extension layer" is claimed but unbuilt; scope is broad for a solo maintainer** | 8 provider types + autonomy ladder + learning loop + eval harness is a multi-person-year program for one maintainer. | Strategic |

---

## 7. Best-practice deltas (from `context7` + web research)

- **Agent loops** (Anthropic "building effective agents"): RunLore already aligns well — single tight loop, structured output, adversarial self-critique, honest uncertainty. **Gaps:** add explicit **context compaction / tool-result elision** for long loops, and a **hard** token-budget kill (not advisory). The verify-can-only-weaken design is a recognized good pattern; keep it.
- **Retrieval/RAG:** the many-to-one symptom→cause problem is **structurally unsolvable in the lexical layer** (the alert title rarely shares tokens with its root-cause runbook). Consensus fix: **index cause/resolution text, field-boost, metadata pre-filter, then BM25+vector hybrid via RRF.** RunLore's structural-agreement idea is sound but mis-implemented (post-gate at k=1). Abstention/confidence-on-retrieval is a real pattern — RunLore's derived confidence is good practice, *once the scorer is correct*.
- **LLM-as-judge:** the literature is unanimous on judge bias (self-preference, position, verbosity), the need for a **cross-family judge**, a **human-κ golden set**, and **deterministic entity-level precision** rather than a single fuzzy score. RunLore needs all three. ITBench's own finding — *more turns correlate with worse scores because strong models over-claim* — means an eval with **no over-claim penalty cannot see the dominant failure mode**.
- **Agentic memory:** Letta/MemGPT, Mem0, Zep all implement explicit **decay + reinforcement from outcomes**; the OKF enrichment loop and Karpathy's LLM-wiki are read→generate→human-push. RunLore's outcome-ledger→decay→recurrence design is a **credible path**, but it is currently *capture-only* — the minimum viable closed loop is `Episodes()` + Bayesian-smoothed resolve-rate biasing recall + invalidate-on-contradiction (never pure-mtime) decay.
- **Landscape:** backend-agnosticism is **dead as a differentiator** (MCP commoditized it). "What-changed" deploy-correlation already ships in Azure SRE Agent, Datadog Bits, incident.io. kagent already brands "agents as CRDs versioned in Git, reviewed in PRs." **The only structurally durable moat is a *communal*, provenance-tracked knowledge commons with network effects** — which the closed incumbents *cannot* copy because lock-in is their business model. The pitch under-emphasizes exactly the one defensible thing.

---

## 8. Viability & interest assessment

**Genuinely novel:** the **A1 outcome ledger** (no OSS SRE agent records whether a *recalled answer* preceded resolution) + an **open, git-versioned, PR-reviewed, provenance-tracked** catalog. The adversarial verify pass, first-class `unresolved`, and derived recall confidence are above the OSS median. **Everything else is commodity.**

**The moat, by decreasing fragility:** (1) the GitOps git-diff spine is a *real depth advantage* but a 12–18-month copyable head-start — and **currently inert against private repos** (`Differ.Token` unset); (2) the per-tenant open catalog is differentiated on *form*, copyable in ~1 year; (3) **the communal, compounding, provenance-tracked commons is the only durable moat** — and it requires distribution + community-building a solo maintainer realistically cannot deliver alongside the engine.

**Realistic adopter:** platform-engineering teams at **GitOps-native, multi-cloud, lock-in-allergic** Series B–D / reliability-mature mid-market companies who reject single-telemetry-vendor (Datadog Bits, Grafana) and single-cloud (Azure SRE Agent, Gemini Cloud Assist) agents. Natural wedge: teams already on HolmesGPT/kagent who feel the pain of un-versioned runbooks. This is a **defensible niche, not a category challenger** to Resolve.ai/Cleric.

**Existential risks:** sub-50% RCA is the *baseline reality* and the eval can't even see over-claiming; **KB-poisoning with no mechanism to overturn a confirmed-wrong belief** (A2 unbuilt); the "learning" claim is **not yet true**; the evidence base is **k3d-only, N=3, single same-family judge, not in CI, with a silent TF-IDF bug**; and **single-maintainer scope**.

**Verdict: worth building** — the engine earns the right to exist. It becomes a confident **yes** when, in order:
1. **The minimum-viable loop closes** — `Episodes()` + bias `deriveRecallConfidence` by Bayesian-smoothed resolve-rate + invalidate-on-contradiction decay. Small, sits in existing A1+recall code, flips "accumulates"→"learns." **Make-or-break.**
2. **Eval detects over-claiming and runs on real signals in CI** — `root_cause_entities` precision, fix TF-IDF→BM25, run on a cluster exercising gitops/metrics/cloud, k-of-n CI gate.
3. **The pitch leads with the communal knowledge commons**, treats what-changed as a feature (and wires `Differ.Token`), and owns propose-and-approve.

If those don't land, RunLore is a strong 18-month product with a copyable moat that Microsoft, kagent, and Aurora will erode. If they do, it owns a niche the funded incumbents structurally cannot follow into.

---

## 9. Improvement roadmap

Effort: **S** ≤1 day · **M** 2–4 days · **L** ≥1 week. Impact is on the learning loop unless noted.

### 9.0 Implementation status (updated 2026-06-23)

**17 of 18 roadmap items merged** — Waves 0–4 are complete bar the one deferred item (#17). Each shipped as its own brainstorm → spec → plan → subagent-implemented → reviewed PR. The full k3d e2e (`hack/e2e-k3d.sh`) passes end-to-end after the batch (**PASS=40 FAIL=0**); it caught + fixed a curation regression from #16's `Verified` gate (the e2e mock didn't answer the verify pass — PR #87).

| # | Item | Status |
|---|------|--------|
| 1 | BM25 scorer | ✅ merged (PR #70) |
| 2 | discovered-resource read+write (disambiguation) | ✅ merged (PR #73) |
| 3 | recall retrieval — wider k + structural pre-filter | ✅ merged (PR #74) |
| 4 | eval entity precision + over-claim penalty (Track A) | ✅ merged (PR #80) |
| 5 | eval stats — N≥10 + k-of-n + variance gate | ✅ merged (PR #78) |
| 6 | recall closed-loop in eval + poisoned-entry proof | ✅ merged (PR #79) |
| 8 | outcome `Episodes()` / `OpenCounts()` read API | ✅ merged (PR #71) |
| 9 | outcome-driven decay (the thesis) | ✅ merged (PR #72) |
| 10 | outcome attribution — per-fingerprint open, race/TTL | ✅ merged (PR #75) |
| 14 | durability — PVC for ledger+audit + fsync | ✅ merged (PR #77) |
| 15 | loop cost — hard token kill | ✅ merged (PR #76) |
| 7 | eval into CI (nightly k-of-n + fail-under) | ✅ merged (PR #81) — nightly+dispatch workflow; needs the `RUNLORE_EVAL_API_KEY` secret to run |
| 11 | curation dedup fingerprint | ✅ merged (PR #83) |
| 12 | curation Phase-2 — `lore curate` CronJob + dormant passes | ✅ **merged** (PR #86 scheduler + Dedup/Lifecycle; PR #92 Queue + Recurrence) — all four passes wired |
| 13 | confirmatory evidence on recall | ✅ merged (PR #84) |
| 16 | `Verified`/provenance in `meetsBar` | ✅ merged (PR #85) |
| 17 | reversible `rollback` op | ❎ **won't build** (decided 2026-06-24) — in-cluster re-pin fights the safety model + diverges cluster↔Git; remediation stays read-only. See §9.4 |
| 18 | HEAD-diff sync + `readyz` gate | ✅ merged (PR #82) |

Specs/plans for each merged item live under `docs/superpowers/specs/` and `docs/superpowers/plans/` on `main`.

**Remaining work:**
- **#17 (reversible `rollback`)** — **decided against** (2026-06-24). A scoped brainstorm found the in-cluster re-pin can't fit the safety model cleanly; remediation stays read-only / propose-and-approve. Full rationale in §9.4 and `docs/design.md` (the "Act" section). *This is the only roadmap item not shipped.*
- **#12's Queue + Recurrence** — ✅ **wired** (PR #92, 2026-06-24), ledger-backed via `Episodes()`: Queue resolves a PR's incident by exact-title match against the ledger; Recurrence opens a knowledge-gap issue idempotently via an existing-issue check. The chosen exact-title join is coarser than the incident fingerprint (a human-gated promotion can fire slightly early on coalesced/cross-namespace incidents) — a documented tradeoff; the fingerprint-precise variant remains a possible future refinement.
- **Nightly eval (#7)** runs once the `RUNLORE_EVAL_API_KEY` repo secret is set (it drives a live LLM, so it can't gate fork PRs and isn't a per-PR blocker — by design).

### 9.1 Ranked improvements

| # | Area | What | Why | Effort | Impact | Depends |
|---|------|------|-----|--------|--------|---------|
| **1** | scorer | Set `ScoringModel="bm25"` at both index sites; test it; re-fit floors from `RecallScore` histogram | Index silently runs TF-IDF; every threshold + "corpus-portable margin" premise is invalid | S | High | — |
| **2** | recall write | Populate `Workload.Name` from alert labels in `FromIncident`; add `affected_resource` to `parseFindings`; set `inv.Resource` to the *discovered* failing workload, not `req.Workload` | Gate 2 collapses to namespace equality; discovered cause is unrecordable | M | High | — |
| **3** | recall retrieval | Index `resource`/`namespace` filterable; widen internal k≈15; structural agreement as **pre-filter**; index cause/resolution text | Correct entry ranked #3 is invisible; alert title ≠ runbook tokens | M | High | 1,2 |
| **4** | eval validity | Add `root_cause_entities`; deterministic recall-gated precision in Track A; over-claim/FP penalty; reserve judge for fuzzy dims | Single same-family judge, `root_cause≥2` accepts shallow, no precision penalty | M | High | 7 |
| **5** | eval stats | N≥10; **k-of-n** pass rule; bootstrap CI; gate on `DimVariance`; log N in report header | N=3 median is a coin flip | S | High | — |
| **6** | eval closed-loop | Thread `Recall` into eval `LoopInvestigator`; assert short-circuit *fired*; seed fixture entry; add **poisoned-entry** case | `known-pattern-recall` tests a path dead by construction | M | High | 1,2 |
| **7** | eval CI | Wire eval into `ci.yaml` with k-of-n + `fail-on-threshold` | Eval never gates merges | S | High | 4,5 |
| **8** | outcome | Implement `Episodes()` (replay JSONL → per-entry recall/resolved/expired/last_confirmed); add `OpenCounts()` | The A1/A2 seam; nothing can compute the decay signal | M | High | — |
| **9** | outcome decay | Bias `deriveRecallConfidence` by Bayesian-smoothed `(resolved+1)/(recalls+2)`; outcome/contradiction-driven (never mtime); surface aggregates onto frontmatter | **The thesis.** "accumulates"→"learns" | L | Highest | 1,2,8 |
| **10** | outcome attribution | Record `open` per fingerprint in a coalesced batch (or group-key map); fix open-before-resolve race; TTL sweep emits `incidents_unresolved_total` | Coalescing orphans N-1; transient-resolve leaks immortal opens | M | Med-High | 8 |
| **11** | curation dedup | Replace title-equality with deterministic fingerprint (`inv.Resource.Ref()`+alertname+root-cause token-set) stored in PR frontmatter | The 5× flood is not prevented | M | Med | 2 |
| **12** | curation Phase-2 | `lore curate` CronJob in chart; light up Queue/Lifecycle/Recurrence (resolved-webhook signal, forge `updated_at`, ledger-backed `RecurrenceStore`) | 3 of 4 passes dormant; no scheduler; ~14% merge rate | M | Med | 8 |
| **13** | recall verify | On short-circuit, run a minimal confirmatory step (`pod_status`/`kube_events`) and feed *that* to verify; or cap recalled confidence well below 0.90 | Verify on recall is a no-op | M | Med | 2 |
| **14** | durability | StatefulSet/PVC for ledger+audit (not `emptyDir`); `f.Sync()` after audit writes; fix the misleading `values.yaml:70` comment | Learning signal + audit chain destroyed on restart | M | Med | — |
| **15** | loop cost | Provider token usage → **hard** kill; tool-result elision once consumed; repeated-`(tool,args)` loop detection | O(N²) growth, advisory budget, OOM risk | M | Med | — |
| **16** | merge bar | Add `Verified bool` to `Investigation`, set in `verifyFindings`, require in `meetsBar`; require causing+fixing provenance before drafting | `meetsBar` can't enforce verify/provenance | M | Low-Med | — |
| **17** | remediation | Add a reversible `rollback` op (re-pin Kustomization/HelmRelease to prior revision via the what-changed diff) | `auto` can't fix "what-changed" incidents | L | Low (loop) / High (product) | — |
| **18** | sync cost | Gate `Reload` on real HEAD change; leader-only sync or persisted scorch index; gate `readyz` on `cat.Len()>0` | Full rebuild every poll on every replica; cold-start knowledge-free | M | Low | 1 |

### 9.2 Top 5 — implementable slices

**Slice 1 — Fix the silent TF-IDF→BM25 scorer (#1, S).**
Touch `catalog.go:36,68` → factor a `newIndexMapping()` helper that sets `im.ScoringModel = index.BM25Scoring`; re-fit `recall.go:52-63` floors and `fingerprint.go:44` `DupScore` from the live `RecallScore` histogram (prefer percentile floors); fix the now-false "BM25" comments. **Test:** `TestIndexUsesBM25` asserting the active model. **Proof:** the histogram shifts to a bounded saturating distribution; recall precision@1 holds with no new `low_margin` rejections. Ship the scorer flip + test in one PR, re-tune in a follow-up (independently revertible).

**Slice 2 — Make Gate 2 disambiguate: record the discovered resource (#2, M).**
Read side `investigate.go:48` — derive `Workload.Name`/`Kind` from Alertmanager `pod`/`deployment`/`statefulset` labels. Write side `tools.go:32-92` — add `affected_resource` to the `submit_findings` schema + `findings` struct. `loop.go:201` — prefer the model's discovered resource; fall back to `req.Workload` only when absent. Mirror on `recall.go:154`. Until both sides carry ns/name reliably, gate the short-circuit on `strength == matchExact` (`loop.go:110`) so precision can't regress. **Tests:** `TestGate2DiscriminatesWithinNamespace`, `TestParseFindingsCapturesAffectedResource`. **Proof:** an EKS/k3d scenario with two known causes in one namespace — pre-fix both recall the same entry, post-fix each recalls its own.

**Slice 3 — Implement `Episodes()` (the A1/A2 seam) (#8, M).**
Add `func (l *Ledger) Episodes() []Episode` (replay JSONL, reconstruct open→resolve pairs incl. multiple opens/fingerprint) and `OpenCounts() map[string]Aggregate` (per-entry `recall_count`/`resolved_count`/`expired_count`/`last_confirmed`) to `ledger.go`. Pure additive read-only over the existing append-only file (the in-memory index is lossy by design; the JSONL is not). **Tests:** `TestEpisodesReconstructsRecurrence` (3 opens + 1 resolve → recall_count=3, resolved=1), `TestEpisodesPerEntryAggregate`. *Sequencing note:* before entry-level decay works, fresh investigations must carry an entry link — append a `link` event tying fingerprint→curated-entry-path after `Curate` returns (`main.go:1096`).

**Slice 4 — N≥10 + k-of-n gate + variance gating (#5, S).**
`main.go:465` raise `-n` default to ≥10 for reported claims; `live.go:156-159` replace bare median with a k-of-n rule (e.g. ≥70% clear `root_cause≥2`) + bootstrap CI + **fail/flag** scenarios whose `root_cause` variance exceeds a threshold; log N + CI in the report header. Keep N=1 for fast local smoke. **Tests:** `TestKOfNGate` (`{0,2,1}` fails, `{2,2,1}` passes), `TestHighVarianceFlagsUnreliable`.

**Slice 5 — Exercise the recall closed loop in eval + poisoned case (#6, M).**
`main.go:556` capture the recall return: `model, tools, recall, _ := buildModelAndTools(...)`; add a `Recall` field to `LiveRunner` and thread into the `LoopInvestigator` at `live.go:95-98`; emit an explicit **recall-fired** flag (distinct from `coverage[kb]`) and assert zero non-kb tools ran. Seed a deterministic fixture entry; add `known-pattern-recall-poisoned.yaml` (a crafted high-recall entry with a *wrong* resolution that verify-on-recall must catch). **Tests/proof:** the recall scenario passes *via the actual short-circuit*; the poisoned scenario passes *because the wrong recall was downgraded/rejected*. Wire both into CI under Slice-4's gate.

### 9.3 Sequencing — weighted to the learning loop

```
Wave 0 — Make measurement trustworthy (parallel, no deps)
  ├─ ✅ Slice 1 (#1)  BM25 scorer .............. S → unblocks 3, 9, 18
  ├─ ✅ Slice 4 (#5)  N≥10 + k-of-n + variance . S → unblocks all eval claims
  └─ ✅ Slice 3 (#8)  Episodes() read API ...... M → unblocks 9, 10, 12

Wave 1 — Make recall disambiguate, then prove it
  ├─ ✅ Slice 2 (#2)  Discovered-resource read+write . M → unblocks 3,11,13
  ├─ ✅ #3  resource pre-filter + cause indexing ..... M [needs 1,2]
  └─ ✅ Slice 5 (#6)  Recall into eval + poisoned case  M [needs 1,2] ← first proof the loop closes

Wave 2 — Close the outcome→recall feedback edge (make-or-break)
  ├─ ✅ #10 coalesce/race/TTL attribution ...... M [needs 8]
  ├─ ✅ #9  outcome-driven decay ............... L [needs 1,2,8] ← THE THESIS
  └─ ✅ #13 confirmatory evidence on recall .... M [needs 2]

Wave 3 — Compound faster + harden
  ├─ ✅ #7  eval into CI ....................... S [needs 4,5]
  ├─ ✅ #4  entity-level precision in eval ..... M (Track A; landed ahead of #7)
  ├─ ✅ #11 deterministic dedup fingerprint .... M [needs 2]
  ├─ ✅ #12 curate CronJob + all 4 passes ..... M [needs 8] (Queue+Recurrence wired, PR #92)
  └─ ✅ #16 Verified/provenance in meetsBar .... M

Wave 4 — Reliability & durability (parallel, independent)
  ├─ ✅ #14 StatefulSet/PVC + fsync ............ M
  ├─ ✅ #15 hard token kill + context compaction  M
  ├─ ✅ #18 HEAD-diff sync + readyz ............ M [needs 1]
  └─ ❎ #17 reversible rollback op (product) ... L  ← decided against (§9.4)
```

> **Status (2026-06-24, updated):** Waves 0–4 complete; **17 of 18 items shipped** — only **#17 is decided against** (§9.4). #12's Queue + Recurrence passes are now wired (PR #92), so all four Phase-2 passes are live. The eval-validity cluster (#4/#5/#6) is done; the full k3d e2e passes (PASS=40 FAIL=0). See §9.0 for the per-item PR map and remaining-work notes.

**Fastest credible "we learn" demo:** Wave 0 + Slice 2 + Slice 5 + #9 — a poisoned/stale entry recalls, never resolves, decays below the floor on the next occurrence, triggers a fresh re-investigation that overturns it, observable in the eval harness *and* on the entry's git frontmatter.

### 9.4 Decision (2026-06-24) — #17 reversible `rollback`: won't build

A reversible in-cluster `rollback` op (re-pin a Flux Kustomization / ArgoCD Application to its prior
revision) was scoped through a full brainstorm and **rejected**. The reasons, in order of weight:

1. **It can't fit the safety model cleanly.** A Flux Kustomization has no per-resource revision pin,
   so re-pinning it must patch its owning **GitRepository** — which in the common monorepo layout
   lives in **`flux-system`**, a *built-in protected namespace* the action gate forbids as a target
   (and the namespace-scoped action RBAC doesn't cover). Every implementation path required weakening
   a safety invariant (a new flux-system-patching exception).
2. **It is anti-GitOps.** An in-cluster re-pin makes the cluster's desired state diverge from Git:
   Flux reports drift, the next legitimate Git change collides with the manual pin, and the
   divergence must be remembered and undone.
3. **Little is lost.** The agent already *suggests* a rollback in `suggest`/`approve` mode; only
   executing it is withheld. For a GitOps shop, "the agent advises, a human merges the revert" is the
   correct division of labor — and the upside is modest against the outage risk of a wrong rollback
   on a sub-50%-RCA baseline.
4. **The moat isn't remediation.** Per §8, the durable differentiator is the outcome-validated,
   provenance-tracked communal catalog (now built and closing) — not mutating remediation. Read-only-
   first is a selling point for the safety-conscious, lock-in-allergic buyer.

**If revisited:** the GitOps-correct form is a **Git-revert PR** (reuse the curator/forge path) — in
sync with Git, inherently reversible, reviewed — **not** a cluster patch. Recorded in
`docs/design.md` ("Act" section). This supersedes §6's framing of the autonomy ladder as "functionally
hollow without rollback": that vocabulary gap is now an intentional posture, not a defect.

---

## 10. Method & confidence note

30 findings were produced by 6 adversarial critique lenses, each seeded with a distilled context pack, then **every finding was independently re-verified by a separate skeptical agent that re-read the cited code** (50 agents total, ~2.8M tokens). **0 were refuted**; verification *tightened* several severities downward — notably the recall structural-gate (critical→high) and several outcome-loop items (high→medium), reclassified as **documented, intentional A1/A2 deferrals rather than latent bugs**. Minor line-number drifts and one TF-IDF-normalization nuance were corrected in place; none changed a conclusion. The single highest-confidence, highest-leverage finding — the TF-IDF/BM25 scorer — was corroborated by 3 independent lenses and traced through the bleve v2.6.0 source.
