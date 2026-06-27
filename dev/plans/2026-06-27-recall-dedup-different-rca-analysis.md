# Recall & dedup behaviour when a known symptom recurs with a *different* root cause

|              |                                                                                                      |
| ------------ | ---------------------------------------------------------------------------------------------------- |
| **Status**   | Analysis `v1` — findings only (no code change in this branch)                                         |
| **Date**     | 2026-06-27                                                                                            |
| **Scope**    | The knowledge-entry lifecycle, the recall short-circuit, and the four dedup layers — specifically how RunLore behaves when an entry already exists for a symptom but the new occurrence has a genuinely different RCA. |
| **Question** | *"If an entry exists for a given event, how does it behave if the event is similar but requires another analysis because the RCA is different?"* |
| **Method**   | Direct read of the decisive code (`recall.go`, `curator/fingerprint.go`, `curator/curator.go`, `catalog/entry.go`, `outcome/ledger.go`) cross-checked against `docs/learning-loop.md`, the `recall-trustworthiness` / `recall-disambiguation` design specs, and the curator tests. |

---

## 1. Bottom line

RunLore is **unusually thoughtful** about this exact problem — it has a 3-gate recall, a live-state confirm, an always-on adversarial verify, outcome-driven decay, and a cause-aware dedup fallback. For the **recurrence** case (the *same* cause re-firing) the design is genuinely strong.

But the precise scenario in the question — **same workload + same symptom + a different underlying cause** — is the one case the safeguards do **not** structurally separate, and it surfaces as **two distinct, compounding gaps**:

1. **Recall anchoring (read side).** Instant recall's structural gate keys on the **workload**, never on the cause. So a same-workload/same-symptom incident with a *new* cause passes all three gates and **short-circuits onto the stale prior RCA**. Correction is **outcome-lagging** (it only self-heals after the wrong answer visibly fails to resolve, repeatedly).
2. **Cause-blind dedup (write side).** On the alert/GitOps path a `TriggerKey` always exists, so the open-PR dedup fingerprint is **cause-blind** — and a test asserts this collision *on purpose*. While the prior PR is still open, the genuinely-new RCA is **coalesced away as a "seen again" comment that doesn't even contain the new cause**. After the prior entry is merged, behaviour flips (a different cause may instead spawn a second entry) — so the outcome depends on **PR-open-vs-merged race timing, not on correctness**.

Neither is catastrophic — recall confidence is capped ≤ 0.90, recall is disabled under `auto`, and recovery paths exist — but both are real and both are improvable. Details and prioritized recommendations below.

---

## 2. The knowledge-entry lifecycle (where dedup happens)

An incident passes through **four** distinct mechanisms that all get loosely called "dedup", at different stages:

| # | Layer | Where | Key | Persistence | Suppresses |
|---|-------|-------|-----|-------------|------------|
| 1 | **Intake/trigger dedup** | `internal/trigger/dedup.go:25-42` | Alertmanager fingerprint, else `source/env/ns/name/title` (`source/pipeline.go:95-100`); GitOps watcher keys `ns/name` (`source/watcher.go:42`) | in-memory, window-based (e.g. `30m`), **lost on restart** | re-fires of the *same still-firing* alert within the window |
| 2 | **Coalesce** | `internal/coalesce/coalescer.go:110-158` | `namespace + correlation-labels`, else `GroupKey`, else `ns/Title` | in-memory | folds an alert **storm** into one investigation |
| 3 | **Curation dedup** | `internal/curator/{fingerprint,curator}.go` | two sub-keys (see §4) | open-PR list + bleve index | a re-investigation drafting a duplicate PR |
| 4 | **Phase-2 groomer** | `internal/curate/dedup.go:78-85` | equal fingerprint, else Jaccard ≥ 0.6 over titles | scheduled CronJob | near-identical *open* PRs across history |

Between intake and curation sits the **recall short-circuit** (`internal/investigate/recall.go`, wired in `loop.go:139-188`) — the read side of the catalog.

---

## 3. Recall — the read side (anchoring gap)

### 3.1 What fires recall

`loop.go:139-142` runs recall **before** the ReAct loop and lets it replace the whole investigation (disabled under `auto`):

```go
if li.Recall != nil && (li.Actions == nil || !li.Actions.IsAuto()) {
    if entry, conf := li.Recall.lookup(ctx, req); entry != nil { /* short-circuit */ }
```

The query is the **symptom text** (`recall.go:66`): `query := req.Title + " " + req.Message`. The workload is used only as a *structural filter*, not part of the query.

### 3.2 The three gates (`recall.go:61-146`)

