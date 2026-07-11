# Pre-announcement audit — 2026-07-11

Multi-agent audit of RunLore v0.8.0 (`main` @ 7219121) before the first public announcement.

**Method.** Two orchestrated workflows, 34 agents total: 9 specialist auditors (code quality,
dead code, docs, security, and five datasource-leverage lenses — metrics, logs, kube+gitops,
network+cloud, correlation/context engineering), followed by adversarial verification — every
P0/P1 finding was independently re-audited by a verifier prompted to refute it, and only
confirmed findings appear below. A 3-lens roadmap panel (launch first-impressions, datasource
depth, architecture) fed `2026-07-11-pre-announcement-roadmap.md`.

**Scorecard: 1 P0, 13 P1 (all confirmed high-confidence), 3 verified P2, 45 P2/P3 backlog
items, 1 finding refuted in verification.**

## Executive summary

The codebase is in unusually strong shape for a solo pre-1.0 project — every quality gate
green (build, vet, gofmt, race tests, strict golangci-lint), all 46 packages tested at
80–95% typical coverage, near-zero dead code (one TODO in 329 files), an engine-agnostic
provider contract that genuinely holds, and a security posture the auditor called one of the
strongest they'd seen in a pre-1.0 infrastructure agent, with security docs that are
*verifiably true* against the code.

The confirmed problems cluster in exactly two places:

1. **First-hour experience**: the advertised minimal quickstart refuses to start (P0), four
   docs contradict the fail-closed webhook-token rule, and two ReAct-loop exit paths drop
   alerts silently.
2. **The correlation differentiator**: `Change.When` is never populated so the advertised
   change↔symptom timestamp correlation cannot happen; `what_changed` namespace semantics
   contradict the system prompt for standard Flux layouts; `query_logs` loses pod attribution
   and inverts first-seen timestamps; `pod_logs` returns nothing on multi-container (mesh)
   pods; and the CloudTrail resource-scoping path fails 100% of the time on a real API
   contract limit its fake-backed test can't catch.

None of the datasource findings are architectural — all are addressable within existing
contracts, mostly by rendering fields the code already fetches.

## What to lead with in the announcement

- **Verifiable security docs**: every load-bearing claim in `security-model.md` /
  `security-architecture.md` traced to code and confirmed (redact-before-truncate chokepoint,
  server-authoritative action gating, constant-time fail-closed auth, SSRF redirect guard
  with credential stripping, recall disabled under auto).
- **Model-aware tool engineering**: honest truncation sentinels, dedup-before-cap log
  rendering, `previous=true` crash-output recovery, corrective retryable error messages
  (LogsQL `level=` guardrail), "a tool error is missing data, never evidence".
- **Rigorous recall pipeline**: measured query enrichment (documented 0.096→rank-1 fix),
  calibrated LLM reranker with cost guard, Beta-posterior outcome decay, adversarial verify
  pass, recall hard-disabled under auto-execution.
- **Supply chain**: SHA-pinned actions, distroless nonroot, cosign keyless signing, SBOM +
  SLSA provenance, govulncheck + Trivy in CI.
- **Credible honesty**: sub-50% RCA numbers, enumerated redaction gaps, real pilot stats
  (29 PRs, ~72% closed unmerged) — rare, differentiating transparency.
- **Hygiene**: `deadcode`, `staticcheck U1000`, `go vet`, `gofmt`, `go mod tidy` all clean;
  the working demo (`hack/demo.sh` output matches the README transcript line-for-line); zero
  broken doc links.

## Confirmed findings

### P0 — must fix before announcing

**A1. Advertised minimal quickstart refuses to start** — `docs`
`deploy/helm/runlore/values-minimal.yaml:8-20,43-46`, `docs/getting-started.md:199-364`, `internal/app/config.go:58-64`
`values-minimal.yaml` configures `model.provider: anthropic` with no `server.webhook_token_env`,
but `RequireWebhookAuth` fails closed whenever a model is configured. **Reproduced**: `lore serve`
with the exact shipped values exits 1. The getting-started step-4 "complete production-style
example" has the same flaw → copy-paste yields CrashLoopBackOff. `minimal_values_test.go` only
checks `config.Load`, not the serve-path guard, so CI can't catch a regression.
*Fix*: add the `server:` block + secret key to both, fix the "optional" comment, extend the test
to assert `RequireWebhookAuth` passes on the shipped values.

