# R23 — Poisoned-entry eval scenario + don't serve "ready" with no KB — design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation |
| **Date** | 2026-06-24 |
| **Base** | worktree fast-forwarded to `main` (`d06ad6e`, includes R2 fall-through) |
| **Scope** | (1) Keep `/readyz` at 503 when a catalog is *configured but failed to load* (today it collapses to pure leadership → the pod serves incident traffic blind). (2) Ship a real **poisoned-entry live-eval scenario** under `eval/scenarios/`, run by the live harness, backing the docs claim that the closed loop is *exercised*, not just unit-tested. |
| **Author** | Smana (paired with Claude) |
| **Related** | `2026-06-23-sync-readyz-design.md` (the readyz-on-warmth gate this extends); `2026-06-23-recall-in-eval-design.md` (wired recall into eval but **deferred** the live fixture — D3/§5); `2026-06-24-recall-fallthrough-design.md` (R2: a verify-rejected recall now falls through to a real investigation) |

---

## 1. Findings — challenged against current code

### Part 1 — readyz serves "ready" with no KB on a configured-but-failed catalog — **CONFIRMED**

`buildCatalog` (`cmd/lore/main.go:521-531`) static path returns `nil` on a load failure; the unconfigured path *also* returns `nil` (`:531`). `readyFunc` (`cmd/lore/main.go:1230-1237`) gates only `cat != nil && !cat.Ready()`, so a `nil` catalog imposes **no** catalog gate. A `*catalog.Catalog` cannot distinguish "no catalog configured" from "configured but the load failed". So when an operator *configured* `catalog.dir` (a ConfigMap mount) but the load failed — bad mount, unreadable dir, a `Load` parse error — `buildCatalog` returns `nil`, `readyFunc` imposes no gate, and the leader passes `/readyz` and serves webhook traffic with **no knowledge base** and no `kb_search`/recall tool (those are wired only when `cat != nil`, `main.go:1062-1064`). The pod silently investigates blind.

The prior sync-readyz design (`2026-06-23-sync-readyz-design.md` §B) gates readiness on warmth and treats `nil` as "none configured → no gate". It handles "configured + warm" (static `New` ⇒ `Ready()==true`) and "configured + not-yet-warm" (git-sync `NewEmpty` ⇒ `Ready()==false`), but the **static-load-failure** path is the hole: it returns `nil`, indistinguishable from unconfigured.

**Verdict: real bug, not overstated.** `cmd/lore/main.go:524-525` (`return nil` on static failure), `:1230-1237` (readyFunc cannot tell the two nils apart), `:1062-1064` (nil cat ⇒ no KB tooling).

### Part 2 — the live poisoned-entry *scenario* is genuinely absent — **CONFIRMED**

The docs assert a *scenario*, not a unit test:

- `docs/learning-loop.md:354-357`: "**The closed loop is exercised in eval.** A poisoned-entry **scenario** proves a crafted wrong recall is *caught* by the verify pass — the poisoned answer is withdrawn and the agent **falls through to a real investigation** rather than publishing it."
- `docs/learning-loop.md:382`: "Eval: entity precision + k-of-n + **poisoned-entry** + CI … the closed loop is **exercised, not assumed**."

What actually exists:

- `eval/scenarios/` (13 files) has **no** poisoned-entry scenario. `known-pattern-recall.yaml` is the only `instant-recall` scenario and it is a *correct* recall that `precheck`-SKIPs unless `RUNLORE_RECALL_READY` is set.
- The only poisoned-recall proof is a **Go unit test**, `internal/eval/live_test.go` `TestRunOnceRecallPoisonedRejected`, driving a fake `ScoredSearcher` + a scripted reject-then-investigate model at the `runOnce` level.
- The recall-in-eval design that added that test **explicitly deferred** the live fixture: `2026-06-23-recall-in-eval-design.md` D3 ("Wiring + unit tests only; **no live-harness fixture seeding**") and §5 ("**Unit-tested, not live-fixture-tested**").

So the docs claim a *scenario exercised by the live eval*; the repo only has a unit test. **Verdict: the live scenario is genuinely absent.** `eval/scenarios/` (no such file), `internal/eval/live.go` (the harness that would run it), `docs/learning-loop.md:354` (the claim).

### Current behaviour the scenario targets (post-R2, present in this base)

`internal/investigate/loop.go:122-182`: on a catalog hit the loop builds a recalled finding, confirms it against current state, runs the adversarial verify pass (catalog content is untrusted), and **if verify withdraws every root cause, falls through to the full ReAct loop** (`loop.go:172-182`) — it does *not* deliver an empty "recall". The scenario asserts *poisoned recall → caught → re-investigated to the true cause*.

---

## 2. Design

### Part 1 — distinguish "configured" from "loaded" at the readiness gate

A `*catalog.Catalog` alone can't carry "was a catalog configured?". Thread that boolean — known from config — into `readyFunc` instead of overloading `nil`.

- Add `catalogConfigured(cfg) bool` next to `modelConfigured` (`main.go:434`): `cfg.Catalog.Dir != "" || cfg.Catalog.Git.URL != ""`.
- Change `readyFunc` to `readyFunc(leader func() bool, cat *catalog.Catalog, configured bool) func() bool` — a configured catalog that is `nil` (failed to load) **or** not yet `Ready()` keeps readiness at 503; an unconfigured catalog imposes no gate.
- Call site `main.go:325`: `readyFunc(leader.Load, cat, catalogConfigured(cfg))`.

