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
| D2 | **Correlation key = Alertmanager group + temporal window** over `namespace + correlation_labels` (default = AM `groupLabels`) | Reuses already-tuned AM grouping; namespace + groupLabels avoids over-coalescing two unrelated issues into one muddy investigation. |
| D3 | **Coalescer = "C + A"**: stop exploding the AM group (free), *plus* an ingress debounce buffer that folds separate POSTs sharing the key | C kills the canonical one-group storm immediately; A catches "node-down fires several groups across POSTs." Buffer is a small clock-injected unit; the investigation loop is untouched. |
| D4 | **Safety limiter mirrors `action/auto.go:reserve()`** — windowed counter on investigation *starts*; soft over-budget → requeue with backoff; hard `circuit_breaker_max` → pause window + one chat alert | Don't invent a new mechanism; never silently drop a soft-limited alert; bounded blast radius for runaway/feedback loops. |
| D5 | **Token efficiency = configurable `max_steps` + `max_tool_output_bytes` truncation + `max_tokens_per_investigation` with graceful conclude** | Tool-output truncation is highest ROI (history re-sent every step). Graceful conclude (submit findings with what you have) beats a hard abort that wastes the whole run. |
| D6 | **`critical`-severity fast-path in v1** | A debounce must never delay the first look at a critical page: flush immediately, but still register the key so later siblings attach instead of spawning new runs. |
| D7 | **State is in-memory on the leader for v1**; persistence deferred to phase 2 | This is cost protection, not mutation safety. AM `repeat_interval` re-sends unresolved alerts after a failover, so dropped pending alerts self-heal. Matches the known action-limiter caveat in the wiki. |
| D8 | **Concurrency stays at 1 by default** (`max_concurrent` knob exists but is discouraged) | Coalescing reduces the need to parallelize; raising concurrency trades the safety ceiling for drain speed. Default preserves today's behavior. |

## 3. Architecture

```
Alertmanager POST  (one group, .alerts[] + groupKey/groupLabels)
        │
        ▼
server.go:handleAlertmanager
        │  (1a) build ONE Incident per group (not per alert); thread groupKey/groupLabels through
        │  engine.Decide(group): trigger policy + dedup — only investigate-worthy groups proceed
        ▼
internal/coalesce.Coalescer                         ← NEW · in-memory · clock-injected · mutex-guarded
        │  key = namespace + correlation_labels      (correlation_labels empty ⇒ use AM groupLabels)
        │  • debounce buffer  : fold same-key groups; flush when quiet for `debounce`,
        │                       OR `max_wait` since first, OR `max_batch` reached
        │  • cooldown / in-flight map : same key seen within `cooldown` ⇒ "still firing (N)", suppress
        │  • critical fast-path       : severity=critical ⇒ flush now, still seed the cooldown key
        ▼  flush ⇒ one coalesced Incident (siblings summarized in the seed context) ⇒ enqueuer.Enqueue
        │
        ▼
investigate.go:Queue.process
        │  (2) rate-limit gate BEFORE Investigate():
        │      • soft  max_per_window      → over budget: wq.AddAfter(key, backoff)  (drains later, dedups while waiting)
        │      • hard  circuit_breaker_max → pause for the rest of the window + ONE chat alert ("storm — paused N")
        ▼
loop.go:Investigate
        │  (3a) max_steps from config            (was hardcoded 20)
        │  (3b) truncate each tool result to max_tool_output_bytes BEFORE appending to history (head+tail+marker)
        │  (3c) accumulate token estimate; over max_tokens_per_investigation ⇒ force graceful conclude
        │  (·)  Model.Complete wrapped with shared 429/5xx exponential backoff + jitter
        ▼
   findings → curator   (unchanged)
```

`trigger.Deduper` is **kept** as a first-pass exact-fingerprint filter; the Coalescer adds correlation-level folding on top of it.

### 3.1 Layer 1 — Coalescer (`internal/coalesce`, NEW)

A small, self-contained package. One `Coalescer` struct, mutex-guarded, with an injected `clock` (so tests advance time deterministically — no wall-clock reads).

**State**