### P1 — first-hour experience

**A2. Two ReAct-loop exits deliver no notification (silent alert drop)** — `code-quality`
`internal/investigate/loop.go:527-531,626-628`
The prose-inconclusive and max-steps exits return with only a `Warn` log — no `deliver()`, so
no Slack/Matrix message, no ledger, no KB draft, after paid model calls. Realistic on
OpenAI-compatible local servers (vLLM/Ollama) that don't reliably enforce `tool_choice`.
*Fix*: synthesize an inconclusive Investigation and deliver, mirroring the timeout/refusal
pattern; structurally, funnel all six exits through one `finish()` (see roadmap).

**A3. Webhook-token rule wrong in four docs** — `docs`
`docs/configuration.md:294-299`, `docs/security-model.md:107-109`, `docs/security-architecture.md:243-245,261`, `docs/troubleshooting.md:46-47`
All four claim the token is warning-only / mandatory-only-under-auto; the code fails startup
whenever a model is configured. Only getting-started and the harbor example are right — the
docs contradict each other.

**A4. `actions.mode=approve` requires `audit_log_path`, docs say auto-only** — `docs`
`docs/configuration.md:184-187`, `docs/upgrade-uninstall.md:61`, `docs/getting-started.md:342-359`, `internal/config/config.go:983-994`
Uncommenting the documented approve example fails startup validation.

**A5. Internal SDD specs/plans leaked back into `docs/`** — `dead-code`
`docs/superpowers/plans/` (10 files), `docs/superpowers/specs/` (5 files)
15 artifacts dated 2026-06-30→07-09 re-accumulated after commit c546527 deliberately moved the
tree to `dev/` so "docs/ presents as product". Cheap `git mv` + a handful of intra-tree links.

### P1 — correlation & datasource correctness

**B1. `Change.When` is never populated — the advertised change↔symptom timestamp correlation cannot happen** — `correlation` + `ds-kube-gitops` (found independently by both)
`internal/providers/gitops/flux/flux.go:213-222`, `internal/providers/gitops/argocd/dynamic.go:210-225`, `internal/investigate/whatchanged_tool.go:63-67`
The renderer's `at <RFC3339>` branch is dead code for both GitOps engines — only CloudTrail
sets `When`. The data is already in hand: ArgoCD parses `status.history` but discards the
adjacent `deployedAt`; the Flux differ resolves the ToRev commit whose `Committer.When` is
free. Both providers also ignore the `TimeWindow` argument entirely. The verifier called this
"the single highest-leverage timeline fix".

**B2. `what_changed` namespace semantics contradict the system prompt for standard Flux/ArgoCD layouts** — `correlation` + `ds-kube-gitops` (found independently by both)
`internal/providers/gitops/flux/flux.go:86-92`, `internal/providers/gitops/argocd/argocd.go:80-86`, `internal/investigate/loop.go:57-58,64-65`
The prompt says "scope what_changed to the failing workload's namespace" but the providers
filter on the GitOps object's *own* namespace — and in the default Flux bootstrap every
Kustomization lives in `flux-system` (which the same prompt states two lines earlier). The
instructed query returns a false "no changes found" on the flagship feature; the workaround
(`namespace=flux-system`) returns every app's full git patch, uncapped. `GetResource` already
solved this with a fallback (`dynamic.go:111-128`) — `Changes()` didn't get the same treatment.

**B3. Diff content unbounded per file/call; global truncation blunt and off by default outside Helm** — `ds-kube-gitops`
`internal/investigate/whatchanged_tool.go:74-76`, `internal/config/load.go`, `internal/investigate/truncate.go:12-26`
Every `FileDiff.Patch` prints in full; only the Helm chart sets `MaxToolOutputBytes`, and when
the cap does bind, head+tail truncation cuts diffs mid-hunk — the model can reason from a diff
missing the breaking hunk.

**B4. `query_logs` drops all stream fields — no pod/container attribution** — `ds-logs`
`internal/investigate/renderlog.go:48-65`, `internal/logs/victorialogs/victorialogs.go:163-173`
`parseNDJSON` preserves `kubernetes.pod_name` etc.; the renderer never reads `Fields`, and
grouping merges identical messages from *different* pods. The model can't tell one failing
replica from all of them, or pivot to `pod_status` on the right workload.

