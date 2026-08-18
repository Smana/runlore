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

`gitops_resource_status` resolves GitOps objects. On the Argo CD deployment where this was
observed, asked about anything else it answers:

```
NOT FOUND — searched the given namespace, flux-system, and all namespaces by name;
the object genuinely does not exist (likely the cascade root).
```

That sentence asserts non-existence, and nominates the absence as the root cause. It is
returned for a kind the tool never supported. Observed live: asked about a
`VMServiceScrape` that demonstrably existed (verified `uid`, single served CRD version,
`auth can-i` = yes), the model quoted the answer as evidence, built its mechanism on it, and
advised recovering the object from Git history while the object sat in the cluster.

**This is engine-conditional, and the Argo CD mechanism is not the one it looks like.**
`argocd.Provider.ResourceStatus` (`internal/providers/gitops/argocd/inspect.go`) **ignores
`w.Kind` entirely** and calls `GetApplication(namespace, name)`. So the kind is not
"unsupported" — it is discarded, and the answer is whatever the *Application* lookup returns:

- no Application by that name ⇒ the NOT FOUND sentence above, for a question about an object
  that was never looked for;
- **an Application by that name ⇒ a false *positive*.** Ask `kind=HelmRelease name=foo` where
  Application `foo` exists and you get that Application's health, sync and refs, labelled
  `HelmRelease foo`. That is worse than the negative: it is another object's real state,
  presented as this one's.

On **Flux** the same question fails safe today. `flux.dynamicReader.GetResource`
(`internal/providers/gitops/flux/dynamic.go:100`) rejects a kind outside `kindToGVR` with
`unsupported kind %q`; `GitOpsStatusTool.Call` propagates it, and `runTool` renders it as
`error: unsupported kind "VMServiceScrape"`. Opaque, but it asserts nothing about the world.
So the Flux path already draws the distinction #503 asks for; the fix is to give the Argo CD
path the same honesty (and to stop advertising kinds the configured engine cannot own at all).

The aggravating factor on both engines:

- **`kind` is a free-form string** in the tool schema, so nothing stops the question. A second
  finding cited three such calls — `HelmRelease`×2, `Kustomization` — as evidence on a cluster
  with no Flux CRDs. Three calls, three confident answers about Applications, zero information
  about the objects named.

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
are readable today **by the built-in toolset**; all must be inferred from consequences. (An
operator can already wire an external MCP server that exposes such a read — `appendMCPTools`
in `internal/app/investigate.go` appends any discovered remote tool to the loop's tool set.
That is opt-in, unbounded by RunLore's own RBAC posture, and not a substitute for a
first-class tool with the error taxonomy below.)

### 3. A failed knowledge write is invisible to the human (#506)

A real incident reached **85% confidence** with an `action_suggested` verdict — above
`forge.min_confidence: 0.75`, not in `forge.skip_verdicts`. (Both keys live on the `forge:`
block — `config.Forge.MinConfidence` / `.SkipVerdicts`. There *is* a `curate:` block, and it
holds neither, so `curate.min_confidence` would be edited into a struct that has no such
field.) The curator tried and got a 403:

```
ERROR curate findings  err="open PR: … status 403: Resource not accessible by integration"
INFO  delivered findings  confidence=0.85 curated_url=""
```

The delivered card showed **nothing**. It simply lacked the `📚 Knowledge base:` line — which
is also what a below-threshold finding and a `no_action` verdict look like. Three different
outcomes render identically:

| outcome | card | is anything wrong? |
|---|---|---|
| below `forge.min_confidence` | no link | no, by design |
| verdict in `forge.skip_verdicts` | no link | no, by design |
| **forge write failed** | no link | **yes** |

The operator discovered it only by chance, because they wrote a thread note and
`internal/thread` **does** report the failure. The curator path does not.

