# RunLore Investigation Coalescing & Rate Limiting — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-22 |
| **Scope** | Cost & throughput controls on the alert → investigation → LLM path: (1) coalesce correlated alerts, (2) a safety rate-limit backstop, (3) per-investigation token efficiency |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | `internal/server/server.go` (webhook ingress), `internal/investigate/{investigate,loop}.go` (queue + ReAct loop), `internal/trigger/dedup.go` (existing deduper), `internal/action/auto.go` (existing `reserve()` rate-limit pattern); wiki `[[runlore]]` panel verdict (in-memory-on-leader caveat) |

---

## 1. Why this exists

A correlated alert storm — one node dies → `KubeNodeNotReady` + 40× `KubePodNotReady` + `KubeDeploymentReplicasMismatch` — currently triggers **one full ReAct investigation per alert**. The code makes the cost concrete:

- `server.go:handleAlertmanager` parses the Alertmanager payload and enqueues **one investigation per alert** (`for _, inc := range incidents { Enqueue(FromIncident(inc)) }`) — it *explodes* Alertmanager's own grouping back into N items.
- `investigate.go:Queue.Run` processes the workqueue **serially, one at a time per leader** (HA is leader-elected, so it's effectively serial globally).
- `loop.go:Investigate` runs up to **`maxSteps = 20`** LLM calls per investigation, re-sending a **growing, uncapped message history** on every step.

So a 40-alert storm is **≈ 40 sequential investigations × up to 20 steps ≈ 800 LLM calls**, trickling out long after the incident is understood, re-deriving *one* root cause 40 times. RunLore can't fire concurrent requests today (it's serial) — the failure mode is **not** a burst of parallel calls; it's a **slow-grinding queue of redundant, expensive work**, compounded by unbounded per-investigation token growth (history re-sent each step × large tool outputs).

Three orthogonal levers fix this:

1. **Coalescing** — collapse correlated alerts into one investigation. Storms are correlated, so this *eliminates* the redundant work rather than slowing it down. This is where ~all the savings live.
2. **Safety rate limit** — a global per-window budget + circuit breaker for everything coalescing *can't* catch: uncorrelated storms, flapping alerts, a reinvestigate/feedback loop, or RunLore's own remediation actions generating fresh alerts.
3. **Per-investigation token efficiency** — cap steps, truncate tool outputs, budget tokens. Trims the cost of each individual investigation.

Two existing seams make this cheap: a windowed `trigger.Deduper` already exists, and `action/auto.go:reserve()` already implements the exact windowed-counter rate-limit pattern we want to mirror.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **Three independent layers**: coalescing + safety rate-limit + per-investigation token budget | Orthogonal levers. Coalescing saves the most; the limiter is the backstop; token controls trim each run. Each ships and tests on its own. |
| D2 | **Correlation key = Alertmanager group + temporal window**: default key = AM `groupKey` (falls back to `namespace + alertname`); `correlation_labels` overrides to `namespace + chosen label values` | Reuses already-tuned AM grouping; `groupKey` *is* the group identity, so no over-coalescing of unrelated issues. |
| D3 | **Coalescer = "C + A"**: stop exploding the AM group (free), *plus* an ingress debounce buffer that folds separate POSTs sharing the key | C kills the canonical one-group storm immediately; A catches "node-down fires several groups across POSTs." Buffer is a small clock-injected unit; the investigation loop is untouched. |
| D4 | **Safety limiter = `ratelimit.Window` (the `auto.go:reserve()` pattern)** — sliding-window cap on investigation *starts*; over-budget → `AddRateLimited` backoff, dropping a key only after `max_requeues` (AM re-fires it) | Bounds spend to `max_per_window`/`window` regardless of volume; overflow self-limits via client-go backoff — no hot loop, never silently lost. |
| D5 | **Token efficiency = configurable `max_steps` + `max_tool_output_bytes` truncation + `max_tokens_per_investigation` with graceful conclude** | Tool-output truncation is highest ROI (history re-sent every step). Graceful conclude (submit findings with what you have) beats a hard abort that wastes the whole run. |
| D6 | **`critical`-severity fast-path in v1** | A debounce must never delay the first look at a critical page: flush immediately, but still register the key so later siblings attach instead of spawning new runs. |
| D7 | **State is in-memory on the leader for v1**; persistence deferred to phase 2 | This is cost protection, not mutation safety. AM `repeat_interval` re-sends unresolved alerts after a failover, so dropped pending alerts self-heal. Matches the known action-limiter caveat in the wiki. |
| D8 | **Concurrency stays at 1 by default** (`max_concurrent` knob exists but is discouraged) | Coalescing reduces the need to parallelize; raising concurrency trades the safety ceiling for drain speed. Default preserves today's behavior. |
| D9 | **Observability via OpenTelemetry** — instrument with the OTel metric API, export through the Prometheus exporter on `GET /metrics` (scrape-friendly for VictoriaMetrics); inert until enabled | The feature is a cost control, so it must be measurable; OTel keeps instrumentation vendor-neutral while the Prometheus exporter fits the platform's `VMServiceScrape` model. |

