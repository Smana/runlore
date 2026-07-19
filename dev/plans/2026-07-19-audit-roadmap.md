# Audit roadmap — Now / Next / Later (2026-07-19)

Derived from `2026-07-19-project-audit.md` (all claims code-verified there; evidence
file:lines live in the audit doc). Selection rule: an item makes this list only if it
adds something missing or creates real user value — no gold-plating. Each N-item has an
implementation plan under `dev/superpowers/plans/2026-07-19-*.md`, sized for one
worktree/PR each, TDD, full CI gate suite per repo convention.

Theme: **make the learning loop safe to succeed** — retire bad knowledge, scale the
index with the KB, cap the bill — before widening integrations.

## Now — quick wins (days each)

| # | Item | Value | Plan |
|---|---|---|---|
| N1 | **Cost-DoS guards**: default `investigation.rate_limit` cap (generous, disable-able; CHANGELOG behavior-change note) + cap alerts-per-payload in the Alertmanager decoder + failed-auth backoff on control/webhook endpoints | Bounds the model bill and closes both audit MEDIUMs; the last security gap an evaluator will probe | `2026-07-19-cost-dos-guards.md` |
| N2 | **Embedding cache + chunked batches**: cache vectors by content hash (skip unchanged entries on reload), chunk the embed request, and log/WARN + metric when a reload drops vectors (today the error is silently discarded) | Turns "1 merged PR = re-embed everything" into "re-embed 1"; fixes a silent recall-quality regression; prerequisite for hybrid (N8) | `2026-07-19-embed-cache-chunking.md` |
| N3 | **Recall runner-up fallback**: on a `low_outcome` rejection, evaluate the next structurally-agreeing candidate before abandoning recall; exclude the rejected entry from the near-miss lead (as the verify-rejection path already does) | A decayed/poisoned winner no longer disables instant recall while a healthy corrected entry sits at rank 2 — stops paying full investigations indefinitely | `2026-07-19-recall-runner-up.md` |

## Next — the heart of the roadmap (the learning-loop lifecycle)

| # | Item | Value | Plan |
|---|---|---|---|
| N4 | **KB retirement**: a `curate` pass that opens a retire/supersede PR when a *merged* entry sits below the outcome floor (Lifecycle today only closes stale unmerged PRs; the decay signal already exists — this connects them, human-reviewed, on-brand) | Closes the missing half of the loop: validation exists, garbage collection doesn't. Answers "what happens when it learns something wrong?" | `2026-07-19-kb-retirement.md` |
| N5 | **👎 recovery path**: when the re-investigation forced by a standing 👎 reaches the *same* fingerprint, record it as confirming evidence (and surface it on the contested entry) instead of only a dedup comment | Ends the permanent full-cost re-investigation loop a single mistaken 👎 creates today; keeps the fail-safe bias (no vote quorum — safety over cost) | `2026-07-19-downvote-recovery.md` |
| N6 | **OKF staleness**: parse `status` / `last_validated` (+ accept `okf_version`) from frontmatter as `docs/design.md` promises; age-aware down-weighting in recall; curator stamps `last_validated` on confirmations | Adds the missing time dimension — a drifted runbook that still "resolves" stops looking as fresh as yesterday's; closes the design-doc drift | `2026-07-19-okf-staleness.md` |
| N7 | **Persistent git mirror for `whatchanged`**: per-repo bare mirror + incremental fetch shared across investigations (replaces full-history clone-per-call; shallow clones would break the history walks) | Biggest per-investigation latency win on real repos; the code already TODOs it | `2026-07-19-whatchanged-git-mirror.md` |
| N8 | **Hybrid recall eval**: extend the retrieval eval to real embedding-backed hybrid, measure the cosine thresholds (currently self-described "placeholders"), and graduate hybrid from EXPERIMENTAL — or document why not | The eval already honestly shows BM25-only fires 0/11 and the reranker is load-bearing; hybrid is the designated successor and is currently unmeasured. Depends on N2 | `2026-07-19-hybrid-recall-eval.md` |
| N9 | **MCP remote-tool allowlist**: opt-in per-server `tools:` allowlist (+ deny-by-default option), making "read-only extension layer" enforceable rather than prompt-promised | Security-conscious operators can safely connect third-party MCP servers — unlocks the extension story credibly | `2026-07-19-mcp-tool-allowlist.md` |

## Later — strategic, when adoption pulls (roadmap only, no plans yet)

- **Config-only sources & notifiers** — the biggest adoption limiter (every new alert
  source or channel needs Go). The registries exist; a generic *templated* webhook
  source + notifier covers most vendors cheaply before per-vendor work. Unify
  Slack/Matrix into the notify registry while there (two mechanisms for one concept
  today). Builds on `2026-06-27-extensible-sources-notifiers-design.md`.
- **Multi-KB federation** (org + team + vendor bundles with precedence) — wait for
  multi-team pull.
- **Persisted vectors / ANN / incremental indexing** — only when real KBs pass ~1-2k
  entries; N2 buys the headroom first.
- **A handful of `testing.B` benchmarks** on the paths above (embed reload, recall
  query, ledger replay, clone/fetch) so the perf work has regression guardrails.

## Deliberately not on the roadmap

Ledger-file split (well-tested; don't churn), deduper eviction (microseconds; fold in
opportunistically), `/metrics` auth + TLS posture + name-only `observedResources`
fallback (one-line doc fixes — fold into the next docs pass), PagerDuty replay window
(bounded by dedup), informer `TransformFunc` + resolve-path fsync batching (real but
scale-dependent; revisit with benchmarks). Webhook fail-open in non-model setups stays
with deferred item S3 (`2026-07-11-v0.9.0-remaining-roadmap.md`) — maintainer policy
decision.