**B5. `query_logs` timestamps are last-seen, not first-seen** — `ds-logs`
`internal/logs/victorialogs/victorialogs.go:66-92`, `internal/investigate/renderlog.go:24-47`
VictoriaLogs `limit+offset` pagination returns newest-first; `groupLogLines` takes first-in-slice-order
as "first seen". The model anchors error onset at the *latest* occurrence and can rule out the
actual culprit deploy. (Metrics tools deliberately ship `@min/@max` timestamps for exactly this.)

**B6. `pod_logs` silently returns nothing for any multi-container pod** — `ds-logs`
`internal/providers/cluster/cluster.go:61-66`, `internal/providers/providers.go:263-271`
`PodLogOptions.Container` is never set; the API rejects log requests on multi-container pods
and the error is swallowed by `continue`. Every pod with an istio/linkerd/cloudsql sidecar →
"no log lines matched". Fake-clientset tests can't catch it (no container validation).

**B7. `cloud_what_changed` resource-scoping fails 100% of the time** — `ds-network-cloud`
`internal/providers/cloud/aws/cloudtrail.go:24-41`, `aws_test.go:157-160`
Two `LookupAttributes` are sent when a resource is given; the CloudTrail API accepts exactly
one ("Currently the list can contain only one item" — pinned SDK doc). The fake-backed test
asserts the broken shape, and the tool description actively steers the model into the path.

**B8. IP-based network providers silently ignore the namespace/pod selector** — `ds-network-cloud`
`internal/network/awsvpc/awsvpc.go:85`, `internal/network/gcpfirewall/gcpfirewall.go:91`, `internal/investigate/query_tools.go:185-218`
The schema *requires* `namespace`; the result carries no hint that scoping was ignored and is
VPC-wide. No tool exposes pod IPs, so the model cannot bridge IP→pod — internet-scanner REJECT
noise is attributable to the workload under investigation.

**B9. `cloud_resource_health` caps at 25 ASGs region-wide *before* the cluster filter** — `ds-network-cloud`
`internal/providers/cloud/aws/resourcehealth.go:58-79,119-134`
In a shared account the cluster's ASGs can be missed entirely — the exact capacity-failure
signal the tool exists for. The SDK supports server-side tag filters (`eks:cluster-name`).

### Verified P2 (confirmed by verification, kept out of P1 by design intent)

- **C1. No metric/label discovery** (`ds-metrics`) — provider exposes only query/query_range;
  "no series matched" is a dead end. Custom app metrics are unguessable; the model burns loop
  steps trial-and-erroring names. Verifier downgraded P1→P2 (well-known names are in-weights)
  but it remains the #1 leverage gap.
- **C2. Recall near-misses discarded; in-loop `kb_search` re-suffers the vocabulary-mismatch
  problem recall already solved** (`correlation`) — below-threshold-but-structurally-correct
  runbooks shape nothing; `kb_search` queries are unenriched despite per-investigation rebinding.
- **C3. Context budget unlimited by default in code** (`correlation`) — `0 ⇒ unlimited` for
  `MaxToolOutputBytes` and `MaxTokensPerInvestigation`; only the Helm chart caps them. The
  compaction/nudge/hard-kill machinery is excellent but never engages on the default path
  (CLI, MCP, eval, bare Docker).

### Refuted in verification (for the record)

- *"No deterministic seed context pack — first steps burn paid turns on fetches the code could
  do up-front"*: the mechanism is as described, but the cost claim is falsified by Anthropic
  prompt caching (rolling cache breakpoints make the growing prefix a guaranteed hit), and it
  is an optimization proposal, not a defect. Kept as a roadmap idea, not a finding.

## Backlog (P2/P3, unverified — 45 items)

Full details with evidence live in the workflow outputs; titles by dimension:

**code-quality**: coverage cold spots in composition root/server (P2); 418-line
`LoopInvestigator.Investigate` (P3); startup probe uses `http.DefaultClient` against project
convention (P3); Slack `response_url` failures silently swallowed (P3).

**docs**: metrics/logs backend auth keys (`token_env`, `headers`) documented nowhere incl.
values.yaml (P2); `curate` block and `model.max_tokens` missing from configuration.md (P2);
`timeout` missing from observability.md result enum (P3); troubleshooting startup-error quote
omits pagerduty (P3); AGENTS.md stale "no cluster-mutating code in v1" (P3).