## 3. Architecture

```
Alertmanager POST  (one group, .alerts[] + groupKey/groupLabels)
        │
        ▼
server.go:handleAlertmanager
        │  ParseAlertmanager → per-alert Incidents (now carrying GroupKey)
        │  engine.Decide(inc): trigger policy + dedup — only investigate-worthy incidents reach Coalescer.Add
        ▼
internal/coalesce.Coalescer                         ← NEW · in-memory · clock-injected · mutex-guarded
        │  key = AM groupKey (else namespace+alertname); correlation_labels overrides to namespace+label values
        │  • debounce buffer  : fold same-key groups; flush when quiet for `debounce`,
        │                       OR `max_wait` since first, OR `max_batch` reached
        │  • cooldown / in-flight map : same key seen within `cooldown` ⇒ "still firing (N)", suppress
        │  • critical fast-path       : severity=critical ⇒ flush now, still seed the cooldown key
        ▼  flush ⇒ one coalesced Incident (siblings summarized in the seed context) ⇒ enqueuer.Enqueue
        │
        ▼
investigate.go:Queue.process
        │  (2) rate-limit gate BEFORE Investigate():
        │      • over max_per_window starts/window → wq.AddRateLimited(key) backoff (drains as the window rolls)
        │      • a key drops only after max_requeues (AM repeat_interval re-fires it) + ONE throttle notice/window
        ▼
loop.go:Investigate
        │  (3a) max_steps from config            (was hardcoded 20)
        │  (3b) truncate each tool result to max_tool_output_bytes BEFORE appending to history (head+tail+marker)
        │  (3c) estimate history tokens (len/4); over max_tokens_per_investigation ⇒ nudge submit_findings (graceful)
        │  (·)  Model.Complete already retries 3× via httpx.DoWithRetry (429/5xx)
        ▼
   findings → curator   (unchanged)
```

`trigger.Deduper` is **kept** as a first-pass exact-fingerprint filter; the Coalescer adds correlation-level folding on top of it.

### 3.1 Layer 1 — Coalescer (`internal/coalesce`, NEW)

A small, self-contained package. One `Coalescer` struct, mutex-guarded, with an injected `clock` (so tests advance time deterministically — no wall-clock reads).

**State**

```go
type batch struct { incidents []config.Incident; firstSeen, lastSeen time.Time }
type Coalescer struct {
    cfg     Config
    now     func() time.Time        // injected (house style; matches trigger.Deduper) — no Clock interface
    out     func([]config.Incident) // flush sink: build one Request + enqueuer.Enqueue (Decide already ran at ingress)
    notify  func(key string, n int) // chat "still firing (N)" suppression notice; may be nil
    mu      sync.Mutex
    pending map[string]*batch       // key → accumulating batch
    recent  map[string]time.Time    // key → last flush time (cooldown + in-flight)
}
```

**`key(inc)`** — when `cfg.CorrelationLabels` is set: `namespace + "/" + join(inc.Labels[l] for l in CorrelationLabels)`. Otherwise the AM group identity `inc.GroupKey`, falling back to `namespace + "/" + inc.AlertName` when empty. Requires threading `GroupKey` from `ParseAlertmanager` onto `config.Incident` (see §5).

