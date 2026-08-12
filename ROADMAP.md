# Roadmap

Where RunLore is going, and what it is deliberately not going to do.

**No dates.** RunLore is built by a single maintainer (see
[Who maintains this](README.md#who-maintains-this)), so committing to a calendar would be
dishonest. This is direction and ordering, not a schedule. Ordering changes when someone
running RunLore tells me it should — see [What would change this](#what-would-change-this).

Everything below is either already public in the repo or tracked as an open issue. Shipped
work lives in [`CHANGELOG.md`](CHANGELOG.md); the current stability surface — what is
eval-tested versus what is merely functional — is in
[Project status](README.md#project-status--stability).

## Now

Model-provider robustness, plus one security hardening. These are the things that make
RunLore fail badly today, and they are what a new adopter hits first.

| Item | Why it matters |
|---|---|
| [Gemini 3.x is unusable](https://github.com/Smana/runlore/issues/392) — the client omits `thought_signature` on function-call parts, so investigations fail on turn 2 | One of three advertised model providers is broken end-to-end |
| [glm-4.6 stalls on forced `tool_choice`](https://github.com/Smana/runlore/issues/391), silently degrading verify, recall reranking and the eval judge | Silent degradation of the safety passes is worse than a loud failure |
| [Model-comparison report publishes vacuous columns](https://github.com/Smana/runlore/issues/394) when cases lack `ground_truth` | The published eval is a trust artifact; it must not overstate what it measured |
| [Wrap untrusted tool outputs in an explicit delimiter](https://github.com/Smana/runlore/issues/362) (project-wide) | Prompt-injection hardening on the boundary where cluster data enters the model |

## Next

Widening what RunLore can plug into **without requiring Go**, and making recall scale with
the knowledge base.

- **Templated webhook source** — wire Grafana, Datadog and similar alert sources by config
  alone, no new Go provider per vendor.
- **Templated notifier** — the same idea on the delivery side (Teams, Discord…), via Go
  templates rather than a new notifier type each time.
- **Persisted vector cache + incremental indexing** — path-keyed document IDs and a cache
  invalidated by model and dimension, so a growing catalog does not mean re-embedding
  everything on reload.
- **Graduating hybrid recall out of `EXPERIMENTAL`** — the criteria are already written down
  and public in
  [Configuration](https://runlore.io/docs/configuration/configuration/); it graduates when a
  live-endpoint measurement run meets them, or it gets documented as a dead end. It will not
  graduate on vibes.

## Later

Direction, not commitment — these may change shape entirely or never be built.

- **Multi-KB federation** — *design only*, deliberately. A merged index with per-source
  weights and a single writable KB. It carries open questions and an explicit "when NOT to
  build this" gate, and it stays a design until someone has the problem for real.
- **Hot-path benchmarks** — catalog reload, recall query, ledger replay, ingest storm.
  Hermetic, and deliberately not in the PR gate.
- **The `auto` autonomy rung** — experimental, frozen, and not recommended on real clusters.
  It stays off by default. The supported posture remains read-only → `suggest` → `approve`,
  with a human in the loop. This will not be unfrozen quietly.

## Not on the roadmap

Saying no is part of the design. RunLore will not:

- **Match the big agents on native integrations.** RunLore ships a narrow, deliberate native
  tool set and an MCP client. Vendor coverage comes through MCP, not through 50 hand-written
  toolsets. This is a permanent choice, not a backlog gap.
- **Approximate-nearest-neighbour search.** Exact search is fine at the catalog sizes RunLore
  targets; ANN is a non-goal until roughly 1–2k entries make it necessary.
- **Write to your infrastructure outside the action gate.** Writes go to Git via reviewed
  PRs. The `approve` rung executes only *reversible*, allowlisted operations after an
  explicit human approval.

Smaller deliberate non-goals — ledger-file splitting, deduper eviction, PagerDuty replay
windows — are listed with their reasoning in the audit roadmap under `dev/plans/`.

## What would change this

The ordering above reflects one maintainer's read of what matters, informed by running
RunLore on one platform. That is a narrow sample and I know it.

If you are running RunLore — or evaluating it and stopped at something specific — that is the
input most likely to reorder this list. Open an
[issue](https://github.com/Smana/runlore/issues), start a
[discussion](https://github.com/Smana/runlore/discussions), or add yourself to
[`ADOPTERS.md`](ADOPTERS.md). "We stopped because X" is more useful than a feature request.
