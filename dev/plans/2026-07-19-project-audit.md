# Project audit — code quality · performance · security (2026-07-19)

Full-project audit at v0.9.2 (main `69d53c3`), focused on the four core features: the
learning loop, instant recall, the OKF knowledge base, and the pluggable system. Five
parallel read-only deep-dives (learning loop, recall/KB, plugin system, security,
performance), then **every roadmap-bound claim was re-verified against the code**
(file:line) before being written down here. Companion docs:

- Roadmap: `2026-07-19-audit-roadmap.md` (Now / Next / Later)
- Implementation plans: `dev/superpowers/plans/2026-07-19-*.md`

## Headline

The code is not the problem — the **lifecycle of what the agent learns** is. The gate
suite is green (build / vet / gofmt / golangci-lint 0 issues, all tests pass, 79.9%
coverage), security holds against fully-attacker-controlled model output, and the hot
ingestion path is well-engineered. What's missing sits above the code: knowledge is
validated but never retired, the index doesn't scale with the KB's own success, and
pluggability is Go-only in practice. The unifying risk: **RunLore's success is its main
technical exposure** — every merged KB PR triggers a full-corpus re-embed, every open PR
slows curator dedup, and nothing ever prunes bad knowledge.

## Code quality

- Coverage 79.9% total. Weak spots are wiring, not logic: `internal/app` 41.2%,
  `cmd/lore` 38.5%, `internal/source/gitops` 53.8%, `internal/server` 69.4%,
  `internal/executor/flux` 72.2%. Everything else ≥ 80%.
- `internal/outcome/ledger.go` (~1,075 lines) carries **two independent implementations
  of the same open→resolve LIFO pairing** — the incremental cache
  (`applyOpenLocked`/`applyResolveLocked`, `ledger.go:575-642`) and the full replay in
  `Episodes()` (`ledger.go:909-944`), documented to diverge above
  `maxPendingResolvesPerFingerprint`. Guarded only by tests (the 63KB test file); the
  loop's biggest consistency risk on future edits. No action planned — don't churn a
  well-tested file — but treat any edit there as high-risk.
- **Zero `testing.B` benchmarks in the repo.** None of the performance properties below
  have regression guardrails.
- The docs are unusually honest and match the code — with one drift: `docs/design.md`
  §Learn promises entries carry `status`, `confidence`, `last_validated`; none are
  parsed by `internal/catalog/load.go` (roadmap item N6).

## Security

No critical or high findings. Everything claimed in `docs/security-model.md` was
verified faithful in code; no doc overstates a protection. Verified solid: the
server-authoritative action gate (`deriveSafety` discards model-authored safety fields;
policy re-checked at both exec boundaries; approve claims under mutex — no TOCTOU),
hash-chained `0600` fsync'd audit with verify-on-open, three-boundary redaction incl.
the REDACT-B64 payload-wide scrub, SSRF redirect guard + cross-host credential
stripping, Slack HMAC ±5min, Matrix `/whoami` attribution anchor, fail-closed config
(`approve`/`auto` requirements; cleartext-key rejection). CSRF on approval endpoints is
effectively closed by design: auth is a custom `X-Approval-Token` header, no cookies
exist to forge.

Remaining, medium:

- **No default investigation rate limit** — `MaxPerWindow > 0` gates the limiter
  (`internal/app/serve.go:186`) and no default is applied (`internal/config/load.go:91`),
  so out of the box the count of investigations is unbounded (per-incident cost is
  capped, count is not). Token-holding callers, a misconfigured Alertmanager, or one
  large batched payload can run up the model bill. → roadmap N1.
- **No brute-force throttling on auth endpoints** — token compares are constant-time
  (`internal/server/server.go:191-196,442-452`) but guessing is unlimited. → roadmap N1.
- Webhook fail-open only exists when **no model is configured** — `RequireWebhookAuth`
  refuses to start otherwise (`internal/app/config.go:59-65`) and the no-model case
  warns loudly and is a documented deliberate default; the NetworkPolicy half is already
  owned by deferred item S3 (`2026-07-11-v0.9.0-remaining-roadmap.md`). Not re-raised.

