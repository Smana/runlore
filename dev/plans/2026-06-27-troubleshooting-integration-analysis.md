# Troubleshooting process — per-integration methods, signals, and gaps

|              |                                                                                                            |
| ------------ | ---------------------------------------------------------------------------------------------------------- |
| **Status**   | Analysis `v1` — findings only (no code change in this branch)                                               |
| **Date**     | 2026-06-27                                                                                                  |
| **Scope**    | How RunLore gathers diagnostic information during an investigation, integration by integration — what it pulls, *how*, and where it falls short of established SRE troubleshooting practice. Companion to the recall/dedup analysis (`2026-06-27-recall-dedup-different-rca-analysis.md`). |
| **Method**   | Read of the investigation engine + every tool/provider (`internal/investigate/*`, `internal/providers/*`, `internal/{metrics,logs,network,whatchanged}/*`), cross-checked against the system prompt, the integration matrix in `README.md`, and external best-practice sources (partial deep-research run — see §9). |
| **Frame**    | Best practice is judged against the standard SRE toolkit: **"what changed" first** (the #1 RCA signal), the **four golden signals** / **RED** (Rate-Errors-Duration) / **USE** (Utilization-Saturation-Errors), **cross-signal correlation around the incident timestamp**, and **runbook-guided** investigation. |

---

## 1. Bottom line

RunLore's investigation **engine** is genuinely strong: a disciplined ReAct loop, an always-on adversarial verify pass, a what-changed-first / runbook-early system prompt with an explicit "correlation ≠ causation" rigor block, honest `unresolved`, and a clean read-only posture. The **methodology** is close to best practice.

The weaknesses are almost entirely in **signal breadth and time-awareness inside individual tools**, not in the loop. Three themes recur across integrations:

1. **No time-anchoring / no trends.** Metrics are **instant-at-now only** (the range API exists but is unwired); the what-changed `TimeWindow` is accepted but ignored; nothing overlays a change timestamp on a metric/log spike. An agent that can't look at the incident *window* is structurally weak at the one thing RCA needs most — correlating a symptom's onset to a change.
2. **Shallow per-signal depth.** K8s has no previous-container logs and no general workload-log reader; logs surface only 50 lines with structured fields dropped; Hubble never queries L7/DNS (the most common connectivity signal); cloud has no AWS Health/PHD. Each tool sees the *first layer* of its signal, not the diagnostic layer underneath.
3. **The render cap (50 rows) is the real ceiling**, often tighter than the provider cap and sometimes hiding the providers' own "narrow your query" hints.

None of this is broken — it's a young, well-architected agent whose tools are deliberately minimal. The recommendations in §8 are mostly *additive signals* behind the existing interfaces.

> Two items the earlier review (`dev/plans/2026-06-25-review-roadmap.md`) flagged are **already resolved** in current code — see §7. They should not be carried forward as open gaps.

---

## 2. The investigation engine (the meta-layer)

**A classic ReAct tool-calling loop** (`internal/investigate/loop.go:120-351`): the model is given the system prompt + tool specs, calls tools, results are redacted + truncated + fed back, and it finishes by calling the reserved `submit_findings` tool. Backstops: **MaxSteps = 20** (`loop.go:201-206`), **token budget** (shipped 100k, `values.yaml:61`) with a one-shot nudge then hard-kill, **10-minute** per-investigation timeout, **32 KiB** tool-output cap (shipped).

**Methodology is encoded in the prompt** (`loop.go:19-71`), and it is good:
- *"investigate … (start with `what_changed`)"*; *"Search the knowledge base EARLY with `kb_search`"*; *"call `pod_status` … FIRST … then `kube_events`"*; drill GitOps `gitops_resource_status` → `gitops_tree` → `controller_logs`.
- *"reason about both change-caused and no-change causes."*
- **Anti-anchoring on tool failure:** *"A tool ERROR … means MISSING DATA — it is NEVER evidence of a problem."*
- **Rigor block:** *"Correlation is NOT causation … you MUST read its actual diff …"*; explicit confidence calibration (> 0.7 verified, < 0.4 guess); *"don't invent a different cause and ignore the runbook."*
- **Prompt-injection defence:** incident text / tool output / catalog content are marked UNTRUSTED.

**Verification is always on** (`verify.go`, `Verify: true` hard-coded): a second adversarial LLM call judges each root cause **only on its evidence** and can `keep / downgrade / reject` — it can **only lower** confidence; a fully-rejected finding loses its actions and is recorded as `unresolved`.

**Tool surface (12 read-only tools + `submit_findings`)**, conditionally registered by configured backend (`internal/app/investigate.go:32-141`): `what_changed`, `gitops_resource_status`, `gitops_tree`, `kb_search`, `query_metrics`, `query_logs`, `network_drops`, `controller_logs`, `pod_status`, `kube_events`, `cloud_what_changed`, `cloud_resource_health`.

**MCP is server-only.** `internal/mcp` exposes RunLore's `gitops_what_changed` to *external* agents (HolmesGPT/kagent/Claude Desktop); RunLore is **not** an MCP client and **cannot consume external MCP toolsets** — the 12 tools are all native Go. This caps how fast its signal breadth can grow relative to HolmesGPT's 60+ pluggable toolsets.

---

## 3. Per-integration analysis

### 3.1 Kubernetes — `internal/providers/cluster/cluster.go`, `kube_tools.go`, `controllerlogs_tool.go`

**Gathers (3 signals):** `pod_status` (per-container waiting/terminated reasons; a *good* OOM→memory-limit correlation at `cluster.go:123-127`, and CrashLoop cause pulled from `LastTerminationState`); `kube_events` (Warning-only by default, most-recent-first, count-aware); `controller_logs` (Flux controllers only). All read-only, redacted, single-cluster (in-cluster config then ambient kubeconfig, `app/kube.go:13-34`).

**Best practice:** for a workload that won't run, an SRE pulls `describe pod` (events + conditions + restart counts), **`logs --previous`** on CrashLoop, owner/rollout status, node conditions, and scheduling reasons — then OOM via the `OOMKilled`/exit-137 marker. Note (deep-research, GKE docs): under **cgroup v1 the OOM killer may kill a child process, not PID 1, so the container is *never* marked `OOMKilled`** — relying solely on the reason/exit-137 signal misses these; cgroup v2 is all-or-none.

**Gaps:**
- **No previous-container logs** — `PodLogOptions` never sets `Previous` (`cluster.go:54`); and `controller_logs` is enum-locked to the four Flux controllers, so **there is no tool to read an arbitrary crashing workload's logs over the K8s API at all**. Without VictoriaLogs configured, the agent is blind to application stdout/stderr. *(Highest-value K8s gap.)*
- **OOM detection inherits the cgroup-v1 blind spot** above — `cluster.go:113` keys on `Reason=="OOMKilled"`.
- **No restart counts** (`RestartCount` never read) — the canonical "how bad is this CrashLoop" number.
- **No owner references / rollout status** — no Deployment/ReplicaSet/StatefulSet desired-vs-ready, no `Progressing`/`ReplicaFailure`. Can't map a pod to its controller or see a stuck rollout.
- **No pod conditions / scheduling detail / readiness gates / `nodeName`**; pending-pod reasons appear only if a `FailedScheduling` event still exists.
- **Evicted pods** can render with no reason (`p.Status.Reason`/`Message` never read).
- **No node conditions/taints/pressure, no PV/PVC, no HPA/PDB/ResourceQuota, no Service/Endpoints.**
- **Requests/limits**: only *memory* limits, and only to annotate an OOM — no CPU, no requests, no throttling.
- **Structural quirk:** `Events` uses `Limit: 200` applied **before** the Warning filter and time-sort (`cluster.go:140,159,173`) — in a noisy namespace the genuinely-recent Warnings can be crowded out by Normal events and never seen.

### 3.2 GitOps + what-changed — `whatchanged_tool.go`, `gitops_*_tool.go`, `internal/whatchanged/*`, `internal/providers/gitops/*`

**Gathers:** a **go-git two-endpoint tree diff**, path-scoped to the failing workload's `spec.path` (segment-boundary match), delivered to the LLM as a **raw unified diff**. Flux uses HEAD-gap revision logic (`revisionRange`, `flux.go:125-146`) — it diffs `lastApplied..sourceHEAD` so it catches the *breaking* commit, not just the last healthy one. Argo uses `history[len-2]..current`. `gitops_resource_status` reads Flux Ready conditions / Argo health+sync+operationState + Events; `gitops_tree` walks the dependency/managed-resource tree to the root not-Ready/NOT-FOUND node.

**Best practice:** "what changed" is the **#1 RCA signal** — correlate the *most recent* deploy/config change to the incident onset, classify it (image bump, chart bump, drift), and confirm it plausibly affects *this* workload. The canonical method (deep-research, Grafana Knowledge Graph) is to correlate a **change** insight with an **error** insight **on the same service in a time window**.

**Gaps:**
- **HelmReleases are excluded from the change spine** — Flux `Changes()` iterates only Kustomizations (`flux.go:77`); no helm-controller `status.history`/`lastAttemptedRevision`/chart-version delta. **A bad Helm upgrade is invisible to `what_changed`** unless it lands as a Kustomization-path YAML change. *(Highest-value GitOps gap.)*
- **No change classification** — `ChangeChartBump`/`ChangeImageBump`/`ChangeDrift` are defined but **never emitted** (both providers emit only `ChangeSync`). Image-tag changes appear only incidentally inside the raw YAML diff.
- **`TimeWindow` accepted but unused** (`flux.go:74-75`, `argocd.go:70-71`) — **no time-correlation**: changes aren't filtered/ranked by when they happened relative to the incident, and **no commit timestamp/author/message** is surfaced (`differ.go:117`). The strongest human-readable "what/why/when" signal is dropped.
- **Argo multi-source** — only `source`/`sources[0]` is diffed; a change in a separate values/chart repo (`sources[1]`) is never seen.
- **No drift** (live vs desired) — only Git↔Git; Argo `OutOfSync` is deliberately not treated as a failure.
- **Kustomize overlays / Flux `postBuild.substitute(From)`** outside `spec.path` are invisible (the rendered build is never diffed).
- **Truncation risk** — the differ has *no* internal size cap; the only bound is the **32 KiB egress truncation**, which can clip a large monorepo diff's decisive hunk (it keeps head+tail, elides the middle).

### 3.3 Metrics (Prometheus / VictoriaMetrics) — `query_tools.go`, `internal/metrics/prometheus/*`

**Gathers:** free-form PromQL via `query_metrics`, **instant query evaluated at "now"** (`query_tools.go:56`; `at = time.Time{}`). Same Prometheus HTTP API for both backends.

**Best practice:** range-query the **golden signals / RED / USE** across the **incident window**, anchor evaluation at the alert timestamp, use `rate()`/`histogram_quantile()` over an appropriate window, re-run the firing alert's own expression, and overlay the deploy timestamp on the spike.

**Gaps:**
- **Instant-only, pinned to now** — `QueryRange` is fully implemented (`prometheus.go:68-99`) but **wired to no tool**. The agent gets a single scalar, never a trend; **for a resolved/past incident "now" may already look healthy → a false all-clear.**
- **No incident-time anchoring** — can't evaluate at the alert timestamp; to look back the model must bake `[5m]`/`offset` into the PromQL string.
- **No golden-signal/RED/USE templates** — pure free-hand PromQL; the model must recall metric names and rate windows from memory.
- **No metric/label discovery** — no `/labels`, `/series`, `/metadata` → the model guesses metric names and silently gets `"no series matched"` on a wrong guess; can't explore cardinality.
- **No exemplars** (metric→trace pivot), **no alert→metric correlation** (the firing query isn't auto-run), **no deploy↔spike overlay.**
- **Render is first-50, not top-N** by magnitude (`query_tools.go:64`).

### 3.4 Logs (VictoriaLogs / LogsQL) — `query_tools.go`, `internal/logs/victorialogs/*`

**Gathers:** `query_logs` builds LogsQL in Go from structured params (container/namespace/level) — a *good* guardrail that even rejects the common `level=` (Loki/Prometheus) mistake (`buildLogsQL`, `query_tools.go:142-164`). Window `[now-60m, now]`; fetches up to 1000 lines, **renders only 50 to the model**.

**Best practice (deep-research, VictoriaLogs/Loki docs):** always **time-bound** (`_time`) — an unbounded query scans everything; **scope by stream labels first** (inspects only stream labels, big IO/CPU win); put **line filters early**; detect **volume spikes** with `_time:1d error | stats by (_time:1h) count()`; prefer exact matchers over regex; isolate JSON-parse failures with `| json | __error__!=""`.

RunLore **does** time-bound and stream-scope (good), but:

**Gaps:**
- **Only 50 lines reach the model** (`maxToolRows`), and the provider's own "truncated at 1000 — narrow the query" sentinel sits at index ~1000 and is **never shown** (shadowed by the 50-row cap). 50 lines is small for real triage.
- **Structured `Fields` are dropped at render** — the LLM sees only `time + _msg` (`query_tools.go:221`), so it can't attribute a line to a pod/node unless that's already in the message.
- **No volume-spike / error-rate-over-time helper** — the high-signal `| stats by (_time:1h) count()` pattern is available in LogsQL but never offered/guided.
- **Ordering not enforced newest-first** (no `| sort`; relies on backend default).
- **Loki is unsupported** — VictoriaLogs-only; Loki appears only as the syntax the `level=` guard warns *against*. A Loki shop gets no logs tool.

### 3.5 Network flows (Cilium Hubble · AWS VPC Flow Logs · GCP Firewall Logs) — `query_tools.go`, `internal/network/*`

**Gathers:** `network_drops` over the configured provider — **dropped/denied flows only** (Hubble `DROPPED`, VPC `REJECT`, GCP `DENIED`), `[now-60m, now]`, capped 100 then rendered ≤ 50.

**Best practice:** for "service can't reach X," the highest-signal checks are **DNS** (NXDOMAIN/refused/latency) and **L7** (HTTP 5xx/4xx, gRPC status), then policy-drop attribution to the *specific* NetworkPolicy, then conntrack/new-vs-established.

**Gaps:**
- **Hubble L7 and DNS are never queried** — only `Verdict_DROPPED` (`hubble.go:79-92`). The single most common connectivity signal (DNS failures, HTTP 5xx) is unavailable. *(Highest-value network gap.)*
- **No L4 in the output** — `flowToLine` drops port/protocol, so the model can't tell which port was denied.
- **No policy attribution** — `drop_reason` gives `POLICY_DENIED` but not *which* `CiliumNetworkPolicy`/`NetworkPolicy`.
- **Cloud flow logs ignore the selector** — AWS-VPC and GCP `Drops()` discard the namespace/pod selector and return VPC-wide denials with **numeric** protocol; no security-group/NACL attribution, no `tcp-flags`/`flow-direction`/custom fields.
- **GCP's own truncation sentinel is dead** under the 50-row render cap (same shadowing as logs).
- **Hubble transport is plaintext** (`insecure.NewCredentials()`, `hubble.go:39`) — a posture note, not a troubleshooting gap.

### 3.6 Cloud (AWS only) — `cloud_tools.go`, `internal/providers/cloud/aws/*`

**Gathers:** `cloud_what_changed` (CloudTrail **mutating** events, default 90 min, sorted newest-first, capped 25) — good for "manual/infra change invisible to GitOps"; `cloud_resource_health` (EKS nodegroup status + health issues, ASG scaling activities, EC2 instance status checks).

**Best practice:** correlate **AWS Health / Personal Health Dashboard** (account-specific events, maintenance, degradations), CloudTrail **write events including `errorCode`** (AccessDenied/throttle spikes are strong correlators), and infra-level capacity signals (ENI/IP exhaustion, spot interruptions).

**Gaps:**
- **No AWS Health / PHD** — the scope's prime example (account-specific service events/maintenance) is not queried at all.
- **No ENI / IP-exhaustion signals** (`DescribeNetworkInterfaces` / VPC-CNI pool) — a classic EKS connectivity/scheduling root cause is invisible.
- **CloudTrail `errorCode`/`errorMessage` dropped** (`eventToChange`, `cloudtrail.go:92-114`) — AccessDenied/throttling spikes are lost.
- **Single-region**, no cross-region fan-out; ASG↔cluster scoping is a fragile name-substring match.
- **`cloud_resource_health` ignores the time window** — only point-in-time describes + the last 5 ASG activities.
- **GCP control-plane lens is entirely absent** — cloud tools gate on `cloud.provider == "aws"` (`investigate.go:132`); there is no `gcp/` provider. On GKE, "what changed at the cloud layer" (firewall/IAM/GKE-upgrade/Cloud-NAT) is blind — only `gcpfirewall` for `network_drops` exists.
- **Config footgun:** a typo in `cloud.provider` **silently disables** the cloud tools with **no log** (exact `== "aws"`, no `default`/warn branch and no `Validate()` coverage), unlike `network.provider` which warns on an unknown value.

---

## 4. Cross-cutting observations

- **Redaction is a solid egress boundary.** `redact.Secrets` runs on every tool output and the seed prompt *before* truncation and before anything reaches the model / KB PR / chat (`loop.go:199,341`).
- **The 50-row render cap is the effective ceiling** for most tools — frequently tighter than the provider cap, and it **shadows providers' own "narrow your query" hints** (logs, GCP firewall). Meanwhile the byte cap defaults to *unlimited* (the shipped Helm value sets 32 KiB) — an inconsistent pair.
- **Time-awareness is the systemic weak spot** — metrics-at-now, unused what-changed window, no change↔spike overlay. This is the biggest single lever on RCA quality.
- **The LLM authors raw PromQL/LogsQL with no cost guard** beyond a 30 s HTTP timeout and `maxSteps=20` — a wide-window/high-cardinality query is bounded only loosely.
- **Backend coverage is asymmetric:** metrics (Prom+VM) and GitOps (Flux+Argo) are dual; logs (VictoriaLogs-only) and cloud (AWS-only) are single — matching the README's honest "golden path" framing.

---

## 5. Per-integration gap summary (prioritized)

| Integration | Highest-value missing signal | Severity |
|-------------|------------------------------|----------|
| Kubernetes | previous-container logs + a general workload-log reader over the K8s API | **High** |
| Metrics | range queries + incident-time anchoring (wire the existing `QueryRange`) | **High** |
| GitOps | HelmReleases in the change spine; commit time/author; use the `TimeWindow` | **High** |
| Network | Hubble L7 + DNS flows (not just drops) | **High** |
| Cloud | AWS Health/PHD; CloudTrail `errorCode`; ENI/IP exhaustion | Medium |
| Logs | raise the 50-line render cap for logs; keep structured fields; volume-spike `stats` | Medium |
| K8s | restart counts, owner/rollout status, node conditions, evicted-pod reason | Medium |

---

## 6. Methodology assessment (what's already good)

Worth stating plainly so it isn't lost among the gaps — these match best practice and should be preserved:

- **What-changed-first + runbook-early ordering**, encoded in the prompt and reinforced by self-describing tool descriptions.
- **Honest `unresolved`** as a first-class outcome; **adversarial verify that can only lower confidence**.
- **"Tool error = missing data, not evidence"** anti-anchoring instruction.
- **Correlation ≠ causation** rigor block with explicit confidence calibration.
- **Read-only** tool surface; mutations only *proposed*, server-gated.
- **Redaction** before egress; **untrusted-data** framing against prompt injection.

---

## 7. Corrections to the prior review (`dev/plans/2026-06-25-review-roadmap.md`)

Two items previously flagged are **resolved in current `main`** and should not be carried forward:

1. **Argo CD parity — DONE.** Argo CD fully implements `GitOpsInspector` (`providers/gitops/argocd/inspect.go`; compile-time assertion `argocd.go:185-188`), so `gitops_resource_status` and `gitops_tree` **are** registered for Argo. The "deep tools no-op on Argo" gap is closed. (Stale comments at `investigate.go:42-43` / `providers.go:186` still say "Flux does" — cosmetic only.)
2. **Differ GitHub-App auth — DONE.** The production differ is built with a token source (`internal/app/gitops.go:21`, `BuildForgeTokenSource`), used by the live watch/investigate path, CLI, eval, and MCP. A token-source error **aborts the clone loudly** rather than going silently unauthenticated (`differ.go:127-130`; test `TestDifferRemoteTokenError`). Private-repo what-changed is authenticated as shipped.

---

## 8. Recommendations

Ordered by leverage; each is independently shippable behind the existing provider interfaces. **Items 1–2 are implemented in this branch**; the rest are follow-up slices.

1. ✅ **Implemented (this branch).** **Wire `QueryRange`** into a `query_metrics_range` tool — per-series first→last with min/max over a bounded window, so the agent sees a metric rise/spike/recover instead of a single "now" scalar. (Full incident-*timestamp* anchoring — evaluating at the alert time — remains a follow-up.)
2. ✅ **Implemented (this branch).** **Added a general workload-log tool** (`pod_logs`) over the K8s API (namespace + optional selector) **with `previous=true`** for CrashLoop crash output. Closes the largest K8s blind spot. (Per-container targeting for multi-container pods is a follow-up.)
3. **Bring HelmReleases into the change spine** (helm-controller `status.history` / chart-version delta) and **emit `ChangeImageBump`/`ChangeChartBump`**; surface **commit time + message** and start **using `TimeWindow`** to rank changes by proximity to the incident.
4. **Add Hubble L7 + DNS queries** to `network_drops` (or a sibling tool), and include L4 port/protocol + policy name in the output.
5. **Add an AWS Health/PHD probe** and surface CloudTrail `errorCode`/`errorMessage`; add an ENI/IP-exhaustion check.
6. **Decouple the logs render cap** from `maxToolRows` (raise it for logs), keep structured fields, and add a volume-spike `stats` helper.
7. **Smaller hardening:** surface restart counts + owner/rollout status + evicted-pod reason in `pod_status`; warn (don't silently disable) on an unknown `cloud.provider`; reconcile the 50-row vs unlimited-byte cap.
8. **Tidy the stale Argo "Flux does" comments** (§7) so the code reads true.

---

## 9. Note on the external best-practice grounding

The external-source pass (deep-research fan-out) **completed only partially** — it hit an account session limit during verification, so the final synthesis did not run. It returned **15 adversarially-verified claims** (3-0 / 2-1 votes), the load-bearing ones folded in above:

- **K8s:** OOMKilled confirmed via `describe pod` / exit-137; **cgroup-v1 can hide OOM kills** (child-process kill) — GKE docs.
- **Logs:** mandatory `_time` bound; stream-label scoping cuts IO/CPU; line-filters early; volume-spike via `stats by (_time:1h) count()`; exact matchers > regex; JSON-parse-error isolation — VictoriaLogs + Loki docs.
- **Change correlation:** correlate change-insight with error-insight on the same service in a window — Grafana Knowledge Graph.

The metrics (RED/USE specifics), network, and cloud best-practice claims largely failed verification under the session limit; the gap analysis above for those rests on the code read + standard SRE practice and can be re-grounded with a follow-up research pass after the limit resets.

---

## 10. References

- **Engine/prompt/verify:** `internal/investigate/loop.go` (loop `:120-351`; system prompt `:19-71`); `verify.go`; `tools.go`; `internal/app/investigate.go:32-141`; MCP `internal/mcp/server.go`, `internal/app/mcp.go`.
- **Kubernetes:** `internal/providers/cluster/cluster.go`; `internal/investigate/kube_tools.go`, `controllerlogs_tool.go`; `internal/app/kube.go`.
- **GitOps/what-changed:** `internal/investigate/{whatchanged,gitops_status,gitops_tree}_tool.go`; `internal/whatchanged/differ.go` (`:46-78,119-148,223-231`); `internal/providers/gitops/{flux,argocd}/*`.
- **Metrics:** `internal/investigate/query_tools.go:29-80`; `internal/metrics/prometheus/prometheus.go` (`:43-99`).
- **Logs:** `internal/investigate/query_tools.go:132-224`; `internal/logs/victorialogs/victorialogs.go`.
- **Network:** `internal/investigate/query_tools.go:82-130`; `internal/network/{hubble,awsvpc,gcpfirewall}/*`.
- **Cloud:** `internal/investigate/cloud_tools.go`; `internal/providers/cloud/aws/{cloudtrail,resourcehealth}.go`.
- **Integration matrix / intent:** `README.md` (Supported integrations); `docs/design.md`; `docs/data-sources.md`.
- **Prior review:** `dev/plans/2026-06-25-review-roadmap.md` (items in §7 now resolved).