```go
type batch struct { alerts []Alert; firstSeen, lastSeen time.Time }
type Coalescer struct {
    cfg     CoalesceConfig
    clock   Clock
    out     func(Incident)          // = enqueuer.Enqueue  (Decide already ran at ingress)
    notify  func(string)            // chat "still firing (N)" / suppression notices
    mu      sync.Mutex
    pending map[string]*batch       // key → accumulating batch
    recent  map[string]time.Time    // key → last flush time (cooldown + in-flight)
}
```

**`key(inc)`** = `namespace + "/" + join(sorted(values for cfg.CorrelationLabels))`; when `CorrelationLabels` is empty, fall back to the alert's AM `groupLabels` (requires threading `groupKey`/`groupLabels` from `trigger.ParseAlertmanager` into the `Incident` — see §5).

**`Add(inc)`** (called per group from the webhook handler):
1. `k := key(inc)`.
2. **Critical fast-path** — if any alert in the group is `severity=critical`: `flush(k, inc.alerts)` immediately, set `recent[k]=now`. (Bypasses debounce but seeds cooldown so later siblings attach.)
3. **Cooldown / in-flight** — else if `recent[k]` within `cfg.Cooldown`: increment a per-key suppressed counter, emit a throttled "`<alertname>` still firing (N), suppressed" chat note, return. No enqueue.
4. **Buffer** — else append alerts to `pending[k]` (creating it, stamping `firstSeen`, if absent), update `lastSeen=now`. If `len(pending[k].alerts) ≥ cfg.MaxBatch` → `flush(k, …)`.

**`run(ctx)`** — a single background sweeper (ticks ~`debounce/2`): for each pending batch, `flush` when `now-lastSeen ≥ debounce` **OR** `now-firstSeen ≥ maxWait`. One goroutine, deterministic under a fake clock (tick = advance + sweep).

**`flush(k, alerts)`** — build one coalesced `Incident` whose seed context summarizes all member alerts ("N correlated alerts, key=`k`: …"), call `out(inc)`, set `recent[k]=now`, delete `pending[k]`.

**Why this shape:** the loop, the queue, and the model layer don't change at all — coalescing is purely an ingress concern, isolated behind one `Add(Incident)` call and one `out` callback. Fully unit-testable without a cluster or an LLM.

### 3.2 Layer 2 — Safety rate limiter

Extract `action/auto.go:reserve()`'s windowed-timestamp logic into a reusable `internal/ratelimit.Window{Max, Window}` (a targeted refactor: `auto.go` and the new investigation gate both use it, killing the duplication). The `Window` records start timestamps and reports `Allow(now) bool` + `Count(now) int`.

Gate inside `investigate.go:Queue.process`, immediately before `Investigate()` is invoked:

```
n := limiter.Count(now)
switch {
case n < cfg.MaxPerWindow:            limiter.Record(now); proceed
case n < cfg.CircuitBreakerMax:       wq.AddAfter(key, backoff)            // soft: requeue, drains as budget frees
default:                              notifyOncePerWindow("storm — paused"); drop  // hard: stop the hot loop
}
```

- **Soft over-budget** never drops: the client-go workqueue already coalesces by key and supports `AddAfter`, so duplicates collapse while they wait.
- **Hard breaker** stops new starts for the rest of the window (prevents a runaway/feedback loop from hammering forever) and posts exactly one chat alert.
- **Provider 429/5xx** — wrap `Model.Complete` (the `anthropic`/`gemini` clients) with capped exponential backoff + jitter; a shared "consecutive throttles" counter, above a threshold, makes `limiter.Allow` return false for a cool-off so we stop adding load while the provider is saturated.
- **State:** in-memory on the leader (D7). Resets on failover; acceptable for cost control.

### 3.3 Layer 3 — Per-investigation token efficiency

All in `loop.go:Investigate`:

