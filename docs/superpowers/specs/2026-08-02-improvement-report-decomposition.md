# Decomposition — RunLore improvement report (28 July 2026)

- **Date:** 2026-08-02
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Input:** `runlore-improvement-report.pdf` (9 pages, prepared 28 July 2026)

## Why this document exists

The report spans six independent workstreams — funnel, docs IA, proof assets, positioning,
integrations, and a strategic content bet. It is far too large for one spec. This page is the
index: what each sub-project owns, what it depends on, and the order they ship in. **Each
sub-project has its own spec, its own plan, and its own PR set.**

## Report claims verified against the repo (2026-08-02)

Three of the report's findings are stale or imprecise. They are corrected here so no spec
inherits a wrong premise.

| Report claim | Verified state | Effect |
|---|---|---|
| §9.1 README contradicts Getting Started on `helm repo add` / OCI | **Stale** — `README.md:216` already states "OCI artifact on GHCR — no `helm repo add`" | Dropped from S2 |
| §5.1 "logs support is VictoriaLogs only" | **Stale** — Loki shipped in `c744a8b` (`internal/logs/loki`) | S5 integration list shortened |
| §2.1 "the demo prints trigger-policy log lines" | **True of `hack/demo.sh`**, but `lore demo investigate` already renders a real verdict card — it just requires an API key. The keyless mock exists only in `internal/app/eval_compare_test.go` | S1 reframed: the gap is *keyless*, not *missing* |
| §9.3 eval scorecard link 404s | **Confirmed and worse** — `RUNLORE_EVAL_API_KEY` is unset, so every nightly run since the workflow landed took the graceful-skip path (26–32s "success"). The `eval-scorecard` branch **does not exist on the remote**; the README badge and the benchmarking link are both dead | S3 |
| §9.2 Flux/Argo CD listed as Required | **Confirmed** — `getting-started.md:28` vs README "GitOps isn't required" | S2 |
| §9.4 site-relative repo links | **Confirmed** — `getting-started.md:243`, `benchmarking.md:38` | S2 |
| §9.5 ~120-line `values.yaml` as the golden path | **Confirmed** — the annotated block runs `getting-started.md:247–425` | S2 |
| §6 a commons catalog gives "day-1 instant recall value" | **Wrong as the engine stands** — instant recall's structural filter rejects a resource-less entry against a workload-carrying incident (`recall_test.go:427`, `nearmiss_gate_test.go:131`). `kb_search` has no such filter, so commons entries *do* ground investigations | S6 reframed |

## Sub-projects

| # | Spec | Owns | Depends on |
|---|---|---|---|
| S1 | [`s1-funnel-demo-cli`](2026-08-02-s1-funnel-demo-cli-design.md) | Keyless recorded-transcript demo, zero-config `lore investigate`, `install.sh` | — |
| S2 | [`s2-getting-started-integrations`](2026-08-02-s2-getting-started-integrations-design.md) | Three-tier Getting Started, `/docs/integrations` section, values profiles, §9 bugs | S1 (tier 1 is S1's demo) |
| S3 | [`s3-proof-assets`](2026-08-02-s3-proof-assets-design.md) | Eval scorecard resurrection + `/eval` page, cost table, recall caveats, ADOPTERS, OpenSSF | — (needs a repo secret from the owner) |
| S4 | [`s4-positioning`](2026-08-02-s4-positioning-design.md) | Homepage rewrite, three `/compare/*` pages, MCP promotion | — |
| S5 | [`s5-integrations`](2026-08-02-s5-integrations-design.md) | GitLab forge, Grafana Alerting source, Elasticsearch/OpenSearch logs | S2 (each adds an integration page) |
| S6 | [`s6-seed-catalog-distribution`](2026-08-02-s6-seed-catalog-distribution-design.md) | Commons OKF catalog + `runlore-kb-commons` repo, distribution artifacts | — |

## Sequencing

**Wave 1 (parallel, one worktree + one PR each):** S1, S3, S4.
These touch disjoint files — Go CLI + `hack/`, CI workflows + eval, website homepage + new
`/compare` section.

**Wave 2:** S2 (needs S1 to exist before it can lead with it), S5 (three separate PRs, each
locally verified), S6 (content authoring, then the new repo).

**Final gate:** a `/security-review` pass across the whole set, *plus* a per-PR security pass on
every new integration in S5 (token handling, TLS defaults, redaction, NetworkPolicy egress) and
on `install.sh` in S1.

## Explicitly out of scope

Deferred per the report's own advice or the owner's direction:

- **Microsoft Teams notifier** — dropped by the owner.
- **OpenTelemetry traces** — the report calls it a 2027 bet; expensive, not funnel-blocking.
- **Azure / GCP "what changed"** — AWS covers the likely early adopters.
- **CNCF Sandbox application** — needs a second maintainer first (report §8.1); Q1 2027 target.
- **Conference CFPs, blog cadence, CNCF Slack channel** — owner-driven, not repository work.
- **Submitting** the distribution PRs to third-party repos — S6 prepares them; the owner submits.
</content>
</invoke>