1. **Structural pre-filter** — keep only candidates whose stored `Resource` agrees with the alert workload (`resourceAgrees`, `recall.go:165-184`). After the `recall-disambiguation` slice this correctly separates *different workloads in one namespace*.
2. **Margin** — the top agreeing hit must clear `MinScore` and beat the runner-up by `MarginGap` (or a lone hit clears `SoloFloor`).
3. **Outcome decay** — `outcomeFactor = (resolved+k)/(recalls+k)`; reject when below `OutcomeFloor` (`recall.go:127-140`). **Only applies when an `Outcome` ledger is wired** — `Outcome == nil` ⇒ no decay at all.

Confidence is **derived and capped at 0.90** (`deriveRecallConfidence`, `recall.go:207-216`), then multiplied by the outcome factor. A recall is then **confirmed against live state** (`confirm.go` — `pod_status` + `kube_events`, capping at 0.70 if it can't gather state) and run through the **adversarial verify** pass (`verify.go`).

### 3.3 Why the question's scenario slips through

`resourceAgrees` answers **"is this the same workload?"** — it has **no notion of cause**. For `argocd/airflow:Degraded` recurring on the *same* `airflow` workload:

- Gate 1 → `matchExact` ✓ (same workload)
- Gate 2 → top hit is a clear lexical winner (same symptom text) ✓
- Gate 3 → if the prior entry has historically resolved, its factor is high ✓

→ recall **fires and adopts the prior cause wholesale** (`recalledInvestigation`, `recall.go:221-239`). The only fresh evidence gathered is `confirm.go`'s `pod_status`+`kube_events`; if the new cause is surface-similar, `verify` (which judges the *adopted* hypothesis, and was never asked to look for an *alternative* cause) can **keep** it. The agent does **not** run `what_changed`, logs, metrics, or network on this path.

The honesty hedges are real but soft: confidence ≤ 0.90 and an `Unresolved` note *"recalled from the catalog without a fresh investigation — confirm it still applies"* (`recall.go:231`). Correction is **outcome-lagging**: the wrong answer must visibly fail to resolve enough times for `outcomeFactor` to fall below the floor (with a `Beta(1,1)`-ish prior, `k≈2`, that takes several non-resolving recalls).

> The `kb_search` tool path makes anchoring *explicit*: the system prompt says *"use its diagnosis and resolution as your primary hypothesis and verify it — don't invent a different cause and ignore the runbook"* (`loop.go:66-67`). There is **no** counterpart instruction *"if live evidence contradicts the runbook, abandon it and pursue the new cause."*

---

## 4. Curation dedup — the write side (cause-blind on the trigger path)

### 4.1 Two sub-keys

| Function | Used against | Includes the cause? | Definition |
|----------|--------------|---------------------|------------|
| `DupFingerprint` (exact sha256) | **open PRs** (`duplicateOpenPR`, `curator.go:103-118`) | **No, when a TriggerKey exists** | `fingerprint.go:84-110` |
| `Fingerprint` (fuzzy BM25 query) | the **merged catalog** (`Novelty`, `curator.go:54-65`) | **Yes** (`fingerprint.go:23-34`) | includes `RootCauses[0].Summary` |

`DupFingerprint` is **TriggerKey-first** (`fingerprint.go:86-91`):

```go
if tk := strings.ToLower(strings.TrimSpace(inv.TriggerKey)); tk != "" {
    sum := sha256.Sum256([]byte(ref + "|trigger:" + tk))   // RCA prose is NOT an input
    return hex.EncodeToString(sum[:])
}
// only when there is NO trigger key (e.g. human `lore investigate "<symptom>"`):
//   sha256(ref + "|" + significant-tokens-of-RootCauses[0].Summary)
```

On the **alert path** the TriggerKey is the Alertmanager fingerprint; on the **GitOps path** it is `resourceRef + ":" + conditionReason` (e.g. `argocd/airflow:Degraded`) — both deterministic, both **independent of the cause**. So **two genuinely different causes on the same failing resource produce the same fingerprint**. This is asserted by a test that uses *materially different* causes on purpose (`fingerprint_test.go:186-203`):

```go
const tk = "argocd/airflow:Degraded"
a := RootCauses{{Summary: "ArgoCD git repository authentication failure"}}
b := RootCauses{{Summary: "Missing ExternalSecret for database credentials: Secret does not exist"}}
// asserts fa == fb — "same trigger key must hash alike across reworded causes"
```

A git-auth failure and a missing-secret failure are not "rewordings" — yet the design deliberately collapses them. (This is the intended `#137` behaviour for *re-investigations of one ongoing incident*; the side effect is that it can't tell that apart from *a new cause on the same trigger*.)

### 4.2 The curator decision tree (`curator.go:35-88`)

```
0. inv.Recalled            → return (never re-curate a recall)
1. !meetsBar               → return (chat-only, NO repo artifact)
2. catalog BM25 ≥ DupScore → return ("duplicates a catalog entry; not filing")  ← DROP
2b. duplicateOpenPR match  → Comment(coalesceComment); return                    ← COALESCE
3. else                    → OpenPR(draftKBEntry)                                ← NEW PR
```

There is **no UPDATE/EDIT path** — `OpenPR` always creates a *new branch + new file* (`github.go:110-151`); the only forge mutations anywhere are `OpenPR/OpenIssue/Comment/ReplaceLabel/Close`. So an entry's RCA is never rewritten in place; the three terminal outcomes are **new PR**, **coalesce-comment**, or **drop**.

### 4.3 What happens to the new "Y" cause — split on open-vs-merged

**Case A — the prior "X" PR is still OPEN.** `duplicateOpenPR` matches the cause-blind marker → the curator posts:

```go
// curator.go:140-142
"RunLore saw this incident again (confidence %.0f%%). Coalesced rather than re-filed."
```

➡️ The new, different cause is **silently dropped** — not appended, not opened separately, and **the comment does not contain the new RCA text**, so a reviewer of the canonical PR never learns the recurrence had a different cause. The Phase-2 groomer would `Close` a second such PR as an equal-fingerprint duplicate (`curate/dedup.go:81-83`) if one ever got filed.

**Case B — the prior "X" entry is already MERGED.** The open-PR layer **cannot fire** — because the merged file's `fingerprint:` frontmatter is **dead data**: the catalog loader only parses `type/title/description/resource/tags` (`load.go:60-66`) and never reads the fingerprint back. So only the cause-*aware* BM25 `Novelty` applies:

- score ≥ `DupScore` (default **5.0**) → dropped as a catalog duplicate, chat-only (`curator.go:61-64`).
- score < `DupScore` → treated as novel → **files a separate second entry** for the same symptom with the new cause (`curator.go:80`).

➡️ **Net asymmetry:** before merge, a differing RCA is *coalesced away and lost*; after merge, the *same* differing RCA may instead *spawn a second entry*. Which one happens depends on **PR-open-vs-merged timing, not on whether the cause is genuinely new.**

---

## 5. The entry model can't represent "same symptom, multiple causes"

`internal/catalog/entry.go:5-14` — one entry is one flat markdown card holding **one** root-cause narrative as prose in `Body`:

```go
type Entry struct { Type, Title, Description, Resource string; Tags []string; Body, Path string }
```

The read model carries **no fingerprint, confidence, occurrence count, outcome weight, timestamp, or version**. Multiple causes for one symptom can only exist as free text a human writes into `Body`. Combined with the no-edit curator (§4.2), this means RunLore has **no first-class way to record "symptom S has been caused by X and, separately, by Y"** — the data model itself forecloses it.

Outcome weighting (`outcome/ledger.go`) feeds **recall confidence decay**, **merge-readiness**, and **recurrence issues** — but **no path edits a merged entry's RCA**. A wrong-but-self-resolving entry keeps a high `outcomeFactor` and stays trusted indefinitely.

---

## 6. Documentation ↔ code drifts found along the way

Two places where the docs describe behaviour the code does **not** implement — both worth fixing in their own right because they will mislead a maintainer reasoning about exactly this question:

1. **`docs/learning-loop.md` §5** states the dedup fingerprint is `sha256(resource-ref + "|" + normalized cause token-set)` and that *"two different terse causes on one resource can't collide."* That describes only the **fallback** branch. On the dominant alert/GitOps path the fingerprint is **cause-blind** (`fingerprint.go:86-91`). The doc should state the TriggerKey-first behaviour and its consequence explicitly.
2. **`docs/design.md:121,224-229`** says *"novel + uncertain → open a GitHub ISSUE."* The implementation does the opposite — `curator.go:1-6,47-51`: uncertain ⇒ **chat-only, NO artifact**, and *"It never opens issues."* (The only issues are ledger-driven knowledge-gap issues, not per-finding confidence routing.)

---

## 7. Recovery paths that DO exist (and why they're not enough)

| Mechanism | What it does | Why it's not preventive |
|-----------|--------------|--------------------------|
| **Outcome decay** (`recall.go:127-140`) | a recalled entry that never resolves decays below the floor → forces a fresh investigation → can be overturned | **lagging** — needs repeated non-resolution; and **off** when no ledger is wired |
| **`reinvestigate` label** (`reinvestigate.go:60`) | a human-labelled KB issue triggers a fresh run; its `Request` carries **no workload**, so recall can't fire (`resourceAgrees` rejects empty namespace) — guaranteed fresh investigation | **manual** — a human must notice and label |
| **Adversarial verify** (`verify.go`) | can `reject` a recalled cause that contradicts the confirm-step evidence → fall through to full investigation | only validates the *adopted* cause; never searches for the alternative |
| **Human PR review** | a reviewer could spot a "seen again" coalesce comment | the comment omits the new cause, so there's nothing to spot |

---

## 8. Risk assessment

- **Likelihood:** moderate and real. `resourceRef:reason` is a *coarse* identity — one workload's `Degraded`/`CrashLoopBackOff` legitimately has many causes over its lifetime (bad image → OOM → missing secret → migration failure). Each transition is exactly this scenario.
- **Blast radius:** bounded. Recall confidence is capped ≤ 0.90, carries a "confirm it still applies" hedge, is delivered to a **human** (not auto-executed — recall is disabled under `auto`), and the wrong RCA is a *suggestion*. The KB harm (a buried/lost new cause) is a **knowledge-quality** regression, not an outage.
- **Worst case:** a recurring symptom whose cause has changed is repeatedly answered with the stale RCA (until outcome-decay kicks in), and the correct new RCA is never captured because it keeps coalescing onto the still-open prior PR. The catalog silently encodes "symptom S ⇒ cause X" as settled when it isn't.

---

## 9. Recommendations (future slices — not implemented here)

Ordered by leverage. Each is independently shippable in the project's slice style.

1. **Make the coalesce comment carry the new RCA + flag divergence.** Smallest, highest-value change: when `duplicateOpenPR` coalesces, include the new finding's `RootCauses[0].Summary` in the comment and label it when the cause token-set diverges from the open PR's. Turns a silent drop into a visible "same trigger, *different* cause observed" signal a reviewer can act on. No new identity needed.
2. **Cause-divergence gate on recall (read side).** Before short-circuiting, compare the recalled entry's stored cause/evidence against the `confirm.go` live signals; if they diverge beyond a threshold, **fall through to a full investigation** instead of adopting the prior RCA. This directly closes the first-occurrence anchoring gap rather than waiting for outcome decay.
3. **Add an "evidence contradicts the runbook → pursue the new cause" instruction** to the `kb_search` guidance in `loop.go:66-67`, balancing the current pure-anchoring wording.
4. **Decide the dedup contract for "same trigger, new cause" explicitly.** Either (a) blend a coarse cause signal into `DupFingerprint` even on the trigger path (so a clearly-different cause forks a new PR), or (b) keep trigger-keying but make §9.1 the safety net. Document the choice; today the open-vs-merged asymmetry is *emergent*, not designed.
5. **Persist + read back the entry fingerprint.** Have the catalog loader parse the `fingerprint:` frontmatter (`load.go:60-66`) so post-merge dedup isn't BM25-text-only — removes the open-vs-merged behavioural split.
6. **Fix the two doc/code drifts** (§6) regardless of the above.

---

## 10. References

- **Recall:** `internal/investigate/recall.go` (gates `:61-146`; `resourceAgrees` `:165-184`; `recalledInvestigation` `:221-239`); `confirm.go`; `verify.go`; wired at `loop.go:139-188`; anchoring prompt `loop.go:66-67`.
- **Curation dedup:** `internal/curator/fingerprint.go:84-110` (cause-blind `DupFingerprint`) + `:23-34` (cause-aware `Fingerprint`); `internal/curator/curator.go:35-88,140-142`; test `internal/curator/fingerprint_test.go:186-203`; Phase-2 `internal/curate/dedup.go:78-85`.
- **Entry model:** `internal/catalog/entry.go:5-14`; loader `internal/catalog/load.go:60-66`.
- **Outcome ledger:** `internal/outcome/ledger.go` (decay `recall.go:127-140,200-202`).
- **Re-investigation:** `internal/investigate/reinvestigate.go:60`.
- **Intake/coalesce:** `internal/trigger/dedup.go:25-42`; `internal/coalesce/coalescer.go:110-158`.
- **Design intent:** `docs/learning-loop.md` (esp. §3, §5, §6); `dev/superpowers/specs/2026-06-22-recall-trustworthiness-design.md`; `dev/superpowers/specs/2026-06-23-recall-disambiguation-design.md`.
- **Drifts:** `docs/learning-loop.md` §5 vs `fingerprint.go:84-110`; `docs/design.md:121,224-229` vs `curator.go:1-6,47-51`.