**Scope correction:** the *metric* is already there. `curator.recordCuration(ctx, "pr",
"error")` fires on both failure paths, and `runlore_curations_total{kind="pr",result="error"}`
was confirmed incremented on the live deployment for this exact 403. So this is a
human-surface bug only, and it is alertable today via
`increase(runlore_curations_total{result="error"}[15m]) > 0`.

### 4. No operator note has ever survived verify to fire an instant recall (#504)

Four recalls across two incidents at recall confidence 0.82 / 0.85 / 0.90 / 0.92, **four
rejections**:

> "A match to a knowledge-base entry is not causal evidence."
> "The instant recall reference to a KB article is not a tool result."
> "A Slack thread opinion is not evidence."

Truncated titles were cited in some, but the objection is to the **category**, not the title
quality.

**It is not, however, structural, and the design must not claim it is.** The tempting story —
`Concept` was chosen for operator notes because a bare note has no Symptom/Cause/Resolution
(#482), and that is exactly the evidence chain verify demands — does not survive reading the
code:

- **Nothing in `internal/investigate` reads `catalog.Entry.Type`.** Verify cannot tell a
  `Concept` from an `Incident`.
- **`renderForReview` (`internal/investigate/verify.go`) never renders `Prior`.** An
  `Incident`'s Symptom/Cause/Resolution — the evidence chain the story contrasts — does not
  reach the reviewer either, even though `recalledInvestigation` copies it into `inv.Prior`.
- **`recalledInvestigation` (`internal/investigate/recall.go`) builds the same shape for every
  entry type**: summary = `Title + " — " + Description`, one tautological evidence bullet, plus
  whatever `confirmRecall` gathered.

What the reviewer actually judges is therefore the **description text**, and
`conceptDescription` (`internal/thread/note.go`) hardcodes *"Operator knowledge captured from a
%s thread by @%s."* — which is, near enough verbatim, what the third verdict above objects to.
That is a content property, and content is changeable. Saying "structurally impossible" tells
a reader not to try; the honest claim is that a note **as filed today** carries nothing
admissible, and four out of four is what the reviewer has done every time it was asked.

Note also that the *primary* write route is a comment on the curator's open PR (typed
`Incident`/`Playbook`); `Concept` is the standalone route only, taken when there is no open PR.