**`Add(inc config.Incident)`** (called per surviving incident from the webhook handler):
1. `k := key(inc)`.
2. **Critical fast-path** — if `inc.Severity == "critical"`: flush immediately (this incident plus any already-pending siblings for `k`), set `recent[k]=now`. Bypasses the debounce but seeds the cooldown so later siblings attach.
3. **Cooldown / in-flight** — else if `recent[k]` is within `cfg.Cooldown`: increment a per-key suppressed counter, emit a throttled "`<alertname>` still firing (N), suppressed" note, return. No enqueue.
4. **Buffer** — else append to `pending[k]` (creating it + stamping `firstSeen` if absent), set `lastSeen=now`. If `len(pending[k].incidents) ≥ cfg.MaxBatch` → flush now.

To avoid re-entrancy, `Add`/`sweep` compute the batch to flush *under* the lock, then call `out(...)` *after* unlocking.

**`Run(ctx, tick)`** — a single background sweeper: each tick, `sweep()` flushes every pending batch where `now-lastSeen ≥ debounce` **OR** `now-firstSeen ≥ maxWait`. Deterministic under the injected `now` (tests call `sweep()` directly after advancing the clock).

**flush** — build ONE coalesced `config.Incident` from the batch (representative incident + a `Message` summary, "N correlated alerts, key=`k`: …"), call `out(batch.incidents)`, set `recent[k]=now`, delete `pending[k]`.

**Why this shape:** the loop, the queue, and the model layer don't change at all — coalescing is purely an ingress concern, isolated behind one `Add(Incident)` call and one `out` callback. Fully unit-testable without a cluster or an LLM.

### 3.2 Layer 2 — Safety rate limiter

A new `internal/ratelimit.Window{max, window, now}` records investigation-start timestamps over a sliding window and reports `Allow() bool` (slide → cap → record) and `Count() int`. It is the exact `action/auto.go:reserve()` shape; an **optional** follow-up task refactors `auto.go` onto `Window` to kill the duplication, sequenced *after* `Window` is tested and with `auto_test.go` re-run to prove no regression.

Gate inside `investigate.go:Queue.process`, immediately before `Investigate()`:

```
if starts != nil && !starts.Allow() {              // over the per-window start budget
    if wq.NumRequeues(k) >= maxRequeues {           // bounded: don't requeue a key forever
        notifyThrottleOncePerWindow(); wq.Forget(k); return   // drop; Alertmanager repeat_interval re-delivers
    }
    wq.AddRateLimited(k); notifyThrottleOncePerWindow(); return   // exp. backoff; drains as the window rolls
}
// budget OK → Investigate
```

- **Bounded spend:** at most `max_per_window` investigations start per `window`, regardless of alert volume or key cardinality — the core safety guarantee.
- **Overflow never silently lost:** `AddRateLimited` re-queues with client-go's exponential backoff (self-limiting — no hot loop); a key drops only after `max_requeues`, and AM's `repeat_interval` re-fires it.
- **Throttle visibility:** one chat notice per window when throttling engages.
- **Provider 429/5xx:** the model clients already retry 3× via `httpx.DoWithRetry`; a global breaker on *sustained* throttling is phase-2.
- **State:** in-memory on the leader (D7); resets on failover, acceptable for cost control.

### 3.3 Layer 3 — Per-investigation token efficiency

All in `loop.go:Investigate`:

- **3a `max_steps`** — read from config where the hardcoded `20` default lives (`loop.go` ~L122-128). Default stays 20; now tunable down for cost-sensitive deploys.
- **3b `max_tool_output_bytes`** — in the path that turns a tool result into a message (after execution, before append to `messages`), cap each result: if over budget, keep first `H` + last `T` bytes joined by `\n…[truncated N bytes]…\n`. This is the **single highest-ROI lever** — tool outputs (k8s describe/events, CloudTrail, logs) are the worst offenders *and* are re-sent on every subsequent step.
- **3c `max_tokens_per_investigation`** — accumulate a running token estimate: prefer the provider-reported usage (Anthropic `usage`, Gemini `usageMetadata`) when present, else `len(content)/4`. When cumulative exceeds the budget, inject a "token budget reached — submit your findings now with what you have" nudge and force a final `submit_findings` step. **Graceful conclude, not hard abort** — we keep the partial investigation's value.

