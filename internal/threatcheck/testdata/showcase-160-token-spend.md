---
type: Incident
title: AgentFleetTokenSpendHigh — runaway agent run xplane-run-mb6bstvz burning 6.36M input tokens in retry loop
description: 'AgentRun xplane-run-mb6bstvz is in a runaway token-burn loop, consuming 6.36M of the fleet''s 11.4M tokens/hour (55%). The burn is overwhelmingly INPUT tokens (~1.2M input vs ~10K output per 5-min window from 17:16–17:42), indicating the agent repeatedly re-sends large context without producing meaningful output — a classic retry/loop pattern. No enforced per-run token cap exists (per the alert annotation: ''until SP4 PR 2''), so the run burned unchecked until its rate naturally dropped to 0 at ~17:43. The prescribed remediation is to manually revoke this run: kubectl annotate agentrun -n agents xplane-run-mb6bstvz agents.ogenki.io/revoked=budget-run'
resource: agents/xplane-run-mb6bstvz
tags:
    - runlore
    - incident
    - agentrun
    - agents
timestamp: "2026-10-28T17:48:27Z"
fingerprint: 9d0dee6cdc0c9e15f346187a430738c762e335cd1d762424df83f0e2c7095642
confidence: 0.8
---

## Decision

- **why keep:** AgentRun xplane-run-mb6bstvz is in a runaway token-burn loop, consuming 6.36M of the fleet's 11.4M tokens/hour (55%). The burn is overwhelmingly INPUT tokens (~1.2M input vs ~10K output per 5-min window from 17:16–17:42), indicating the agent repeatedly re-sends large context without producing meaningful output — a classic retry/loop pattern. No enforced per-run token cap exists (per the alert annotation: 'until SP4 PR 2'), so the run burned unchecked until its rate naturally dropped to 0 at ~17:43. The prescribed remediation is to manually revoke this run: kubectl annotate agentrun -n agents xplane-run-mb6bstvz agents.ogenki.io/revoked=budget-run
- **confidence:** 80%

## Symptom

AgentFleetTokenSpendHigh — runaway agent run xplane-run-mb6bstvz burning 6.36M input tokens in retry loop

Affected resource: AgentRun agents/xplane-run-mb6bstvz

## Investigate

- alert_rule: expr=sum(increase(gen_ai_client_token_usage_sum{ar_agent=~"system:serviceaccount:agents:.*", gen_ai_token_type=~"input|output"}[1h])) > 1e7 — fires when fleet exceeds 10M tokens/hr; currently 11.45M
- query_metrics topk: {ar_agent="system:serviceaccount:agents:xplane-run-mb6bstvz"} = 6.357342e+06 — more than 4x the next noisiest run (5ltxfgqn at 1.43M)
- query_metrics_range by token_type for mb6bstvz: input tokens peaked at 1.378M/5min while output peaked at only 11K/5min — extreme input/output imbalance confirming a retry/loop, not productive work
- query_metrics_range rate for mb6bstvz: rate climbed from 2410/min to ~4900/min (17:19–17:39), then dropped to 0 at 17:43 — the run appears to have stopped or completed naturally
- pod_status: xplane-run-mb6bstvz Running ready=4/4 age=30m on gke-gcp-0-agents-gvisor — pod still live and could resume burning
- alert annotation: 'No enforced per-run cap exists until SP4 PR 2. Revoke the noisiest run by hand: kubectl annotate agentrun -n agents <name> agents.ogenki.io/revoked=budget-run'

## Cause

1. **AgentRun xplane-run-mb6bstvz is in a runaway token-burn loop, consuming 6.36M of the fleet's 11.4M tokens/hour (55%). The burn is overwhelmingly INPUT tokens (~1.2M input vs ~10K output per 5-min window from 17:16–17:42), indicating the agent repeatedly re-sends large context without producing meaningful output — a classic retry/loop pattern. No enforced per-run token cap exists (per the alert annotation: 'until SP4 PR 2'), so the run burned unchecked until its rate naturally dropped to 0 at ~17:43. The prescribed remediation is to manually revoke this run: kubectl annotate agentrun -n agents xplane-run-mb6bstvz agents.ogenki.io/revoked=budget-run** (80%)

## Resolution

- Revoke the noisiest run to prevent resumption: kubectl annotate agentrun -n agents xplane-run-mb6bstvz agents.ogenki.io/revoked=budget-run. The run's token rate dropped to 0 at 17:43 but the pod is still Running (ready=4/4) and could resume. The alert will continue firing until the 1h rolling window drops below 10M tokens (~1 hour from peak at 17:42). (reversible=true)

## Unresolved

- Whether the CompositionRevision bb692a4 'pipeline status unknown' warning is a real defect or transient — needs human inspection of the composition revision YAML which RBAC blocked
- Whether mb6bstvz's token burn stopping at 17:43 means the run completed naturally or was externally stopped — the pod is still Running ready=4/4, so it could resume
- Whether the v0.29.7 agent-harness image (used by mb6bstvz) has a known bug causing retry loops that was fixed in v0.29.9 (used by newer runs) — could not access release notes or the image changelog

