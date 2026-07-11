# Pre-announcement roadmap — 2026-07-11

Synthesized from the 2026-07-11 audit (`2026-07-11-pre-announcement-audit.md`) and a 3-lens
brainstorm panel (launch first-impressions, datasource depth, sustainable architecture).
Finding IDs (A1, B4, C2…) refer to the audit report.

## Phase 0 — pre-announcement punch list (~1–2 days, all S except one M)

Ordered by first-hour blast radius:

1. **Make the advertised quickstart boot** (A1, S) — add `server: { webhook_token_env:
   RUNLORE_WEBHOOK_TOKEN }` + the secret key to `values-minimal.yaml` and the getting-started
   step-4 example; fix the "optional" header comment; extend `minimal_values_test.go` to
   assert `app.RequireWebhookAuth` passes on the shipped values so the quickstart can never
   silently regress.
2. **Docs truthfulness pass on the fail-closed rules** (A3+A4, S) — one PR: state the real
   three-tier webhook-token rule in configuration.md / security-model.md /
   security-architecture.md / troubleshooting.md; fix the approve-requires-`audit_log_path`
   claims in configuration.md + upgrade-uninstall.md; add `audit_log_path` to the
   getting-started approve example. Fold in the five-minute SECURITY.md fix (delete the false
   "reads Kubernetes Secrets" claim — it *under*sells the least-privilege story).
3. **Close the silent-drop exits — structurally** (A2, M) — synthesize an inconclusive
   Investigation + `deliver()` on the prose-inconclusive and max-steps paths, then extract a
   single `finish(result, inv)` funnel (via `defer` + named return) so no future exit path
   can forget delivery. Table-driven test: every exit condition → exactly one `OnComplete`.
   First slice of the 418-line-function refactor; take only the `finish()` extraction now.
4. **Safe-by-default budgets in code** (C3+B3, S) — default `MaxToolOutputBytes=32768` and
   ~100k tokens/investigation in `applyDefaults` exactly as `tool_timeout` is defaulted;
   `-1` (or `unlimited: true`) as the explicit escape hatch; test that a zero-value config
   produces a bounded loop. "Ran it once, it burned $X" is a launch-thread killer.
5. **`pod_logs` on multi-container pods** (B6, S) — iterate `pod.Spec.Containers` setting
   `PodLogOptions.Container` (honor the `default-container` annotation), prefix lines with
   the container name, and render `<pod>/<container>: log stream failed: <err>` instead of
   silently skipping. A large share of the SRE early-adopter audience runs meshes; today the
   flagship crash-loop tool is blind on their clusters.
6. **Evict the SDD leak** (A5, S) — `git mv docs/superpowers/{plans,specs}` → `dev/superpowers/`,
   fix intra-tree links. Restores the deliberate June 27 convention; consider a CI guard.
7. **System-prompt stopgap for the Flux namespace contradiction** (B2, S) — one-line prompt
   edit in `loop.go` so the two instructions stop conflicting for Flux users, until the real
   namespace resolution lands in Phase 1.

## Phase 1 — week 1: make the differentiator true

The theme: every confirmed correlation finding is a missing *join key* — identity or time —
that the code already has in hand.