Low / informational (documented, not roadmap-bound): PagerDuty HMAC lacks a freshness
window (replay re-triggers a deduped investigation — bounded); `observedResources`
falls back to name-only matching across allowlisted namespaces
(`internal/investigate/observedresources.go:66-79`, code-commented tradeoff — the doc
should state it); `/metrics` unauthenticated (standard posture; document); listener is
plain HTTP by design (TLS at ingress — document); prompt-injection defense is the
action gate, not prompt hygiene (accepted limitation, `docs/security-architecture.md` §7).

## Performance

Already well-engineered (keep): the ingestion path (dedup → debounce → coalesce →
single rate-limited worker), the ledger's O(1) incremental aggregates + atomic
compaction, Anthropic prompt caching (correct ≤4-breakpoint shape; note compaction
resets the message-tier cache once per compaction event — correct, just not free),
bounded parallel tool execution (cap 4, redact-before-truncate), git-sync fast-forward
with HEAD-gated re-index, disk (not memory) clones.

Three structural costs grow with KB/repo size, not alert rate:

1. **Full-corpus re-embed on every KB HEAD move.** `ReloadContext` re-embeds *all*
   entries whenever git-sync sees a new HEAD (`internal/catalog/catalog.go:81-86` →
   `hybrid.go:44-53`); RunLore merges its own PRs, so this is self-reinforcing. The
   batch is a **single unbounded request** (`internal/embed/embed.go:62-66` — the whole
   corpus in one `Input`), and an embed failure is **silently discarded** (the `verr`
   branch drops vectors without even a log), degrading hybrid recall to BM25-only with
   no signal. → roadmap N2.
2. **Full-history clone per `what_changed` call.** `git.PlainCloneContext` with no
   `Depth` (`internal/whatchanged/differ.go:170`; `NoCheckout` shipped as G1 — worktree
   skipped, object store not); the clone cache is request-scoped by design
   (`clonecache.go:13`). Dominant per-investigation latency on large repos; the code
   comments the TODO itself (`differ.go:216`). A naive shallow clone breaks
   `revisionsInWindow`/`lastPathChange` history walks — the fix is a persistent bare
   mirror + incremental fetch. → roadmap N7.
3. Deduper evicts by scanning the whole map on every ingested event under the ingest
   mutex (`internal/trigger/dedup.go:34-38`). **Demoted on double-check**: the map is
   bounded by the window and the scan is microseconds at realistic storm rates. Fold an
   amortized sweep into other work if convenient; not roadmap-worthy alone.

Minor (not roadmap-bound): resolve-path fsync-per-line inline in the webhook handler
(`internal/outcome/ledger.go:725-741` — batch on mass-resolve someday); cluster-wide
informers cache full objects with no `TransformFunc` (`flux/dynamic.go:264`); audit
chain replay is O(all actions ever) at startup by design (fine for approve-rung usage);
ledger startup replay is capped by compaction unless `max_events: 0`.

## Core features

### Learning loop — closes, but nothing retires bad knowledge

The loop genuinely closes: outcome + feedback → `outcomeFactor` (Beta posterior,
`internal/investigate/recall.go:495`) → recall confidence → future behavior; verified in
eval. The ledger is high-craft (see quality). Gaps:

- **No retirement of merged entries.** `curate.Lifecycle` closes stale *unmerged PRs*
  only (`internal/curate/lifecycle.go:27-56`); no path prunes/supersedes a *merged*
  entry whose outcome decayed below floor. Validation exists; garbage collection
  doesn't. → roadmap N4.
- **Decayed winner blocks recall with no runner-up.** The outcome gate evaluates only
  the single top structurally-agreeing candidate (`recall.go:239-252`); a `low_outcome`
  rejection abandons recall entirely even when a healthy corrected entry is ranked
  second — and the near-miss lead injected into the seed doesn't exclude the rejected
  entry (only the verify-rejection path does, `recall.go:281`). → roadmap N3.
