# Evidence honesty — design

**Date:** 2026-08-17
**Status:** draft
**Closes:** #503, #505, #506, #504 (the last as a documented decision, not a behaviour change)

## Problem

Four findings from a live validation of `main-6e50f57` on a shared-0 cluster share one
root theme: **the agent, and the human reading it, are told things that are not true, or are
told nothing when something failed.** They are grouped because the fixes interlock — #505
provides the capability #503's callers actually wanted, and #504 is the reason not to
"fix" #503 by loosening a gate.

### 1. A tool reports an out-of-scope question as a fact about the world (#503)

`gitops_resource_status` resolves GitOps objects. Asked about anything else it answers:

```
NOT FOUND — searched the given namespace, flux-system, and all namespaces by name;
the object genuinely does not exist (likely the cascade root).
```

That sentence asserts non-existence, and nominates the absence as the root cause. It is
returned for a kind the tool never supported. Observed live: asked about a
`VMServiceScrape` that demonstrably existed (verified `uid`, single served CRD version,
`auth can-i` = yes), the model quoted the answer as evidence, built its mechanism on it, and
advised recovering the object from Git history while the object sat in the cluster.

Two aggravating factors:

- **`kind` is a free-form string** in the tool schema, so nothing stops the question.
- On an **ArgoCD** deployment the Flux kinds are *supported* but can never exist, so they
  return the same authoritative negative **unconditionally**. A second finding cited three
  such calls — `HelmRelease`×2, `Kustomization` — as evidence on a cluster with no Flux CRDs.
  Three calls, three confident negatives, zero information.

The codebase already holds the principle for this, applied to a different tool
(`internal/app/investigate.go`, `clusterTools`):

> `controller_logs` enumerates the Flux controllers in flux-system, so it is a
> dead/misleading tool on an ArgoCD deployment: it is registered **ONLY** when the configured
> GitOps engine is Flux. Making registration a function of the known engine capability keeps
> the gate in one testable place.

`gitops_resource_status` never got that treatment.

### 2. There is no way to read an object's spec (#505)

RunLore reads pod status, events, pod and controller logs, metrics, logs, cloud state and
GitOps objects — but **not the `spec` of an arbitrary Kubernetes object**. From a delivered
finding's own data-gaps section:

> No tool in this toolset can directly read/query the live VMServiceScrape custom resource's
> spec (namespaceSelector/selector) or confirm its exact match criteria — inference is based on
> absence of any matching namespace/pods/metrics plus the operator note, **not a direct read of
> the CR's YAML**.

That is an accurate self-assessment and it is why the #503 investigation went wrong. The
answer was one field:

```console
$ kubectl -n observability get vmservicescrape datagrok-rabbitmq -o jsonpath='{.spec.namespaceSelector}'
{"matchNames":["datagrok"]}      # a namespace that does not exist on this cluster
```

A large class of incidents is *"the spec says X and reality is Y"*: a Service selector
matching no pods, a scrape CR targeting an absent namespace, a NetworkPolicy with no egress to
kube-dns, an HPA on a metric that never reports, a PVC bound to a vanished StorageClass. None
are readable today; all must be inferred from consequences.

### 3. A failed knowledge write is invisible to the human (#506)

A real incident reached **85% confidence** with an `action_suggested` verdict — above
`curate.min_confidence: 0.75`, not in `skip_verdicts`. The curator tried and got a 403:

```
ERROR curate findings  err="open PR: … status 403: Resource not accessible by integration"
INFO  delivered findings  confidence=0.85 curated_url=""
```

The delivered card showed **nothing**. It simply lacked the `📚 Knowledge base:` line — which
is also what a below-threshold finding and a `no_action` verdict look like. Three different
outcomes render identically:

| outcome | card | is anything wrong? |
|---|---|---|
| below `min_confidence` | no link | no, by design |
| verdict in `skip_verdicts` | no link | no, by design |
| **forge write failed** | no link | **yes** |

The operator discovered it only by chance, because they wrote a thread note and
`internal/thread` **does** report the failure. The curator path does not.

**Scope correction:** the *metric* is already there. `curator.recordCuration(ctx, "pr",
"error")` fires on both failure paths, and `runlore_curations_total{kind="pr",result="error"}`
was confirmed incremented on the live deployment for this exact 403. So this is a
human-surface bug only, and it is alertable today via
`increase(runlore_curations_total{result="error"}[15m]) > 0`.

### 4. Operator notes can never fire instant recall (#504)

Six recalls across two incidents at reranker confidence 0.82–0.92, **six rejections**:

> "A match to a knowledge-base entry is not causal evidence."
> "The instant recall reference to a KB article is not a tool result."
> "A Slack thread opinion is not evidence."