1. **Stamp `Change.When` + resolve workload→GitOps-object namespace + bound diffs** (B1+B2+B3, M)
   — the flagship fix. `When`: ArgoCD `history[last].deployedAt` (one extra map read); Flux
   ToRev `Committer.When` from the already-resolved commit, falling back to the Ready
   condition's `lastTransitionTime`; render both commit and reconcile time when they differ.
   Namespace: match Flux `spec.targetNamespace`/inventory and Argo `destination.namespace`;
   on zero matches, retry name-matched across namespaces with a note (mirror the
   `pod_status` empty-selector fallback and `GetResource`'s existing pattern) so "no changes"
   is never a silent false negative. Bounding: diffstat first, ~200 lines per file with an
   explicit marker, cap total changes; honor `TimeWindow`. Once stamped, GitOps and CloudTrail
   changes sort into one coherent timeline for free.
2. **Join-key correctness in log rendering** (B4+B5, S) — `groupLogLines` tracks min/max Time
   explicitly (never assume input order); render `[pod-6f9d…/container] msg (x137 across 3
   pods, first 10:02:41Z → last 10:59:57Z)`. First-seen now lines up with `Change.When`.
   Shuffled-timestamp multi-pod fixture test.
3. **Fix the CloudTrail one-attribute contract bug** (B7, S) — single `ResourceName` lookup
   attribute, filter read-only client-side; fix the test asserting the broken shape; surface
   `errorCode`/`errorMessage` while in the file (failed mutating calls are the highest-signal
   events). Verify once against a real account.
4. **Scope honesty in network + ASG tools** (B8+B9, S) — IP-based providers prepend one
   sentinel line ("results are VPC-wide, NOT scoped to <ns>/<name>"); `pod_status` gains
   `podIP`/`nodeName` (already on the object, zero API calls) so the model can bridge IP→pod;
   ASG scan filters server-side by `eks:cluster-name` tag (also kills the wasted per-ASG
   activity calls).
5. **Discovery layer** (C1, M) — `MetricsProvider.LabelValues` (`/api/v1/label/__name__/values`
   scoped by matcher+window) behind a `discover_metrics` tool; log-field discovery via
   `/select/logsql/field_names` folded into `query_logs`' empty path. Highest-leverage single
   line: rewrite the dead-end empty-result strings into recovery instructions ("no series
   matched — use discover_metrics with {namespace=…} to list what this workload exports").
6. **`logs_error_summary` — exploit VictoriaLogs analytics** (M) — optional `LogStats`
   interface (type-asserted, like `GitOpsInspector`): `/select/logsql/hits` histogram +
   `collapse_nums | stats by (_msg) | sort | limit 10`. Output: "errors 3/5m baseline →
   412/5m spike starting 10:02" + top shapes with first→last. Tool description: "use FIRST
   for any log investigation; drill into query_logs after." This is the demo moment that
   separates RunLore from "an LLM with kubectl".
7. **Recall near-miss injection + server-enriched `kb_search`** (C2, M) — on recall non-fire
   with ≥1 structurally-agreeing candidate, inject it into the seed as explicitly UNVERIFIED
   prior knowledge (same untrusted-data framing as alert text; disabled under auto like
   instant recall); server-side, append the normalized workload ref + alertname to `kb_search`
   queries the way `buildRecallQuery` does. Eval scenario: near-miss seed changes the tool
   sequence without changing the verdict when the runbook is wrong.
8. **Zero-cluster full-investigation demo: `lore demo investigate`** (M) — replay a fixture
   incident (crashloop-after-deploy with a real diff, network-drop, distractor) through the
   genuine ReAct loop using the eval harness's fake providers + the user's API key, streaming
   each step to stdout; recorded-transcript fallback for the keyless case. `hack/demo.sh`
   shows the trigger policy; this shows the agent *thinking* — the top-of-README asset.
   Doubles as composition-root construction-path coverage.
9. **Architecture guards, cheap while small** (S/M each):
   - **Reflection-walk egress redaction** — replace field enumeration with a walk over all
     exported string fields of the serialized Investigation + short verbatim allowlist;
     reflective test so a new field without redaction fails CI, not the next audit.
   - **Lint-enforce `httpx.SecureClient`** — forbidigo rule against `http.DefaultClient` /
     `http.Get` outside `internal/httpx`; fix the two escaped call sites.
   - **Capability-gated tool registration** — tools as a pure function of a `Capabilities`
     struct (fixes `controller_logs`-under-ArgoCD); table test asserting the exact tool set
     per config permutation; flip the k3d e2e to every merge.

## Phase 2 — quarter

1. **`incident_timeline` — server-fused chronology** (L) — phase 1: a tool fanning out to
   GitOps Changes + CloudChanges + kube_events, merged time-sorted ("14:02 [git] payments
   abc123 (+libpq bump) | 14:31 [flux] reconciled | 14:33 [event] BackOff…"). Phase 2: a
   per-investigation timeline accumulator fed from the single tool-output chokepoint, with a
   regenerated TIMELINE block pinned by the compactor's keep-list before `submit_findings` —
   survives compaction, and gives the verify pass a deterministic artifact to check RootCause
   claims against (closes the groundedness gap). Depends on Phase 1 items 1–2; hard to copy
   without the engine-agnostic Change model.
2. **Provider conformance suite** (M) — `internal/providers/providertest`: cap→sentinel,
   window honored, scope-unsupported note, standard empty-result shape; wire all existing
   providers, document "new providers must pass providertest". Fixes the sibling drift the
   audit found (awsvpc/hubble silent caps vs gcpfirewall/cloudtrail sentinels) and makes
   Azure/Loki/Datadog contributions safe. Pairs with **`internal/render`** consolidation:
   CapLines, rune-safe TruncateBytes, GroupLines with min/max timestamps, StreamIdentity,
   per-line byte cap — one canonical implementation for the suite to assert against.
3. **TimeWindow as a real contract** (L) — providers return `When`-stamped items, caller
   filters uniformly; `since_minutes` on `kube_events`; window-honored assertion in
   providertest. Do while there are ~5 change-producing providers, not 15.
4. **Backend-flavor exploitation** (M) — probe `/api/v1/status/buildinfo` once, expose
   `Flavor()`; when VictoriaMetrics: append MetricsQL guidance to tool descriptions
   (`outliersk` for anomaly-only series, `default 0` gap-fill), rank `discover_metrics` by
   `/api/v1/status/tsdb` series counts, add a `stream_context` param to `query_logs`
   (lines around the first occurrence — the crash-context view for evicted pods). Mostly
   prompt-text per line of code; a credible "deep Victoria integration" quarter bullet.
5. **Backlog worth scheduling**: Matrix linkification parity with Slack; markdown
   image-beacon defense on GitHub surfaces; digest-pinned base images; Hubble/gRPC TLS
   option; `kube_events` newest-first paging; `pod_status` restart counts + timestamps;
   coalesced-batch constituent preservation; ownerReferences walk + live-vs-Git drift
   detection; remove the legacy `network:{url}` shim before real adoption.

## Explicitly rejected

- **Deterministic seed context pack** — refuted in verification: prompt caching already
  amortizes the cost the proposal targets. Revisit only if a non-caching provider matters.