**security**: egress redaction misses model-authored `RuledOut`/`DataGaps`/`Hypothesis.ChangeRef`
— violates the project's own #197 rule (P2); Matrix notifier auto-linkifies attacker-influenceable
URLs, inconsistent with the Slack anti-phishing stance (P2); no defense against markdown
image-beacon exfil in GitHub-rendered surfaces (P2); webhook unauthenticated by default +
permissive default NetworkPolicy ingress (P2); SECURITY.md over-claims "reads Kubernetes
Secrets" — no code path does and RBAC grants none (P3, five-minute fix worth doing
pre-announcement); Docker base images tag-pinned not digest-pinned (P3); Hubble Relay gRPC
always plaintext (P3).

**ds-metrics**: series cap keeps arbitrary first-50, no aggregation steering in descriptions
(P2); `since_minutes`/`step_seconds` unclamped, step not derived from window — Prometheus's
11k-point limit reachable (P2); range summary can't distinguish step change from ramp (P2);
scalar PromQL results return a raw Go unmarshal error (P3); MetricsQL/TSDB-status strengths
unused despite VM being the headline backend (P3).

**ds-logs**: only raw-line endpoint used — hits histograms, stats/top/collapse_nums, facets,
stream_context untapped (P2); hardcoded collector field conventions undocumented/unconfigurable —
empty results ambiguous at adopter sites (P2); `query_logs` bypasses the pod_logs namespace
confinement (same logs, unrestricted) (P2); `controller_logs` is Flux-only but registered
unconditionally for ArgoCD deployments (P2); unlimited `max_tool_output_bytes` outside Helm
(P3, subsumed by C3); silent 5-pod fan-out cap, exact-match-only dedup (P3).

**ds-network-cloud**: awsvpc/hubble caps bind silently — no sentinel, unlike gcpfirewall/
cloudtrail (P2); awsvpc hard-codes the default v2 flow-log format — custom formats silently
return "no dropped flows" (P2); CloudTrail `errorCode`/`errorMessage` dropped — failed mutating
calls (InsufficientInstanceCapacity, UnauthorizedOperation) are the highest-signal events and
render as successes (P2); Karpenter capacity and spot interruptions invisible (P2); Hubble
client always dials plaintext (P3).

**ds-kube-gitops**: `kube_events` Limit:200 fetches an arbitrary name-ordered page — newest
events can be missing in busy namespaces (P2); `pod_status` omits restart counts and all
timestamps — the only tool with no time anchor (P2); GitOps inspector events drop
timestamps/counts (P2); GitOps Changes ignore the time window, latest revision only (P2);
no ownerReferences walk, no live-spec-vs-Git drift detection — `kubectl edit` causes are
structurally invisible (P3); full non-bare clone per repo per investigation (P3 — `isBare`/
`NoCheckout` is a one-line fix).

**dead-code**: legacy `network:{url}` shim — remove around launch while only pilot users
exist (P2); two exported test-only functions (P3); 116 internal planning docs tracked under
`dev/` in the public repo (P3 — consider whether that's intended as public design history).

**correlation**: coalesced alert batches collapse to `batch[0]` + title digest — constituent
workloads lost from the seed (P2); neither verify nor the eval judge ever sees tool
transcripts — groundedness asserted, unverifiable (P2); `kube_events` has no time-window
param; `what_changed` uncapped rows (P2); `truncateOutput` can split UTF-8 runes (P3).

## Dimension verdicts (one line each)

| Dimension | Verdict |
|---|---|
| Code quality | Above the bar; one P1 (silent drops), rest polish. |
| Dead code | Near-zero; the hygiene itself is announcement-worthy. |
| Docs | Exceptionally strong; one P0 coherence failure around the new fail-closed guard. |
| Security | No P0s; strongest dimension; margins of the untrusted-output story need tightening. |
| Metrics | Thoughtful token economics; discovery is the gap. |
| Logs | Disciplined engineering; three P1s directly mislead meshed/multi-replica investigations. |
| Kube+GitOps | Strong micro-design; macro correlation plumbing (When, namespace, bounding) is the gap. |
| Network+Cloud | Structurally well designed; one deterministic API bug + silent-scope traps. |
| Correlation | Far above typical v1; gaps concentrated in exactly the differentiator. |

Roadmap: see `2026-07-11-pre-announcement-roadmap.md`.