### 3.4 Layer 4 — Observability (OpenTelemetry)

The feature is a cost/throughput control, so it must be *measurable* — otherwise you can't tune the windows or prove the savings. RunLore has no self-instrumentation today (no OTel, no `/metrics`), so this adds the first telemetry foundation, kept minimal and **inert by default**.

- **Instrument with the OTel metric API** (`go.opentelemetry.io/otel`), exported via the **Prometheus exporter** (`go.opentelemetry.io/otel/exporters/prometheus`) on a new `GET /metrics` route on the existing server mux. This fits the platform's scrape model (VictoriaMetrics + `VMServiceScrape`) better than OTLP push, while keeping vendor-neutral instrumentation. OTLP push is a config option deferred to phase-2.
- Because OTel instruments are safe to call under a no-op provider, the instrumentation code is **unconditional** — no nil-checks; metrics simply do nothing until `telemetry.metrics_enabled` wires the exporter.
- A small `internal/telemetry` package builds the meter provider + exporter and a `Metrics` struct of instruments, passed to the Coalescer, Queue, and LoopInvestigator.

Instruments (all prefixed `runlore_`):

| Metric | Type | Meaning |
|---|---|---|
| `alerts_received_total` | counter | incidents passing `Decide` into the coalescer |
| `alerts_coalesced_total` | counter | incidents folded into an existing batch (no own investigation) |
| `alerts_suppressed_total{reason="cooldown"}` | counter | incidents dropped by cooldown |
| `investigations_started_total` | counter | investigations actually begun |
| `investigations_throttled_total` | counter | starts requeued by the rate limiter |
| `investigations_dropped_total` | counter | keys dropped after `max_requeues` |
| `coalesce_batch_size` | histogram | incidents per flushed batch |
| `investigation_tokens_estimated` | histogram | per-investigation token estimate |
| `tool_output_truncated_bytes_total` | counter | bytes elided by output truncation |
| `recall_hits_total{result="verified\|downgraded\|rejected"}` | counter | KB instant-recall short-circuits, by verify-pass result |
| `recall_tokens_saved_total` | counter | estimated tokens saved by recall short-circuits (0 LLM calls) |
| `recall_score` | histogram | BM25 score at the recall decision — tunes the (non-portable) `min_score` |

