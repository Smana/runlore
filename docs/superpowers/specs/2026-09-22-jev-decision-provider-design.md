# Design — a decision provider: typed decisions for the gates that only need one

- **Date:** 2026-09-22
- **Status:** Approved (brainstorming)
- **Owner:** Smaine Kahlouch
- **Adds:** a `Decider` provider kind under a top-level `decision_model:` block, one implementation
  (TypeSafe Jev), and two consumers
- **Touches:** `internal/providers/providers.go`, `internal/decide/` (new), `internal/investigate/rerank.go`,
  `internal/curator/curator.go`, `internal/config/config.go`, `internal/telemetry/metrics.go`,
  `internal/eval/` (shadow reporting)

## Problem

Several RunLore gates do not need a language model. They need one bounded judgement with a
trustworthy confidence attached: *is this the right runbook for this incident?*, *is this finding the
same pattern as that entry?* Today each is an LLM call that forces a named tool and parses the
arguments back, which costs a round trip, depends on the endpoint honouring forced `tool_choice`
([#391](https://github.com/Smana/runlore/issues/391) is exactly that failure), and asks the model to
*assert* a calibrated confidence rather than deriving one.

A System One model answers that shape natively. It takes a state plus typed questions and returns
typed values with a probability distribution and a confidence computed from the spread. It generates
no text, so content embedded in an alert or a runbook cannot instruct it.

The scope here is deliberately narrow: the gates that are already one call and already gate on a
threshold. The investigation loop, the adversarial verify pass and every text-producing path stay on
the model provider.

## Decisions (locked during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Provider shape | **A new `Decider` kind, not a `ModelProvider`** | Jev takes typed questions and returns typed answers. Forcing it behind `Complete` would mean synthesising a fake tool call and throwing the probabilities away, which is the exact mismatch this buys us out of |
| Client | **Hand-rolled, modelled on `internal/embed`** | The established precedent for non-chat inference here: plain JSON over `httpx.SecureClient` with `DoWithRetry`. TypeSafe ships no Go SDK, so this is also the only option that keeps the single static binary |
| First consumers | **Recall reranker and curation dedup** | Both are one call, both already gate on a threshold, both fail safe today. The semantic KB advisory follows if it costs nothing extra |
| Reranker rollout | **`shadow` before `jev`** | The reranker sits in front of the recall short-circuit on every incident. Shadow mode measures agreement on live traffic while the LLM keeps deciding |
| Reranker failure | **Fall through to a full investigation** | Identical to today's LLM-reranker failure path. One backend per call, no hidden second call, no circuit breaker to test |
| Dedup verdict | **Three tiers: skip, annotate, file** | A confident duplicate is skipped and recorded as a confirmation; a middling one is filed *with the suspect named in the body* so the reviewer decides; a low one files as today |
| Dedup knobs | **Extend `forge:`, do not invent `curation:`** | `dup_score`, `min_confidence` and `skip_verdicts` are already `forge.*`. A new block would leave an operator setting `forge.dup_score` and `curation.dedup_backend` to configure one gate, and the new band edges living apart from the threshold they replace — the same placement defect the row below argues against |
| Config placement | **Top-level `decision_model:`, never under `model:`** | It is a different wire protocol from a different vendor answering a different question. Nesting it under `model:` would invite an operator to read it as another LLM endpoint and to expect `max_tokens`, `effort` or `thinking` to mean something there. `model.embeddings` is the counter-precedent, but that serves the model layer alone |
| Switch | **An explicit `enabled` flag, not block presence** | `model.verify` and `model.chat` use presence as the switch, which is fine for a block an operator sets once. This one needs to be turnable **off in a hurry** without deleting the endpoint and key-env config, so it follows `sources.gitops` and `catalog.instant_recall` instead |
| Credentials | **`api_key_env`, the env var NAME** | `configmap.yaml` renders the whole `config:` block verbatim (`toYaml`), so a literal key in config lands in a **ConfigMap** in plaintext rather than a Secret. Every existing provider takes `api_key_env` for exactly this reason |
| Default | **Off** | Every consumer keeps its current backend as the default. Nothing changes until an operator opts in |

## Non-goals

- **Per-incident model routing.** The one lever that would materially cut spend, and the one that
  restructures the loop for two investigation models. Its own spec.
- **Replacing the adversarial verify pass.** It is the gate against plausible-but-wrong, which is
  reasoning, and a System One model can be wrong at high confidence like any other.
- **Anything in the trigger policy or the coalescer.** Both are deterministic, free and instant. A
  network call per alert would make the cheap path slower and add egress before any filter runs.
- **Anything in the action gate.** Reversibility, blast radius and target are validated server-side
  from the op. No model of any kind belongs there.

## Where it sits

```mermaid
flowchart TD
    A[incident] --> B[BM25 retrieval + structural filter]
    B --> C{score floor + spend guard}
    C -->|below| F[full investigation]
    C -->|above| D[rerank decision]
    D -->|llm| D1[model: forced rerank_match]
    D -->|jev| D2[decision model: choice over candidates]
    D -->|shadow| D3[both; LLM decides, agreement recorded]
    D1 --> E{named candidate above threshold?}
    D2 --> E
    D3 --> E
    E -->|no| F
    E -->|yes| G[confirm against live state]
    G --> H[adversarial verify]
    H -->|withdrawn| F
    H -->|kept| I[post the known resolution]
```

Only the rerank decision changes. The guards before it and the two gates after it are untouched,
which is what keeps a wrong Jev answer from reaching a human.

## The contract

```go
// Decider answers bounded, typed questions about a state — no generation.
type Decider interface {
    Decide(ctx context.Context, state string, qs []Question) (Answers, error)
}
```

`Question` carries an id, a kind (`choice`, `score`, `noul`), instructions and criteria.
The request budget is 64k tokens, of which the state plus the longest question may use 32k
— ample for every consumer here, whose largest state is a few kilobytes.
**`noul` is TypeSafe's own name for its boolean primitive** — it returns a probability in
`[0,1]` that a statement is true — so it is spelled that way here deliberately and must be
carried verbatim into the wire types. It is not a typo for `bool`. `Answers`
maps each id to its chosen value, the full probability distribution, and a confidence in `[0,1]`.
Questions in one request are evaluated independently against the same state, so a consumer that needs
two judgements pays for one round trip.

The interface lives beside the other provider contracts in `internal/providers/providers.go`. The
implementation lives in `internal/decide/typesafe/`, with the wire types unexported.

## The reranker

`catalog.instant_recall.rerank_backend` takes `llm` (default, today's behaviour), `jev`, or `shadow`.

The question is one `choice` over the candidate entry ids plus an explicit `none` option, carrying the
existing prompt's rules: match on topical fit rather than proving the cause, and a wrong match is
worse than no match. The fire gate keeps its shape, a named candidate at or above a confidence bar,
and the false-recall guard is unchanged: an id outside the candidate set, an error, or `none` all fall
through.

**Thresholds are per backend and not comparable.** `rerank_threshold` (default 0.7) continues to
govern the LLM backend. `rerank_threshold_jev` governs Jev and has **no default**; it is required when
the backend is `jev`. **Shadow mode reads it too** — `shadowFired` (`internal/investigate/rerank.go`)
gates the decider's side of the comparison on this same threshold, exactly as `jev` would. Leaving it
unset (zero) does not exempt shadow from the gate; it measures agreement at a threshold of **zero**,
where any non-`none` choice counts as a fire regardless of confidence — an operating point no live
`jev` configuration will ever run at. `Validate` range-checks it whenever it is set, under any
backend, so an out-of-range value fails at startup rather than silently producing a meaningless
agreement number.

Shadow mode makes both calls, keeps the LLM's verdict, and records agreement. It doubles the rerank
cost while enabled, which is the point of it being a mode rather than the default.

**Two-phase promotion from `shadow` to `jev`.** The threshold has no defensible default, so shadow
mode exists to produce one from real traffic before `jev` ever decides anything:

1. Run `shadow` with `rerank_threshold_jev` **unset** to fill `runlore_decision_model_confidence`'s
   histogram — this is the only phase where "unset" is the right value, because the goal here is the
   distribution itself, not a fire/no-fire count against it.
2. Read a candidate bar off that distribution.
3. Set `rerank_threshold_jev` to the candidate bar **while still in `shadow`**, and read
   `runlore_decision_model_shadow_total`'s agreement at that operating point — the same bar `jev`
   would actually run at, not zero.
4. Only once that agreement is acceptable, switch `rerank_backend` to `jev`.

Two new metrics, following the existing recall naming:

| Metric | Shape |
|---|---|
| `runlore_decision_model_confidence` | Histogram of returned confidence, labelled by consumer. The distribution a threshold is read off |
| `runlore_decision_model_shadow_total` | Counter labelled `consumer` and `agreement` (`agree`, `disagree`, `error`) |

## Curation dedup

One `noul` question: do these two entries describe the same incident pattern? The exact-fingerprint
check runs first and is unchanged, because it needs no threshold.

| Confidence | Action |
|---|---|
| High | Do not file. Record a confirmation, exactly as a fingerprint match does today |
| Middling | File the PR, naming the suspected duplicate in the body |
| Low | File as today |

The two band edges are config, not constants, and both are **required when `dedup_backend` is `jev`**,
for the same reason the reranker's threshold is: a default nobody measured would be invented, not
chosen. This replaces the `dup_score` BM25 gate, which `curator.go` already documents as inert: on a fifty-entry
catalog measured 2026-08-14 the scores run below 1.0 against a default of 5.0, and RunLore re-filed an
entry for a fault the catalog already held.

## Config

A **decision model is not an LLM**, and the config has to say so at a glance. It gets its own
top-level block, and it deliberately carries none of `model:`'s vocabulary: no `max_tokens`, no
`effort`, no `thinking`. None of them mean anything to a model that emits no tokens.

```yaml
decision_model:                       # a System One model: typed questions in, typed answers out.
  enabled: true                       #   NOT an LLM, and deliberately not nested under model:
  provider: typesafe                  # the only implementation for now
  base_url: https://api.typesafe.ai
  model: jev-latest
  api_key_env: TYPESAFE_API_KEY       # the env var NAME. Never the key itself — see below
catalog:
  instant_recall:
    rerank_backend: shadow            # llm (default) | jev | shadow
    # rerank_threshold_jev: omitted on purpose here — this is PHASE 1 of the two-phase
    # promotion (see "The reranker" above): shadow still reads the threshold to decide
    # a fire, so leaving it unset measures agreement at zero. The point of phase 1 is
    # the confidence histogram, not that number; set it once you have a candidate bar.
forge:                                # dedup config already lives here, beside dup_score
  dedup_backend: bm25                 # bm25 (default) | jev
  # dedup_skip_above / dedup_annotate_above: required once dedup_backend is jev
```

**Never a literal key.** `deploy/helm/runlore/templates/configmap.yaml` renders the whole `config:`
block verbatim, so an `api_key` field would put the credential in a **ConfigMap** in plaintext,
readable by anyone with `get configmaps` and absent from every Secret-handling path the chart has.
`api_key_env` names the variable instead, which is what every other provider here takes. An empty
value means keyless, for a gateway that injects the credential itself.

**`enabled: false` wins, and it does not error.** The obvious design is to reject a config whose
consumer points at `jev` while the block is disabled. That is wrong: the flag exists so an operator
can stop every decision call *during an incident* without also editing two consumer keys, and a
validation error at startup would make the kill switch useless exactly when it is needed. So a
disabled block makes every consumer fall back to its default backend, and the fallback is logged
once per consumer at startup, naming each one.

| Outcome at load | Case |
|---|---|
| **Error** | `enabled: true` with no `provider`, `base_url` or `model` |
| **Error** | `rerank_backend: jev` with no `rerank_threshold_jev` — the bar would be invented |
| **Error** | `dedup_backend: jev` missing either band edge, a band outside `(0,1]`, or skip below annotate |
| **Warn, fall back** | A consumer on `jev` or `shadow` while the block is absent or disabled |

## Error handling

| Failure | Behaviour |
|---|---|
| The decision model errors, times out, or is unreachable | Reranker: fall through to a full investigation. Dedup: fall back to the BM25 path |
| Answer names an id outside the candidate set | Treated as no match, as today |
| Confidence below the backend's bar | No fire, counted under the existing `rerank_low_confidence` rejection reason |
| Shadow call fails | The LLM verdict stands and the agreement metric records the failure; an investigation must never be affected by the shadow arm |
| Rate limited | Surfaced as an error and treated as the first row. No retry storm in front of a gate that has a free fall-through |

The decider is never the reason an incident goes uninvestigated.

## Testing

- **Client:** `httptest` fixtures for each primitive, an error body, a rate limit, and a malformed
  answer. Fixtures are hand-written from the published API shape, so the spec must say so, as the GCP
  provider's docs already do for its lenses.
- **Reranker:** table tests over the three backends with a fake `Decider`, asserting the fall-through
  on every failure mode and that the fire gate reads the right threshold per backend.
- **Dedup:** the three tiers, plus the fingerprint check still short-circuiting first.
- **Eval:** the replay harness builds recall with no reranker, so `rank()` never runs there and the
  replay corpus cannot measure shadow agreement. That number is measured by
  `runlore_decision_model_shadow_total` under `lore serve`, where shadow mode actually runs. The two
  `poisoned-recall` cases remain the precision testbed for recall firing/withdrawal, which the replay
  harness does exercise.
- **Config:** both rejection paths.

## Risks

| Risk | Mitigation |
|---|---|
| **Egress to a fourth vendor.** Alert titles, labels and runbook excerpts leave the boundary | Off by default. `redact.Secrets` before the call, as at the model boundary. Stated plainly in the security architecture page, not implied |
| **Hosted only.** No self-hosted option, so the in-cluster keyless story does not extend to Jev | Documented as a constraint. The LLM backends remain first-class, and the sovereignty-conscious default is unchanged |
| **A confident wrong match.** Calibration is not correctness | Unchanged downstream gates: live-state confirmation, then the adversarial verify pass. A withdrawn recall falls through to a full investigation |
| **Vendor and protocol sprawl**, which the design doc explicitly rejects | One narrow interface, one implementation, off by default, with a deterministic fall-through on every path |
| **Threshold with no defensible default** | Shadow mode exists to produce the number. `jev` refuses to start without it |

## A caveat this spec inherits

The `submit_findings` guidance that made the nightly green states the *shape* the eval
scores: name the resource the cause sits on rather than the one that failed, and name the
identifier it turns on. The corpus grades exactly that — a tag on the `poisoned-recall`
cases, and `harbor-db` rather than the failing `harbor-core`. The comment guarding
`submitFindingsSpec` restricts EXAMPLES to faults outside the corpus, which is the weaker
protection; restating the rubric's categories is the stronger form of teaching the answer.

The guidance stands on its own merit, because an on-call needs the identifier whatever the
eval measures. The measurement is what is weakened. Anything this spec's shadow mode
concludes from the same corpus inherits the caveat, which is a reason to hold out a case
the prompt has never been tuned against before reading a shadow-agreement number as truth.

## Open questions

1. ~~**TypeSafe's data retention and terms.**~~ **Answered 2026-09-23, and the answer is
   mixed.** It does not rule the design out, because every consumer is off by default, but it
   does fix what the published docs must say.

   | Question | Answer |
   |---|---|
   | Trained on submitted state? | **No.** The privacy policy commits twice: "We will not train or fine tune any artificial intelligence or machine learning models on your prompts or other Input" |
   | Disclosed to third parties? | No, "other than our service providers", which are not named |
   | Retention period | **Not stated.** "as long as reasonably necessary" — no number of days |
   | Zero data retention | Offered, but **enterprise-gated**: by request to TypeSafe, not a config flag |
   | Self-hosted or on-premises | **Not offered.** Cloud API only |

   The no-training commitment is the strong part and is what makes this viable at all. The
   unbounded retention window and the absence of any self-hosted option are the weak parts,
   and they land squarely on the audience this project targets: a team that runs its model
   in-cluster specifically so incident data does not leave, and that can currently do so for
   every existing provider.

   So: the feature stays off by default, and the docs must say plainly that enabling it sends
   alert titles, labels and runbook excerpts to a hosted third party with no published
   retention period, and that a deployment which requires zero retention has to arrange it
   with TypeSafe directly. That is a disclosure, not a footnote.
2. **The semantic KB advisory.** A stretch item with a mechanical test: include it if it is two
   `noul` questions over the existing client, reusing `decision_model:` with no new config key and no new
   question kind. Anything more and it is deferred, not squeezed in.
3. **Agreement bar for promoting `shadow` to `jev`.** Deliberately not fixed here — it is a reading of
   real traffic, not a number to invent now. **Not from the replay corpus:** that harness builds recall
   with no reranker, so it cannot measure shadow agreement at all (see § Testing). The reading comes from
   `runlore_decision_model_shadow_total` under `lore serve`, at the end of the two-phase procedure in
   § The reranker.

4. **Per-case shadow evidence would need a reranker in the replay harness.** Adding one changes the
   replay fire gate for every catalog case and adds a paid model call per case, which invalidates the
   existing baselines — its own change, deliberately not bundled here. Until someone makes it, a
   recall-gate change cannot be measured by `lore eval` replay at all, which is a trap for the next
   recall feature too.

## Acceptance criteria

- `rerank_backend: llm` and an absent or disabled `decision_model:` block reproduce today's behaviour
  byte for byte,
  proven by the existing recall tests passing unchanged.
- With `shadow`, an investigation's outcome is independent of the decider arm, including when it
  fails, and agreement is visible in the metrics and the replay report.
- With `jev`, every failure mode falls through to a full investigation, and no path can fire a recall
  on an id the reranker was not offered.
- Dedup's three tiers are observable, and no tier can close or skip a human-labelled artifact.
- The quality gate passes: `go build ./... && go vet ./... && go test ./... && gofmt -l .` clean, and
  `hack/lint.sh` reporting `0 issues`.
