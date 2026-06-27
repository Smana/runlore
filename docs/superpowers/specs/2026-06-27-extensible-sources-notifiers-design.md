# Extensible sources & notifiers — typed adapter registries

Status: proposed
Date: 2026-06-27
Scope: `internal/source/` (new), `internal/notify/`, `internal/trigger/`, `internal/investigate/`, `internal/config/`, `internal/server/`, `cmd/lore/main.go`

## Goal

Make RunLore **easily extensible with new event sources and notifiers**: adding one
should be *dropping a single self-registering file* — no edits to `cmd/lore/main.go`
wiring and no edits to the central `config.Config` struct. The bar is *"easy to add a
typed adapter that yields a good investigation seed"*, not *"accept every event format"*.

This is a refactor of the intake (left edge) and delivery (right edge); the
investigation loop and learning loop are untouched.

> **Release gate (hard constraint).** A release is cut by pushing a `v*` git tag,
> which fires `.github/workflows/build-image.yml` (`push: tags: ['v*']`). **No `v*`
> tag is pushed until this refactor lands.** Branch pushes still build dev images;
> that is not a release. Only `v0.1.0` exists today.

## Problem — extension today requires editing core wiring

Two event sources enqueue investigations, each with **bespoke, non-shared** wiring:

- **Alertmanager** — `POST /webhook/alertmanager` handled inline in
  `internal/server/server.go:369-424`: bearer auth, 1 MB body cap, `trigger.ParseAlertmanager`
  → `config.Incident`, `engine.Decide` (policy + dedup), resolved→outcome-ledger,
  coalesce-or-enqueue. All hand-wired into one method.
- **GitOps failures** — an in-cluster dynamic informer (Flux `WatchKustomizations`,
  Argo CD `WatchApplications`) → `providers.FailureEvent` channel →
  `investigate.DrainFailures` (`internal/investigate/failures.go:37`) → cascade-suppress
  + dedup + debounce → enqueue. Wired in `cmd/lore/main.go` via `startGitOpsFailureWatch`.

Notifiers are better — they share `providers.Notifier.Deliver` (`internal/providers/providers.go:278`)
and fan out via `notify.Multi` — but selection is a hand-written `buildNotifier`
(`cmd/lore/main.go:823-842`) that must be edited for every new sink, and each sink's
config is a statically-typed field on `config.Config` (`notify.slack`, `notify.matrix`).

Net: adding a Datadog source or a Teams notifier means editing `main.go`, the central
`Config` struct, and the server — three core files — and re-implementing cross-cutting
concerns (auth, body cap, policy, dedup) or remembering to call the right helpers. That
is the opposite of "drop one file".

The good news: everything already converges on **one normalized type**,
`investigate.Request` (`internal/investigate/investigate.go:32`), with a `Source` enum.
That seam is the foundation; this design generalizes it.

## Non-goal — CloudEvents as the extension mechanism

CloudEvents was evaluated as the ingress standard and **deliberately rejected as the
centerpiece**, because measured against RunLore's intent (run investigations, find root
causes) it standardizes the wrong layer:

1. **It standardizes the envelope, not the semantics RunLore needs.** The pipeline and
   trigger policy consume `Workload{namespace,kind,name}`, `Severity`, `Environment`,
   `Fingerprint`, `Labels`. CloudEvents provides `id/source/type/specversion/time` — none
   of which RunLore uses — and puts everything that matters in an opaque `data` blob you
   still parse per source. It does *zero* work toward extracting a workload + symptom.
2. **RunLore is a terminal sink, not a node in an event mesh.** CloudEvents' interop value
   accrues to routers (Argo Events, Knative, brokers) that forward events; RunLore consumes
   and stops.
3. **The source set is small and high-value, not long-tail.** Alerts (Prometheus —
   dominant), GitOps reconcile failures (native, the differentiator), and later
   Datadog/Sentry/PagerDuty — ~5 ever. Each deserves a *typed* adapter precisely because
   investigation quality depends on extracting the workload/severity well; a generic
   blob-into-prompt yields worse root causes.
4. **A generic ingress fights RunLore's noise control.** Trigger policy, dedup, coalesce,
   cascade-suppress, and debounce all key off *structured* fields an arbitrary CloudEvent
   won't reliably carry — forcing either un-filterable noise (LLM cost) or a per-source
   mapping config that is just a worse, untyped adapter.

**Forward door:** CloudEvents may later return as a *single optional escape-hatch adapter*,
`internal/source/cloudevents`, implemented exactly like any other typed adapter
(`Decode(CloudEvent) → []Request`, dispatching on `ce.type`) — explicitly second-class,
for shops already on Argo Events/Knative. The registry makes it a drop-in if ever wanted.
It is out of scope here.

## Architecture

Two symmetric registries with **core-owned transports**, so an adapter writes only the
semantic bit (bytes/object → `[]investigate.Request`) and inherits all cross-cutting
behavior.

