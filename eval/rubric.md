# RunLore eval rubric

Two grading tracks (see `docs/superpowers/specs/2026-06-21-runlore-eval-harness-design.md` §5).

## Track A — coverage (deterministic)

From the recorded tool-call trace. `coverage = |mandatory expected_sources touched| / |expected_sources|`.
Pass requires `coverage == 1.0`. `optional_sources` are bonus, never gating. Any errored tool is flagged.

Data-source groups: `gitops` (what_changed, flux_resource_status, flux_tree), `kubernetes` (pod_status,
kube_events, controller_logs), `metrics` (query_metrics), `logs` (query_logs), `network` (network_drops),
`aws` (cloud_what_changed, cloud_resource_health), `kb` (kb_search).

## Track B — RCA quality (LLM-judge, blind, stronger model)

| Dimension | Max | Meaning |
|---|---|---|
| root_cause | 3 | 0 wrong / 1 symptom-only / 2 correct-shallow / 3 correct + true root |
| evidence | 3 | cited facts pertinent & true |
| solution | 3 | suggested action vs expected: correct, actionable, reversibility right |
| description | 3 | clarity, completeness, honest unresolved |
| calibration | 2 | high confidence only when correct; confident-wrong penalised hardest |

## Pass gate (per scenario, median over N=3)

`root_cause >= 2` AND `coverage == 1.0` AND no confident-wrong run.

> **Authoring note.** Natural-scenario `precheck` label-selectors are best-effort. A wrong selector
> matches nothing → the precheck exits non-zero → the scenario SKIPs (never a false pass), so confirm
> them against the live cluster (`kubectl get pods -n <ns> --show-labels`) before relying on a SKIP vs
> a real run. Namespaces are confirmed: Harbor → `tooling`, vector → `observability`.
