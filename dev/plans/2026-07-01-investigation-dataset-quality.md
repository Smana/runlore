# Investigation dataset quality — what the model sees, what the human reads

|              |                                                                                                      |
| ------------ | ---------------------------------------------------------------------------------------------------- |
| **Status**   | Analysis `v1` — top findings **implemented in this branch** (marked ✅); the rest ranked as follow-ups |
| **Date**     | 2026-07-01                                                                                            |
| **Scope**    | The *dataset* an investigation runs on: how each tool's payload is serialized into the prompt (time, shape, magnitude, attribution, noise), what the seed prompt carries, how truncation shapes what survives, and what the delivered notification actually tells an on-call. Companion to `2026-06-27-troubleshooting-integration-analysis.md`, which covered signal *breadth* per integration — this covers signal *fidelity* end to end. |
| **Method**   | Direct read of every render path (`internal/investigate/*_tool.go`, `query_tools.go`, `kube_tools.go`, `truncate.go`, `loop.go` seed prompt), the payload types (`internal/providers/providers.go`), the providers that populate them (`cluster/cluster.go`, `gitops/flux`, `logs/victorialogs`), the alert intake (`source/alertmanager`), and the delivery path (`notify/format.go`, `slack.go`, `matrix.go`) — cross-checked against the eval rubric and the graded baseline transcripts (`eval/reports/*`). |

---

## 1. Bottom line

The investigation *engine* and the tool *breadth* were already audited (2026-06-27) and are solid. The
remaining quality ceiling is the **dataset**: several renderers strip exactly the dimensions RCA needs
— **time** and **attribution** — and the alert intake silently discards the alert's most informative
text before the investigation even starts. The judge transcripts show where this lands: runs score
`root_cause 2/3 — "correct but shallow … does not state that a change introduced this state"`. The
model can't narrate "deploy at 14:02, first crash at 14:03" when no tool output contains a timestamp
it can line up against another.

Five findings dominate (all fixed in this branch):

1. **Alert annotations were never parsed.** `amAlert` had no `annotations` field
   (`source/alertmanager/alertmanager.go:27-32`), so `Request.Message` was **empty for every
   alert-triggered investigation** — the templated description ("container X memory at 97% of limit
   for 15m"), the summary, and `runbook_url` were all dropped at the door. The seed prompt for an
   alert was effectively just the alertname. This was the single cheapest, highest-value fix in the
   codebase. ✅
2. **The model was never told when the incident started.** `Request.At` (Alertmanager `startsAt`)
   existed but `seedPrompt` (`loop.go:535`) never rendered it — and every tool window
   (`since_minutes`) is relative to *now*. The model could only guess how far back to look; a default
   60m window silently misses an onset 90 minutes ago. `Request.Labels` (pod, container, instance,
   cluster…) were equally dropped. ✅
3. **Renderers strip time and repeat structure.** `kube_events` sorted by recency but the
   `KubeEvent` type carried **no timestamp** — the model saw order, never *when*. `pod_logs` rendered
   bare messages with **no timestamps at all** (`PodLogOptions.Timestamps` unset). `what_changed`
   dropped `Change.When` even when the engine knew it. `query_metrics_range` reported `min/max` but
   not **when** the max happened — the one fact that correlates a spike to a deploy. ✅
4. **Noise crowds out signal under the 50-row cap.** No renderer deduplicated repeated lines: a
   CrashLoopBackOff emits the same panic block every restart, so the 50-row budget
   (`maxToolRows`, `query_tools.go:14`) was spent on 50 copies of one line while the *distinct*
   second error never rendered. Log-shaped output is now grouped — each distinct message renders
   once with `(xN, last <time>)` — so the cap counts distinct messages. ✅
5. **The shared notification text omitted the two anchors an on-call reads first.** `notify.Format`
   (used by Matrix, the webhook fallback, the CLI, and Slack's plain-text fallback) never named the
   **affected resource** nor the root cause's **ChangeRef** (the latter existed only in Slack Block
   Kit). ✅

What is **already good** and was verified, not assumed: `pod_status`'s failure surface (OOM →
memory-limit tie-in, last-termination reason/exit-code — the fix the first eval baseline forced);
the selector-matched-nothing fallback that prevents "workload not deployed" hallucinations;
`buildLogsQL`'s guardrail against invalid `level=` syntax; events Warning-first + most-recent-first;
VictoriaLogs returning newest-first so the row cap keeps the *recent* lines; redaction strictly
before truncation; and the honest empty-result strings ("no series matched", "no dropped flows")
that keep absence-of-signal legible.

---

## 2. Model-facing dataset — per-source rendering audit

For each source: does the rendered text preserve **time**, **shape**, **magnitude/units**, and
**attribution** (which pod/object emitted it)?