```
internal/source/            NEW
  registry.go               Descriptor, Register(), BuildEnabled()
  webhook.go                shared HTTP transport: auth, body-cap, route, error→status, metrics
  watcher.go                shared watcher runner: drains <-chan Request into the pipeline
  pipeline.go               shared admit path: policy → dedup → coalesce/debounce → Enqueue
  alertmanager/             retrofit (Webhook.Decode)
  gitops/                   retrofit (Watcher.Watch, wraps the existing informer)
internal/notify/
  registry.go               NEW: Register(), BuildEnabled() → Multi
  slack.go, matrix.go       retrofit: self-register via init()
cmd/lore/main.go            SHRINKS: blank-import adapters; BuildEnabled; mount/start
```

### Data flow

```
  webhook POST ─▶ [core webhook transport: auth, cap, route] ─▶ adapter.Decode ─┐
                                                                                 ├─▶ ingest pipeline ─▶ Enqueue ─▶ queue
  informer/poll ─▶ [core watcher runner] ─▶ adapter.Watch ───────(Request)──────┘   (policy · dedup · coalesce/debounce · rate-limit)
```

The adapter's only job is to produce a good investigation seed. Auth, body cap, trigger
policy, dedup, coalesce, rate-limit live once in the core — no adapter can forget them or
do them inconsistently.

## Components

### Source registry & adapter contracts

```go
package source

type Kind int
const ( Webhook Kind = iota; Watcher )

// Admission preserves the two distinct intents (see Trigger policy below).
type Admission int
const ( MatchGated Admission = iota; EnableGated )

type Descriptor struct {
    Name      string            // unique; panics on duplicate at init()
    ConfigKey string            // sub-tree under `sources:` this adapter decodes
    Kind      Kind
    Admission Admission
    Path      string            // webhook kind only, e.g. "/webhook/datadog"
    Build     func(Deps) (any, error) // returns a Webhook or a Watcher
}

func Register(d Descriptor)                          // from each adapter's init()
func BuildEnabled(cfg *config.Config, d Deps) ([]Built, error)

// The only thing a contributor implements:
type Webhook interface { Decode(body []byte, h http.Header) (DecodeResult, error) }
type Watcher interface { Watch(ctx context.Context) (<-chan investigate.Request, error) }

// DecodeResult keeps Decode pure: firing alerts become Requests; resolved alerts
// become Resolutions the PIPELINE (not the adapter) routes to the outcome ledger.
type DecodeResult struct {
    Requests []investigate.Request
    Resolved []Resolution // {Fingerprint string; At time.Time}; empty for non-alert sources
}
```

`Deps` carries shared dependencies a `Build` may need (k8s client, gitops provider,
logger, raw config node, telemetry). A new alert-like webhook source = one file: a pure
`Decode` + a `Descriptor` registered in `init()`.

### Shared transports

- **Webhook transport** (`webhook.go`): one `http.HandlerFunc` factory. Given an adapter
  + its auth config it checks the bearer token (shared, constant-time, as
  `server.go:357-367` does today), caps the body at 1 MB, calls `Decode`, maps errors
  (`*http.MaxBytesError`→413, decode error→400), and feeds `DecodeResult` into the
  pipeline. The server mounts each webhook source at its `Path`.
- **Watcher runner** (`watcher.go`): starts each `Watch(ctx)` in its own goroutine and
  drains the channel into the pipeline. Reconnection/resync stays in the existing
  informer; a watcher error is logged and backed off without crashing the process or
  sibling sources; bounded channel, drop-with-log under backpressure (current behavior).

### Ingest pipeline

One admit path for every source (`pipeline.go`):

```
admit(req, desc):
  switch desc.Admission:
    case MatchGated:  if !trigger.Match(req, cfg.Triggers.Incidents) { drop }
    case EnableGated: if !cfg.Triggers.GitOpsFailures.Enabled { drop }
                      if isCascadeFailure(req) { drop }
                      debounce(req)   // re-check still-failing after window
  dedup(req)
  coalesce / rate-limit
  Enqueue(req)
route(resolved): outcome ledger
```

Cross-cutting stages (dedup, coalesce, rate-limit, resolved→ledger) run once and cover
all sources; the per-intent difference is isolated to the `Admission` switch.

### Notifier registry

