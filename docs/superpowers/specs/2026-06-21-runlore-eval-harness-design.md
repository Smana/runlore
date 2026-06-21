# RunLore Eval Harness — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-21 |
| **Scope** | Phase 1: live-fire evaluation harness (`lore eval`) against a real cluster |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | `docs/design.md` §10 (Evaluation), the designed-but-unimplemented `lore eval` replay harness |

---

## 1. Why this exists

RunLore needs to be *deeply tested* across three axes the author cares about:

1. **Breadth** — varied incident test cases spanning the whole hypothesis space (what-changed, saturation, network, node/cloud, dependency, cert, DNS, storage).
2. **Data-source coverage** — proof the investigation actually pulls from **all** wired signals: AWS, Flux, logs, metrics (plus kubernetes, network, KB).
3. **Quality** — root-cause correctness, evidence relevance, proposed-solution quality, and write-up clarity.

Today none of this is repeatable. The `lore eval` replay harness is **designed (`docs/design.md` §10) but not implemented**. This spec defines a **reusable, committed eval harness** whose first run doubles as the current-state quality assessment, and which grows into the deterministic CI regression gate the design envisions.

**This is a testing/eval initiative, deliberately scoped narrow.** A separate, follow-on initiative (workstream B) will redesign RunLore's *learning workflow* (issue/PR lifecycle, merge policy, the compounding loop). This harness produces the graded evidence that the learning-workflow design will build on — and scenario #9 (instant-recall) is the explicit bridge to it.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **Testing campaign first**, learning-workflow design is a follow-on spec | Bottom-up: observe real graded behaviour, then formalise learning. |
| D2 | **Deliverable = a reusable eval harness** (not a one-shot report) | Re-runnable regression gate; first run *is* the assessment. |
| D3 | **Hybrid, sequenced execution** — live-fire now, record tool I/O → replay corpus / CI gate later | Live-fire is the only mode that verifies real data-source wiring (goal #2); recording makes the deterministic gate free. |
| D4 | **Harness lives in the runlore repo** (`lore eval` + `eval/` tree); scenarios are portable data | It tests RunLore; cluster-specifics stay as YAML data, not code. |
| D5 | **Trigger via `lore investigate` CLI by default**; synthetic webhook opt-in per scenario | Deterministic, no dedup/trigger-policy noise; webhook mode tests the React path where wanted. |
| D6 | **Curation sandboxed by default** (`--no-curate`); `--curate` opt-in | Don't spam real `runlore-kb` during a campaign; `--curate` can intentionally feed workstream B. |
| D7 | **Cloud strictly describe-only** — no induced AWS mutations anywhere | Safety; cloud coverage relies on existing CloudTrail history + Describe calls. |
| D8 | **Grading = two tracks**: deterministic coverage + LLM-judge RCA quality | Coverage never depends on a fuzzy judgement; directly answers goal #2. |
| D9 | **Judge model: stronger/different than the model under test, blind to the producer** | Reduces self-grading bias. |
| D10 | **N=3 runs per scenario**, report median + variance | Handles LLM non-determinism; high variance is itself a finding. |
| D11 | Added cause classes: **cert expiry, DNS, PVC/storage** | Requested; rounds out the hypothesis space. |

## 3. Architecture

The harness is a new `lore eval` subcommand backed by an `eval/` tree. Scenarios are **data** (portable YAML), so mycluster-0 specifics never leak into code.

```
runlore/
  cmd/lore/            + eval subcommand
  internal/eval/       runner, grader, recorder, report  (Go, unit-tested)
  eval/
    scenarios/         one YAML per test case
    rubric.md          grading dimensions + weights
    fixtures/          recorded tool I/O  (Phase 2 replay corpus)
    reports/           scored run outputs, dated  (.md + .json)
```

### Per-scenario flow (the runner loop)

```
load scenario
  └─ setup()        induce/verify the failure state   (skipped for "natural" scenarios)
  └─ precheck       verify the failure precondition holds; SKIP (not fail) if absent
  └─ trigger        `lore investigate "<symptom>"` (default) | synthetic webhook (opt-in)
  └─ capture        structured Investigation + full tool-call trace + audit log
  └─ grade          (a) coverage: deterministic, from the trace
                    (b) RCA quality: rubric + LLM-judge vs ground_truth
  └─ record         dump tool I/O → fixtures/   (seeds Phase 2 replay)
  └─ teardown()     ALWAYS runs (defer) — revert induced faults
repeat N=3, aggregate median + variance
report   per-scenario + summary, pass/fail vs thresholds, regression diff
```

### Scenario schema

```yaml
id: gitops-bad-image-tag
category: what-changed          # hypothesis-space class
description: Flux deploys an unpullable image tag → pods ImagePullBackOff
invasive: true                  # has setup/teardown; natural failures = false
setup:    [ ... reversible steps ... ]
trigger:  { mode: cli, symptom: "app X pods not starting in ns Y" }
ground_truth:
  root_cause: "image tag :v9.9.9 does not exist; pushed by HelmRelease bump"
  expected_sources: [gitops, kubernetes, logs]   # MANDATORY → coverage gate
  optional_sources: []                           # bonus if touched, never gates
  expected_action:  "flux rollback / correct the tag"
  must_reach_root:  true        # symptom-only answer fails
teardown: [ ... ]
```

## 4. The scenario catalog (test matrix)

The matrix is **failure cause × data source it should force**, so collectively the scenarios exercise all 12 tools and prove cross-signal correlation.

**Type legend:** ⦿ = natural (already broken in mycluster-0, real ground truth, zero setup, *graceful skip if absent*) · ⦿ read-only = no fault induced, observes real existing state (cloud history) · ⊕ = reversible induced fault (in the `runlore-eval` throwaway namespace).

| # | id | Category | Forces data source(s) | Type | Ground-truth root cause |
|---|----|----|----|----|----|
| 1 | harbor-registry-iam-quota | dependency / cloud | k8s (`pod_status`,`kube_events`) + AWS | ⦿ | Crossplane accesskey hit IAM `AccessKeysPerUser:2` → Secret `xplane-harbor-access-key` missing `username` → CreateContainerConfigError |
| 2 | harbor-valkey-dependency-down | dependency outage | k8s + logs | ⦿ | `harbor-valkey-primary:6379` connection refused (valkey down) → core/jobservice/nginx CrashLoopBackOff |
| 3 | vector-cilium-ip-exhaustion | node / network | k8s (`kube_events`) + network | ⦿ | Cilium IPAM `no IPs available on the node` → `victoria-logs-vector` stuck ContainerCreating |
| 4 | gitops-bad-image-tag | what-changed | Flux (`what_changed`,`flux_resource_status`) + k8s + logs | ⊕ | HelmRelease/image bumped to a non-existent tag → ImagePullBackOff |
| 5 | gitops-broken-kustomization | what-changed | Flux (`flux_resource_status`,`flux_tree`,`controller_logs`) | ⊕ | Bad value/path → Kustomization `Ready=False`, source healthy |
| 6 | saturation-mem-pressure | saturation | metrics (`query_metrics`) | ⊕ | Memory-hog workload → OOM/throttle, no change in Git |
| 7 | network-policy-drop | network | network (`network_drops`) + logs | ⊕ | Deny-all CiliumNetworkPolicy on throwaway app → egress dropped, looks like an app error |
| 8 | cloud-node-context-readonly | cloud / what-changed | AWS (`cloud_what_changed`,`cloud_resource_health`) | ⦿ read-only | Agent must surface a *real recent* CloudTrail mutation (node/ASG/tag event) + EC2/ASG/EKS health — no induced change; ground truth pinned to a known recent event at run time |
| 9 | known-pattern-recall | instant-recall | KB (`kb_search`) | ⊕ | Re-fire #4's symptom *after* its KB entry exists → must short-circuit via instant recall, *not* re-run the full loop |
| 10 | cert-issuance-expiry | cert expiry | k8s (`kube_events`,`controller_logs` cert-manager) + logs | ⊕ | Throwaway `Certificate` with broken `issuerRef`/ultra-short duration → `NotReady`/expired; TLS path would fail (OpenBao PKI/AppRole chain) |
| 11 | dns-resolution-failure | DNS | network (`network_drops`) + logs (+ metrics CoreDNS) | ⊕ | Throwaway app with a `toFQDNs` policy missing the DNS L7 rule → name resolution fails, masquerades as app errors (documented Cilium gotcha) |
| 12 | pvc-storage-unbound | PVC / storage | k8s (`pod_status`,`kube_events`) + AWS read-only (EBS describe) | ⊕ | Throwaway PVC with bad StorageClass / unschedulable volume → Pod `Pending`, `FailedMount`/`FailedScheduling` |

**Coverage proof** — every data-source group is forced by ≥1 scenario:

| Data source | Scenarios |
|---|---|
| Flux / GitOps | 4, 5, 8 |
| Metrics | 6, (11 CoreDNS) |
| Logs | 2, 4, 7, 10, 11 |
| AWS (read-only) | 1, 8, 12 |
| Kubernetes | 1, 2, 3, 4, 5, 10, 12 |
| Network | 3, 7, 11 |
| KB / instant-recall | 9 |

Scenarios 1–3 cost nothing (already broken). 4–7, 9–12 are reversible and blast-radius-bounded to the `runlore-eval` namespace. 8 is describe-only. This is the **seed** catalog; the schema lets new scenarios be added as data without code changes.

## 5. Grading

Two independent tracks, so quality scoring never hinges on a single fuzzy judgement.

### Track A — Coverage (deterministic)

Parse the tool-call trace + append-only audit log. Per scenario:

- each **mandatory** `expected_source` touched? (binary) → `coverage = |touched ∩ expected| / |expected|`. `optional_sources` (e.g. CoreDNS metrics in #11) count as bonus and **never gate** — this is why the `coverage == 1.0` pass gate is well-defined.
- **cross-signal** bonus: correlated ≥2 sources rather than stopping at the first failing resource.
- **tool-error flag**: any tool that errored (catches broken wiring — e.g. the Pod-Identity/Cilium credential-endpoint hang seen previously).

No LLM in this track — it directly answers goal #2.

### Track B — RCA quality (rubric + LLM-judge vs `ground_truth`)

A **stronger, different judge model, blind to which model produced the answer**, scores each dimension with a written rationale:

| Dimension | Scale | Measures |
|---|---|---|
| Root-cause correctness | 0–3 | 0 wrong · 1 symptom-only · 2 correct-but-shallow · 3 correct + reached true root (`must_reach_root`) |
| Evidence relevance/validity | 0–3 | cited facts pertinent & true — not hallucinated or correlation-only |
| Solution quality | 0–3 | `suggested_action` vs `expected_action`: correct, actionable, reversibility flagged right |
| Description quality | 0–3 | clarity, completeness, honest `unresolved` |
| Confidence calibration | 0–2 | high confidence only when correct; verify-pass correctly downgraded weak claims (**confident-wrong penalised hardest**) |

**Pass gate** per scenario: `RC-correctness ≥ 2` AND `coverage == 1.0` AND no confident-wrong.

The report shows **per-dimension** scores, not just an aggregate, so we see *where* quality breaks.

### Non-determinism

Run each scenario **N=3** times; report **median + variance**. High variance is a first-class finding (flaky investigation). Ground truth is human-authored in the scenario YAML (we induced or diagnosed it).

## 6. Recording, safety, reporting

### Recording → replay corpus (Phase 2 seed)

Every live run, the recorder captures each tool call (`name`, args) + verbatim response, plus model request/response envelopes, keyed by `scenario/run-N` under `eval/fixtures/`. Phase 2 adds `lore eval --replay`, feeding recordings through a tool-shim so investigations re-run **offline and deterministically** → the CI regression gate. The exact determinism strategy (mock tools only vs. also pin model output) is a **Phase-2 detail, intentionally not designed here**.

### Execution & safety

- Induced faults live in a dedicated **`runlore-eval` throwaway namespace**, labelled for one-shot cleanup.
- `setup`/`teardown` idempotent; **teardown always runs (`defer`)**, plus a `lore eval --cleanup` sweeper that removes the eval namespace if a run crashed mid-scenario.
- **Natural scenarios (1–3) precheck their precondition and SKIP (not fail)** when the failure isn't present — the cluster may be repaired by run time; a missing natural failure must not read as a regression.
- Cloud strictly describe-only. Curation sandboxed (`--no-curate`) by default.
- Per-run wall-clock guard on top of the agent's `maxSteps=20`. **Token cost recorded per run.**

### Reporting

`eval/reports/YYYY-MM-DD-HHMM.md` + a machine-readable JSON sibling:

- **Per scenario:** coverage result, per-dimension median scores + variance, pass/fail, judge-rationale excerpts, tool-error flags, token cost.
- **Summary:** matrix pass-rate, a **coverage heatmap** (scenario × data source), **regression diff vs the previous report**, ranked top gaps.
- The **first run is the baseline** — and *is* the current-state quality assessment.

## 7. Phasing

| Phase | Scope | Deliverable |
|---|---|---|
| **Phase 1** (this spec) | live-fire harness, scenarios 1–12, both grading tracks, recorder, baseline report | committed `lore eval` + `eval/` tree + first assessment report |
| **Phase 2** (separate spec) | `lore eval --replay` deterministic CI gate over recorded fixtures; optional promptfoo driver | regression gate wired into CI |

## 8. Out of scope

- The **learning-workflow redesign** (issue/PR lifecycle, merge policy, dedup, the compounding loop, a possible dedicated learning agent) — that is workstream B, a follow-on spec that consumes this harness's output.
- Inducing AWS mutations (cloud is describe-only).
- Multi-replica / distributed eval execution.
- The Phase-2 determinism strategy and promptfoo integration.

## 9. Success criteria

- `lore eval` runs the full 1–12 catalog against mycluster-0, N=3, and emits a dated report + JSON.
- Coverage track deterministically reports, per scenario, which of {AWS, Flux, logs, metrics, k8s, network, KB} were exercised, with a scenario×source heatmap and tool-error flags.
- RCA-quality track produces per-dimension median scores from a blind, stronger judge.
- Induced faults are fully reverted (verified clean by `--cleanup`); natural scenarios skip gracefully when their precondition is absent.
- Tool I/O is recorded under `eval/fixtures/` for every run, ready to seed Phase 2.
- The baseline report names the **top quality/coverage gaps** — the input to workstream B.