- **A standing 👎 can lock an entry into a permanent re-investigation loop.** Fast
  single-👎 decay is deliberate and documented (`recall.go:476-494`) and 👎 correctly
  bypasses the recurrence cooldown — but when the forced re-investigation reaches the
  *same* RCA, curator dedup turns it into a comment instead of a new entry
  (`internal/curator/curator.go:82-100`), so nothing ever supersedes or vindicates the
  entry: every recurrence re-investigates at full cost until a human intervenes. The
  gap is a **recovery path** (a confirming re-investigation is evidence), not a vote
  quorum — a quorum would trade safety for cost. → roadmap N5.
- For non-resolvable sources (GitOps failures, Alertmanager without `send_resolved`),
  trust moves only via sparse manual votes — known, documented, no better signal
  available; not roadmap-bound.
- Resolve credit is temporal-correlational (alert cleared ≠ recalled fix worked);
  `resolvesSince` already blocks the worst false-credit cases. Accepted limitation.

### Instant recall — reranker is load-bearing; hybrid is unmeasured

- Defense-in-depth gating is strong (structural pre-filter → fire gate → outcome decay
  → live-state confirm → adversarial verify; disabled under `auto`), and the retrieval
  eval is honest: at production thresholds **BM25-only recall fires 0/11** — the
  default-on LLM reranker is what makes recall work.
- Hybrid (cosine-gated) recall is EXPERIMENTAL with defaults the config itself calls
  "conservative placeholders, not measured values"
  (`internal/config/config.go:422-426`); the eval never exercises real hybrid. Enabling
  it today buys unmeasured quality plus the N2 re-embed cost. → roadmap N8.
- Brute-force O(n) cosine per query (`hybrid.go:87-93`) — fine at hundreds of entries,
  a ceiling at thousands. Later-horizon (ANN/persisted vectors), after N2.

### OKF knowledge base — consumable, but no staleness signal

- Genuinely open on the read side: plain markdown + frontmatter, foreign-bundle-tolerant
  validation (`internal/kbvalidate/kbvalidate.go:62-83`), and MCP `kb_search`/`kb_get`
  make it consumable without RunLore. Write-side identity (`fingerprint`,
  `alert_resource`) is RunLore-specific — acceptable.
- **No time dimension anywhere**: `status`/`confidence`/`last_validated` are promised in
  `docs/design.md` §Learn but never parsed; `timestamp` is parsed and unused. A drifted
  runbook that still "resolves" looks as fresh as yesterday's. → roadmap N6.
- `okf_version` appears only in an example file the loader skips; no version negotiation.
  Fold into N6.

### Pluggable system — excellent interfaces, Go-only in practice

- The `internal/providers/providers.go` contract file and the
  optional-capability-via-type-assertion pattern (`GitOpsInspector`, `LogStats`,
  `ProgressNotifier`, …) are genuinely good design; sources and notifiers have clean
  self-registration registries with fail-loud unknown-key detection.
- In practice only the **investigation toolbox** is extensible without recompiling
  (outbound MCP client, tools only). Sources, notifiers, metrics/logs/cloud/model
  providers, embedders, forges, and executors all require Go. MCP widens what the agent
  can *see*, not what it can *do*, where alerts come from, or where results go.
- **MCP "read-only" is prompt-enforced only.** The client itself declines to retry
  because "remote MCP tools may have side effects" (`internal/mcp/client.go:95`), yet
  there is no per-server tool allowlist and no approval gate for remote calls. → roadmap N9.
- `BuildModelAndTools` (`internal/app/investigate.go`) hardcodes the provider matrix
  (~210 lines) — known, also the registration point named by the v0.9.0 roadmap. A
  registry refactor is only worth it alongside the config-only-providers work (Later).

## Double-check log (claims dropped or reframed)

Per maintainer request every proposal was adversarially re-verified; three changed:

1. "Webhook fail-open by default" — **overstated**: fail-closed whenever a model is
   configured (`app/config.go:59-65`); the residual is deliberate, warned, and owned by
   deferred S3. Dropped; N1 keeps only rate-cap + payload cap + auth backoff.
2. "Feedback needs a statistical quorum" — **reframed**: single-👎 fast decay is
   documented design and fail-safe toward cost, not wrongness; the real gap is the
   dedup-blocked recovery path (N5).
3. "Deduper O(n) eviction" — **demoted**: bounded map, microsecond scans at realistic
   rates; not worth a PR on its own.