`providers.Notifier.Deliver` already exists. Add `notify.Register(Descriptor{Name,
ConfigKey, Build})` called from each sink's `init()`, and `notify.BuildEnabled(cfg, Deps)
→ *Multi`, replacing the hand-written `buildNotifier`. Slack (bot + webhook) and Matrix
retrofit by self-registering. Native richness (Slack Block Kit approve/reject buttons,
`slack.go:197-211`) is preserved because each sink stays a typed adapter. A future generic
outgoing-webhook sink (and a "CloudEvents-out" option on it) is just another registered
adapter.

## Decision A — unified trigger policy across heterogeneous sources

`config.Incident` is **retired**. The alertmanager adapter's `Decode` parses Alertmanager
JSON → `[]investigate.Request` directly (removing the Incident→Request double-hop).

- **Promote `Severity` and `Environment` onto `investigate.Request`** (they already exist
  on the retired `Incident`, and independently shape the prompt and Slack formatting). The
  trigger matcher is reworked to read `Request` (severity, environment, `Workload.Namespace`,
  `Title`=alertname, labels) instead of `Incident`.
- **Two admission modes** (declared per source in its `Descriptor`):
  - `MatchGated` — incident-style matcher (alertmanager; future Datadog/Sentry — alert-like:
    "only critical/prod").
  - `EnableGated` — admitted by enable flag + cascade-suppress + debounce (GitOps; "every
    failure matters").
- Resolved alerts: `Decode` returns them as `Resolution`s; the pipeline routes them to the
  outcome ledger (side-effects stay in core, `Decode` stays pure and golden-testable).

## Decision B — config: raw-node per adapter, clean break

The central `config.Config` no longer grows per adapter. Introduce `sources: { <name>:
<raw> }` and reuse `notify: { <name>: <raw> }`; each adapter's `Build` receives its own
`yaml.Node` and decodes its typed sub-config. Adding Datadog = a new `sources.datadog:`
block, **zero edits to `Config`**. The central struct keeps only cross-cutting concerns
(`triggers`, `investigation`, `leader_election`, `model`, `catalog`, …).

**Clean break (no shims)** — justified at `v0.1.0` (one tag, no external config users to
protect):

- `notify.slack` / `notify.matrix` already fit the `notify.<name>` shape — they become
  registered adapter keys.
- A new `sources:` map replaces the implicit alertmanager wiring (`server.webhook_token_env`
  → `sources.alertmanager.token_env`) and the gitops wiring
  (`triggers.gitops_failures.enabled` → `sources.gitops.enabled`, keeping its debounce).
- The shared **match policy stays under `triggers.incidents`** as the default for all
  `MatchGated` sources (a per-source override is a future option, not now); GitOps keeps
  `triggers.gitops_failures`.
- `examples/` and `docs/` are updated to the new shape in the same change.

## Error handling

- Webhook: 401 (auth), 413 (body cap / `*http.MaxBytesError`), 400 (decode), 202
  (accepted, incl. policy-filtered), 202 (enqueued).
- Watcher: per-source goroutine; errors logged + backed off, never crash the process or
  siblings; bounded channel, drop-with-log under load.
- Startup: configured-but-invalid adapter → **fail fast** (non-zero exit, consistent with
  current `Validate()`); absent config key → adapter disabled. Duplicate `Register` name →
  panic at `init()`.
- Notifier `Multi`: unchanged — best-effort fan-out, errors joined and logged, one sink's
  failure does not block others.

## Testing

- **`Decode` is pure → golden-payload table tests** per source (real captured JSON in
  `testdata/`, assert the exact `[]Request`: workload, severity, environment, fingerprint).
  This is the investigation-quality guard.
- **Pipeline tested once, centrally:** admission modes (MatchGated drops non-matching;
  EnableGated drops cascades + debounces), dedup, coalesce, resolved→ledger routing.
- **Transports tested once** (httptest for webhook auth/cap/status mapping; fake dynamic
  client for the watcher) and cover every adapter of that kind.
- Existing slack/matrix/alertmanager/gitops tests retrofit onto the new seams and stay green.

## Isolation / boundaries

- `internal/source` — registry + transports + pipeline. Depends on `investigate`
  (Request, Enqueuer), `trigger` (matcher), `config`, `telemetry`.
- `internal/source/<name>` — one package per adapter; depends only on `internal/source`
  interfaces + `investigate.Request` + its own client lib; self-registers via `init()`.
  Readable and testable without reading the core.
- `internal/notify` + `internal/notify/<name>` — symmetric.
- `cmd/lore/main.go` — blank-imports adapter packages (init side-effects), calls
  `source.BuildEnabled` + `notify.BuildEnabled`, mounts webhook routes + starts watchers.
  The bespoke `handleAlertmanager` body, `startGitOpsFailureWatch` wiring, and
  `buildNotifier` collapse to a few lines. The server keeps its non-source routes (slack
  interactions, actions approve/reject, healthz/readyz, metrics).

## Sequencing (headline; full task breakdown is the implementation plan)

1. Introduce registry + transports + pipeline with the two existing sources retrofitted
   behind them — behavior-preserving, tests green.
2. Retire `config.Incident`, promote `Request` fields, unify policy (Decision A).
3. Config clean break to `sources.<name>` / `notify.<name>` (Decision B); update
   `examples/` and `docs/`.
4. Notifier registry retrofit (Slack, Matrix self-register).
5. Prove extensibility with one new drop-in adapter (e.g. a generic outgoing-webhook sink).

Each step keeps the suite green. No `v*` tag is pushed at any point.