- **3a `max_steps`** — read from config where the hardcoded `20` default lives (`loop.go` ~L122-128). Default stays 20; now tunable down for cost-sensitive deploys.
- **3b `max_tool_output_bytes`** — in the path that turns a tool result into a message (after execution, before append to `messages`), cap each result: if over budget, keep first `H` + last `T` bytes joined by `\n…[truncated N bytes]…\n`. This is the **single highest-ROI lever** — tool outputs (k8s describe/events, CloudTrail, logs) are the worst offenders *and* are re-sent on every subsequent step.
- **3c `max_tokens_per_investigation`** — accumulate a running token estimate: prefer the provider-reported usage (Anthropic `usage`, Gemini `usageMetadata`) when present, else `len(content)/4`. When cumulative exceeds the budget, inject a "token budget reached — submit your findings now with what you have" nudge and force a final `submit_findings` step. **Graceful conclude, not hard abort** — we keep the partial investigation's value.

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
    circuit_breaker_max: 40 # hard stop for the window
  max_concurrent: 1        # discouraged to raise; preserves serial default
  max_steps: 20
  max_tool_output_bytes: 16384
  max_tokens_per_investigation: 120000
```

Defaults are chosen so the feature is **safe-on**: coalescing enabled, generous budgets that only bite during genuine storms. All exposed through the Helm chart `deploy/helm/runlore/values.yaml` (the dedup window already lives there; this extends the same block).

## 5. Code seams (where each change lands)

| Change | Location |
|---|---|
| Thread AM `groupKey`/`groupLabels` into `Incident` | `internal/trigger/incident.go` (`ParseAlertmanager`) |
| One Incident per group; per-group `Decide`, then `Coalescer.Add` (replaces per-alert `Enqueue`) | `internal/server/server.go:handleAlertmanager` (~L326-349) |
| New coalescer package | `internal/coalesce/` (`coalescer.go`, `coalescer_test.go`) |
| Extract reusable windowed limiter | `internal/ratelimit/window.go` (from `internal/action/auto.go:reserve`) |
| Rate-limit gate + circuit breaker | `internal/investigate/investigate.go:Queue.process` |
| `max_steps`, tool-output truncation, token budget | `internal/investigate/loop.go:Investigate` |
| Shared 429/5xx backoff wrapper | around `Model.Complete` (`internal/model/{anthropic,gemini}` or a wrapper) |
| `Investigation` config sub-struct + Helm values | `internal/config/config.go`, `deploy/helm/runlore/values.yaml` |

## 6. Trade-offs accepted in v1

- **Failover loses in-memory state** → pending batches dropped, budget refills. Mitigated by AM `repeat_interval` re-sending unresolved alerts. Phase-2 persists (Redis/leader-lease annotation). Matches the existing in-memory-on-leader caveat already flagged for the action limiter.
- **Debounce adds ≤ `debounce` (30s) latency-to-first-investigation** — fine on SRE timescales, and the `critical` fast-path (D6) bypasses it for pages that matter.
- **Over-coalescing** if the key is too coarse → two unrelated issues fold into one investigation. Bounded by keying on AM groupLabels + namespace + `max_batch`; tune `correlation_labels` to widen/narrow.
- **Cooldown can mask a genuinely new root cause** that shares a correlation key during the window. Mitigated by the visible "still firing (N)" chat note; a materially changed alert-set can be allowed to break cooldown (phase-2 refinement).

## 7. Testing

- **`coalesce` unit tests** (injected clock): intra-group collapse, cross-POST fold within `debounce`, `max_wait` cap, `max_batch` flush, cooldown suppression + counter, critical fast-path, in-flight attach.
- **`ratelimit` unit tests** mirroring `auto_test.go`: window reset, soft-requeue, circuit-breaker trip, notify-once-per-window.
- **`loop` unit tests**: truncation marker + head/tail retention, `max_steps` honored from config, token-budget-triggered graceful `submit_findings`, 429 backoff path.
- **Integration**: extend `hack/e2e-k3d.sh` (mock model) with a synthetic 40-alert storm → assert **one** investigation runs and the suppressed counter reports the rest.

## 8. Out of scope / phase 2

- Persisting coalescer + limiter state across failover.
- History compaction / summarization of old steps within a single investigation (beyond truncation).
- Cheap-model triage → pro-model escalation (model tiering).
- Letting a materially-changed alert set break an active cooldown.
- Per-namespace / per-severity budget partitioning.

These are deliberately deferred; v1 is the three-layer mechanism with safe-on defaults.