| Tool | Time | Shape | Attribution | Verdict |
|---|---|---|---|---|
| `what_changed` | ❌→✅ `Change.When` was dropped at render (`whatchanged_tool.go:55`); now rendered when non-zero. **Flux never sets it** (no commit timestamp — `flux.go` maps only revisions), so the fix pays off fully only with follow-up F3. | full unified diff — good | Kustomization kind/name — good | diff content good; *when* was the gap |
| `query_metrics` | instant-at-now only (by design; description now points at the range tool) | single scalar | full label set via `formatMetric` — good | fine as the "value now" probe |
| `query_metrics_range` | ❌→✅ `min/max` had no timestamps; now `min=1@t max=9@t` | first/last/min/max — adequate compact trend | labels — good | the spike is now *datable* |
| `query_logs` | per-line RFC3339 — good | newest-first (VictoriaLogs orders by `_time` desc) | ⚠️ `Fields` (pod/node) dropped at render — follow-up F5 | now deduped with counts ✅ |
| `pod_logs` | ❌→✅ no timestamps at all; now `Timestamps: true` + parsed into `LogLine.Time` | chronological tail (300/pod, ≤5 pods) | pod-name prefix — good | time was the gap; now deduped ✅ |
| `controller_logs` | ❌→✅ same reader; now timestamped + deduped | tail | pod-name prefix — good | ✅ |
| `pod_status` | n/a (point-in-time) | per-container reasons + last termination + memory limit — **good** | pod + container names — good | already strong; no change |
| `kube_events` | ❌→✅ `KubeEvent` had no time field; `LastSeen` added end-to-end | most-recent-first, count-weighted (`x13`) — good | Kind/Name — good | the count existed; the *when* didn't |
| `network_drops` | provider-dependent (`LogLine.Time`); rendered when present ✅ | drops only (breadth gap — prior doc) | message-embedded | deduped ✅ (drop storms collapse) |
| `cloud_what_changed` | RFC3339 per event — **already good** | newest-first, capped 25 | actor (`ManagedBy`) + resource | the model to copy |
| `gitops_resource_status` / `gitops_tree` | condition/event lines as provider renders them | Ready/reason/message + refs — good | kind/ns/name — good | no change needed |

### 2.1 Truncation policy

- `MaxToolOutputBytes` **defaults to 0 = unlimited** (`config.go:176`); the shipped Helm values set
  **32 KiB** (`values.yaml:63`). One flat byte cap for heterogeneous tools.
- `truncateOutput` (`truncate.go`) keeps **head + tail, elides the middle**. For a *diff* this is
  reasonable (header + trailing hunks survive). For *logs* it's the wrong shape — but after this
  branch it rarely fires on logs: dedup collapses repetition and the 50-distinct-row cap bounds log
  output well under 32 KiB, so byte truncation is now effectively the *diff* backstop it suits.
- The real ceiling for list-shaped output is `maxToolRows = 50`, applied *before* byte truncation.
  Pre-dedup, 50 rows of a crash loop ≈ 1 distinct fact. Post-dedup, 50 rows ≈ 50 distinct facts —
  the cap's information content improved by up to ~50× without raising a single limit.
- Per-tool byte caps (bigger for `what_changed`, smaller for status tools) remain a follow-up (F2)
  but are much less urgent now that the noisiest producers dedup.

### 2.2 Time windows

Defaults: logs/metrics-range/drops 60m, pod/controller logs 30m, cloud 90m — all documented in each
schema and all **relative to now**. The seed prompt now states the incident start time, its age, and
explicitly: *"Tool time windows (since_minutes) are relative to now — size them to cover the start
time."* Anchoring queries at `req.At` server-side (an `around_incident=true` mode) is follow-up F8.

### 2.3 Seed prompt

