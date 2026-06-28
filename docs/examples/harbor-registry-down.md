# End-to-end example: Harbor registry down (IAM access-key quota)

A complete walkthrough of RunLore handling a **real** production incident on an
EKS cluster — from an Alertmanager alert to a merged knowledge-base entry — plus
what the learning loop does on the **next** occurrence.

This is a real run, not a contrived demo. The cluster is the
[`cloud-native-ref`](https://github.com/Smana/cloud-native-ref) platform (Crossplane +
Flux + Cilium on EKS); the knowledge base is
[`Smana/runlore-kb`](https://github.com/Smana/runlore-kb).

## The incident

The Harbor container registry went unavailable. The symptom on the cluster:

```
$ kubectl get pods -n tooling | grep harbor-registry
harbor-registry-756cc78495-q428d   1/2   CreateContainerConfigError   0   21m
```

The `registry` container could not start:

```
registry: CreateContainerConfigError —
  couldn't find key username in Secret tooling/xplane-harbor-access-key
```

Alertmanager fired a `critical` alert and, via a `webhook_configs` receiver,
delivered it to RunLore's authenticated incident webhook:

```
POST /webhook/alertmanager        (Authorization: Bearer <token>)
{ alertname: HarborRegistryDown, severity: critical,
  namespace: tooling, deployment: harbor-registry }
```

RunLore's trigger policy admitted it (`severity: critical` matched) and started an
investigation:

```json
{"msg":"incident","alert":"HarborRegistryDown","severity":"critical","investigate":true,"reason":"matched trigger policy"}
```

## The investigation

RunLore ran its ReAct loop against live cluster + cloud state. The tools it chose:

| Tool | What it found |
|------|---------------|
| `pod_status` | `harbor-registry` waiting on `CreateContainerConfigError: couldn't find key username in Secret tooling/xplane-harbor-access-key` |
| `kube_events` (×2) | A persistent Warning on the Crossplane `AccessKey/xplane-harbor`: `LimitExceeded: Cannot exceed quota for AccessKeysPerUser: 2` |
| `gitops_resource_status` | The Crossplane managed resource responsible for the Secret was not Ready |
| `what_changed` | Recent changes around the failing resources |
| `kb_search` | Surfaced a prior related article in the catalog |

It then connected the dots into a causal chain and concluded with **0.95
confidence**:

```json
{"msg":"investigation complete","title":"HarborRegistryDown","confidence":0.95,"root_causes":1}
{"msg":"curated as PR","url":"https://github.com/Smana/runlore-kb/pull/72","confidence":0.95}
```

## The root cause

The Crossplane `AccessKey/xplane-harbor` resource had hit the AWS IAM quota
**`AccessKeysPerUser: 2`**, so it could not create a new access key. That left the
Kubernetes Secret `xplane-harbor-access-key` without its `username` key, which in
turn made the `harbor-registry` pod fail with `CreateContainerConfigError`.

The fix (captured in the entry's resolution): delete an old/unused access key on
the IAM user so Crossplane can create Harbor's key, then reconcile.

## Closing the knowledge base

RunLore opened a draft PR with a structured entry — decision card, symptom,
evidence-cited investigation steps, root cause, and resolution — which was
reviewed and merged:

> **KB entry:** [`harbor-registry-down-due-to-iam-access-key-quota-limit.md`](https://github.com/Smana/runlore-kb/blob/main/harbor-registry-down-due-to-iam-access-key-quota-limit.md)
> **PR:** [Smana/runlore-kb#72](https://github.com/Smana/runlore-kb/pull/72)

The entry carries `resource: tooling/harbor-registry` in its frontmatter — the
load-bearing field for the next part.

## What happens next time — the learning loop

The payoff of curating knowledge is what RunLore does on the **next** occurrence.

**Same symptom, same cause (a recurrence).** When `tooling/harbor-registry` fails
the same way again, instant recall matches the merged entry on the
resource + symptom and can short-circuit the whole investigation, delivering the
known cause and resolution in seconds instead of re-running an LLM investigation.

**Same symptom, a _different_ cause.** This is the interesting case, and RunLore
is deliberate about it. We tested it directly: with a knowledge entry asserting
cause *X* for a workload, we made the workload fail live with a genuinely
different cause *Y* (same `CrashLoopBackOff` symptom). The observed behavior:

1. **Recall fires and anchors** on the stored cause *X* (resource + symptom match
   are cause-blind), short-circuiting the loop with a high prior confidence.
2. **The live-state confirm/verify pass catches the contradiction** — the live
   evidence shows cause *Y*, not *X* — and **downgrades** the finding
   (`recall_hits{result="downgraded"}`), roughly halving the confidence.
3. The result is delivered to a human as a **low-confidence, hedged** suggestion
   (with an "unresolved" note), **never auto-applied**, and **no incorrect entry
   is filed** (recalled findings skip curation).

So a stale entry can't silently produce a confident wrong answer when the live
evidence clearly disagrees — the operator sees a flagged, low-confidence hint. The
residual risk (documented, not yet eliminated) is the *surface-similar* case,
where a new cause resembles the stored one closely enough that the confirm step
may not flag it; correction then relies on the outcome ledger over repeated
non-resolutions.

## Operational notes from this run

- **Authentication is mandatory.** With a model configured, RunLore refuses to
  start an unauthenticated alert webhook (alert content flows into the LLM prompt
  and bills the model). The Alertmanager receiver authenticates with a shared
  bearer token.
- **Pod discovery must be robust.** An earlier build concluded "workload not
  deployed" because `pod_status` was queried with a guessed label selector that
  matched nothing — indistinguishable from an empty namespace. `pod_status` now
  falls back to listing the whole namespace when a selector matches nothing, so a
  guessed selector can't poison the investigation.
- **Model choice matters for confident root-causing.** A smaller/faster model was
  inconsistent on real incidents (inconclusive on this very case); a stronger
  reasoning model produced the confident, correct, evidence-cited RCA above.