**None of that argues for relaxing verify.** An adversarial test settles it: a deliberately
false entry fooled the reranker completely (0.92, *"the canonical runbook for this exact
incident"*) and fired instant recall — and **verify was the only gate that caught it**, on
evidence, after which the full loop reached the correct answer and rebutted the note.
Removing or softening that gate converts a caught error into a delivered one. The real fix is
to change what a note *files*, not what verify accepts.

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

- Weakening verify, or bypassing it on the recall path (see §4 above). Note that "bypass it for
  `Concept` entries" is not even implementable as stated without new plumbing — the verify path
  does not know an entry's type.
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

ArgoCD → `["Application"]`. Flux → the **eight** kinds in `flux.kindToGVR`: `Kustomization`,
`HelmRelease`, `GitRepository`, `OCIRepository`, `HelmRepository`, `HelmChart`, `Bucket`,
`ExternalArtifact`. The tool *description* currently advertises only the first seven —
`ExternalArtifact` is resolvable but undocumented, which #508 corrects; deriving the set from
`kindToGVR` rather than restating it is what stops that drift recurring. This mirrors
`clusterTools`' gate and keeps the capability decision in one testable place.

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
- **RBAC is the boundary, and `pod_logs` shows what that has to mean here.** `pod_logs` is
  *not* an example of "whatever the ClusterRole grants is readable" — it is the opposite.
  `pods/log` is granted **only through namespaced Roles** over `rbac.controllerLogNamespaces`,
  never cluster-wide, explicitly because logs may carry secrets/PII and tool output is sent to
  an external LLM; and on top of that there is an **app-layer allowlist**,
  `investigation.pod_log_namespaces` → `PodLogsTool.AllowedNamespaces`, so the model is bounded
  by the incident namespace plus that list even where RBAC would permit more. Two layers, not
  one. `resource_spec` needs the same shape, for the same reason.
- **The chart's ClusterRole is a narrow allowlist, not a general read.** It grants Flux kinds
  (`kustomizations`, `gitrepositories`, `helmreleases`, `ocirepositories`, `helmrepositories`,
  `helmcharts`, `buckets`, `externalartifacts`), Argo kinds (`applications`,
  `applicationsets`, `appprojects`), `pods` (status/metadata, **not** `pods/log`) and `events`
  — and nothing else. A `resource_spec` bounded by today's ClusterRole therefore **could not
  read a `VMServiceScrape`**, the motivating case, without widening the chart. That widening is
  the real decision this design has to make explicitly, not a detail; a denial must be reported
  **as a denial** either way, never as absence.
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

**Scope correction.** An earlier draft of this design scoped two pages and attributed a
quotation to them — *"a wrong KB entry is answered with confidence in seconds, forever"*. That
string appears **nowhere** in `website/` or `README.md`; it originated in the issue's own
framing, not in the docs. And `slack.md` makes no recall claim at all: its only mentions of
recall are that a finding with no PR may be an instant recall, and that feedback weighs recall
trust. Neither is wrong, so slack.md is **out of scope**.

The nearest real text is `learning-loop.md` §6 — *"an entry recalled only from those sources
therefore keeps firing at full confidence however wrong it has become"* — which is about a
**merged curated entry** on a deployment with no ground-truth resolve channel, and is accurate.

What actually needs writing is on the learning-loop page: an operator note widens `kb_search`
and, measured, has not survived verify; the reason is what a note *files*, not a rule in the
code (see §4 above); and the rejections are currently doing safety work (the adversarial
result). The terminology collision on that page — "note" used for both a curated entry and an
operator note — has to be resolved at the same time, or the correct sentence reads as the
wrong one. `reviewing-knowledge.md` lists thread capture beside investigation findings under
"all four end the same way", which soft-implies equal recall-eligibility; it needs one sentence
saying where a captured note actually pays off.

## Testing

Each change gets a test watched failing against deliberately broken code, per the repo's
standard. The load-bearing ones:

- **#503**: an ArgoCD-configured tool must not advertise Flux kinds; an out-of-scope kind must
  produce an answer that does **not** contain the word "exist"; and — the case the current
  design missed — asking for `kind=HelmRelease name=foo` where Application `foo` exists must
  **not** return that Application's status under the `HelmRelease` label. All three fail today.
- **#505**: `Secret` refused; a redacted value never appears in output; a 403 renders as a
  denial and not as absence.
- **#506**: a curate error reaches the rendered card. The mutation to guard against is the
  error being logged but not carried — exactly today's behaviour.
- **#504**: a guard that **derives** its truth rather than scanning prose. A prose scan for
  "operator notes enable instant recall" cannot fail from the code side and is trivially
  evaded (reword the verb, put a decimal in the sentence, split it in two). Instead: drive the
  real `thread.ConceptEntry` and assert via `kbvalidate.HasIncidentSections` that a filed note
  carries no evidence chain and that its description still matches the sentence the page
  quotes; and, in `internal/investigate`, assert that `renderForReview` over a real
  `recalledInvestigation` shows the description, shows none of the entry body, and is
  **byte-identical for `Concept` and `Incident`** — which is what makes the "not a rule in the
  code" correction checkable. The live measurement (four/four) can only be pinned as a
  page-vs-record consistency check; that half must say so in its own doc comment.

Measured impact from the validation run, for context on why #503 leads: with the false
negative, a wrong mechanism at 60% confidence, 15 model calls, ~$0.70. With a human note that
merely said *don't trust that tool's NOT FOUND*, the correct mechanism at 75%, 7 calls, ~$0.18.