Before: `Incident: <title> (source=…). Workload: ns/name. Reason: <severity>. Message: <empty for
alerts>. Severity… Environment…` — and nothing else. After: the same header plus **start time + age**,
**alert labels** (sorted `k="v"`, 300-rune value clip), and **alert annotations** (minus the one
already promoted to Message — so `runbook_url` now reaches the model, which pairs directly with the
system prompt's "search the KB EARLY" instruction). All of it passes through `redact.Secrets` as
before, and stays inside the existing "UNTRUSTED DATA" framing.

---

## 3. Human-facing notification

Read end-to-end (`format.go` → `slack.go` blocks / `matrix.go` HTML):

- **Fixed ✅:** the shared `Format` text now names the **affected resource** (`Resource: HelmRelease
  tooling/harbor`) and each root cause's **What changed: <ChangeRef>**. Slack blocks already showed
  ChangeRef; Matrix/webhook/CLI readers never saw either.
- **Evidence is model-authored prose** (`Hypothesis.Evidence []string`) with no structure — no
  source attribution, no timestamp, no query. The good runs in the eval transcripts show the model
  *voluntarily* writing `"pod_status: …"` / `"(from kube_events)"` prefixes; the bad ones don't.
  Making attribution structural (F1) is the top follow-up: extend `submit_findings` evidence items
  to `{source, at, detail}` and render `• [pod_status 14:03] …`. Deferred from this branch because
  it touches the findings schema, the verify pass, the curator, and recall — a PR of its own.
- **Negative evidence** ("checked metrics/network — clean") builds trust and is currently only
  visible if the model happens to write it into a summary. A `checked_ok` field is F4.
- **Time correlation** in the headline ("deploy at 14:02, first crash at 14:03") is now *possible*
  — the model finally has both timestamps — but not enforced. Expect it to appear in evidence
  naturally; enforce via prompt/schema only if evals show it doesn't.
- Unresolved, confidence badge (max of overall/top root cause), suggested next steps with
  reversibility, KB link, approval buttons: all present and sensible. Per the open PR #196,
  `slack.go` escaping was deliberately left untouched; all changes are in `format.go`/data shape.

---

## 4. Eval grounding

- `eval/reports/2026-06-22-k3d-baseline.md` + the JSON transcripts: coverage is consistently 100%;
  points are lost on **root_cause depth** (`2/3 — "doesn't explicitly state that a change
  introduced this configuration"`) — i.e. change-attribution and time-correlation, exactly what
  findings 1–3 target. The judge's `evidence` scores were saved by the model writing tool names
  into evidence strings by itself; F1 makes that reliable instead of lucky.
- The rubric's `calibration` dimension (confident-wrong penalized hardest) is served by the richer
  dataset too: a model that *sees* the event time and the change time can verify a causal chain
  instead of asserting one.
- Note: the graded runs predate `query_metrics_range`/`pod_logs` (PR #163), so there is no eval
  evidence yet for the metrics/logs renderers; the audit above is code-read-based for those.

---

## 5. Ranked follow-ups (not in this branch)

| # | Item | Why / where | Effort |
|---|---|---|---|
| F1 | **Structured evidence** in `submit_findings` (`{source, at, detail}`) rendered with attribution in chat | turns "lucky" evidence quality into guaranteed; feeds rubric `evidence` + reader trust | M — schema touches verify/curator/recall |
| F2 | Per-tool output caps + error-first line selection inside `truncateOutput` for log-shaped text | one flat 32 KiB cap suits a diff, not 10k log lines; keep first occurrence of each error | M |
| F3 | Flux commit **timestamp/author/message** on `Change` + HelmReleases in the change spine + honor `TimeWindow` | carried from 2026-06-27 §8.3; `what_changed` now renders `When` the moment Flux supplies it | M–L |
| F4 | `checked_ok` (negative evidence) field in findings + notification section | "what came back clean" builds on-call trust | S–M |
| F5 | Render `LogLine.Fields` attribution (pod/node) in `query_logs` | VictoriaLogs stream fields are dropped today | S |
| F6 | `kube_events`: fetch window > `Limit: 200` or field-select Warning server-side | in a noisy namespace Normal events crowd the 200 before the Warning filter (`cluster.go:163`) | S |
| F7 | Raise `maxToolRows` for log tools post-dedup | 50 *distinct* messages is usually enough; revisit with eval data | S |
| F8 | Incident-anchored queries (`around_incident` mode using `req.At` server-side) | removes the model's window arithmetic entirely | M |
| F9 | Seed prompt: include the alert's own PromQL expr when Alertmanager provides `generatorURL` | re-running the firing expression is the canonical first metric query | S |

---

## 6. Implemented in this branch (summary)

1. **Alert annotations parsed + promoted** — `Request.Message` from `description`→`summary`→`message`;
   full `Request.Annotations` threaded (`source/alertmanager`, `investigate.Request`).
2. **Seed prompt time-anchor + labels/annotations** — start time with age and window guidance;
   sorted, value-clipped labels/annotations; message-annotation not duplicated (`loop.go`).
3. **Timestamps everywhere they existed but weren't shown** — `KubeEvent.LastSeen` (type, provider,
   render); `pod_logs`/`controller_logs` per-line kubelet timestamps; `what_changed` renders
   `Change.When`; `query_metrics_range` renders `min/max@time`.
4. **Log dedup** — shared `renderLogLines` groups identical messages `(xN, last <t>)` before the
   50-row cap, across `pod_logs`, `controller_logs`, `query_logs`, `network_drops`.
5. **Notification anchors** — `Format` names the affected resource and per-root-cause ChangeRef
   (shared text path only; `slack.go` untouched per PR #196).

Plus matching tool-description updates (`query_metrics` → points at the range tool; `kube_events` →
states ordering/timestamps/counts) so the model knows the semantics it's reading.

---

## 7. References

- Renderers: `internal/investigate/{query_tools,kube_tools,podlogs_tool,controllerlogs_tool,whatchanged_tool,cloud_tools,renderlog}.go`; caps `query_tools.go:14`, `truncate.go`.
- Seed/loop: `internal/investigate/loop.go` (`seedPrompt`, system prompt); intake `internal/source/alertmanager/alertmanager.go`.
- Payloads/providers: `internal/providers/providers.go`; `internal/providers/cluster/cluster.go`; `internal/providers/gitops/flux/flux.go`; `internal/logs/victorialogs/victorialogs.go`.
- Delivery: `internal/notify/{format,slack,matrix}.go`.
- Grounding: `eval/rubric.md`, `eval/reports/2026-06-22-k3d-baseline.md` + JSON transcripts; prior analyses `dev/plans/2026-06-27-troubleshooting-integration-analysis.md`, `dev/plans/2026-06-25-review-roadmap.md`.