The headline proof metric falls out directly: `alerts_received_total − investigations_started_total` ≈ the redundant investigations (and their tokens) that coalescing eliminated. **Recall (KB cache) hits are deliberately labelled by verify result, not counted raw:** a bare KB-hit counter would reward a fast-but-wrong cache (recall's known symptom≠cause failure mode), so the KPI is *trustworthy* hits. High-cardinality detail (entry id, alert, score) stays in structured logs, never metric labels.

## 4. Configuration

New `Investigation` sub-struct in `internal/config/config.go`, alongside the existing `Triggers` / `Actions`:

```yaml
investigation:
  coalesce:
    enabled: true
    debounce: 30s          # quiet period before flushing a batch
    max_wait: 2m           # hard cap from first alert to flush
    max_batch: 50          # flush immediately at this many alerts
    cooldown: 10m          # suppress re-investigation of the same key
    correlation_labels: [] # [] ⇒ use Alertmanager groupLabels
  rate_limit:
    max_per_window: 20     # soft budget: investigation starts per window
    window: 1h
    max_requeues: 10       # drop a key after this many backoff requeues (AM repeat re-fires it)
  max_concurrent: 1        # discouraged to raise; preserves serial default
  max_steps: 20
  max_tool_output_bytes: 16384
  max_tokens_per_investigation: 120000

telemetry:
  metrics_enabled: true    # expose OpenTelemetry metrics on GET /metrics (Prometheus exposition)
  # otlp_endpoint: ""      # optional OTLP push target (phase-2); empty ⇒ scrape-only via /metrics
```

Defaults are chosen so the feature is **safe-on**: coalescing enabled, generous budgets that only bite during genuine storms. All exposed through the Helm chart `deploy/helm/runlore/values.yaml` (the dedup window already lives there; this extends the same block).

## 5. Code seams (where each change lands)

| Change | Location |
|---|---|
| Thread AM `GroupKey` onto `config.Incident` | `internal/config/config.go` (`Incident`), `internal/trigger/incident.go` (`ParseAlertmanager`) |
| Per-incident `Decide`, then `Coalescer.Add` (replaces per-alert `Enqueue`); construct + `Run` the Coalescer | `internal/server/server.go` (`handleAlertmanager` + the server constructor) |
| New coalescer package | `internal/coalesce/` (`coalescer.go`, `coalescer_test.go`) |
| New windowed limiter (optional later: refactor `auto.go` onto it) | `internal/ratelimit/window.go` |
| Rate-limit gate (start cap + `max_requeues` drop) | `internal/investigate/investigate.go:Queue.process` |
| `max_steps`, tool-output truncation, token-budget nudge | `internal/investigate/loop.go:Investigate` |
| OTel meter + Prometheus exporter on `GET /metrics`; instrument coalescer/limiter/loop | `internal/telemetry/`, `internal/server/server.go` (mux) |
| 429/5xx already retried 3× by `httpx.DoWithRetry` — no v1 change | `internal/model/*` |
| `Investigation` + `Telemetry` config + Helm values + VMServiceScrape | `internal/config/config.go`, `deploy/helm/runlore/` |

## 6. Trade-offs accepted in v1

- **Failover loses in-memory state** → pending batches dropped, budget refills. Mitigated by AM `repeat_interval` re-sending unresolved alerts. Phase-2 persists (Redis/leader-lease annotation). Matches the existing in-memory-on-leader caveat already flagged for the action limiter.
- **Debounce adds ≤ `debounce` (30s) latency-to-first-investigation** — fine on SRE timescales, and the `critical` fast-path (D6) bypasses it for pages that matter.
- **Over-coalescing** if the key is too coarse → two unrelated issues fold into one investigation. Bounded by keying on AM `groupKey` (the group's own identity) + `max_batch`; tune `correlation_labels` to widen/narrow. The `runlore_coalesce_batch_size` histogram surfaces this in practice.
- **Cooldown can mask a genuinely new root cause** that shares a correlation key during the window. Mitigated by the visible "still firing (N)" chat note; a materially changed alert-set can be allowed to break cooldown (phase-2 refinement).

## 7. Testing

- **`coalesce` unit tests** (injected clock): intra-group collapse, cross-POST fold within `debounce`, `max_wait` cap, `max_batch` flush, cooldown suppression + counter, critical fast-path, in-flight attach.
- **`ratelimit` unit tests** mirroring `auto_test.go`: window slide/reset, `Allow` caps at `max`, `Count` peek. Queue-gate test: over-budget → `AddRateLimited`, drop after `max_requeues`, notify-once-per-window.
- **`loop` unit tests**: truncation marker + head/tail retention, `max_steps` honored from config, token-budget nudge forces `submit_findings`.
- **`telemetry` test**: `/metrics` serves Prometheus exposition with the `runlore_*` series; instruments increment on coalesce/suppress/throttle.
- **Integration**: extend `hack/e2e-k3d.sh` (mock model) with a synthetic 40-alert storm → assert **one** investigation runs and the suppressed counter reports the rest.

## 8. Out of scope / phase 2

- Persisting coalescer + limiter state across failover.
- History compaction / summarization of old steps within a single investigation (beyond truncation).
- Cheap-model triage → pro-model escalation (model tiering).
- Letting a materially-changed alert set break an active cooldown.
- Per-namespace / per-severity budget partitioning.
- Plumbing provider-reported token usage (Anthropic `usage`, Gemini `usageMetadata`) through `CompletionResponse` for exact budgeting.
- A hard global circuit-breaker / investigation kill-switch (beyond the soft window cap).
- OTLP *push* export + distributed traces (v1 ships scrape-only metrics via the Prometheus exporter).
- Joining a recall (KB cache) hit to its *real* outcome — did the cached answer resolve the incident — vs. the v1 verify-pass-result proxy. This is the broader learning-loop "close the outcome loop" work from the project review.

These are deliberately deferred; v1 is the three-layer mechanism (+ observability) with safe-on defaults.