Truncated titles were cited in some, but the objection is **categorical**, and one rejection
was of a hand-written entry with a clean, specific, untruncated title. It is structural:
`Concept` was chosen for operator notes precisely because a bare note has no
Symptom/Cause/Resolution (#482), and that is exactly the evidence chain verify demands.

**This must not be "fixed" by relaxing verify.** An adversarial test settles it: a
deliberately false entry fooled the reranker completely (0.92, *"the canonical runbook for this
exact incident"*) and fired instant recall — and **verify was the only gate that caught it**,
on evidence, after which the full loop reached the correct answer and rebutted the note.
Removing or softening that gate converts a caught error into a delivered one.

## Scope

In scope:

1. **#503** — make the advertised `kind` set a function of the configured engine; constrain it
   in the schema; separate "this tool cannot answer that" from "that object is absent".
2. **#505** — a read-only `resource_spec` tool, so the question #503's callers were really
   asking has an honest answer.
3. **#506** — carry the curate failure to the card, mirroring `internal/thread`.
4. **#504** — **documented as a known limitation**, plus the honest framing in the docs. No
   behaviour change to verify.

Out of scope:

- Weakening verify, or bypassing it for `Concept` entries (see above).
- Note *graduation* (promoting a confirmed note to `Incident` with an investigation's evidence
  attached). It is the most promising real fix for #504 and deserves its own design — it
  changes the curation lifecycle, not a tool boundary.
- The 403 itself: a GitHub App permission on the operator's side, not a RunLore bug.

## Design

### #503 — engine-derived kinds, and an honest negative

Three changes, smallest first:

**a. Derive the kind set from the engine.** A single exported helper returns the kinds the
configured engine can actually own:

```go
// gitopsKinds returns the resource kinds gitops_resource_status can resolve for the
// configured engine. Flux kinds cannot exist on an ArgoCD deployment and vice versa, so
// advertising them invites a question whose only possible answer is a misleading negative.
func gitopsKinds(engine string) []string
```

ArgoCD → `["Application"]`. Flux → the seven Flux kinds. This mirrors `clusterTools`' gate and
keeps the capability decision in one testable place.

**b. Constrain the schema.** `kind` gains an `enum` built from that set, so the provider
rejects an out-of-scope kind before it reaches the tool. This is the change that actually
prevents the failure, rather than describing it.

**c. Separate the two negatives.** The `NotFound` branch keeps its wording *only* for a kind
this tool owns. An unsupported kind gets a different answer that says what it is and, crucially,
what it is **not**:

```
VMServiceScrape is not a GitOps object this tool resolves (supported on this deployment:
Application). This says NOTHING about whether the object exists — use resource_spec or
pod_status. Do not treat this as evidence of absence.
```

The supported-kind wording is also softened: *"the object genuinely does not exist (likely the
cascade root)"* makes a causal claim from one name lookup. *"not found by name in …"* carries
the same information without nominating a root cause.

### #505 — `resource_spec`

A read-only tool: `{kind, name, namespace}` → the object's `spec` and `status`.

Security is the whole design, because this widens what crosses the provider boundary:

- **`Secret` is never readable.** Hard exclusion, not a redaction reliance.
- **Redaction before egress**, reusing the existing `redact.Secrets` pass — specs embed
  credential references and occasionally inline values.
- **RBAC is the boundary**, as with `pod_logs`. Whatever the chart's ClusterRole grants is
  what is readable, and a denial is reported **as a denial**.
- **Error taxonomy** — this is the #503 lesson applied at birth. Four outcomes, four answers:
  *not permitted* / *kind not served by this cluster* / *object absent* / *here it is*. Only
  the third is evidence about existence.
- **Discovery-driven kind resolution**, so CRDs work without a code change per operator.

### #506 — carry the failure to the card

`internal/app/investigate.go` currently logs and drops the error. Carry it instead:

```go
if ref, err := cur.Curate(...); err != nil {
    log.Error("curate findings", "err", err)
    found.CurateError = err.Error()   // new field, rendered by notify
}
```

`internal/notify/format.go` gains the mirror of its existing `CuratedURL` branch:

```
⚠️ Could not save to the knowledge base: <reason>
```

Rendered for every sink that renders the finding text. The reason is already
operator-facing in `internal/thread`'s equivalent, so there is no new disclosure class.

### #504 — say it plainly

`website/content/docs/integrations/notifications/slack.md` and the learning-loop page
currently imply the opposite of what happens: *"a wrong KB entry is answered with confidence in
seconds, forever."* For operator notes that is not true — they widen `kb_search` and never
short-circuit. State it, and state why it is the safe behaviour (the adversarial result above).

## Testing

Each change gets a test watched failing against deliberately broken code, per the repo's
standard. The load-bearing ones:

- **#503**: an ArgoCD-configured tool must not advertise Flux kinds, and an out-of-scope kind
  must produce an answer that does **not** contain the word "exist". Both fail today.
- **#505**: `Secret` refused; a redacted value never appears in output; a 403 renders as a
  denial and not as absence.
- **#506**: a curate error reaches the rendered card. The mutation to guard against is the
  error being logged but not carried — exactly today's behaviour.
- **#504**: `docsguard`-style assertion that no page claims operator notes enable instant
  recall.

Measured impact from the validation run, for context on why #503 leads: with the false
negative, a wrong mechanism at 60% confidence, 15 model calls, ~$0.70. With a human note that
merely said *don't trust that tool's NOT FOUND*, the correct mechanism at 75%, 7 calls, ~$0.18.
