# Design — S3: Proof assets

- **Date:** 2026-08-02
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Source:** improvement report §4, §9.3
- **Requires from the owner:** the `RUNLORE_EVAL_API_KEY` repo secret
- **Index:** [decomposition](2026-08-02-improvement-report-decomposition.md)

## Problem

RunLore's positioning is honesty, and the assets that would prove it are either invisible or
broken.

**The scorecard is dead.** `README.md` carries a shields endpoint badge pointing at
`eval-scorecard/badge.json`, and `benchmarking.md:13` calls the nightly numbers public. Verified
2026-08-02: `RUNLORE_EVAL_API_KEY` is **not set** (`gh secret list` returns only
`RELEASE_PLEASE_TOKEN`), so every nightly run takes the graceful-skip path — 26–32 second
"successes" with a `::warning::` annotation. The `eval-scorecard` branch **does not exist on the
remote**. The badge renders broken and the link 404s. The publishing machinery in `eval.yaml:80–110`
is complete and correct; it has simply never had a key.

**The headline recall metric overstates itself.** `learning-loop.md:146` presents
`rerank on → 11/11 (1.00), precision 1.00, 0/2 negatives` as "measured on the eval harness". But
`recalleval_test.go:831` injects `scriptedReranker{accept: rerankTargets(cases), conf: 0.9}` — a
stand-in that accepts the correct target by construction. The row measures the **fire gate's
plumbing and the corpus's structural agreement**, not a model's reranking judgment. A hostile
reader who opens the test will find that in two minutes, and it will cost more credibility than the
number buys.

**No cost figure is published,** though the data exists: `providers.Usage` is provider-reported and
already summed by the eval runner's `CountingModel`, and the Slack footer already shows it.

**No `ADOPTERS.md`, no OpenSSF Scorecard badge.** SBOMs already ship (`.goreleaser.yaml:48`); they
just need verifying and mentioning.

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Scorecard fix | Owner sets the secret; I bootstrap the branch from one committed run | The page must not be empty on day one, and the nightly must keep it fresh afterwards |
| `/eval` page source | `docs.yml` fetches `scorecard.md` from the `eval-scorecard` branch at build time | The scorecard lives on an orphan branch by design; copying it into `main` would fight the nightly |
| Rebuild trigger | `eval.yaml` fires a `repository_dispatch`; `docs.yml` listens | Otherwise the page only refreshes when someone pushes to `website/` |
| Missing-branch behaviour | Render a "not yet published" page, never fail the build | A broken docs deploy is worse than a candid placeholder |
| Recall numbers | State the corpus **and** the scripted-reranker caveat in RunLore's own voice | The project's entire positioning is honesty about sub-50% reality; applying it to its own headline metric is the credible move |
| Numbers in prose | Pinned by a reflection test against the computed values | Owner's standing preference for doc/code drift guards |
| Cost table | Extend `lore eval scorecard` with a cost section | Keeps one publishing path; a separate command would drift |

## Scope

### In scope

1. Bootstrap the `eval-scorecard` branch (one real run, committed) and confirm the badge resolves.
2. `/eval` page on runlore.io, fed from the branch at build time, with a placeholder fallback.
3. `repository_dispatch` wiring: `eval.yaml` → `docs.yml`.
4. Cost-per-investigation section in the scorecard (full loop vs instant recall).
5. Recall-metric honesty pass in `learning-loop.md` + `README.md`, with a drift guard.
6. `ADOPTERS.md` + a README ask.
7. OpenSSF Scorecard workflow + badge; verify SBOMs are attached to releases.

### Non-goals (YAGNI)

- Replacing the scripted reranker in `recalleval_test.go` with a live model — that would make a
  unit test cost money and flake. The fix is disclosure, not re-measurement.
- A live-fire nightly (`--live` needs a cluster).
- Historical charting of the scorecard beyond the `history.jsonl` the renderer already writes.
- A public dashboard app.

## Design

### `/eval` page pipeline

```
eval.yaml (nightly, key present)
  → lore eval scorecard -report … -dir <worktree>
  → push eval-scorecard branch                     [already implemented]
  → POST /repos/Smana/runlore/dispatches {event_type: scorecard-updated}   [new]

docs.yml
  on: push(website/**) | workflow_dispatch | repository_dispatch(scorecard-updated)   [new]
  → git fetch origin eval-scorecard --depth=1                                          [new]
  → render website/content/eval.md from scorecard.md + front matter                    [new]
  → hugo build
```