**Fork — why thread a bool (Option B) over returning `catalog.NewEmpty()` on failure (Option A):** Option B is surgical — it fixes *exactly* the readiness gate and leaves investigation-tool composition untouched. Option A (non-nil empty catalog) would additionally wire a `kb_search` tool + instant-recall over an empty index in the one-shot eval/curate/investigate paths (`main.go` callers that discard the catalog), a broader, unintended behaviour change. The readiness bug lives at the readiness gate; fix it there.

**Fail-closed permanently on a configured static failure — intended.** A static catalog has no syncer to flip `cat` from nil to ready, so the pod stays 503. Correct: an unmountable/unparseable configured catalog is a misconfiguration that should surface loudly (never-ready → alerts/crashloop) rather than silently serve blind. A git-sync catalog still starts not-ready (`NewEmpty`) and flips ready on first successful sync — unchanged.

### Part 2 — a real poisoned-entry live-eval scenario

Add `eval/scenarios/poisoned-recall-rejected.yaml`, an **invasive** scenario the live harness runs end-to-end through the production recall path (`runEvalLive` threads `Recall`):

- **Setup** induces a *real* fault whose true cause is **distinct** from a planted poisoned catalog entry: reuse `eval/scenarios/manifests/eval-victim-app.yaml` + a bad image tag (`:v9.9.9-does-not-exist`) → `ImagePullBackOff`.
- **Poisoned catalog entry** must be seeded into the catalog the eval reads (the catalog is operator-provided; the harness seeds none). So the scenario `precheck`-gates on the seeded poisoned entry being present and recall enabled, exactly as `known-pattern-recall.yaml` env-gates on `RUNLORE_RECALL_READY`. Gate: `test -n "$RUNLORE_POISON_READY"`; absent ⇒ SKIP (not fail) — mirrors every API-key/cluster-state-dependent scenario.
- **Ground truth** names the *true* cause (bad image tag) with `expected_sources: [kubernetes, logs]`. A correct run: recall surfaces the poisoned entry → confirm/verify rejects it (it doesn't match current state) → fall through to the full loop → the agent reaches the real cause. The judge grades the **delivered** finding: a poisoned (wrong) delivery scores `root_cause` low / `confident_wrong` ⇒ fails the gate; a correct re-investigation passes. The scenario *is* the closed-loop proof.

A companion poisoned **OKF entry** ships as a documented fixture (`eval/scenarios/manifests/poisoned-recall-entry.md`) so an operator seeding the eval catalog has the exact entry to plant and a reviewer can see what "poisoned" means. It is a fixture, not auto-loaded.

**Fork — invasive + precheck-gated over a self-contained offline fixture:** the live harness seeds no catalog (recall-in-eval D3 deferred harness catalog-seeding, still out of scope). The poisoned entry is curated into the operator catalog out-of-band; the env-gate is the established pattern for recall scenarios needing pre-seeded KB state, and SKIPs cleanly in CI/local.

### Part 2 — harness-level deterministic guard

The live scenario needs a cluster + seeded catalog + API key, so it can't run in CI. To keep a deterministic regression guard at the **`RunScenario`** level (the path the live scenario flows through, above `runOnce`), add a `RunScenario` test driving a poisoned `Recall` through an invasive scenario with a reject-then-investigate model, asserting the aggregated runs never deliver the poisoned cause and do deliver the fresh finding. This complements the existing `runOnce`-level `TestRunOnceRecallPoisonedRejected` by proving the wiring carries the fall-through through setup/teardown/aggregate.

---

## 3. Components / seams

| Change | Location |
|---|---|
| `catalogConfigured(cfg)` helper | `cmd/lore/main.go` (near `modelConfigured`) |
| `readyFunc` takes `configured bool`; gate configured-but-failed/unwarm 503 | `cmd/lore/main.go` (`readyFunc`, call site `:325`) |
| `TestReadyFunc` — add configured-but-nil (failed) case + unconfigured passthrough | `cmd/lore/main_test.go` |
| Poisoned-entry live scenario (invasive, precheck-gated) | `eval/scenarios/poisoned-recall-rejected.yaml` |
| Poisoned OKF entry fixture | `eval/scenarios/manifests/poisoned-recall-entry.md` |
| Harness-level poisoned fall-through test (`RunScenario`) | `internal/eval/live_test.go` |

## 4. Testing

- `cmd/lore/main_test.go` `TestReadyFunc`: configured + nil catalog (load failed) + leader=true ⇒ **not ready**; configured + cold ⇒ not ready; configured + warm ⇒ tracks leader; unconfigured + nil + leader=true ⇒ ready (unchanged passthrough).
- `internal/eval/live_test.go`: a `RunScenario` over an invasive scenario with a poisoned `Recall` + reject-then-investigate model → never delivers the poisoned cause; a fresh finding is delivered. Judge-independent; no API key / no cluster.
- `eval/scenarios/poisoned-recall-rejected.yaml`: parses via `LoadScenarios`; SKIPs without `RUNLORE_POISON_READY`.
- Gate green: `go build/vet/test ./... && gofmt -l . && golangci-lint run ./...` (+ `--enable gosec`).

## 5. Out of scope

- Live-harness catalog fixture seeding (still deferred) — the env-gate stands in.
- A "recalled" column in the live report.