The render step prepends Hugo front matter (`title: Eval scorecard`, `layout`, a "last published"
line derived from the branch's commit date) to the fetched markdown. If the fetch fails, it writes a
short placeholder explaining that no nightly has published yet and linking the workflow — honest,
and it keeps `benchmarking.md`'s link alive either way.

`benchmarking.md:13` and the README badge's **link target** are repointed at `/eval`, so the site is
the canonical surface and the branch is an implementation detail. The badge *image* keeps reading
`badge.json` from the branch — that is what makes it update without a site rebuild.

### Cost section

Pricing already exists end-to-end: `config.Model.Pricing` (`config.go:586`) feeds
`evalCostUSD(cfg, usage)` (`eval.go:135`) into `Report.CostUSD`. Setting `model.pricing` in
`eval/ci.runlore.yaml` is therefore all the configuration this needs.

What is missing is **per-case** token attribution — the report carries only campaign totals
(`replay_report.go:23`). Since `Runner.RunN` is strictly sequential (`eval.go:207–224`), a
before/after snapshot of `CountingModel.Total()` around each `runOne` yields that run's usage
exactly. The rendered scorecard then gains:

| | median in tok | median out tok | est. cost |
|---|---|---|---|
| full investigation | … | … | $X |
| instant recall | … | … | $Y |

The recall/loop discriminator already exists: `ReportCase.RecallShortCircuit`
(`replay_report.go:44`) counts the repeats answered from the catalog. Cases with a non-zero count
are the recall rows; the rest are full-loop rows.

Cost is rendered **only** when prices are configured, and the model + date are stated alongside, so
the figure is never a naked number.

### Recall-metric honesty pass

The `learning-loop.md` table keeps its numbers and gains, in the same blockquote:

- what the corpus is — label positives, negatives, and the fixture catalog they are scored against;
- that rerank-**on** is measured with a **scripted reranker**, so the row measures the gate and the
  corpus's structural agreement, **not** a model's reranking accuracy;
- a pointer to the ITBench sub-50% baseline already cited in `benchmarking.md`, so the reader has
  the field number next to the fixture number.

`README.md`'s recall claims get the same one-line caveat.

**Drift guard** — `TestRecallDocClaimsMatchMeasurement` parses the markdown table in
`learning-loop.md` and asserts each cell equals the value computed by the same code path
`TestRecallEvalRerankFireRate` measures. Editing the prose without editing the measurement (or the
reverse) fails CI. Mutation-tested during implementation: change one cell, confirm red, revert.

### ADOPTERS + OpenSSF

`ADOPTERS.md` with a table (organization, since, how they use it, contact-optional) seeded with the
maintainer's own usage, plus a README line asking for entries. OpenSSF Scorecard via
`ossf/scorecard-action` on a weekly schedule publishing to the security tab, with the badge added to
the README badge row. SBOM: confirm `.goreleaser.yaml:48`'s `sboms:` block attaches `*.sbom` to the
GitHub release, then say so in `SECURITY.md` (the report's point is that this work is already done
and unmarketed).

## Testing

| Test | Guards |
|---|---|
| `TestRecallDocClaimsMatchMeasurement` | doc numbers can't drift from the measurement |
| `TestScorecardCostSection` | cost rows render only with prices, and the recall/loop split is correct |
| `TestScorecardPlaceholder` | the docs build succeeds with no `eval-scorecard` branch |
| Manual | one nightly run end-to-end after the secret lands: branch pushed → dispatch fired → `/eval` shows the fresh numbers → badge green |

## Risks

| Risk | Mitigation |
|---|---|
| The first published scorecard is red | That is the point — `eval.yaml` already publishes on `always()`. A red scorecard published on schedule is the asset; a hidden one is not |
| Nightly eval cost | Replay only, 13 scenarios × n=5, on a cheap model; the existing `ci.runlore.yaml` governs it |
| Disclosing the scripted reranker weakens the claim | It converts an attackable claim into a defensible one, and is on-brand. The real fire-rate story (0/11 → 11/11 at default thresholds) survives intact — what changes is what the reader thinks it measures |
| `repository_dispatch` needs elevated permissions | The job already has `contents: write`; the dispatch is scoped to the same repo |

## Acceptance criteria

1. The `eval-scorecard` branch exists, the README badge resolves, and `benchmarking.md`'s link
   reaches a live page.
2. `runlore.io/eval` renders the current scorecard, and updates automatically within one nightly
   cycle of a new run.
3. The docs build passes with the branch absent.
4. The scorecard shows a dated, model-attributed cost-per-investigation table splitting full loop
   from instant recall.
5. `learning-loop.md` states the corpus and the scripted-reranker caveat; the drift guard fails on
   an edited number (proven by mutation, reverted).
6. `ADOPTERS.md` exists and is linked from the README; the OpenSSF badge renders; SBOM presence is
   verified and documented.
</content>
